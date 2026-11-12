#!/usr/bin/env bash
# Offline tests for scripts/live-matrix-local.sh (the D-61 nightly live test on a local Matrix
# homeserver, Conduit). No real container runtime, registry or homeserver is used: a fake container
# CLI (an absolute path, not named like the tool, never on PATH) starts a fake Matrix client API on
# the 127.0.0.1 port the runner asks for, and a fake go records names only. Everything binds
# 127.0.0.1.
#
# Usage: scripts/test-live-matrix-local.sh
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
RUNNER=$HERE/live-matrix-local.sh
T=$(umask 077; mktemp -d "${TMPDIR:-/tmp}/mx-local-test.XXXXXX") || exit 1
trap 'kill_fakes; rm -rf "$T"' EXIT
pass=0; fail=0
ok() { pass=$((pass+1)); echo "ok   $1"; }
bad() { fail=$((fail+1)); echo "FAIL $1"; }

mkdir -p "$T/bin"
FAKE_CTR=$T/bin/fake-container-cli
FAKE_GO=$T/bin/fake-go-cli
FAKE_MX=$T/bin/fake-matrix.py

# --- fake Matrix client API: the endpoints the runner's setup uses. Issued access tokens are
# written to $STATE/tokens so the test can look for them in argv and output; passwords the setup
# sends are written to $STATE/passwords for the same reason. Nothing else reads them.
cat > "$FAKE_MX" <<'PY'
import json, os, secrets, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port, state = int(sys.argv[1]), sys.argv[2]
fail_on = os.environ.get("FAKE_MX_FAIL", "")
def rid(): return secrets.token_hex(10)
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def reply(self, code, obj):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers(); self.wfile.write(b)
    def do_GET(self):
        if self.path == "/_matrix/client/versions": return self.reply(200, {"versions": ["v1.11"]})
        self.reply(404, {"errcode": "M_NOT_FOUND"})
    def do_PUT(self): self.handle_any("PUT")
    def do_POST(self): self.handle_any("POST")
    def handle_any(self, m):
        n = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(n) or b"{}")
        p = self.path[len("/_matrix/client/v3"):]
        with open(state + "/calls", "a") as f: f.write(f"{m} {p}\n")
        if fail_on and p.startswith(fail_on): return self.reply(500, {"errcode": "M_UNKNOWN"})
        if p == "/register":
            if body.get("password"):
                with open(state + "/passwords", "a") as f: f.write(body["password"] + "\n")
            if (body.get("auth") or {}).get("type") != "m.login.dummy":
                return self.reply(401, {"session": rid(), "flows": [{"stages": ["m.login.dummy"]}]})
            t = "syt_" + secrets.token_hex(12)
            with open(state + "/tokens", "a") as f: f.write(t + "\n")
            return self.reply(200, {"user_id": "@" + body["username"] + ":rysh-live.invalid", "access_token": t})
        if not self.headers.get("Authorization", "").startswith("Bearer syt_"):
            return self.reply(401, {"errcode": "M_MISSING_TOKEN"})
        if p == "/createRoom": return self.reply(200, {"room_id": "!" + rid() + ":rysh-live.invalid"})
        if p.startswith("/join/"): return self.reply(200, {"room_id": "!x:rysh-live.invalid"})
        return self.reply(200, {})
HTTPServer(("127.0.0.1", port), H).serve_forever()
PY

# --- fake container CLI: logs argv; run starts the fake API on the published loopback port.
# containers = every container the runtime has; ps answers from it (the label filter is the
# runner's own business, so the fake lists them all and the test seeds only ours).
cat > "$FAKE_CTR" <<'SH'
#!/usr/bin/env bash
S=$FAKE_STATE
if [ "$1" = --context ]; then echo "$2" >> "$S/ctr-context"; shift 2; else echo none >> "$S/ctr-context"; fi
printf '%s\n' "$*" >> "$S/ctr-argv"
case "$1" in
  info) [ -e "$S/runtime-down" ] && exit 1; echo ok ;;
  image)
    [ "$2" = inspect ] || exit 1
    [ -e "$S/image-present" ] && { echo '[{}]'; exit 0; }; exit 1 ;;
  pull) touch "$S/image-present"; exit 0 ;;
  ps)
    for c in $(cat "$S/containers" 2>/dev/null); do echo "$c"; done ;;
  run)
    name=""; pub=""; prev=""
    for a in "$@"; do
      [ "$prev" = --name ] && name=$a
      [ "$prev" = -p ] && pub=$a
      prev=$a
    done
    echo "$name" >> "$S/containers"
    port=$(echo "$pub" | cut -d: -f2)
    python3 -I "$FAKE_MX_PY" "$port" "$S" >/dev/null 2>&1 &
    echo $! > "$S/mx-pid"
    echo "cid-$name" ;;
  stop) : ;;
  rm)
    shift; while [ "${1#-}" != "${1:-}" ]; do shift; done
    for n in "$@"; do grep -v -x -F "$n" "$S/containers" > "$S/c.tmp" 2>/dev/null; mv "$S/c.tmp" "$S/containers"; done
    [ -s "$S/mx-pid" ] && { kill "$(cat "$S/mx-pid")" 2>/dev/null; : > "$S/mx-pid"; } ;;
  *) exit 1 ;;
esac
SH

# --- fake go: records its argv and the NAMES in its environment; emits go test -json lines.
cat > "$FAKE_GO" <<'SH'
#!/usr/bin/env bash
S=$TMPDIR   # the runner keeps only PATH HOME TMPDIR ... for go; the test points TMPDIR at the case dir
printf '%s\n' "$*" > "$S/go-argv"
compgen -e | sort > "$S/go-env-names"
for n in RYSH_LIVE_MATRIX_BOT_TOKEN RYSH_LIVE_MATRIX_ECHO_TOKEN; do
  v=${!n:-}
  if [ -n "$v" ] && grep -q -x -F "$v" "$S/tokens"; then echo "$n issued" >> "$S/go-token-check"; else echo "$n not-issued" >> "$S/go-token-check"; fi
done
[ -n "${RYSH_LIVE_MATRIX_BOT_TOKEN:-}" ] && [ "${RYSH_LIVE_MATRIX_BOT_TOKEN:-}" != "${RYSH_LIVE_MATRIX_ECHO_TOKEN:-}" ] && echo distinct > "$S/go-token-distinct"
case "${RYSH_LIVE_MATRIX_SERVER_URL:-}" in http://127.0.0.1:*) echo loopback > "$S/go-url" ;; *) echo other > "$S/go-url" ;; esac
case "${RYSH_LIVE_MATRIX_ROOM_ID:-}" in '!'*) echo roomid > "$S/go-room" ;; *) echo other > "$S/go-room" ;; esac
case "$(cat "$S/go-mode" 2>/dev/null || echo pass)" in
  pass) echo '{"Action":"pass","Test":"TestLiveMatrixOutbound"}'; echo '{"Action":"pass","Test":"TestLiveMatrixRoundTrip"}'; exit 0 ;;
  fail) echo '{"Action":"fail","Test":"TestLiveMatrixRoundTrip"}'; exit 1 ;;
  hang) touch "$S/go-started"; sleep 30; exit 0 ;;
esac
SH
chmod +x "$FAKE_CTR" "$FAKE_GO"

kill_fakes() { for f in "$T"/case-*/mx-pid; do [ -s "$f" ] && kill "$(cat "$f")" 2>/dev/null; done; return 0; }

# run_case <name> [VAR=value ...]: runs the runner with the fakes; state in $T/case-<name>.
run_case() {
  local name=$1; shift
  S=$T/case-$name; mkdir -p "$S"; : > "$S/tokens"; : > "$S/passwords"; [ -e "$S/containers" ] || : > "$S/containers"
  for a in "$@"; do case $a in FAKE_GO_MODE=*) echo "${a#FAKE_GO_MODE=}" > "$S/go-mode" ;; esac; done
  env -i PATH="/usr/bin:/bin:/usr/sbin:/sbin" HOME="$T" TMPDIR="$S" \
    MX_LOCAL_TEST=1 MX_LOCAL_CTR="$FAKE_CTR" MX_LOCAL_READY_SECS=20 \
    FAKE_STATE="$S" FAKE_MX_PY="$FAKE_MX" \
    AWS_SECRET_ACCESS_KEY=fake-standing-name GH_TOKEN=fake-standing-name "$@" \
    bash "$RUNNER" "$FAKE_GO" > "$S/out" 2> "$S/err"
  echo $? > "$S/rc"
}
rc() { cat "$T/case-$1/rc"; }
# own_name <case>: the run's container name, unique per run (infra-ops 2026-11-09 condition 1).
own_name() { grep '^run ' "$T/case-$1/ctr-argv" | grep -o -E -- '--name [^ ]+' | cut -d' ' -f2; }
# rm_only_own <case>: every rm names exactly this run's container (infra-ops condition 1: clean up
# only that name, never other containers), with -v and no stop first.
rm_only_own() {
  local S=$T/case-$1 n; n=$(own_name "$1")
  [ -n "$n" ] && grep -q '^rm ' "$S/ctr-argv" && [ -z "$(grep '^rm ' "$S/ctr-argv" | grep -v -x -F "rm -f -v $n")" ]
}
no_stop() { ! grep -q '^stop ' "$T/case-$1/ctr-argv"; }
left_own() { local n c; n=$(own_name "$1"); [ -n "$n" ] || { echo 0; return; }; c=$(grep -c -x -F "$n" "$T/case-$1/containers"); echo "${c:-0}"; }
no_secret_leak() { # no issued token or sent password in the runner's output, error stream or any argv
  local S=$T/case-$1 v
  while read -r v; do
    [ -z "$v" ] && continue
    grep -q -F "$v" "$S/out" "$S/err" "$S/ctr-argv" "$S/go-argv" 2>/dev/null && return 1
  done < <(cat "$S/tokens" "$S/passwords")
  return 0
}

[ -f "$RUNNER" ] || { echo "FAIL runner missing: scripts/live-matrix-local.sh"; echo "# 0 passed, 1 failed"; exit 1; }

# 1. happy path, image already cached.
S=$T/case-happy; mkdir -p "$S"; touch "$S/image-present"
run_case happy
[ "$(rc happy)" = 0 ] && ok "happy: rc 0" || bad "happy: rc $(rc happy) ($(tail -3 "$S/err" | tr '\n' ' '))"
RUNL=$(grep '^run ' "$S/ctr-argv")
[ -n "$RUNL" ] && [ -z "$(echo "$RUNL" | grep -o -E -- '-p [^ ]+' | grep -v -E '^-p 127\.0\.0\.1:[0-9]+:6167$')" ] \
  && ! echo "$RUNL" | grep -q -E -- '(^| )(-P|--publish-all|--publish|--network|--net)( |=|$)' && echo "$RUNL" | grep -q -E -- '-p 127\.0\.0\.1:[0-9]+:6167' \
  && ok "happy: published on 127.0.0.1 only" || bad "happy: publish not loopback-only"
echo "$RUNL" | grep -q -E 'matrixconduit/matrix-conduit@sha256:[0-9a-f]{64}( |$)' && ok "happy: image by digest" || bad "happy: image not by digest"
echo "$RUNL" | grep -q -E -- '-e CONDUIT_ALLOW_FEDERATION=false( |$)' && ok "happy: federation off" || bad "happy: federation not turned off"
echo "$RUNL" | grep -q -E -- '-e CONDUIT_ALLOW_CHECK_FOR_UPDATES=false( |$)' && ok "happy: update check off (no phone-home)" || bad "happy: update check not turned off"
echo "$RUNL" | grep -q -E -- '--tmpfs /data( |$)' && echo "$RUNL" | grep -q -E -- '-e CONDUIT_DATABASE_PATH=/data( |$)' && ! echo "$RUNL" | grep -q -E -- '(^| )(-v|--volume|--mount)( |=)' \
  && ok "happy: database on tmpfs, no volume or bind mount" || bad "happy: database not on tmpfs, or a volume/mount given"
echo "$RUNL" | grep -q -i -E 'token|password|secret' && bad "happy: credential-shaped word in the run argv" || ok "happy: no credential-shaped name in the run argv"
case "$(own_name happy)" in rysh-live-matrix-[0-9]*) ok "happy: container named rysh-live-matrix-<pid>" ;; *) bad "happy: container name '$(own_name happy)'" ;; esac
grep -q '^pull' "$S/ctr-argv" && bad "happy: pulled although cached" || ok "happy: no pull when cached"
[ "$(left_own happy)" = 0 ] && ok "happy: container removed" || bad "happy: container left"
rm_only_own happy && ok "happy: rm -f -v of this run's name only" || bad "happy: rm argv $(grep '^rm ' "$S/ctr-argv" | tr '\n' ';')"
no_stop happy && ok "happy: no stop before rm" || bad "happy: stop before rm"
grep -q -E 'ours left: 0' "$S/err" && ok "happy: leftover count 0 logged" || bad "happy: no 'ours left: 0' line"
[ "$(grep -c . "$S/out")" = 2 ] && ! grep -q -v '^{' "$S/out" && ok "happy: stdout is go test JSON only" || bad "happy: stdout not JSON-only"
[ "$(grep -c ' issued$' "$S/go-token-check")" = 2 ] && ok "happy: both tokens reach go by environment" || bad "happy: tokens not in go env"
[ -e "$S/go-token-distinct" ] && ok "happy: bot and echo tokens differ" || bad "happy: bot and echo tokens are the same"
[ "$(cat "$S/go-url")" = loopback ] && ok "happy: server URL is http://127.0.0.1" || bad "happy: server URL not loopback"
[ "$(cat "$S/go-room")" = roomid ] && ok "happy: room id handed to go" || bad "happy: no room id"
grep -q -x -E 'AWS_SECRET_ACCESS_KEY|GH_TOKEN|FAKE_STATE|FAKE_MX_PY|MX_LOCAL_TEST|MX_LOCAL_CTR' "$S/go-env-names" && bad "happy: a parent name reached go" || ok "happy: standing names do not reach go"
miss=""; for n in RYSH_LIVE_MATRIX_SERVER_URL RYSH_LIVE_MATRIX_BOT_TOKEN RYSH_LIVE_MATRIX_ROOM_ID RYSH_LIVE_MATRIX_ECHO_TOKEN GOFLAGS GOTOOLCHAIN; do
  grep -q -x "$n" "$S/go-env-names" || miss="$miss $n"
done
[ -z "$miss" ] && ok "happy: go env has the 4 live names + GOFLAGS/GOTOOLCHAIN" || bad "happy: missing in go env:$miss"
grep -q -E -- '-tags livechannels -run \^TestLiveMatrix -json -count=1 -timeout 10m \./internal/channels/' "$S/go-argv" && ok "happy: go test command" || bad "happy: go argv $(cat "$S/go-argv")"
[ "$(grep -c '^POST /register$' "$S/calls")" -ge 6 ] && ok "happy: 3 accounts registered (dummy auth)" || bad "happy: register calls $(grep -c '^POST /register$' "$S/calls")"
no_secret_leak happy && ok "happy: no token or password in output or argv" || bad "happy: a token or password leaked"
[ "$(sort -u "$S/ctr-context")" = colima ] && [ "$(grep -c . "$S/ctr-context")" = "$(grep -c . "$S/ctr-argv")" ] && ok "happy: every container call names --context colima" || bad "happy: a call without --context colima"
[ "$(grep -c . "$S/tokens")" -ge 3 ] && [ "$(grep -c . "$S/passwords")" -ge 3 ] && ok "happy: (sanity) fake issued tokens and saw passwords" || bad "happy: fake saw no tokens/passwords (vacuous)"

# 2. image missing: never pulled at night; rc 3 (infrastructure), nothing started.
run_case nopull
S=$T/case-nopull
[ "$(rc nopull)" = 3 ] && ! grep -q '^pull' "$S/ctr-argv" && ! grep -q '^run ' "$S/ctr-argv" && grep -q INFRA "$S/err" && ok "image missing: rc 3, no pull, nothing started" || bad "image missing: rc $(rc nopull)"

# 3. a leftover of ours from a killed run, and a foreign container: neither is touched (infra-ops
# condition 1); the leftover is counted and logged so qa's evidence shows it.
S=$T/case-leftover; mkdir -p "$S"; touch "$S/image-present"; printf '%s\n' rysh-live-matrix-1 someone-elses-db > "$S/containers"
run_case leftover
[ "$(rc leftover)" = 0 ] && ok "leftover: run still passes" || bad "leftover: rc $(rc leftover)"
grep -q -x -F rysh-live-matrix-1 "$S/containers" && grep -q -x -F someone-elses-db "$S/containers" && ok "leftover: old and foreign containers untouched" || bad "leftover: removed a container that is not this run's"
rm_only_own leftover && ok "leftover: rm only of this run's name" || bad "leftover: rm argv $(grep '^rm ' "$S/ctr-argv" | tr '\n' ';')"
grep -q -E '^ps .*label=rysh\.channels\.mx-local=1' "$S/ctr-argv" && grep -q -i 'leftover' "$S/err" && ok "leftover: counted by our label and logged" || bad "leftover: not counted/logged"

# 4. an account step fails: rc 3, go never runs, container removed.
S=$T/case-setupfail; mkdir -p "$S"; touch "$S/image-present"
run_case setupfail FAKE_MX_FAIL=/createRoom
[ "$(rc setupfail)" = 3 ] && ok "setup fails: rc 3" || bad "setup fails: rc $(rc setupfail)"
[ -e "$S/go-argv" ] && bad "setup fails: go ran" || ok "setup fails: go not run"
[ "$(left_own setupfail)" = 0 ] && rm_only_own setupfail && ok "setup fails: own container removed (rm -f -v)" || bad "setup fails: container left or wrong rm"
no_secret_leak setupfail && ok "setup fails: no token or password leaked" || bad "setup fails: a secret leaked"

# 5. go fails: its rc is passed on, container removed.
S=$T/case-gofail; mkdir -p "$S"; touch "$S/image-present"
run_case gofail FAKE_GO_MODE=fail
[ "$(rc gofail)" = 1 ] && [ "$(left_own gofail)" = 0 ] && rm_only_own gofail && ok "go fails: rc 1, container removed" || bad "go fails: rc $(rc gofail)"

# 6. container runtime down (colima not started): rc 4 = SKIP, nothing run, never started by us.
S=$T/case-down; mkdir -p "$S"; touch "$S/runtime-down"
run_case down
[ "$(rc down)" = 4 ] && ! grep -q '^run ' "$S/ctr-argv" && grep -q 'SKIP' "$S/err" && ok "runtime down: rc 4 SKIP, nothing started" || bad "runtime down: rc $(rc down)"

# 7. TERM while go runs: container removed, go stopped.
S=$T/case-term; mkdir -p "$S"; touch "$S/image-present"; : > "$S/tokens"; : > "$S/passwords"; : > "$S/containers"
echo hang > "$S/go-mode"
env -i PATH="/usr/bin:/bin:/usr/sbin:/sbin" HOME="$T" TMPDIR="$S" MX_LOCAL_TEST=1 MX_LOCAL_CTR="$FAKE_CTR" MX_LOCAL_READY_SECS=20 \
  FAKE_STATE="$S" FAKE_MX_PY="$FAKE_MX" bash "$RUNNER" "$FAKE_GO" > "$S/out" 2> "$S/err" &
RP=$!
for _ in $(seq 1 100); do [ -e "$S/go-started" ] && break; sleep 0.2; done
kill -TERM "$RP"; wait "$RP"; trc=$?
[ -e "$S/go-started" ] && [ "$trc" -ge 128 ] && [ "$(left_own term)" = 0 ] && rm_only_own term && ok "TERM: container removed, rc $trc" || bad "TERM: rc $trc, started=$([ -e "$S/go-started" ] && echo y || echo n)"

# 8. test hooks on a real run: refused before any container call.
S=$T/case-hooks; mkdir -p "$S"
env -i PATH="/usr/bin:/bin:/usr/sbin:/sbin" HOME="$T" MX_LOCAL_CTR="$FAKE_CTR" FAKE_STATE="$S" bash "$RUNNER" "$FAKE_GO" > "$S/out" 2> "$S/err"; hrc=$?
[ "$hrc" = 2 ] && [ ! -e "$S/ctr-argv" ] && grep -q 'MX_LOCAL_CTR' "$S/err" && ok "hooks on a real run: refused (rc 2), no container call" || bad "hooks: rc $hrc"

# 9. usage: no go binary given.
env -i PATH="/usr/bin:/bin" HOME="$T" bash "$RUNNER" > /dev/null 2>&1; urc=$?
[ "$urc" = 2 ] && ok "usage: rc 2 without a go binary" || bad "usage: rc $urc"

echo "# $pass passed, $fail failed"
[ "$fail" = 0 ] && [ "$pass" -gt 0 ]
