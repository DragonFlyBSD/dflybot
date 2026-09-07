#!/usr/bin/env python3
#
# Copyright (c) 2026 Aaron LI
#
# Build a dflybot "seen" database from WeeChat IRC channel logs.
#
# The produced JSON matches the per-channel seen.json consumed by dflybot
# (see seen.go), i.e.:
#   data_dir/<channel>/seen.json
#     {"version": 1, "channel": ..., "updated_at": <unix>, "nicks": {...}}
# Event times are stored as Unix seconds (UTC).
#
# WeeChat log lines look like "<ts>\t<prefix>\t<text>":
#   -->  <nick> (~user@host) has joined #channel     (join)
#   <--  <nick> (~user@host) has quit ...             (leave)
#        <nick> (~user@host) has left #channel
#        <nick> (~user@host) is now known as <new>    (nick change)
#   @aly / +nick / aly   channel messages (mode chars prefix the nick)
#   * / other markers    action (/me) lines: "<nick> does something"
# Logs start with '#' comment/separator lines, which are skipped.
#
# Usage:
#   weechat2seen.py [options] <weechatlog> [<weechatlog> ...]
#
# By default the log timestamps are interpreted in the local time zone of
# this machine (WeeChat default); pass --utc if they were recorded in UTC.
#
# Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)

import argparse
import json
import re
import sys
from datetime import datetime, timezone
from pathlib import Path

SEEN_VERSION = 1

# IRC nick charset, optionally preceded by channel mode chars (~ & @ % +).
NICK_RE = re.compile(r"^[~&@%+]*([A-Za-z0-9_\-\[\]\\`^{}|]+)$")
# Join line: "<nick> (~user@host) has joined #channel"
JOIN_RE = re.compile(r"^(\S+) \(\S+\) has joined (\S+)$")
# Nick change: "... is now known as <new>"
RENAME_RE = re.compile(r"is now known as (\S+)$")


class SeenBuilder:
    """Accumulates the seen records for one channel.

    Keys are lowercased nicks (IRC is case-insensitive); each record keeps
    the latest observed spelling for display and the last seen event times
    (Unix seconds, 0 = unknown), mirroring SeenEntry in seen.go.
    """

    def __init__(self):
        self.records = {}  # lower nick -> {display, join, exit, message}
        self.channels = set()

    def _entry(self, nick):
        key = nick.lower()
        rec = self.records.get(key)
        if rec is None:
            rec = {"display": nick, "join": 0, "exit": 0, "message": 0}
            self.records[key] = rec
        return rec

    def known(self, nick):
        return nick.lower() in self.records

    def join(self, nick, ts, channel):
        if channel:
            self.channels.add(channel)
        rec = self._entry(nick)
        rec["display"] = nick
        rec["join"] = ts

    def leave(self, nick, ts):
        rec = self._entry(nick)
        rec["exit"] = ts

    def rename(self, old, new, ts):
        old_key = old.lower()
        rec = self.records.pop(old_key, None)
        if rec is None:
            return
        rec["display"] = new
        rec_key = new.lower()
        if rec_key in self.records:
            # Merge with an existing (unexpected) record of the new nick.
            cur = self.records[rec_key]
            for field in ("join", "exit", "message"):
                if cur[field] == 0:
                    cur[field] = rec[field]
            cur["display"] = new
        else:
            self.records[rec_key] = rec

    def message(self, nick, ts):
        rec = self._entry(nick)
        rec["display"] = nick
        rec["message"] = ts


def parse_ts(text, utc):
    dt = datetime.strptime(text, "%Y-%m-%d %H:%M:%S")
    if utc:
        dt = dt.replace(tzinfo=timezone.utc)
    # Naive datetime: .timestamp() interprets it in the local time zone.
    return int(dt.timestamp())


def classify(prefix, text):
    """Classify a log line into (kind, nick, extra) or None to ignore.

    kinds: join/leave/rename/message/action.  prefix is the stripped 2nd
    column.
    """
    # Ignore Topic and Channel info lines.
    if prefix == "--":
        return None

    if prefix in ("-->", "<--"):
        m = re.match(r"^(\S+) (.*)$", text)
        if not m:
            return None
        nick, rest = m.group(1), m.group(2)
        if prefix == "-->":
            m = JOIN_RE.match(text)
            if m:
                return "join", nick, m.group(2)
            return None  # e.g. our own join summary lines
        rm = RENAME_RE.search(rest)
        if rm:
            return "rename", nick, rm.group(1)
        return "leave", nick, None

    # Speaker lines: the prefix column is the nick, possibly mode-prefixed.
    m = NICK_RE.match(prefix)
    if m:
        nick = m.group(1)
        rm = RENAME_RE.search(text)
        if rm and text.startswith(nick):
            return "rename", nick, rm.group(1)
        if text.startswith(nick) and re.search(r"\swas kicked by\s", text):
            return "leave", nick, None  # kicked (some clients)
        return "message", nick, None

    # Other prefixes (info lines, action markers, ...): the text may be an
    # action (/me) such as "<nick> does something", or a kick notice such as
    # "<nick> was kicked by <op> (...)".  Return kind 'action'/'leave'; the
    # action is recorded as a message only if that nick is known in the log
    # (checked at apply time, to avoid info lines like "Topic for ...").
    m = re.match(r"^(\S+)(?:\s.*)?$", text)
    if m:
        nm = NICK_RE.match(m.group(1))
        if nm:
            nick = nm.group(1)
            rest = text[len(nm.group(0)):]
            if re.match(r"^\s+was kicked by\s", rest):
                return "leave", nick, None
            return "action", nick, None
    return None


def main(argv):
    ap = argparse.ArgumentParser(
        prog="weechat2seen",
        description="Build a dflybot seen.json from WeeChat IRC channel logs")
    ap.add_argument("logs", nargs="+", metavar="log", help="WeeChat log file")
    ap.add_argument("-o", "--output", default="seen.json", metavar="FILE",
                    help="output seen.json (default: seen.json)")
    ap.add_argument("--channel", metavar="CHAN",
                    help="channel name (auto-detected from join lines)")
    ap.add_argument("--utc", action="store_true",
                    help="log timestamps are UTC (default: local time)")
    ap.add_argument("-v", "--verbose", action="store_true")
    args = ap.parse_args(argv)

    events = []  # (ts, kind, nick, extra), applied in chronological order
    stats = {"lines": 0, "skipped": 0, "join": 0, "leave": 0,
             "rename": 0, "message": 0, "action": 0}
    for path in args.logs:
        if not Path(path).is_file():
            sys.exit("error: log file not found: %s" % path)
        with open(path, encoding="utf-8-sig", errors="replace") as f:
            for line in f:
                stats["lines"] += 1
                line = line.rstrip("\n")
                if not line or line.startswith("#"):
                    continue  # log header/comment lines
                fields = line.split("\t", 2)
                if len(fields) < 3 or not fields[0] or not fields[2]:
                    stats["skipped"] += 1
                    continue
                try:
                    ts = parse_ts(fields[0], args.utc)
                except ValueError:
                    stats["skipped"] += 1
                    continue
                kind = classify(fields[1].strip(), fields[2])
                if kind is None:
                    stats["skipped"] += 1
                    continue
                knd, nick, extra = kind
                events.append((ts, knd, nick, extra))
                stats[knd] += 1
    # Apply in time order (stable, so equal timestamps keep file order).
    events.sort(key=lambda e: e[0])
    builder = SeenBuilder()
    for ts, kind, nick, extra in events:
        if kind == "join":
            builder.join(nick, ts, extra)
        elif kind == "leave":
            builder.leave(nick, ts)
        elif kind == "rename":
            builder.rename(nick, extra, ts)
        elif kind == "action":
            if builder.known(nick):
                builder.message(nick, ts)
        else:
            builder.message(nick, ts)

    # Resolve the channel name.
    if args.channel:
        channel = args.channel
    elif len(builder.channels) == 1:
        channel = next(iter(builder.channels))
    elif not builder.channels:
        sys.exit("error: no channel found in the logs; pass --channel")
    else:
        sys.exit("error: logs contain multiple channels (%s); pass --channel"
                 % ", ".join(sorted(builder.channels)))

    # Serialize records: keyed by the latest observed nick spelling, with
    # zero (unknown) times omitted, matching seen.go's JSON structure.
    nicks = {}
    for rec in builder.records.values():
        entry = {}
        for field in ("join", "exit", "message"):
            if rec[field] > 0:
                entry[field] = rec[field]
        nicks[rec["display"]] = entry
    updated_at = events[-1][0] if events else 0
    data = {"version": SEEN_VERSION, "channel": channel,
            "updated_at": updated_at, "nicks": nicks}

    out = Path(args.output)
    out.parent.mkdir(parents=True, exist_ok=True)
    with open(out, "w", encoding="utf-8") as f:
        json.dump(data, f, indent=2, sort_keys=True)
        f.write("\n")

    if args.verbose:
        for k in ("lines", "join", "leave", "rename", "message",
                   "action", "skipped"):
            print("%-10s %d" % (k, stats[k]))
    print("channel: %s, nicks: %d, last event: %s -> %s" %
          (channel, len(nicks),
           datetime.fromtimestamp(updated_at, tz=timezone.utc)
           if updated_at else "n/a", out))


if __name__ == "__main__":
    main(sys.argv[1:])
