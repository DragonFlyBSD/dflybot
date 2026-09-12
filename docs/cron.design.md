# cron.sh — Implementation Spec v2 (DragonFly BSD deployment)

Revised after the round-2 review.  Companion to `cron.sh`; do not commit
until agreed.

## 1. Goal and scope

Run `dflybot` and the four monitors on DragonFly BSD inside one tmux session,
supervised by cron:

- `cron.sh` is invoked from the user's crontab (and manually).
- It manages a dedicated tmux session `dflybot` with one window per program;
  window names equal program names:
  `dflybot`, `git-monitor`, `jenkins-monitor`, `github-monitor`, `web-monitor`
  (window 0 is `dflybot`, the others follow in that order).
- Subcommands: `help`, `status`, `start`, `open [program]`, `stop`, `close`.

All programs run as the same unprivileged user that owns the crontab.
The script runs on DragonFly BSD (and Linux, for testing) in POSIX `sh`.

## 2. Window model: persistent shells + send-keys (adopted)

Instead of running the program as the pane's only process (which ties the
window's life to the program and forces `remain-on-exit` + `respawn-pane`),
each window permanently hosts an interactive shell.  A program is started
"as if typed" in its window with `tmux send-keys -l`:

```
sh -c 'echo $$ > "/tmp/dflybot-<uid>/<name>.pid"; exec "<bin>" -config "<conf>"'
```

Why this works:

- The pane's first process is the shell, so the window and the session
  survive program stops and crashes by construction; `stop` trivially keeps
  the windows, no `remain-on-exit` needed.
- The inner `sh -c` records its own pid (`$$`) and then `exec`s the program;
  exec preserves the pid, so the pid file always holds the program's pid.
  `cron.sh` can signal and poll the program independently of the pane shell.
- A stop or crash returns the window to its shell prompt; a later `start`
  simply re-issues the same command line (self-heal).  A shell exit (user
  typed `exit`/Ctrl-D) closes the window; the next `start` recreates it.
- Program stdout/stderr stays in the window; `open` shows it, and the
  tmux scrollback keeps it after the program exits.

Running-state check (used by `status`/`start`/`stop`): the pid file must
exist with a positive pid, and `ps -p <pid> -o comm=` must start with the
program name.  The comm prefix guards against stale pid files and pid reuse,
and avoids matching wrapper shells (no `pgrep -f` false positives).

Known limitation (documented in help): only start programs through `cron.sh`;
typing a program directly into a managed window bypasses the pid file and can
lead to a duplicate instance on the next `start`.  Interactive typing in a
window while a program is down can also garble the auto-restart line; the
windows are meant for watching output.

## 3. Layout and environment

- Deployment root: one flat directory holding `cron.sh`, all binaries, all
  `.toml` configs, and `data/`.  `cron.sh` sets CWD there:
  `cd "$(dirname "$(realpath "$0")")"` (with a fallback when `realpath(1)`
  is absent).
- Binaries are OS-suffixed, matching the Makefile outputs:
  `os=$(uname -s | tr '[:upper:]' '[:lower:]')`; run `<name>.${os}` (e.g.
  `dflybot.dragonfly`).  This avoids accidentally executing a foreign
  (Linux) plain binary on DragonFly.
- Every window is created with working directory = deployment root; each
  program is started with absolute binary and config paths
  (`-config <name>.toml`).  Relative `data_dir` values in the tomls (e.g.
  `./data/`, `./data/github/`) therefore resolve under the root.
- `PATH` is NOT modified by the script.  `tmux` must be on `PATH` (pkgsrc
  installs to `/usr/pkg/bin`); a missing `tmux` gives a clear error.  The
  crontab entry must set `PATH` accordingly.
- `#!/bin/sh`, `set -u`, no bashisms.

## 4. Runtime state and locking

Runtime state under `/tmp/dflybot-<uid>/` (mode 0700), one deployment per
user per host:

- `<name>.pid` — per-program pid files;
- `lock` — lock file for the mutating commands.

Mutating subcommands (`start`, `stop`, `close`) take an exclusive lock by
re-executing the script under the platform lock tool; a `DFLYBOT_LOCKED`
environment guard prevents recursion:

- DragonFly/BSD: `lockf(1)` — `lockf -k -t <secs> lock "$0" ...`
  (see `lockf.1`: `-k` keeps the lock file, `-t` bounds the wait; on
  timeout lockf exits 75, EX_TEMPFAIL).
- Linux: `flock(1)` — `flock -w <secs> lock "$0" ...`
- Chosen by availability (`command -v flock`, else `command -v lockf`).
- Lock wait bound: 300 s (operations are short; both tools release the lock
  automatically if the holder dies).
- fd inheritance: `flock`/`lockf` do not set close-on-exec, so the locked
  child inherits a dup of the lock fd.  The locked child therefore closes
  inherited fds 3-9 before doing any work; the lock itself stays held by
  the flock/lockf parent until cron.sh exits, while programs started via
  tmux can no longer keep the lock alive.
- Polling sleeps use integer seconds only (`sleep 1`), because DragonFly's
  `sleep(1)` may not accept fractional seconds.

`status` and `help` take no lock.

## 5. Subcommands

### `cron.sh help` (also `-h`, `--help`)
Usage, command summary, managed programs, example crontab, deployment notes.
Exit 0.

### `cron.sh status`
- tmux missing or session absent => per-program `stopped`, exit 1.
- Otherwise, per program: `running` (with window index and pid) or `stopped`
  (with window state: exists/idle or missing), plus a summary line.
- Exit 0 iff all five programs are running.

### `cron.sh start`  (idempotent, self-healing; safe for cron)
Under the lock:
1. If the session is missing, create it detached with window 0 (a plain
   shell) named `dflybot`:
   `tmux new-session -d -s dflybot -n dflybot -c <root>`.
2. For each program, in order (`dflybot` first — the monitors need its
   webhook):
   - window missing       => `tmux new-window -t dflybot -n <name> -c <root>`;
   - program not running  => binary `<name>.${os}` and config `<name>.toml`
     must exist and be readable/executable, then send-keys the start line and
     poll (0.5 s interval, 10 s bound) until the pid file reports the program
     running; report failure (with a hint to `open` the window) otherwise;
   - program already running => skip (report).
Exit 0 iff every program is running at the end.

### `cron.sh open [program]`  (default: `dflybot`)
Refuses to run inside a tmux session (`$TMUX` set): prints an error and
exits (no nested tmux).  Otherwise, requires the session and the window to
exist, then `select-window -t dflybot:<name>` and
`attach-session -t dflybot` (attach cannot select a window by itself, so the
session's active window is set first).

### `cron.sh stop`  (keeps session and windows)
Under the lock: for each program with a live pid, SIGTERM, poll up to 10 s,
then SIGKILL if still alive; remove its pid file.  The windows remain at
their shell prompts with the program's last output visible.  Exit 0.

### `cron.sh close`  (stop + teardown)
Under the lock: stop all programs as above, then `kill-session -t dflybot`
and remove the pid files.  Any extra windows the user added to the session
are removed too.

## 6. Messages and exit codes

| exit | meaning                                              |
|------|------------------------------------------------------|
| 0    | success; status: all programs running                |
| 1    | operational error (start/stop failure, tmux/lock tool missing, session or window missing for `open`); status: not all running |
| 2    | usage error (unknown subcommand / program)           |
| 75   | lockf lock timeout (DragonFly) forwarded             |

Example crontab (see `cron.sh help`):

```
PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/pkg/bin
# self-heal once an hour; log quietly
0 * * * * /path/to/dflybot/cron.sh start >> /path/to/dflybot/cron.log 2>&1
```

## 7. Deployment checklist (DragonFly)

- One flat directory with `cron.sh`, the OS-suffixed binaries, the tomls,
  and `data/`.
- Build the binaries on the DragonFly host (`make`, plus `make -C` for each
  monitor) or copy the `*.dragonfly` binaries produced by the Makefiles.
- Fill the `???` secrets in each toml.
- Install tmux; put its directory on `PATH` in the crontab.
- `cron.sh status` / `cron.sh open` from a normal (non-tmux) terminal.

## 8. Decisions adopted (round-2 review)

1. send-keys window model (Section 2); no `remain-on-exit`/`respawn-pane`.
2. Lock via `lockf(1)` on DragonFly/BSD and `flock(1)` on Linux, chosen by
   availability; `DFLYBOT_LOCKED` guard; re-exec of `$0`.
3. OS-suffixed binaries `<name>.${os}` selected via `uname -s`.
4. Deployment root = script directory, entered via
   `cd "$(dirname "$(realpath "$0")")"`.
5. `open` refuses to run inside a tmux session.
6. No `PATH` modification inside the script.
7. Graceful stop: SIGTERM, 10 s poll, SIGKILL.
8. `start` self-heals: recreates missing windows and restarts dead programs.

## 9. Minor defaults chosen (no further approval needed, adjustable)

- Runtime dir `/tmp/dflybot-<uid>`; one deployment per user per host.
- Poll/terminate timeouts: 10 s start wait, 10 s TERM grace, 300 s lock
  bound.
- Send-keys line quoted with double quotes around the pid file, binary and
  config paths (paths must not contain `'`).

## 10. Non-goals

- No log-file redirection for the programs themselves (output lives in the
  window and its scrollback).
- No restart backoff or alerting; cron cadence is the retry policy.
- No management of programs started outside the session.
- No nested-tmux support for `open`.
