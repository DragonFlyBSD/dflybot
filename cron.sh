#!/bin/sh
#
# Copyright (c) 2026 Aaron LI
#
# cron.sh - Manage dflybot and its monitor programs in a tmux session.
#
# Designed for DragonFly BSD (cron + tmux), also runs on Linux.
#
# Model: the session 'dflybot' has one window per program; each window hosts
# a persistent interactive shell, and the programs are started "as if typed"
# in their windows with 'tmux send-keys'.  The windows and the session
# therefore survive program stops and crashes, and a periodic 'start' simply
# re-issues the start command for programs that are no longer running.
#
# Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)

set -u

session_name='dflybot'
# Programs, in start order.  dflybot first: the monitors need its webhook.
progs='dflybot git-monitor github-monitor jenkins-monitor redmine-monitor web-monitor'
progs_n=$(printf '%s\n' "$progs" | wc -w | tr -d ' ')

# Timeouts (seconds)
term_wait=10        # grace between SIGTERM and SIGKILL
start_wait=10       # wait for a program to come up after starting it
lock_wait=300       # wait bound for the lock (mutating commands)

NL='
'

die() {
	echo "cron.sh: $*" >&2
	exit 1
}

warn() {
	echo "cron.sh: $*" >&2
}

usage_err() {
	[ $# -gt 0 ] && warn "$*"
	usage >&2
	exit 2
}

usage() {
	cat <<EOF
usage: cron.sh <command> [args]

Manage the '$session_name' tmux session that runs dflybot and its monitor
programs, one program per window (window names = program names).

commands:
  help                 show this help
  status               show the session/window/program status
  start [program]      create the session and windows, start all programs
                       or the specified program; idempotent and self-healing
                       (safe for cron(8))
  open [program]       attach to the session at the program's window
                       (default: $progs_first); refuses to run inside tmux
  stop [program]       stop all programs or the specified program,
                       keep the session and windows
  close                stop all programs, close the session and windows

programs:
  $progs

example crontab (runs every hour; PATH must include tmux):
  PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/bin
  0 * * * * /path/to/cron.sh start >> /path/to/cron.log 2>&1

notes:
  * the script and all programs run from the directory holding cron.sh;
    binaries are started as <name>.$(uname -s) (e.g. dflybot.dragonfly).
  * program output goes to its window; watch it with 'cron.sh open <name>'.
  * start programs only through this script, so their pid files are kept.
EOF
}

# Determine the deployment root from this script's own location.
resolve_root() {
	self=$(realpath "$0") || die "realpath failed"
	root=$(CDPATH= cd -P -- "$(dirname -- "$self")" && pwd -P) ||
		die "cannot determine script directory"
}

# Check the given program is valid.
check_prog() {
	name=$1
	for p in $progs; do
		if [ "$p" = "$name" ]; then
			return 0
		fi
	done
	usage_err "unknown program: $name (choose from: $progs)"
}

os=$(uname -s 2>/dev/null | tr '[:upper:]' '[:lower:]') || die "uname failed"

uid=$(id -u) || die "id failed"
rundir="/tmp/${session_name}-${uid}"
lockfile="$rundir/lock"
mkdir -p "$rundir" || die "cannot create runtime dir: $rundir"
chmod 700 "$rundir"

require_tmux() {
	command -v tmux >/dev/null 2>&1 || die "tmux not found"
}

# Run the given subcommand while holding the lock.  Re-executes this script
# under lockf(1) (DragonFly/BSD) or flock(1) or; DFLYBOT_LOCKED guards
# against recursion.  Exits when the lock cannot be acquired.
run_locked() {
	[ "${DFLYBOT_LOCKED:-0}" -eq 1 ] && return 0
	require_tmux
	ex_tempfail=75
	if command -v lockf >/dev/null 2>&1; then
		DFLYBOT_LOCKED=1 lockf -k -t "$lock_wait" "$lockfile" "$0" "$@"
	elif command -v flock >/dev/null 2>&1; then
		DFLYBOT_LOCKED=1 flock -E $ex_tempfail -w "$lock_wait" \
			"$lockfile" "$0" "$@"
	else
		die "neither lockf(1) nor flock(1) available"
	fi
	rc=$?
	[ "$rc" -eq $ex_tempfail ] && warn "failed to lock: $lockfile"
	exit "$rc"
}

session_exists() {
	tmux has-session -t "$session_name" 2>/dev/null
}

win_exists() {
	tmux list-windows -t "$session_name" -F '#{window_name}' 2>/dev/null |
		grep -Fqx "$1"
}

# Report whether the named program is running, via its pid file plus a
# process-name check (guards against stale pid files and pid reuse).
is_running() {
	_pf="$rundir/$1.pid"
	[ -r "$_pf" ] || return 1
	_pid=$(cat "$_pf" 2>/dev/null) || return 1
	[ "$_pid" -gt 0 ] 2>/dev/null || return 1
	_comm=$(ps -p "$_pid" -o comm= 2>/dev/null) || return 1
	case "$_comm" in
	"$1"*) return 0 ;;
	*) return 1 ;;
	esac
}

win_index() {
	tmux display-message -p -t "$session_name:$1" '#{window_index}' 2>/dev/null
}

# Create the session and windows, then start every program.
cmd_start() {
	name=$1
	require_tmux
	rc=0
	if ! session_exists; then
		tmux new-session -d -s "$session_name" -n dflybot -c "$root" ||
			die "failed to create session '$session_name'"
		echo "start: created session '$session_name'"
	fi

	if [ -n "$name" ]; then
		check_prog "$name"
		start_one "$name" || rc=1
	else
		for name in $progs; do
			start_one "$name" || rc=1
		done
	fi
	exit "$rc"
}

# Ensure one window exists and its program is running.
start_one() {
	name=$1

	if ! win_exists "$name"; then
		if tmux new-window -t "$session_name" -n "$name" -c "$root"; then
			echo "start: $name: created window"
		else
			warn "start: $name: failed to create window"
			return 1
		fi
	fi

	if is_running "$name"; then
		echo "start: $name: already running (pid $(cat "$rundir/$name.pid"))"
		return 0
	fi

	bin="$root/$name.$os"
	if [ ! -x "$bin" ]; then
		warn "start: $name: binary not found/executable: $bin"
		return 1
	fi
	conf="$root/$name.toml"
	if [ ! -r "$conf" ]; then
		warn "start: $name: config not found: $conf"
		return 1
	fi

	# Start "as if typed" in the window: a nested sh records its own pid
	# (which the exec preserves, so the pid file holds the program's pid)
	# and then execs the program; the window's own shell survives.
	pidfile="$rundir/$name.pid"
	line="sh -c 'echo \$\$ > \"$pidfile\"; exec \"$bin\" -config \"$conf\"'$NL"
	if ! tmux send-keys -l -t "$session_name:$name" "$line"; then
		warn "start: $name: send-keys failed"
		return 1
	fi

	i=0
	while [ "$i" -lt "$start_wait" ]; do
		is_running "$name" && break
		sleep 1
		i=$((i + 1))
	done
	if is_running "$name"; then
		echo "start: $name: started (pid $(cat "$pidfile"))"
		return 0
	fi
	warn "start: $name: failed to start; check the window with 'cron.sh open $name'"
	return 1
}

cmd_status() {
	require_tmux
	rc=0
	if ! session_exists; then
		echo "session '$session_name': not running"
		for name in $progs; do
			echo "$name: stopped (no session)"
		done
		exit 1
	fi

	nrun=0
	for name in $progs; do
		if is_running "$name"; then
			pid=$(cat "$rundir/$name.pid")
			idx=$(win_index "$name")
			echo "$name: running (window $idx, pid $pid)"
			nrun=$((nrun + 1))
		else
			if win_exists "$name"; then
				echo "$name: stopped (window exists, idle)"
			else
				echo "$name: stopped (no window)"
			fi
			rc=1
		fi
	done
	echo "summary: $nrun/$progs_n programs running"
	exit "$rc"
}

# Stop all programs; the session and windows are kept.
cmd_stop() {
	name=$1
	require_tmux
	rc=0
	if [ -n "$name" ]; then
		check_prog "$name"
		stop_one "$name" || rc=1
	else
		for name in $progs; do
			stop_one "$name" || rc=1
		done
	fi
	exit "$rc"
}

# Stop one program (TERM, then KILL after the grace period).
stop_one() {
	name=$1
	if ! is_running "$name"; then
		echo "stop: $name: not running"
		rm -f "$rundir/$name.pid"
		return 0
	fi

	pid=$(cat "$rundir/$name.pid")
	echo "stop: $name: sending SIGTERM to pid $pid"
	kill -TERM "$pid" 2>/dev/null || true
	i=0
	while [ "$i" -lt "$term_wait" ]; do
		is_running "$name" || break
		sleep 1
		i=$((i + 1))
	done
	if is_running "$name"; then
		echo "stop: $name: still running; sending SIGKILL"
		kill -KILL "$pid" 2>/dev/null || true
		sleep 1
		if is_running "$name"; then
			warn "stop: $name: failed to stop pid $pid"
			rm -f "$rundir/$name.pid"
			return 1
		fi
	fi
	rm -f "$rundir/$name.pid"
	echo "stop: $name: stopped"
	return 0
}

# Stop all programs, then close the session and windows.
cmd_close() {
	require_tmux
	rc=0
	for name in $progs; do
		stop_one "$name" || rc=1
	done
	if session_exists; then
		tmux kill-session -t "$session_name" ||
			warn "close: failed to kill session '$session_name'"
		echo "close: session '$session_name' closed"
	else
		echo "close: session '$session_name' not running"
	fi
	for name in $progs; do
		rm -f "$rundir/$name.pid"
	done
	exit "$rc"
}

cmd_open() {
	name=${1:-dflybot}
	check_prog "$name"

	require_tmux
	if [ -n "${TMUX:-}" ]; then
		die "already inside a tmux session"
	fi
	session_exists ||
		die "session '$session_name' not running"
	win_exists "$name" ||
		die "window '$name' not in session '$session_name'"

	tmux select-window -t "$session_name:$name" ||
		die "failed to select window '$name'"
	tmux attach-session -t "$session_name"
}

resolve_root
progs_first=$(echo $progs | cut -d' ' -f1)

if [ $# -eq 0 ]; then
	usage_err "missing subcommand"
fi
cmd=$1
shift
case "$cmd" in
help | -h | --help)
	usage
	exit 0
	;;
status)
	cmd_status
	;;
start | stop | close)
	run_locked "$cmd" "$@"
	# Only reached as the locked child (DFLYBOT_LOCKED=1).  Close the
	# inherited dup of the lock fd (lockf/flock keep the lock themselves),
	# so that programs started through tmux cannot keep the lock alive
	# after this script exits.
	i=3
	while [ "$i" -lt 10 ]; do
		eval "exec $i>&-" 2>/dev/null
		i=$((i + 1))
	done
	case "$cmd" in
	start) cmd_start "$@" ;;
	stop) cmd_stop "$@" ;;
	close) cmd_close ;;
	esac
	;;
open)
	cmd_open "$@"
	;;
*)
	usage_err "unknown subcommand: $cmd"
	;;
esac
