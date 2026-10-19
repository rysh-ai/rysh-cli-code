#!/usr/bin/env bash
# Run the opt-in LiveKit live test (internal/meeting, fake STT/executor/TTS)
# without opening a single socket on the host.
#
# Why: its WebRTC clients (pion via server-sdk-go) gather ICE host candidates
# on every interface and bind wildcard STUN/mDNS sockets; server-sdk-go
# v2.16.3 builds pion's SettingEngine privately, so there is no interface/IP
# filter to pass. On macOS that raises the Application Firewall
# "meeting.test wants to accept incoming network connections" dialog for
# every rebuilt test binary.
#
# How: a livekit-server --dev container bound to 127.0.0.1 with no published
# ports anchors a private network namespace; a golang container running
# `go test` joins it (--network container:<anchor>), so ws://127.0.0.1:7880 is
# the LiveKit server, ICE runs inside the namespace, and nothing listens on
# the Mac. Same harness as rysh-server scripts/lk-live-tests.sh.
#
# Usage: scripts/lk-live-tests.sh [extra go test flags, e.g. -race -count=3]
#   LK_LIVE_LOG  optional file for the full `go test` output
set -euo pipefail
repo=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
# Mount the workspace at the same path so go.mod's replace (../rysh-shared,
# worktrees/rysh-shared symlink) resolves as on the host: find the real dir.
ws=$repo
while [ "$ws" != / ] && { [ ! -d "$ws/rysh-shared" ] || [ -L "$ws/rysh-shared" ]; }; do ws=$(dirname "$ws"); done
modcache=$(go env GOMODCACHE)
id=lk-live-$$
anchor=$id-livekit
cleanup() { docker rm -f "$anchor" "$id-test" >/dev/null 2>&1 || true; }
trap cleanup EXIT

docker run -d --name "$anchor" livekit/livekit-server:v1.13.7 --dev --bind 127.0.0.1 --node-ip 127.0.0.1 >/dev/null
if [ -n "$(docker port "$anchor")" ]; then echo "anchor publishes ports; refusing" >&2; exit 1; fi
docker volume create lk-live-gocache >/dev/null
docker run -d --name "$id-test" --network "container:$anchor" \
  -v "$ws:$ws" -v "$modcache:/go/pkg/mod:ro" -v lk-live-gocache:/root/.cache/go-build \
  -w "$repo" -e GOWORK=off -e GOFLAGS=-mod=readonly -e GOPROXY=off -e GOTOOLCHAIN=local \
  -e LK_LIVE_ALLOW_HOST=1 -e LIVEKIT_TEST_URL=ws://127.0.0.1:7880 \
  golang:1.25 go test -count=1 -run 'TestLiveKitHostEndToEnd' -v "$@" ./internal/meeting >/dev/null

# While the test runs: no host socket from any test binary; every socket of
# the test lives inside the private namespace.
sleep 12
echo "== host sockets of test binaries (must be empty):"
lsof -nP -iUDP -iTCP 2>/dev/null | awk 'NR==1 || $1 ~ /\.test$|^go$/' | sed -n '2,$p' || true
echo "== namespace listeners (UDP bound + TCP LISTEN, local addresses):"
{ docker exec "$id-test" sh -c "awk 'NR>1{print \$2}' /proc/net/udp; awk 'NR>1 && \$4==\"0A\"{print \$2}' /proc/net/tcp" 2>/dev/null || true; } |
  while IFS=: read -r hex port; do
    printf '%d.%d.%d.%d:%d\n' 0x${hex:6:2} 0x${hex:4:2} 0x${hex:2:2} 0x${hex:0:2} 0x$port
  done | sort -u | tr '\n' ' ' || echo "(test already finished)"
echo
status=$(docker wait "$id-test")
[ -n "${LK_LIVE_LOG:-}" ] && docker logs "$id-test" >"$LK_LIVE_LOG" 2>&1
docker logs "$id-test" 2>&1 | grep -v 'pion\.\|^20[0-9][0-9]/' | grep -E '_test\.go:[0-9]+|^(--- |ok|FAIL|PASS)|DATA RACE|panic|^go: ' || true
exit "$status"
