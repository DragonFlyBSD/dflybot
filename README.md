# DragonFly Bot

Simple IRC bot for the DragonFly BSD project, plus a family of monitor
utilities that announce project activity to the `#dragonflybsd` IRC channel
(and Telegram) via dflybot's webhook API.

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

## Monitor utilities

Each monitor lives in its own subdirectory as a separate Go module, runs
periodically, saves per-target state and history under `data_dir`, and
announces events through dflybot's webhook.  They share the `monitor/` module
(webhook poster, poll loop, state and history persistence helpers).

- **git-monitor** — polls git repositories and announces new commits and
  tags (per repo: `git clone --mirror`, then periodic updates).
- **jenkins-monitor** — polls the Jenkins CI pipelines and announces build
  failures/recoveries and node offline/online changes.
- **github-monitor** — polls the GitHub events API and announces the
  configured issue and pull request activity (create/comment/close/merge/
  update/reopen).
- **web-monitor** — probes web services and announces site
  failures/recoveries (with hysteresis) and expiring/expired TLS
  certificates.

## AI Disclaimer

A major fraction of the code is written by AI/LLM.  However, I (Aaron LI) have
reviewed all the code and ensured it followed the design and works.
