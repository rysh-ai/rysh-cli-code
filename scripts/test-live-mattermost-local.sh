#!/usr/bin/env bash
# Offline tests for scripts/live-mattermost-local.sh (the D-61 nightly live test on a local
# Mattermost). No real container runtime, registry or Mattermost is used: a fake container CLI
# (an absolute path, not named like the tool, never on PATH) starts a fake Mattermost API on the
# 127.0.0.1 port the runner asks for, and a fake go records names only. Everything binds 127.0.0.1.
#
# Usage: scripts/test-live-mattermost-local.sh
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
RUNNER=$HERE/live-mattermost-local.sh
T=$(umask 077; mktemp -d "${TMPDIR:-/tmp}/mm-local-test.XXXXXX") || exit 1
trap 'kill_fakes; rm -rf "$T"' EXIT
pass=0; fail=0
ok() { pass=$((pass+1)); echo "ok   $1"; }
bad() { fail=$((fail+1)); echo "FAIL $1"; }

mkdir -p "$T/bin"
FAKE_CTR=$T/bin/fake-container-cli
FAKE_GO=$T/bin/fake-go-cli
FAKE_MM=$T/bin/fake-mattermost.py

# --- fake Mattermost API: the endpoints the runner's setup uses. Issued tokens are written to
# $STATE/tokens so the test can look for them in argv and output; nothing else reads them.
cat > "$FAKE_MM" <<'PY'
import json, os, secrets, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port, state = int(sys.argv[1]), sys.argv[2]
fail_on = os.environ.get("FAKE_MM_FAIL", "")
ids = {}
def nid(): return secrets.token_hex(13)
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def reply(self, code, obj, hdr=None):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        for k, v in (hdr or {}).items(): self.send_header(k, v)
        self.send_header("Content-Length", str(len(b)))
        self.end_headers(); self.wfile.write(b)
    def do_GET(self):
        if self.path == "/api/v4/system/ping": return self.reply(200, {"status": "OK"})
        self.reply(404, {"id": "not_found"})
    def do_PUT(self): self.handle_any("PUT")
    def do_POST(self): self.handle_any("POST")
    def handle_any(self, m):
        n = int(self.headers.get("Content-Length") or 0)
        self.rfile.read(n)
        p = self.path[len("/api/v4"):]
        with open(state + "/calls", "a") as f: f.write(f"{m} {p}\n")
        if fail_on and p.startswith(fail_on): return self.reply(500, {"id": "fake.fail"})
        if p != "/users" and p != "/users/login" and not self.headers.get("Authorization", "").startswith("Bearer "):
            return self.reply(401, {"id": "no_auth"})
        if p == "/users/login":
            t = "sess" + secrets.token_hex(12)
            with open(state + "/tokens", "a") as f: f.write(t + "\n")
            return self.reply(200, {"id": nid()}, {"Token": t})
        if p.endswith("/tokens"):
            t = "pat" + secrets.token_hex(12)
            with open(state + "/tokens", "a") as f: f.write(t + "\n")
            return self.reply(200, {"id": nid(), "token": t})
        if p == "/bots": return self.reply(201, {"user_id": nid()})
        return self.reply(201 if m == "POST" else 200, {"id": nid()})
HTTPServer(("127.0.0.1", port), H).serve_forever()
PY

# --- fake container CLI: logs argv; run starts the fake API on the published loopback port.
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
    python3 -I "$FAKE_MM_PY" "$port" "$S" >/dev/null 2>&1 &
    echo $! > "$S/mm-pid"
    echo "cid-$name" ;;
  port) echo "127.0.0.1:0" ;;
  stop) : ;;
  rm)
    shift; while [ "${1#-}" != "${1:-}" ]; do shift; done
    for n in "$@"; do grep -v -x -F "$n" "$S/containers" > "$S/c.tmp" 2>/dev/null; mv "$S/c.tmp" "$S/containers"; done
    [ -s "$S/mm-pid" ] && { kill "$(cat "$S/mm-pid")" 2>/dev/null; : > "$S/mm-pid"; } ;;
  *) exit 1 ;;
esac
SH

# --- fake go: records its argv and the NAMES in its environment; emits go test -json lines.
cat > "$FAKE_GO" <<'SH'
#!/usr/bin/env bash
S=$TMPDIR   # the runner keeps only PATH HOME TMPDIR ... for go; the test points TMPDIR at the case dir
printf '%s\n' "$*" > "$S/go-argv"
compgen -e | sort > "$S/go-env-names"
for n in RYSH_LIVE_MATTERMOST_BOT_TOKEN RYSH_LIVE_MATTERMOST_ECHO_TOKEN; do
  v=${!n:-}
  if [ -n "$v" ] && grep -q -x -F "$v" "$S/tokens"; then echo "$n issued" >> "$S/go-token-check"; else echo "$n not-issued" >> "$S/go-token-check"; fi
done
case "${RYSH_LIVE_MATTERMOST_SERVER_URL:-}" in http://127.0.0.1:*) echo loopback > "$S/go-url" ;; *) echo other > "$S/go-url" ;; esac
case "$(cat "$S/go-mode" 2>/dev/null || echo pass)" in
  pass) echo '{"Action":"pass","Test":"TestLiveMattermostOutbound"}'; echo '{"Action":"pass","Test":"TestLiveMattermostRoundTrip"}'; exit 0 ;;
  fail) echo '{"Action":"fail","Test":"TestLiveMattermostRoundTrip"}'; exit 1 ;;
  hang) touch "$S/go-started"; sleep 30; exit 0 ;;
esac
SH
chmod +x "$FAKE_CTR" "$FAKE_GO"

kill_fakes() { for f in "$T"/case-*/mm-pid; do [ -s "$f" ] && kill "$(cat "$f")" 2>/dev/null; done; return 0; }

# run_case <name> [VAR=value ...]: runs the runner with the fakes; state in $T/case-<name>.
run_case() {
  local name=$1; shift
  S=$T/case-$name; mkdir -p "$S"; : > "$S/tokens"; : > "$S/containers"
  for a in "$@"; do case $a in FAKE_GO_MODE=*) echo "${a#FAKE_GO_MODE=}" > "$S/go-mode" ;; esac; done
  env -i PATH="/usr/bin:/bin:/usr/sbin:/sbin" HOME="$T" TMPDIR="$S" \
    MM_LOCAL_TEST=1 MM_LOCAL_CTR="$FAKE_CTR" MM_LOCAL_READY_SECS=20 \
    FAKE_STATE="$S" FAKE_MM_PY="$FAKE_MM" \
    AWS_SECRET_ACCESS_KEY=fake-standing-name GH_TOKEN=fake-standing-name "$@" \
    bash "$RUNNER" "$FAKE_GO" > "$S/out" 2> "$S/err"
  echo $? > "$S/rc"
}
rc() { cat "$T/case-$1/rc"; }
# vols_gone <case>: every rm of our container also removes its anonymous volumes (-v). The image
# declares two (Mattermost data + Postgres), which hold the run's throwaway accounts and tokens;
# without -v each run left both behind in the VM.
# no_stop <case>: the throwaway container is removed with rm -f -v alone; a stop first waits out the
# runtime's 10 s grace (the server ignores SIGTERM), a quarter of every run (2026-11-07 timing).
no_stop() { ! grep -q '^stop ' "$T/case-$1/ctr-argv"; }
vols_gone() { local S=$T/case-$1; grep -q '^rm ' "$S/ctr-argv" && ! grep '^rm ' "$S/ctr-argv" | grep -q -v -E '^rm -f -v '; }
left() { grep -c . "$T/case-$1/containers"; }
no_token_leak() { # no issued token in the runner's output, error stream or any argv
  local S=$T/case-$1 tok
  while read -r tok; do
    [ -z "$tok" ] && continue
    grep -q -F "$tok" "$S/out" "$S/err" "$S/ctr-argv" "$S/go-argv" 2>/dev/null && return 1
  done < "$S/tokens"
  return 0
}

[ -f "$RUNNER" ] || { echo "FAIL runner missing: $RUNNER"; exit 1; }

# 1. happy path, image already cached: no pull; loopback publish; digest; 2 tests pass; cleaned up.
S=$T/case-happy; mkdir -p "$S"; touch "$S/image-present"
run_case happy
[ "$(rc happy)" = 0 ] && ok "happy: rc 0" || bad "happy: rc $(rc happy) ($(tail -3 "$S/err" | tr '\n' ' '))"
RUNL=$(grep '^run ' "$S/ctr-argv")
[ -n "$RUNL" ] && [ -z "$(echo "$RUNL" | grep -o -E -- '-p [^ ]+' | grep -v -E '^-p 127\.0\.0\.1:[0-9]+:8065$')" ] \
  && ! echo "$RUNL" | grep -q -E -- '(^| )(-P|--publish-all|--publish|--network|--net)( |=|$)' && echo "$RUNL" | grep -q -E -- '-p 127\.0\.0\.1:[0-9]+:8065' \
  && ok "happy: published on 127.0.0.1 only" || bad "happy: publish not loopback-only"
grep '^run ' "$S/ctr-argv" | grep -q -E 'mattermost/mattermost-preview@sha256:[0-9a-f]{64}' && ok "happy: image by digest" || bad "happy: image not by digest"
# security-lead N1: no phone-home from a nightly server (diagnostics/telemetry, security-update check).
grep '^run ' "$S/ctr-argv" | grep -q -- '-e MM_LOGSETTINGS_ENABLEDIAGNOSTICS=false' && grep '^run ' "$S/ctr-argv" | grep -q -- '-e MM_SERVICESETTINGS_ENABLESECURITYFIXALERT=false' \
  && ok "happy: diagnostics and update check off" || bad "happy: diagnostics or update check not turned off"
grep -q '^pull' "$S/ctr-argv" && bad "happy: pulled although cached" || ok "happy: no pull when cached"
[ "$(left happy)" = 0 ] && ok "happy: container removed" || bad "happy: $(left happy) container(s) left"
vols_gone happy && ok "happy: container volumes removed (rm -v)" || bad "happy: rm without -v leaves the image volumes (db, data) behind"
no_stop happy && ok "happy: no stop before rm (no 10 s grace wait)" || bad "happy: stop before rm waits out the 10 s grace"
[ "$(grep -c . "$S/out")" = 2 ] && ! grep -q -v '^{' "$S/out" && ok "happy: stdout is go test JSON only" || bad "happy: stdout not JSON-only"
[ "$(grep -c ' issued$' "$S/go-token-check")" = 2 ] && ok "happy: both tokens reach go by environment" || bad "happy: tokens not in go env"
[ "$(cat "$S/go-url")" = loopback ] && ok "happy: server URL is http://127.0.0.1" || bad "happy: server URL not loopback"
grep -q -x -E 'AWS_SECRET_ACCESS_KEY|GH_TOKEN|FAKE_STATE|FAKE_MM_PY|MM_LOCAL_TEST|MM_LOCAL_CTR' "$S/go-env-names" && bad "happy: a parent name reached go" || ok "happy: standing names do not reach go"
for n in RYSH_LIVE_MATTERMOST_SERVER_URL RYSH_LIVE_MATTERMOST_BOT_TOKEN RYSH_LIVE_MATTERMOST_CHANNEL_ID RYSH_LIVE_MATTERMOST_ECHO_TOKEN GOFLAGS GOTOOLCHAIN; do
  grep -q -x "$n" "$S/go-env-names" || bad "happy: $n missing in go env"
done
grep -q -E -- '-tags livechannels -run \^TestLiveMattermost -json -count=1 -timeout 10m \./internal/channels/' "$S/go-argv" && ok "happy: go test command" || bad "happy: go argv $(cat "$S/go-argv")"
no_token_leak happy && ok "happy: no token in output or argv" || bad "happy: a token leaked"
[ "$(sort -u "$S/ctr-context")" = colima ] && [ "$(grep -c . "$S/ctr-context")" = "$(grep -c . "$S/ctr-argv")" ] && ok "happy: every container call names --context colima" || bad "happy: a call without --context colima"
[ "$(grep -c . "$S/tokens")" -ge 3 ] && ok "happy: (sanity) fake issued tokens" || bad "happy: fake issued no tokens (vacuous)"

# 2. image missing: never pulled at night; rc 3 (infrastructure), nothing started.
run_case nopull
S=$T/case-nopull
[ "$(rc nopull)" = 3 ] && ! grep -q '^pull' "$S/ctr-argv" && ! grep -q '^run ' "$S/ctr-argv" && ok "image missing: rc 3, no pull, nothing started" || bad "image missing: rc $(rc nopull)"

# 3. a leftover labelled container from a killed run is removed first; only by our label.
S=$T/case-leftover; mkdir -p "$S"; touch "$S/image-present"; echo rysh-channels-mm-local-old > "$S/containers.seed"
mkdir -p "$S"; cp "$S/containers.seed" "$S/containers"
( S=$T/case-leftover; : > "$S/tokens"
  env -i PATH="/usr/bin:/bin:/usr/sbin:/sbin" HOME="$T" TMPDIR="$S" MM_LOCAL_TEST=1 MM_LOCAL_CTR="$FAKE_CTR" MM_LOCAL_READY_SECS=20 \
    FAKE_STATE="$S" FAKE_MM_PY="$FAKE_MM" bash "$RUNNER" "$FAKE_GO" > "$S/out" 2> "$S/err"; echo $? > "$S/rc" )
grep -q -E '^ps .*label=rysh\.channels\.mm-local=1' "$S/ctr-argv" && grep -q -E '^rm -f( -v)? rysh-channels-mm-local-old' "$S/ctr-argv" && [ "$(left leftover)" = 0 ] \
  && ok "leftover: our labelled container removed, by label filter" || bad "leftover: not removed ($(left leftover) left)"
vols_gone leftover && ok "leftover: volumes removed too (rm -v)" || bad "leftover: rm without -v"

# 4. an account step fails: rc 3, go never runs, container removed.
S=$T/case-setupfail; mkdir -p "$S"; touch "$S/image-present"
run_case setupfail FAKE_MM_FAIL=/bots
[ "$(rc setupfail)" = 3 ] && ok "setup fails: rc 3" || bad "setup fails: rc $(rc setupfail)"
[ -e "$S/go-argv" ] && bad "setup fails: go ran" || ok "setup fails: go not run"
[ "$(left setupfail)" = 0 ] && ok "setup fails: container removed" || bad "setup fails: container left"
vols_gone setupfail && ok "setup fails: volumes removed (rm -v)" || bad "setup fails: rm without -v"
no_stop setupfail && ok "setup fails: no stop before rm" || bad "setup fails: stop before rm"
no_token_leak setupfail && ok "setup fails: no token leaked" || bad "setup fails: a token leaked"

# 5. go fails: its rc is passed on, container removed.
S=$T/case-gofail; mkdir -p "$S"; touch "$S/image-present"
run_case gofail FAKE_GO_MODE=fail
[ "$(rc gofail)" = 1 ] && [ "$(left gofail)" = 0 ] && ok "go fails: rc 1, container removed" || bad "go fails: rc $(rc gofail), $(left gofail) left"
vols_gone gofail && ok "go fails: volumes removed (rm -v)" || bad "go fails: rm without -v"
no_stop gofail && ok "go fails: no stop before rm" || bad "go fails: stop before rm"

# 6. container runtime down (colima not started): rc 4 = SKIP, nothing run, never started by us.
S=$T/case-down; mkdir -p "$S"; touch "$S/runtime-down"
run_case down
[ "$(rc down)" = 4 ] && ! grep -q '^run ' "$S/ctr-argv" && grep -q 'SKIP' "$S/err" && ok "runtime down: rc 4 SKIP, nothing started" || bad "runtime down: rc $(rc down)"

# 7. TERM while go runs: container removed, go stopped.
S=$T/case-term; mkdir -p "$S"; touch "$S/image-present"; : > "$S/tokens"; : > "$S/containers"
echo hang > "$S/go-mode"
env -i PATH="/usr/bin:/bin:/usr/sbin:/sbin" HOME="$T" TMPDIR="$S" MM_LOCAL_TEST=1 MM_LOCAL_CTR="$FAKE_CTR" MM_LOCAL_READY_SECS=20 \
  FAKE_STATE="$S" FAKE_MM_PY="$FAKE_MM" bash "$RUNNER" "$FAKE_GO" > "$S/out" 2> "$S/err" &
RP=$!
for _ in $(seq 1 100); do [ -e "$S/go-started" ] && break; sleep 0.2; done
kill -TERM "$RP"; wait "$RP"; trc=$?
vols_gone term && ok "TERM: volumes removed (rm -v)" || bad "TERM: rm without -v"
no_stop term && ok "TERM: no stop before rm" || bad "TERM: stop before rm"
[ -e "$S/go-started" ] && [ "$trc" -ge 128 ] && [ "$(left term)" = 0 ] && ok "TERM: container removed, rc $trc" || bad "TERM: rc $trc, $(left term) left, started=$([ -e "$S/go-started" ] && echo y || echo n)"

# 8. test hooks on a real run: refused before any container call.
S=$T/case-hooks; mkdir -p "$S"
env -i PATH="/usr/bin:/bin:/usr/sbin:/sbin" HOME="$T" MM_LOCAL_CTR="$FAKE_CTR" FAKE_STATE="$S" bash "$RUNNER" "$FAKE_GO" > "$S/out" 2> "$S/err"; hrc=$?
[ "$hrc" = 2 ] && [ ! -e "$S/ctr-argv" ] && grep -q 'MM_LOCAL_CTR' "$S/err" && ok "hooks on a real run: refused (rc 2), no container call" || bad "hooks: rc $hrc"

# 9. usage: no go binary given.
env -i PATH="/usr/bin:/bin" HOME="$T" bash "$RUNNER" > /dev/null 2>&1; urc=$?
[ "$urc" = 2 ] && ok "usage: rc 2 without a go binary" || bad "usage: rc $urc"

echo "# $pass passed, $fail failed"
[ "$fail" = 0 ] && [ "$pass" -gt 0 ]
