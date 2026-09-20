# urlshort — Implementation Design

Status: design finalized for implementation review.
Target: a small, robust, configurable URL shortener for the DragonFly monitors,
written in Go, served directly on the public internet (no reverse proxy),
running on Linux and DragonFly BSD.

This document is the single source of truth for the implementation. The
"Critical checkpoints and caveats" section is mandatory reading before writing
code; each item has an ID so reviews and commits can reference it.

---

## 1. Purpose and scope

### Goals

- One Go binary that serves both the redirector and the management API on a
  single domain.
- Configurable, meaningful short keys via ordered, operator-defined mapping
  rules; fall back to a random key when nothing matches.
- Namespace-scoped bearer tokens so each monitor only manages its own links.
- Automatic certificate issuance and renewal via ACME with the `tls-alpn-01`
  challenge.
- Persistent link storage that cross-compiles with `CGO_ENABLED=0` for Linux
  and DragonFly amd64.
- Daily-rotated JSONL access logs with bounded retention.

### Non-goals (v1)

- No web UI, no CLI admin tool, no Prometheus metrics.
- No batch API.
- No wildcard certificates (tls-alpn-01 cannot issue them).
- No config hot-reload; restart to apply config changes.
- No server-side fetching/validation of target URLs.
- No target-host allowlist (deferred).

### Intended integration

Monitors call the API before posting a message and substitute the returned
`short_url`. The bot (`dflybot`) is not modified and does not rewrite URLs.
The shared `monitor/` module gets a small optional client helper later; the
shortener itself is standalone.

---

## 2. Glossary

- **key**: the short path stored in the database, always begins with `/`,
  e.g. `/g/d/728aaaa`.
- **target**: the canonical full URL the key redirects to.
- **namespace**: a path prefix owned by a client, e.g. `/g/`. A client may
  create/update/delete only keys that have the prefix.
- **rule**: an ordered mapping from a target URL to a key, using a regexp and a
  key template.
- **client**: a configured API caller with tokens and namespaces.

---

## 3. Critical checkpoints and caveats

These are the non-obvious constraints that will otherwise cause bugs or
production incidents. Implement and test each one explicitly.

- **C1. Storage is pure Go.** Use `go.etcd.io/bbolt`. Do not introduce CGO and
  do not use `modernc.org/sqlite`: its `modernc.org/libc` dependency has no
  DragonFly support, so `GOOS=dragonfly CGO_ENABLED=0` fails. Do not use
  `mattn/go-sqlite3` either, since it breaks the project's
  `CGO_ENABLED=0` cross-build.
- **C2. The TLS handshake deadline is `min(ReadHeaderTimeout, ReadTimeout,
  WriteTimeout)` over positive values.** `net/http` computes it this way
  (verified in `net/http/server.go:tlsHandshakeTimeout`). This design does not
  expose `ReadHeaderTimeout`, so the effective handshake budget is
  `min(read_timeout, write_timeout)` = 30 s. Never assume the separate ACME
  client timeout (`acme.issue_timeout`) extends the handshake budget.
- **C3. `tls-alpn-01` only.** `autocert` uses `tls-alpn-01` exclusively unless
  `Manager.HTTPHandler` is called; calling it enables `http-01`. Only call it
  when `acme.http01_fallback = true`.
- **C4. ACME validation ports are fixed by the CA.** `tls-alpn-01` is always
  validated on port 443; `http-01` on port 80. A non-443 `https_port` or
  non-80 `http_port` makes ACME fail. Validate this at startup.
- **C5. `account_file` sets `Manager.Client.Key`.** Load the PEM private key,
  parse it to `crypto.Signer`, and assign it. autocert then never generates or
  overwrites an account key. Never write to `account_file`. Certificate and
  other state still live in `cache_dir`.
- **C6. Bind IPv4 and IPv6 as separate sockets.** Set `IPV6_V6ONLY=1` on the v6
  socket via `net.ListenConfig.Control` when both families are configured.
  DragonFly is always v6-only, so the flag is a no-op there; on Linux it
  prevents the `0.0.0.0`/`::` `EADDRINUSE` conflict. Never silently ignore a
  bind error.
- **C7. `public_url` is the only source of truth for generated URLs.** Never
  build `short_url` from the request `Host`. Validate the request `Host`
  against `public_url` plus `extra_hosts`, and reject mismatches with `421`.
- **C8. Rules are global and canonical; writes are namespace-scoped.** One
  target maps to one key regardless of caller. A rule key outside the caller's
  namespace yields `403`, not a random fallback.
- **C9. Hash extension never mutates existing keys.** Resolve and insert inside
  one bbolt `Update` transaction. Once a key is published it must remain
  stable forever.
- **C10. Reserved namespace.** `/.api`, `/.well-known/`, `/` (the home page),
  `/robots.txt`, and `/favicon.ico` are reserved. Rule and explicit keys may
  not contain a path segment starting with `.` or `~`, and may not equal `/`,
  `/robots.txt`, or `/favicon.ico`; random keys always start a segment with
  `~`. An existing `/` link is shadowed by the home page and becomes
  unreachable.
- **C11. No synchronous hit counter.** Redirects must not write to bbolt.
  Derive hit counts from access logs if ever needed.
- **C12. Constant-time token comparison.** Hash presented and configured tokens
  with SHA-256 and compare digests with `crypto/subtle`, without early exit
  over clients.
- **C13. Trust forwarding headers only from trusted proxies.** The direct peer
  (`RemoteAddr`) must be in `server.trusted_proxies` (default localhost). When
  it is, use `X-Forwarded-For` (walked right-to-left, skipping trusted proxies)
  and then `X-Real-IP`; otherwise use `RemoteAddr`. Never trust these headers
  from an untrusted peer.
- **C14. Access logging cannot stall requests.** Use a bounded channel and a
  single writer goroutine; drop and count on overflow.
- **C15. Pre-warm uses a regular SNI and normal ALPN, not `acme-tls/1`.**
  Call `Manager.GetCertificate` directly with a synthetic
  `tls.ClientHelloInfo` (see §12.4). The ALPN list must be normal
  (`h2`/`http/1.1`); `acme.ALPNProto` only serves active challenge token
  certs and would fail otherwise.
- **C16. Shutdown order.** Stop accepting, drain with `shutdown_timeout`, close
  bbolt, flush and close access logs, then exit.
- **C17. Certificate type and pre-warm hello.** autocert's `supportsECDSA`
  returns `false` when the `ClientHelloInfo` has nil `SignatureSchemes`,
  `SupportedCurves`, and `CipherSuites`, so a bare hello makes it issue an
  **RSA** certificate. Pre-warm with a synthetic ECDSA-capable hello so the
  cached certificate matches what modern clients negotiate; otherwise the
  first real client triggers a second issuance. Certificate metadata for
  `/status` must come from the served certificate (wrapper for real
  handshakes, return value for pre-warm), not from a separate parse path.
- **C18. ACME internal timeout.** `autocert.GetCertificate` runs issuance
  under its own 5-minute context and cannot be canceled. Bound each ACME HTTP
  call with `acme.Client.HTTPClient.Timeout = acme.issue_timeout` and run
  pre-warm in a goroutine so it never blocks startup or serving.
- **C19. bbolt has no online compaction.** `bbolt.Compact(dst, src, txMaxSize)`
  writes a new compacted database and never modifies `src`. That is what makes
  a compacted backup safe while the service keeps serving.
- **C20. A compacted snapshot is consistent, not point-in-time.** It reflects
  the source at the moment the internal read transaction starts; writes after
  that are absent from the copy. That is fine for a backup.
- **C21. Keep `.part`/temp files in the same directory as their final path.**
  The rename is then same-filesystem and atomic. On any failure, remove the
  temp file and leave the live database untouched.
- **C22. Verify before publishing.** Reopen the compacted file, drain
  `Tx.Check()`, and compare `links`/`targets` counts. Abort on any error
  (see §6.6.3).
- **C23. Startup backups use second-granular filenames.** Scheduled daily
  backups are date-granular (`links-YYYY-MM-DD.db`) and idempotent; startup
  backups are second-granular (`links-YYYY-MM-DDTHHMMSS.db`) so repeated
  restarts to test config do not overwrite each other. Both carry the same
  `YYYY-MM-DD` prefix used by retention.
- **C24. HTTP/2 is explicit and controlled.** HTTP/2 over TLS is enabled by
  default via `server.http2_enabled`. The TLS config passed to
  `tls.NewListener` must list `h2` in `NextProtos`; with `http.Server.TLSConfig`
  left nil, `net/http` installs the h2 handler automatically. Disabling
  requires removing `h2` from ALPN and setting a non-nil `TLSNextProto` without
  an `h2` entry.
- **C25. Trusted proxies gate forwarding headers.** `server.trusted_proxies`
  (default `127.0.0.0/8` and `::1/128`) lists the peers whose
  `X-Forwarded-For`/`X-Real-IP` headers are trusted. `X-Forwarded-For` is
  walked right-to-left, skipping trusted proxies, so a client-injected value
  cannot spoof the result; otherwise `RemoteAddr` is used. The extracted IP
  drives the access log and rate limiting.
- **C26. HSTS is opt-in and HTTPS-only.** `server.hsts.enabled` (default false)
  requires `https_port > 0`; the header is only set on HTTPS responses.
  `preload` requires `include_subdomains = true` and `max_age >= 31536000`.
- **C27. Home page and API index.** `GET /` returns the program name and
  version plus a link to the API; `/` is therefore reserved (C10).
  `GET /.api/v1` (and `/.api/v1/`) returns an unauthenticated JSON index that
  lists the method and path of every API operation.

---

## 4. Architecture and process model

Single process, several long-lived components plus logging:

```
             +---------------------+
  :80  ----->| HTTP redirector     |---> 308 to https://<validated host><path>
             | + ACME http-01 opt  |
             +---------------------+

             +---------------------+
  :443 ----->| HTTPS mux           |
             |  /.api/v1/*  ---> API handlers (auth, namespace checks)
             |  /.well-known/* -> 404
             |  /robots.txt, /favicon.ico
             |  else         ---> redirect handler (bbolt read)
             +---------------------+
                      |
                      v
             +---------------------+
             | bbolt links.db      |
             +---------------------+

             +---------------------+
             | accesslog writer    |  bounded channel -> JSONL daily files
             +---------------------+

             +---------------------+
             | autocert manager    |  cert cache under <data_dir>/acme
             +---------------------+
```

- `GET /` serves the home page; `GET /.api/v1` serves the API index (both
  unauthenticated). `/health` is also unauthenticated; other API endpoints
  authenticate.
- The redirect path is read-only: one `db.View` per request, no writes.
- Links are created/updated/deleted only through the API, in `db.Update`
  transactions.
- A maintenance goroutine (§6.6) produces the daily compacted backup.

---

## 5. Configuration

Full reference. The sample file shipped in the repo uses `???` placeholders for
all tokens.

```toml
# Application log level: debug, info, warn, error
log_level = "info"

# Storage root. Must end with a slash.
# Contains: links.db, acme/, logs/access-YYYY-MM-DD.jsonl
data_dir = "./data/"

[server]
# Listen addresses (IP literals or hostnames). IPv4 and IPv6 are bound
# as separate sockets (C6).
listen_addresses = ["0.0.0.0", "::"]
# 0 disables the listener. At least one of http_port/https_port must be > 0.
http_port  = 80
https_port = 443

# Mandatory. Scheme + host only. No path, query, fragment, userinfo, or
# trailing slash. ASCII/punycode host. Examples:
#   https://example.com
#   http://127.0.0.1:8080      (ACME must be disabled if no domain name)
public_url = "https://example.com"

# Extra hostnames accepted for redirects (C7). If ACME is enabled they are
# also added to autocert's HostPolicy, so a certificate can be issued on
# demand. The generated short_url still always uses public_url.
# extra_hosts = ["www.example.com"]

# Manual TLS certificate and key. Required when acme.enabled = false and
# https_port > 0. Must be absent when acme.enabled = true.
# cert_file = "/etc/ssl/example.com/fullchain.pem"
# key_file  = "/etc/ssl/example.com/privkey.pem"

# HTTP server timeouts, in seconds. read_timeout and write_timeout also
# bound the TLS handshake (C2).
shutdown_timeout = 10
read_timeout     = 30
write_timeout    = 30
idle_timeout     = 60
# Soft request-header limit. Go adds 4096 bytes of bufio slop.
max_header_bytes = 8192

# HTTP/2 over TLS via ALPN. Enabled by default.
http2_enabled = true

# IPs/CIDRs of trusted reverse proxies or CDNs. X-Forwarded-For and X-Real-IP
# are honored only when the direct peer is in this list; X-Forwarded-For is
# walked right-to-left, skipping trusted proxies. Defaults to localhost.
trusted_proxies = ["127.0.0.0/8", "::1/128"]

[server.hsts]
# Strict-Transport-Security header. Only valid when https_port > 0.
enabled = false
# max-age in seconds; 31536000 (1 year) is the conventional value.
max_age = 31536000
# Add "; includeSubDomains".
include_subdomains = false
# Add "; preload"; requires include_subdomains = true and max_age >= 31536000.
preload = false

[acme]
enabled = true
# Let's Encrypt production. Staging:
# https://acme-staging-v02.api.letsencrypt.org/directory
directory_url = "https://acme-v02.api.letsencrypt.org/directory"

# Optional pre-existing ACME account key (PEM; EC P-256 preferred, RSA also
# accepted). When set, autocert uses it and never generates or overwrites
# one (C5). When unset, autocert generates an ECDSA P-256 key and stores it
# in cache_dir under the name "acme_account+key".
# account_file = "/etc/ssl/example.com/acme-account.pem"

# Contact address for CA expiry/problem notices. Optional but recommended.
email = "admin@example.com"
accept_tos = true

# Certificate/account cache. Default: <data_dir>/acme. Must persist and be
# backed up; losing it without account_file creates a new ACME account and
# can hit CA rate limits.
cache_dir = ""

# Renew this many days before expiry. Must be > 0 and < 90.
renew_before_days = 30

# Also allow the http-01 challenge as a fallback. Requires http_port == 80.
http01_fallback = false

# Timeout for each ACME HTTP request (not the whole issuance order) (C18).
issue_timeout = 60

[access]
# Per-source-IP limits for redirects (IPv4 /32, IPv6 /64), and per-token
# limits for the API. Allowed rates are in requests/second.
redirect_rate  = 20
redirect_burst = 40
api_rate       = 5
api_burst      = 10

[access_log]
retention_days = 30
flush_interval = 5      # in seconds

[backup]
# Daily compacted backup of links.db.
enabled = true
# Default: <data_dir>backup/. Must end with "/".
dir = ""
# UTC hour [0,23] at which the daily job runs.
hour_utc = 3
# On startup, always produce a startup backup.
run_on_start = true
# Delete backups older than this many days (0 disables deletion).
retention_days = 30
# Optional cap on the number of backup files (0 = unlimited); bounds a
# restart loop that produces many same-day startup backups.
retention_count = 3
# Compaction transaction size limit in bytes; bounds memory during compaction.
compact_tx_max_bytes = 1048576

# Short-name substitutions usable as {{ abbrev .group }} in rule templates.
# Unknown names are lowercased and passed through unchanged.
[abbreviations]
DragonFlyBSD = "dfbsd"
DeltaPorts   = "dp"
dragonfly    = "d"

# Ordered mapping rules. First full-string match wins.
[[rules]]
name  = "github-pr"
match = '''^https://github\.com/(?P<org>[^/]+)/(?P<repo>[^/]+)/pull/(?P<num>[0-9]+)$'''
key   = "/gh/{{ abbrev .org }}/p/{{ .num }}"

[[rules]]
name  = "github-issue"
match = '''^https://github\.com/(?P<org>[^/]+)/(?P<repo>[^/]+)/issues/(?P<num>[0-9]+)$'''
key   = "/gh/{{ abbrev .org }}/i/{{ .num }}"

[[rules]]
name     = "gitweb-commit"
match    = '''^https://gitweb\.dragonflybsd\.org/(?P<repo>[^/]+?)(?:\.git)?/(?:commit|commitdiff)/(?P<sha>[0-9a-f]{40})$'''
key      = "/g/{{ abbrev .repo }}/{{ .sha }}"
hash        = "sha"   # capture group to auto-extend for uniqueness
hash_minlen = 8       # minimum prefix length (>= 4)

# API clients. Each client must own at least one namespace unless admin.
[[clients]]
enabled    = true
name       = "git-monitor"
namespaces = ["/g/"]
# Plaintext tokens (min 32 chars) and/or SHA-256 hex digests (64 chars).
# Both lists are optional, but at least one entry across both is required.
tokens        = ["???"]
tokens_sha256 = []

[[clients]]
enabled    = true
name       = "github-monitor"
namespaces = ["/gh/"]
tokens        = ["???"]
tokens_sha256 = []

[[clients]]
enabled = true
name    = "admin"
admin   = true
tokens  = ["???"]
```

### 5.1 Validation rules

Startup must fail (exit non-zero, clear message) if any of these fail.

| Area | Condition |
|---|---|
| `public_url` | present; parses; scheme `https` (requires `https_port > 0`), or `http` only when `https_port = 0` and ACME disabled; host non-empty and ASCII; no path other than `/`; no query, fragment, userinfo, trailing slash |
| ports | `http_port > 0` or `https_port > 0` |
| `acme.enabled` | `https_port == 443` (C4) |
| `acme.enabled` + `http01_fallback` | `http_port == 80` (C4) |
| `acme.enabled` | `accept_tos = true`; `directory_url` non-empty; `cert_file`/`key_file` empty |
| ACME disabled + `https_port > 0` | `cert_file` and `key_file` set and readable |
| `account_file` | present and parseable as PEM `crypto.Signer` (EC P-256 preferred, RSA supported); never written |
| `renew_before_days` | `> 0` and `< 90` |
| `server.*_timeout` | non-negative; `read_timeout`, `write_timeout` > 0 when `https_port > 0` (C2) |
| `max_header_bytes` | `>= 4096` (default 8192) |
| `http2_enabled` | boolean (default true) |
| `trusted_proxies` | each entry is an IP or CIDR; a prefix length of 0 warns (trusts every peer) |
| `hsts` | `enabled` requires `https_port > 0` and `max_age > 0`; `preload` requires `include_subdomains` and `max_age >= 31536000` |
| `data_dir` | present, ends with `/`, creatable, writable |
| `backup` | `dir` (default `<data_dir>backup/`) ends with `/`, creatable and writable; `hour_utc` in [0,23]; `retention_days >= 0`; `retention_count >= 0`; `compact_tx_max_bytes > 0` |
| rules | unique `name`; regexp compiles and cannot match the empty string; template parses; `key` starts with `/`; `hash` names an existing capture group; `hash_minlen >= 4` when `hash` set and not set otherwise |
| clients | unique `name`; `enabled` clients have at least one token; plaintext tokens >= 32 chars; `tokens_sha256` entries are 64-char lowercase hex; no token/digest appears in more than one client; enabled non-admin clients have >= 1 namespace |
| namespaces | match `/seg/` (leading and trailing slash), no `..`, no reserved prefix; warn if two clients overlap |

---

## 6. Storage (bbolt)

### 6.1 File and buckets

```
<data_dir>/links.db          mode 0600, dir 0700
  meta    : "schema_version" -> uint32
  links   : key   -> JSON link record
  targets : target -> key
```

Link record:

```json
{
  "target": "https://github.com/DragonFlyBSD/DragonFlyBSD/pull/56",
  "rule": "github-pr",
  "owner": "github-monitor",
  "created_at": "2026-09-11T05:00:00Z",
  "updated_at": "2026-09-11T05:00:00Z"
}
```

### 6.2 Invariants

- `links[key].target` is the canonical target.
- `targets[target] == key` exactly when `links[key]` exists; both are written
  in the same transaction.
- `key` is globally unique (dictated by `links`).
- A target maps to at most one key globally (dictated by `targets`).

### 6.3 Operations

- **Resolve(target) -> key**: read `targets`; returns existing key if present.
- **Create(target, rule, keyCandidate)**: in one `Update`, check `targets`,
  then `links` for the candidate (hash extension loop for rules with `hash`),
  then insert both. Re-check `targets` inside the transaction to remain
  idempotent under concurrency.
- **Get(key)**: read `links`.
- **List(prefix, limit, cursor)**: forward cursor scan of `links` while
  `bytes.HasPrefix`; `cursor` is the last returned key; `limit` capped at 1000.
- **Update(key, target)**: in one `Update`, ensure the new target is free or
  maps to `key`, remove the old `targets` entry, write the new one, bump
  `updated_at`.
- **Delete(key)**: in one `Update`, remove `links[key]` and its `targets`
  entry.

### 6.4 Open options

- `bolt.Open(path, 0600, &bolt.Options{Timeout: time.Second})`.
- No manual `NoSync`; default durability is fine at this write rate.
- Schema version in `meta`. If the file's version is newer than the binary,
  refuse to start. If older, run the migration chain.
- File size does not shrink on delete. See §6.6 for the automated compacted
  backup. `Tx.WriteTo` produces a full-size (non-compacted) copy; use
  `bbolt.Compact` for backups.

### 6.5 Counters

Do not store hit counts (C11). The `/status` endpoint computes link counts on
demand by prefix scan and may cache the result for 60 s.

### 6.6 Automatic compacted backup

The maintenance job produces a compacted, verified copy of the live database
at `<backup.dir>links-...db`. The live database is never modified and writes
are not blocked. The backup content is the compacted database, so backups stay
small even when the live file has free pages.

#### 6.6.1 Scheduling

- One maintenance goroutine owns the job; a mutex prevents overlap.
- When `backup.enabled = false`, no backups and no retention cleanup run.
- Scheduled backup: compute the next run as the next `backup.hour_utc`; use a
  timer, not a ticker, and recompute after each run. Use a date-granular
  filename and skip when that day's file already exists.
- Startup backup: when `backup.run_on_start`, always produce a backup shortly
  after startup (not only when older than 24 h), using a second-granular
  filename so repeated restarts do not overwrite each other (C23).
- On shutdown, stop scheduling and wait for the in-flight job up to
  `shutdown_timeout`.

#### 6.6.2 Procedure

1. Ensure `backup.dir` exists (mode 0700). Default `<data_dir>backup/`.
2. Name the file by trigger:
   - scheduled daily backup: `links-YYYY-MM-DD.db`; if it exists and verifies,
     skip;
   - startup backup: `links-YYYY-MM-DDTHHMMSS.db` (UTC, second granular); every
     startup gets its own file (C23).
   Both forms carry the `YYYY-MM-DD` prefix used by retention.
3. `part := final + ".part"`. Remove any stale `part`.
4. `dst, err := bolt.Open(part, 0600, &bolt.Options{Timeout: time.Second})`.
5. `err = bolt.Compact(dst, live, compact_tx_max_bytes)`. This reads the live
   database through a consistent MVCC snapshot (`src.View`); concurrent writes
   are not included in the snapshot and are not disturbed.
6. `dst.Sync()`; `dst.Close()`.
7. Verify (§6.6.3). On failure: remove `part`, leave the live DB untouched.
8. `os.Rename(part, final)`; then fsync the backup directory.
9. Update `last_backup_at` and apply retention (§6.6.4).

- `part` lives in the same directory as `final`, so the rename is
  same-filesystem and atomic (C21).
- The snapshot is not point-in-time after its read transaction starts; that is
  acceptable for a backup (C20).
- Handle `ENOSPC` by removing `part` and logging; never touch the live DB.
- The bbolt store exposes a `CompactTo(path)` operation (it owns the live
  handle); the maintenance goroutine handles scheduling, directories,
  verification, and retention.

#### 6.6.3 Verification

Reopen the compacted file read-only and:

1. Drain `tx.Check()` and fail on any error (page references, freelist,
   branch/leaf consistency).
2. Require `targets` count == `links` count and, when the database is small
   (say < 100k keys), verify referential integrity for every entry
   (`targets[links[k].target] == k`) (C22).

#### 6.6.4 Retention

- List `links-*.db` in `backup.dir` and parse the leading `YYYY-MM-DD` (both
  filename forms share it). Delete files older than `backup.retention_days`
  (0 disables). Also remove `*.part` older than one day.
- Optional `backup.retention_count` caps the number of backup files (0 =
  unlimited). This bounds a restart loop that produces many same-day startup
  backups.
- Retention runs after a successful backup and at startup.
- Deletion failures only warn; never fail startup or the job.

#### 6.6.5 Failure handling and observability

- Any error leaves the live database untouched.
- Record `last_backup_at`, `last_backup_ok`, and the latest error in memory;
  expose them in `/.api/v1/status` under a `maintenance` object.
- Log an info line on success (size before/after, duration, record counts) and
  a warning on failure.

#### 6.6.6 Caveats

- A compacted backup is a valid bbolt database; restore by stopping the service
  and replacing `links.db`.
- `Tx.WriteTo` produces a full-size (non-compacted) copy; use `Compact` for
  backups.
- `backup.dir` may be a separate filesystem. The `.part` rename always stays
  inside it, so publishing is atomic; just ensure it has room for at least one
  compacted database plus retention.
- Because every startup writes a backup, a crash loop can accumulate files
  quickly; `retention_count` bounds this.
- An optional startup integrity check of the live database is future work.

---

## 7. Mapping rules engine

### 7.1 Matching

- Compile each rule's `match` as RE2 (`regexp.Compile`).
- Reject patterns that can match the empty string at startup.
- Match the **entire** canonical target string. If `FindStringSubmatchIndex`
  does not cover `[0, len(target))`, the rule does not match.
- First matching rule wins; document that order is significant.

### 7.2 Canonicalization

Before matching and before storage:

1. `url.Parse`; reject parse errors.
2. Require scheme `http` or `https` (case-insensitive); lowercase it.
3. Reject URLs with userinfo.
4. Lowercase the host, drop a default port (`:80` for http, `:443` for https).
5. Keep path, query, and fragment exactly as given.
6. Re-serialize with `url.URL.String()`.

`http://` and `https://` of the same host/path are different targets. Path
case is significant.

### 7.3 Key template

- Data is a `map[string]string` of named capture groups, rendered with
  `text/template` and a restricted `FuncMap`:
  - `abbrev`: look up `[abbreviations]`; if absent, return `strings.ToLower(s)`.
  - `lower`: `strings.ToLower`.
  - `printf` and other safe builtins.
- No `{{template}}`/`{{define}}`, no file/env access.

### 7.4 Hash extension

For a rule with `hash = "<group>"` and `hash_minlen = N`:

```
for L := N; L <= len(hashValue); L++ {
    vars[hashGroup] = hashValue[:L]
    key := render(vars)
    if !keyExistsForDifferentTarget(key) { return key }
}
return error("no unique hash prefix")
```

- The check and the following insert happen in the same bbolt `Update`
  transaction (C9).
- Existing keys are never rewritten.
- If all lengths collide, return `409` and log an operator-actionable error.

Trace: commit A `728aaaa0111...` gets `/g/d/728aaaa`. Later commit B
`728aaaa0222...` renders the same 8-char candidate, finds it taken, retries at
9 characters, gets `/g/d/728aaaa0`. Both remain valid under exact lookup.

### 7.5 Generated-key validation

After rendering, validate:

- starts with `/`;
- length `1..256`;
- characters `[A-Za-z0-9/._~-]` only;
- no `..` segment;
- no `%`, `?`, `#`, whitespace, or control characters;
- no path segment starts with `.` or `~` (C10);
- not exactly `/`, `/robots.txt`, or `/favicon.ico` (reserved paths, C10).

Rules that generate invalid or too-long keys are treated as operator errors:
`500` on create, with the rule name and rendered key in the server log.

### 7.6 Rule changes

Rules are consulted only for new targets. Existing links keep their keys. Do
not attempt automatic migration when config changes.

---

## 8. Random fallback

When no rule matches:

- Generate 12 characters from `[A-Za-z0-9]` using `crypto/rand` (no external
  dependency). Base62 length 12 is about 71 bits.
- Key shape: `<first namespace of the requesting client>~<id>`, e.g.
  `/g/~Xk3n9Qz1Lm4p`. `~` is RFC 3986 unreserved and is a rule-forbidden
  segment prefix, so random and rule keys cannot collide structurally (C10).
- The insert still re-checks `links` inside the transaction and regenerates on
  the astronomically unlikely collision.
- A non-admin client with multiple namespaces uses the first one. Document
  that order in the config file.

---

## 9. HTTP layer

### 9.1 Listeners

- Build one listener per (`listen_addresses` x enabled ports).
- Bind IPv4 and IPv6 separately; set `IPV6_V6ONLY=1` on v6 when both are
  configured (C6). Never swallow bind errors.
- HTTPS: create a `tls.Config` from the autocert manager (or the manual cert),
  wrap the listener with `tls.NewListener`, and serve.
- HTTP/2 over TLS is negotiated via ALPN when `server.http2_enabled` (default
  true). List `h2` in the TLS `NextProtos`; `http.Server.TLSConfig` stays nil so
  `net/http` installs the h2 handler. When disabled, remove `h2` from ALPN and
  set a non-nil `TLSNextProto` without an `h2` entry.
- Run each server in its own goroutine and report fatal serve errors to a
  channel that triggers shutdown.

### 9.2 Request gating (all requests)

- Reject absolute-form, asterisk-form, and authority-form request targets
  (`CONNECT`); only `GET`, `HEAD`, and (for the API) the API methods.
- `MaxHeaderBytes = server.max_header_bytes`.
- Validate `Host` (strip port, case-insensitive) against `public_url` host plus
  `extra_hosts`. Mismatch -> `421 Misdirected Request` (C7).
- Middleware order: recover panic -> request ID -> host check -> body limit ->
  rate limit -> access log -> route.

### 9.3 HTTP (port 80) behavior

- If `http01_fallback`, pass `/.well-known/acme-challenge/` to
  `manager.HTTPHandler`.
- All other paths: `308 Permanent Redirect` to
  `https://<validated host><requestURI>`. Preserve the host so `extra_hosts`
  work; it is safe because the host was validated already (C7).
- If `https_port = 0`, port 80 is not a redirector and behaves like the main
  handler (only allowed when ACME is disabled).

### 9.4 HTTPS routing

Order matters:

1. `/.api/...` -> API mux. `GET /.api/v1` (and `/.api/v1/`) returns the
   unauthenticated API index; all other endpoints authenticate.
2. `/.well-known/...` -> `404`.
3. `/` -> `200` home page (GET/HEAD only).
4. `/robots.txt` -> `200`, body `User-agent: *\nDisallow: /\n` (GET/HEAD only).
5. `/favicon.ico` -> `204` (GET/HEAD only).
6. anything else -> redirect handler.

### 9.5 Redirect handler

- `GET`/`HEAD` only. Others -> `405` with `Allow: GET, HEAD`.
- Exact key lookup in `links` (no trailing-slash normalization, query and
  fragment of the incoming URL are ignored).
- Found: `302 Found` with `Location: <target>`, `Cache-Control: no-store`,
  `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`.
  `HEAD` has the same status and headers with no body.
- Not found: minimal `404` page; set `Cache-Control: no-store`.

`302` (not `301`) keeps targets mutable and avoids permanent browser caches.

---

## 10. REST API

Base path `/.api/v1`. All endpoints except `/health` and the API index require
`Authorization: Bearer <token>`. JSON in and out. Body limit 64 KiB,
`DisallowUnknownFields`.

| Method | Path | Auth | Description |
|---|---|---|---|
| GET | `/.api/v1` | none | API index (name, version, method/path of every operation) |
| GET | `/health` | none | liveness |
| GET | `/status` | admin | status and statistics |
| GET | `/whoami` | any | caller identity |
| POST | `/links` | any | resolve or create a key for a target |
| GET | `/links?key=` | any | fetch one link |
| GET | `/links?namespace=&limit=&cursor=` | any | list links by prefix |
| PUT | `/links?key=` | any | retarget a link |
| DELETE | `/links?key=` | any | delete a link |
| POST | `/links/batch` | - | deferred; not in v1 |

### 10.1 `POST /links`

Request:

```json
{"target": "https://github.com/DragonFlyBSD/DragonFlyBSD/pull/56",
 "key": "/gh/dfbsd/p/56"}
```

`key` is optional; when omitted the server applies rules and the random
fallback.

Responses:

- `200` `{"created": false, ...}` when the target already has a key. The
  existing key is returned even if it is outside the caller's namespace; it is
  already published. This check runs before rule matching, so it takes
  precedence over the `403` below. It applies when `key` is omitted (the server
  chose the published key).
- `201` `{"created": true, ...}` on create.
- `403` when a matched rule's key is outside the caller's namespaces (C8), or
  when an explicit `key` is outside them.
- `409` when an explicit `key` exists with a different target, or the target
  exists under a different key.
- `409` when a hash rule cannot find a unique prefix; the server logs an
  operator-actionable error (§7.4).
- `400` for invalid target/key.

Response body:

```json
{"key":"/gh/dfbsd/p/56",
 "short_url":"https://example.com/gh/dfbsd/p/56",
 "target":"https://github.com/DragonFlyBSD/DragonFlyBSD/pull/56",
 "created":true,
 "rule":"github-pr",
 "created_at":"2026-09-11T05:00:00Z",
 "updated_at":"2026-09-11T05:00:00Z"}
```

### 10.2 List

- `namespace` optional; defaults to all of the caller's namespaces. Admin may
  list any prefix.
- `limit` default 100, max 1000.
- `cursor` is the last key of the previous page; empty means start.
- Response: `{"links":[...],"next_cursor":"..."}`.

### 10.3 Errors

```json
{"error":{"code":"forbidden","message":"key /gh/dfbsd/p/56 is outside namespace /g/"}}
```

Codes: `bad_request`, `unauthorized`, `forbidden`, `not_found`, `conflict`,
`method_not_allowed`, `payload_too_large`, `rate_limited`, `internal`.
`429` includes `Retry-After`.

### 10.4 `GET /status` (admin)

Bounded and cheap. Compute link counts on demand (optionally cached 60 s).

```json
{
  "version": "0.1.0",
  "started_at": "2026-09-11T05:00:00Z",
  "uptime_seconds": 12345,
  "server": {
    "public_url": "https://example.com",
    "listeners": ["0.0.0.0:80","[::]:80","0.0.0.0:443","[::]:443"],
    "http_enabled": true,
    "https_enabled": true
  },
  "acme": {
    "enabled": true,
    "directory_url": "https://acme-v02.api.letsencrypt.org/directory",
    "staging": false,
    "certificate": {
      "subject": "example.com",
      "issuer": "R11",
      "not_before": "2026-09-01T00:00:00Z",
      "not_after": "2026-11-30T00:00:00Z",
      "days_left": 80
    },
    "last_prewarm_ok": true,
    "last_prewarm_at": "2026-09-11T05:00:00Z"
  },
  "links": {
    "total": 1234,
    "namespaces": {"/g/": 900, "/gh/": 334}
  },
  "clients": [
    {"name":"git-monitor","namespaces":["/g/"],"admin":false}
  ],
  "access_log": {
    "current_file": "data/logs/access-2026-09-11.jsonl",
    "current_size_bytes": 123456,
    "oldest_retained": "2026-08-12",
    "files": 28
  },
  "db": {"file_size_bytes": 456789, "tx_stats": {}},
  "maintenance": {
    "backup_dir": "data/backup/",
    "last_backup_at": "2026-09-11T03:00:00Z",
    "last_backup_ok": true,
    "last_backup_file": "data/backup/links-2026-09-11.db",
    "last_backup_size_bytes": 234567,
    "backup_files": 12
  },
  "runtime": {"goroutines": 12, "memory_alloc_bytes": 1234567},
  "last_error": {"time": "2026-09-11T04:00:00Z", "message": "..."}
}
```

- Tokens and digests never appear.
- Certificate fields come from the certificate actually served: recorded by
  wrapping `GetCertificate` for real handshakes and from the pre-warm return
  value, ignoring challenge handshakes (C17).
- `last_prewarm_ok`/`last_prewarm_at` track the startup pre-warm (§12).

---

## 11. Authentication and namespaces

- Parse `Authorization: Bearer <token>`; missing/malformed -> `401`.
- At startup, combine `tokens` (hashed with SHA-256) and `tokens_sha256` into
  one digest set per client. Validate uniqueness across clients.
- On a request, compute `sha256(presented)` and compare with
  `crypto/subtle.ConstantTimeCompare` against every configured digest, without
  early exit over clients (C12). Record the matched client.
- No match -> `401`.
- Authorization for every operation (read and write):
  - `admin = true`: all keys.
  - otherwise: the key must have one of the client's namespaces as a prefix
    (`strings.HasPrefix(key, ns)`, `ns` ends with `/`).
- `403` on namespace violation for all operations, including reads. Short
  links are public by design, so existence is not a secret worth a special
  `404` code. A key inside the namespace that does not exist is `404`.
- Never log tokens or the `Authorization` header; log the client name.

---

## 12. ACME / TLS

### 12.1 Manager setup

```
Manager{
  Prompt:      AcceptTOS (when accept_tos),
  Cache:       DirCache(cache_dir or <data_dir>/acme),
  HostPolicy:  HostWhitelist(public_url host + extra_hosts),
  RenewBefore: renew_before_days * 24h,
  Email:       email,
  Client: &acme.Client{
    DirectoryURL: directory_url,
    HTTPClient:   &http.Client{Timeout: issue_timeout},
    Key:          accountKey or nil,
  },
}
```

- `account_file`: read PEM, parse to `crypto.Signer`, assign to
  `Client.Key`. Fail startup on parse error. Never write it (C5). Supported
  types per `acme.Client`: RSA and ECDSA (RS256, ES256/384/512); prefer
  EC P-256.
- Without `account_file`, autocert generates an ECDSA P-256 account key and
  stores it in `cache_dir` as `acme_account+key` (PEM, mode 0600).
- The certificate cache directory must exist with mode 0700 and persist across
  restarts.

### 12.2 TLS config

- Start from `Manager.TLSConfig()` and clone it.
- Keep `NextProtos = ["h2","http/1.1","acme-tls/1"]` (do not remove
  `acme.ALPNProto`).
- `MinVersion = tls.VersionTLS12`; leave cipher defaults to Go.
- Wrap `GetCertificate`:
  - if `len(hello.SupportedProtos) == 1 && hello.SupportedProtos[0] ==
    acme.ALPNProto`, call through and do not record (challenge handshake);
  - otherwise call through and, on success, parse `cert.Certificate[0]` and
    store subject/issuer/not_before/not_after for `/status` (C17).
- Pre-warm calls `Manager.GetCertificate` directly, bypassing the wrapper, so
  it must record the returned certificate itself (C17).

### 12.3 tls-alpn-01 only

Do not call `Manager.HTTPHandler` unless `http01_fallback = true` (C3). With
fallback off, autocert uses `tls-alpn-01` exclusively.

### 12.4 Startup pre-warm

After all listeners are up:

```
hello := &tls.ClientHelloInfo{
    ServerName:       domain,
    SupportedProtos:  []string{"h2", "http/1.1"},
    CipherSuites:     []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
    SupportedCurves:  []tls.CurveID{tls.CurveP256},
    SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
}
go func() {
    defer recover-and-log()
    cert, err := manager.GetCertificate(hello)
    record prewarm result and cert metadata (C17)
}()
```

- The hello advertises normal ALPN (so it is not treated as a challenge token
  request) and ECDSA capability (so autocert caches an ECDSA P-256 cert, the
  same kind modern clients negotiate) (C15, C17).
- Runs in a goroutine; never blocks startup (C18). Bound each ACME HTTP call
  with `issue_timeout`.
- If it fails (DNS not ready, CA unreachable), log, continue serving, and
  report in `/status`.
- If the certificate is already cached, it returns immediately.

### 12.5 Renewal

- autocert starts a renewal timer when a certificate is loaded and renews
  `renew_before_days` before expiry, with up to one hour of jitter (verified in
  `acme/autocert/renewal.go`).
- Renewal failures are logged and surfaced via `/status`; never fatal.
  autocert owns the renewal timer, so the process records failures through the
  wrapped `GetCertificate`: an issuance/renewal error during a handshake is
  stored in the status `last_error`, and certificate expiry is visible through
  `days_left`.
- For very low-traffic sites, the startup pre-warm and the renewal timer cover
  the gap. If the process is down across an expiry, the next start re-issues.

### 12.6 Manual TLS

When `acme.enabled = false`, load `cert_file`/`key_file`, set
`MinVersion = tls.VersionTLS12`, and serve. No renewal is performed; document
that the operator owns renewal.

---

## 13. Access logging

### 13.1 Layout

```
<data_dir>/logs/access-YYYY-MM-DD.jsonl   (UTC date, one JSON object per line)
```

"JSONL" means one object per line. Do not pretty-print.

### 13.2 Record

```json
{"ts":"2026-09-11T05:00:00.123Z",
 "type":"redirect",
 "remote_ip":"203.0.113.7",
 "method":"GET",
 "host":"example.com",
 "path":"/g/d/728aaaa",
 "key":"/g/d/728aaaa",
 "status":302,
 "target":"https://gitweb.dragonflybsd.org/dragonfly.git/commitdiff/728aaaa...",
 "rule":"gitweb-commit",
 "client":"",
 "action":"resolve",
 "request_id":"01J...",
 "user_agent":"curl/8.5.0",
 "referer":"",
 "duration_ms":0.4,
 "bytes":0}
```

- `type` is `redirect`, `api`, `acme`, or `home`.
- API records include `client` and `action` (`create`, `update`, `delete`,
  `resolve`, `status`); request bodies are never logged.
- `remote_ip` is the trusted-proxy-aware client IP (C13, C25).
- `type=acme` covers port-80 http-01 requests when fallback is enabled;
  tls-alpn-01 handshakes are not visible to HTTP handlers and are logged by the
  TLS wrapper if desired.

### 13.3 Writer

- Bounded channel (e.g. 4096) -> single writer goroutine.
- Buffered writes, periodic `flush_interval` flush (C14).
- On channel overflow, increment a dropped counter, emit an occasional warning,
  and drop; never block the request path.
- On shutdown, close the channel, drain with a timeout, flush, and close files.

### 13.4 Rotation and retention

- Rotate lazily when the UTC date changes on write; open
  `access-YYYY-MM-DD.jsonl` with `O_CREATE|O_WRONLY|O_APPEND`, mode 0644.
- Delete files older than `retention_days` at startup and once per UTC day.
  Deletion failures only warn.

---

## 14. Security and robustness

- Direct internet exposure: all of the following are required.
- Timeouts and `MaxHeaderBytes` as configured; `ReadTimeout`/`WriteTimeout`
  also cover the handshake (C2).
- Panic recovery per request; no stack traces or internal paths in responses.
- Body limit 64 KiB; strict JSON decoding.
- Host validation and `public_url`-derived links (C7).
- Key and target validation as in §7.5; target schemes `http`/`https` only;
  target length <= 8 KiB; reject userinfo.
- Rate limiting: per-IP for redirects (IPv4 /32, IPv6 /64) and per-token for
  the API, using `golang.org/x/time/rate` plus a bounded LRU (cap entries and
  periodically evict) so spoofed sources cannot exhaust memory. `429` with
  `Retry-After`.
- Constant-time token comparison (C12); no token logging.
- Never fetch target URLs (no SSRF).
- Response headers: `X-Content-Type-Options: nosniff`, `Referrer-Policy:
  no-referrer`, `Cache-Control: no-store` on redirects and errors. HSTS can be
  enabled manually after issuance is stable.
- Do not set a `Server` header.
- Forwarding headers are honored only from `server.trusted_proxies` (default
  localhost); never trust `X-Forwarded-For`/`X-Real-IP` from other peers (C13,
  C25).
- HSTS is opt-in (`server.hsts`, default off) and HTTPS-only (C26).
- Graceful shutdown on SIGINT/SIGTERM in the order of C16.
- File modes: `data_dir` 0700, `links.db` 0600, `acme/` 0700, log files 0644.
- Ports < 1024 need root or `CAP_NET_BIND_SERVICE`; see §16.

---

## 15. Module layout and build

```
urlshort/
  go.mod                 module github.com/liweitianux/dflybot/urlshort
  Makefile               CGO_ENABLED=0 builds (mirror the root Makefile)
  main.go                config load, wiring, pre-warm, signals, shutdown
  config.go              TOML structs + validator
  server.go              listeners (v4/v6), routing, middleware, timeouts
  tls.go                 autocert setup, account_file, manual certs, pre-warm
  api.go                 REST handlers, status
  redirect.go            short-link handler
  rules.go               canonicalization, regexp + template engine, hash extension
  store.go               Store interface (Get/Create/Update/Delete/List/Resolve)
  store_bbolt.go         bbolt implementation
  auth.go                token digests, constant-time compare, namespaces
  accesslog.go           JSONL writer, rotation, retention
  maintenance.go         daily compacted backup, verification, retention
  version.go             version/build info
  config_test.go
  rules_test.go
  store_bbolt_test.go
  api_test.go
  accesslog_test.go
  maintenance_test.go
  server_test.go
  urlshort.toml          committed sample with ??? tokens
```

- `Store` is an interface so rules/handlers can be tested against an in-memory
  fake. bbolt is the only production implementation.
- The bbolt store exposes a backup operation (`CompactTo`) used by
  `maintenance.go`, which owns scheduling, directories, verification, and
  retention.
- Go version: 1.22 or newer for `http.ServeMux` method/wildcard patterns. The
  root module stays on 1.21; the new module may declare a newer version.
- Build targets: `linux/amd64`, `dragonfly/amd64`, both with
  `CGO_ENABLED=0 -trimpath`, matching the root Makefile style.

---

## 16. Deployment notes

- Run as a dedicated unprivileged user; grant `CAP_NET_BIND_SERVICE` via
  systemd `AmbientCapabilities=CAP_NET_BIND_SERVICE` (or use high ports and
  port forwarding, but ACME requires 80/443).
- `data_dir` must be writable and persistent; back up `links.db` and the ACME
  cache.
- Firewall must allow inbound TCP 80 and 443 from the internet.
- DNS for `public_url` host (and `extra_hosts`) must point at the server.
- First production run: test against the Let's Encrypt staging directory to
  avoid production rate limits, then switch `directory_url`.
- Config file mode 0600 because it contains plaintext tokens.

---

## 17. Testing plan

- **rules**: full-string match; empty-match rejection; canonicalization
  (scheme/host case, default ports, userinfo rejection, query/fragment
  preservation); template render and invalid output rejection; `abbrev`;
  hash extension with shared prefixes; all-collide error; idempotent target
  returns the same key; cross-rule collision returns `409`; reserved-prefix
  rejection.
- **store**: bbolt CRUD; `targets`/`links` consistency; concurrent create of
  the same target yields one key; delete frees the target; list pagination and
  cursor correctness.
- **auth**: plaintext and digest tokens; rotation with multiple entries; wrong
  token; namespace boundary (`/g/` does not match `/git/`); admin; no early
  exit (behavioral, not timing).
- **handlers**: `httptest` with a temp bbolt; GET/HEAD/405; Host mismatch
  `421`; HTTP->HTTPS `308` preserving host; `extra_hosts` accepted; query
  ignored; unknown key `404`; API create/update/delete/status; `401/403/409`;
  body limit `413`; rate limit `429`; `/status` redaction.
- **access log**: UTC rotation with an injected clock; retention deletion;
  writer overflow drop path; drain on shutdown.
- **maintenance**: compact a DB with free pages and verify `Check`, counts, and
  a smaller output file; atomic rename; retention cleanup with an injected
  clock; `retention_count` cap; scheduled backups skip an existing same-day
  file while repeated startup backups get unique second-granular names; failure
  injection (unwritable dir, rename failure) leaves the live DB untouched.
- **TLS**: handlers tested behind an in-memory cert-manager interface; manual
  cert path; pre-warm logic with a fake `GetCertificate`; `GetCertificate`
  wrapper records the right certificate and ignores challenge handshakes.
  Real ACME is never exercised in unit tests.
- **config**: table-driven tests over the validation matrix in §5.1.
- **build**: `GOOS=linux` and `GOOS=dragonfly` with `CGO_ENABLED=0` must
  compile.

---

## 18. Implementation order

1. `go.mod`, `Makefile`, `version.go`, sample `urlshort.toml`.
2. `config.go` with the full validation matrix (§5.1).
3. `store.go` + `store_bbolt.go` + tests.
4. `rules.go` (canonicalization, template, hash extension) + tests.
5. `auth.go` + tests.
6. `accesslog.go` + tests.
7. `server.go` (listeners, mux, middleware, redirect) + tests.
8. `api.go` (CRUD, health, whoami, status) + tests.
9. `tls.go` (autocert, account_file, manual cert, pre-warm) + tests.
10. `main.go` wiring, signals, graceful shutdown.
11. `maintenance.go` (daily compacted backup, startup backup, retention) +
    tests.
12. Cross-build check for linux and dragonfly; staging ACME integration test.

---

## 19. Deferred and future work

- `POST /links/batch` for messages with several URLs.
- Optional per-rule or global target-host allowlist.
- Prometheus metrics; richer status counters.
- Small CLI admin tool.
- Config hot-reload on SIGHUP for rules/clients.
- `monitor/` shared client helper and optional `{{shorten}}` template
  function in dflybot.
- Additional domains / wildcard certificates (requires DNS-01, which autocert
  does not support).
