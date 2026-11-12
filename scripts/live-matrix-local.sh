#!/usr/bin/env bash
# D-61 nightly live test on a local Matrix homeserver (Conduit) on this Mac: the second unattended
# live channel, next to scripts/live-mattermost-local.sh and built the same way. Run from a clean
# rysh-cli worktree, by qa's scripts/qa/nightly.sh (job live-matrix):
#
#   scripts/live-matrix-local.sh <go binary>
#
# Each run starts a fresh Conduit container from a pinned digest, published on 127.0.0.1 only, with
# federation and the update check off and its database on a tmpfs (nothing written to a volume).
# It registers three throwaway accounts (bot, echo user, observer) with generated passwords; the
# access tokens stay in memory (never stored, printed or put in argv), runs the live harness against
# it, and removes the container, also on TERM/INT/HUP.
#
# Registration is open (m.login.dummy) for the container's life: it listens on 127.0.0.1 only,
# lives under a minute and holds nothing but this run's throwaway accounts, so a registration token
# (which would have to travel in argv or a file) buys nothing.
#
# infra-ops host OK 2026-11-09, conditions: (1) the container is named per run and only that name is
# ever removed, never another container; a leftover of ours from a killed run is counted and logged,
# not removed; (2) the run time and the leftover count go in qa's nightly evidence (step lines).
#
# stdout: go test -json only. stderr: step lines (ids and HTTP statuses, never a value).
# Exit: go test's rc; 2 usage/refused; 3 infrastructure (image missing, server not ready, an
#   account step failed); 4 SKIP: the container runtime is down. The runner never pulls an image
#   and never starts the runtime: pull once by day, by digest:
#     /opt/homebrew/bin/docker --context colima pull <IMAGE below>
# Test hooks MX_LOCAL_* work only with MX_LOCAL_TEST=1 (scripts/test-live-matrix-local.sh).
set -u
# Conduit v0.10.15 (multi-arch index: native arm64 on this Mac, no emulation).
IMAGE='matrixconduit/matrix-conduit@sha256:cb4deab33ea1a6da919a0ea99e76be75d6af5936623fd9dde442a17e2d0b320f'
LABEL=rysh.channels.mx-local=1
CONTEXT=colima
HERE=$(cd "$(dirname "$0")" && pwd)

if [ "${MX_LOCAL_TEST:-}" != 1 ]; then
  set_hooks=""; for h in MX_LOCAL_CTR MX_LOCAL_READY_SECS; do [ -n "${!h+x}" ] && set_hooks="$set_hooks $h"; done
  [ -z "$set_hooks" ] || { echo "live-matrix-local: REFUSED: test hooks set on a real run:$set_hooks (names only)" >&2; exit 2; }
fi
CTR=${MX_LOCAL_CTR:-/opt/homebrew/bin/docker}
READY_SECS=${MX_LOCAL_READY_SECS:-300}
[ $# -eq 1 ] && [ -x "$1" ] || { echo "usage: $0 <go binary (absolute path)>" >&2; exit 2; }
GO=$1
cd "$HERE/.." || exit 3

ctr() { "$CTR" --context "$CONTEXT" "$@"; }
step() { echo "$(date '+%H:%M:%S') $*" >&2; }
ours() { ctr ps -a --filter "label=$LABEL" --format '{{.Names}}' 2>/dev/null | grep -c .; }

NAME=""; CHILD=""
cleanup() {
  if [ -n "$CHILD" ]; then kill -TERM "$CHILD" 2>/dev/null; wait "$CHILD" 2>/dev/null; CHILD=""; fi
  if [ -n "$NAME" ]; then
    # This run's name only (infra-ops condition 1). -v for symmetry with the Mattermost runner (the
    # image declares no volume; the database is on a tmpfs). No stop first: rm -f kills at once.
    ctr rm -f -v "$NAME" >/dev/null 2>&1
    step "container $NAME removed; ours left: $(ours)"
    NAME=""
  fi
}
trap cleanup EXIT
trap 'exit 143' TERM; trap 'exit 130' INT; trap 'exit 129' HUP

ctr info >/dev/null 2>&1 || { step "SKIP: container runtime (context $CONTEXT) is not running; not started by this job"; exit 4; }

n=$(ours)
[ "$n" = 0 ] || step "leftover containers of ours from earlier runs: $n (not removed: only this run's own name is ever removed)"

ctr image inspect "$IMAGE" >/dev/null 2>&1 || { step "INFRA: image not present (never pulled at night): pull once by day, by digest: $IMAGE"; exit 3; }

PORT=$(python3 -I -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])') || exit 3
NAME=rysh-live-matrix-$$
ctr run -d --label "$LABEL" --name "$NAME" -p "127.0.0.1:$PORT:6167" --tmpfs /data \
  -e CONDUIT_CONFIG= -e CONDUIT_SERVER_NAME=rysh-live.invalid -e CONDUIT_DATABASE_BACKEND=rocksdb \
  -e CONDUIT_DATABASE_PATH=/data -e CONDUIT_ADDRESS=0.0.0.0 -e CONDUIT_PORT=6167 \
  -e CONDUIT_ALLOW_REGISTRATION=true -e CONDUIT_ALLOW_FEDERATION=false -e CONDUIT_ALLOW_CHECK_FOR_UPDATES=false \
  "$IMAGE" >/dev/null || { step "INFRA: container did not start"; exit 3; }
step "container $NAME up on 127.0.0.1:$PORT only (image by digest)"

# Accounts and the room, then exec of go test in a cut-down environment (the setup's exit 3 = infrastructure).
python3 -I "$HERE/live-matrix-local-setup.py" "$PORT" "$GO" "$READY_SECS" &
CHILD=$!
wait "$CHILD"; rc=$?; CHILD=""
step "go test rc=$rc"
exit "$rc"
