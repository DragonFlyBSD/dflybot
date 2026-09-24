# DragonFly Bot

Simple IRC bot for the DragonFly BSD project, plus a family of monitor
utilities and a URL shortener service.  The `dflybot` relays the messages
for the `#dragonflybsd` IRC channel to Telegram (groups and users), and
provides a webhook API for the monitors to announce project activies.  The
`urlshort` self-hosts the URL shortener service that makes the annoucements
more concise and readable.

## dflybot

The main IRC bot, which:

- joins the configured channels and relays channel traffic between IRC and
  Telegram;
- accepts notifications from the monitor utilities through a local webhook
  service and posts them to IRC and Telegram;
- answers `!ping`, `!opme`, and `!seen <nick>` commands, tracking the seen
  database (`data_dir/<channel>/seen.json`);
- records every joined channel to daily JSONL logs
  (`data_dir/<channel>/YYYY-MM-DD.jsonl`).

## urlshort

A small URL shortener service that serves the redirector and the management
REST API from one domain:

- configurable, meaningful short keys via ordered mapping rules, with a
  random fallback;
- namespace-scoped bearer tokens, one namespace per client;
- automatic certificate issuance and renewal via ACME (`tls-alpn-01` and
  `http-01`);
- persistent link storage (bbolt) and daily-rotated JSONL access and error
  log files;
- redirection and API rate limiters;
- direct internet-facing deployment, or deploying behind a proxy / CDN.

The monitors resolve announcement links through its `POST /links` and
`POST /links/batch` API before posting, replacing long URLs with short ones.
See `docs/urlshort.design.md` for the full design.

## Monitor utilities

Each monitor lives in its own subdirectory as a separate Go module, runs
periodically, saves per-target state and history under `data_dir`, and
announces events through dflybot's webhook.  They share the `monitor/` module
and, when configured, shorten the announced URLs through the `urlshort`
service.

- **git-monitor** — polls git repositories and announces new commits and
  tags (per repo: `git clone --mirror`, then periodic updates).
- **github-monitor** — polls the GitHub events API and announces the
  configured issue and pull request activity (create/comment/close/merge/
  update/reopen).
- **jenkins-monitor** — polls the Jenkins CI pipelines and announces build
  failures/recoveries and node offline/online changes.
- **redmine-monitor** — polls the Redmine (DragonFly's bugtracker) activity
  feed and announces the bug reports and updates.
- **web-monitor** — probes web services and announces site
  failures/recoveries (with hysteresis) and expiring/expired TLS
  certificates.

## monitor module

The shared `monitor/` Go module provides the building blocks used by the
monitor utilities: the webhook poster, the URL shortener client, a
signal-aware context, the periodic poll loop, atomic JSON state and JSONL
history persistence, and log level configuration.  It uses only the standard
library.

## Tools

- **tools/cron.sh** — starts and supervises dflybot and the monitors in a
  tmux session (one window per program), for use from cron or manually.
- **tools/httpdump.py** — a tiny HTTP server that logs incoming requests,
  useful for debugging webhooks.
- **tools/weechat2seen.py** — builds a dflybot `seen` database from WeeChat
  IRC channel logs.

## AI Disclaimer

A major fraction of the code is written by AI/LLM.  However, I (Aaron LI) have
reviewed all the code and ensured it followed the design and works.
