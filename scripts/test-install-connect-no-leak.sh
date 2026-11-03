#!/usr/bin/env bash
# Checks that scripts/test-install-connect.sh leaves nothing running when it exits: its fake
# artifact server (python3 -m http.server) used to outlive it, because SERVER_PID was the pid
# of the subshell around the server, not of the server itself.
#
#   scripts/test-install-connect-no-leak.sh [script]   # default: scripts/test-install-connect.sh
#
# The script runs as the leader of a new process group (set -m). The server never leaves that
# group, so after the script exits the group must be empty. Nothing is looked up by name or
# pattern: liveness is kill -0 on the group this test created, and a leftover is stopped by
# signalling that group only, after checking that the script led it.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
TARGET="${1:-$HERE/test-install-connect.sh}"
LOG=$(mktemp); trap 'rm -f "$LOG"' EXIT

set -m
bash "$TARGET" >"$LOG" 2>&1 & P=$!
set +m
pg=$(ps -o pgid= -p "$P" 2>/dev/null | tr -d ' ')
if [ "$pg" != "$P" ]; then
  # Either the script already exited (pg empty) or it is not its own group leader. Without a
  # group this test created, there is nothing it may check or signal.
  wait "$P"; echo "FAIL: could not confirm the script led its own process group (pgid '${pg}' for pid $P); nothing checked" >&2
  exit 1
fi
wait "$P"; rc=$?

fail=0
if [ "$rc" -ne 0 ]; then echo "FAIL: the script exited $rc:" >&2; cat "$LOG" >&2; fail=1; fi
if kill -0 -- "-$P" 2>/dev/null; then
  echo "FAIL: process group $P still has members after the script exited (a leaked server)" >&2
  kill -TERM -- "-$P" 2>/dev/null
  for _ in 1 2 3 4 5 6 7 8 9 10; do kill -0 -- "-$P" 2>/dev/null || break; sleep 0.2; done
  kill -0 -- "-$P" 2>/dev/null && { kill -KILL -- "-$P" 2>/dev/null; echo "  group $P: SIGKILL sent" >&2; }
  kill -0 -- "-$P" 2>/dev/null && echo "  group $P: still alive" >&2 || echo "  group $P: stopped (it was this test's own group)" >&2
  fail=1
fi
[ "$fail" -eq 0 ] || exit 1
echo "ok: the script passed and left nothing running in its process group ($(tail -1 "$LOG"))"
