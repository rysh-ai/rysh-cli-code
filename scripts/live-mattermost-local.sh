#!/usr/bin/env bash
# D-61 nightly live test on a self-hosted Mattermost on this Mac (rr-order-137 D.1; design:
# out/channels-mattermost-nightly-design.md). Run from a clean rysh-cli worktree, by qa's
# scripts/qa/nightly.sh job live-mattermost:
#
#   scripts/live-mattermost-local.sh <go binary>
#
# Each run starts a fresh Mattermost container from a pinned digest, published on 127.0.0.1 only,
# makes throwaway accounts and two tokens on it (in memory: never stored, printed or put in argv),
# runs the live harness against it, and removes the container, also on TERM/INT/HUP.
#
# stdout: go test -json only. stderr: step lines (ids and HTTP statuses, never a value).
# Exit: go test's rc; 2 usage/refused; 3 infrastructure (image missing, server not ready, an
#   account step failed); 4 SKIP: the container runtime is down. The runner never pulls an image
#   and never starts the runtime (infra-ops 2026-11-03): pull once by day, by digest:
#     /opt/homebrew/bin/docker --context colima pull --platform linux/amd64 <IMAGE below>
# Test hooks MM_LOCAL_* work only with MM_LOCAL_TEST=1 (scripts/test-live-mattermost-local.sh).
set -u
IMAGE='mattermost/mattermost-preview@sha256:88f274b5e5af77cdd28b81c4bd585ef2229f4d54c337240bc82857720d9f1c20'
LABEL=rysh.channels.mm-local=1
CONTEXT=colima
HERE=$(cd "$(dirname "$0")" && pwd)

if [ "${MM_LOCAL_TEST:-}" != 1 ]; then
  set_hooks=""; for h in MM_LOCAL_CTR MM_LOCAL_READY_SECS; do [ -n "${!h+x}" ] && set_hooks="$set_hooks $h"; done
  [ -z "$set_hooks" ] || { echo "live-mattermost-local: REFUSED: test hooks set on a real run:$set_hooks (names only)" >&2; exit 2; }
fi
CTR=${MM_LOCAL_CTR:-/opt/homebrew/bin/docker}
READY_SECS=${MM_LOCAL_READY_SECS:-900}
[ $# -eq 1 ] && [ -x "$1" ] || { echo "usage: $0 <go binary (absolute path)>" >&2; exit 2; }
GO=$1
cd "$HERE/.." || exit 3

ctr() { "$CTR" --context "$CONTEXT" "$@"; }
step() { echo "$(date '+%H:%M:%S') $*" >&2; }

NAME=""; CHILD=""
cleanup() {
  if [ -n "$CHILD" ]; then kill -TERM "$CHILD" 2>/dev/null; wait "$CHILD" 2>/dev/null; CHILD=""; fi
  if [ -n "$NAME" ]; then
    # -v: the image declares two volumes (Mattermost data, Postgres) holding this run's throwaway
    # accounts and tokens; without it every run left both behind in the VM. No stop first: the
    # server ignores SIGTERM, so a stop waited out the 10 s grace; rm -f kills it at once.
    ctr rm -f -v "$NAME" >/dev/null 2>&1
    step "container $NAME removed; ours left: $(ctr ps -a --filter "label=$LABEL" --format '{{.Names}}' 2>/dev/null | grep -c .)"
    NAME=""
  fi
}
trap cleanup EXIT
trap 'exit 143' TERM; trap 'exit 130' INT; trap 'exit 129' HUP

ctr info >/dev/null 2>&1 || { step "SKIP: container runtime (context $CONTEXT) is not running; not started by this job"; exit 4; }

# A run killed with SIGKILL leaves its container behind; remove ours only, by label.
for old in $(ctr ps -a --filter "label=$LABEL" --format '{{.Names}}' 2>/dev/null); do
  ctr rm -f -v "$old" >/dev/null 2>&1 && step "removed a leftover container of ours: $old"
done

ctr image inspect "$IMAGE" >/dev/null 2>&1 || { step "INFRA: image not present (never pulled at night): pull once by day, by digest: $IMAGE"; exit 3; }

PORT=$(python3 -I -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])') || exit 3
NAME=rysh-channels-mm-local-$$
# security-lead N1: no phone-home (diagnostics with an instance id, the security-update check). An
# egress-less network is not used: Docker publishes no port from a --network none/--internal container.
ctr run -d --platform linux/amd64 --label "$LABEL" --name "$NAME" -p "127.0.0.1:$PORT:8065" \
  -e MM_LOGSETTINGS_ENABLEDIAGNOSTICS=false -e MM_SERVICESETTINGS_ENABLESECURITYFIXALERT=false "$IMAGE" >/dev/null \
  || { step "INFRA: container did not start"; exit 3; }
step "container $NAME up on 127.0.0.1:$PORT only (image by digest)"

# Accounts, then exec of go test in a cut-down environment (the setup's own exit 3 = infrastructure).
python3 -I "$HERE/live-mattermost-local-setup.py" "$PORT" "$GO" "$READY_SECS" &
CHILD=$!
wait "$CHILD"; rc=$?; CHILD=""
step "go test rc=$rc"
exit "$rc"
