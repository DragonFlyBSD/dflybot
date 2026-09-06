Monitor Package

Package monitor provides shared building blocks for the *-monitor
utilities (git-monitor, jenkins-monitor, ...) that poll remote services
and announce state changes to IRC via dflybot's webhook API:

- the webhook message poster (Poster/Webhook/NewWebhook),
- a context cancelled on SIGINT/SIGTERM,
- a periodic poll loop,
- atomic JSON state persistence,
- JSONL history appending,
- log level configuration.

The package uses only the standard library.

SPDX-License-Identifier: MIT
Copyright (c) 2025-2026 Aaron LI
