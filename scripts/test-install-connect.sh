#!/usr/bin/env bash
# Exercises scripts/install.sh end to end against a local fake artifact server,
# with a fake `ry` that records how it was called — so the RYSH_CONNECT hand-off
# (design 037) is tested the way `curl | sh` runs it: stdin on a pipe.
#
#   scripts/test-install-connect.sh        # exits non-zero on the first failure
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
INSTALLER="$HERE/install.sh"
TMP=$(mktemp -d); trap 'rm -rf "$TMP"; [ -n "${SERVER_PID:-}" ] && kill "$SERVER_PID" 2>/dev/null || true' EXIT

OS=$(uname -s | tr '[:upper:]' '[:lower:]'); ARCH=$(uname -m)
case "$ARCH" in x86_64|amd64) ARCH=amd64;; aarch64|arm64) ARCH=arm64;; esac

# Fake artifact tree: latest.txt, the tarball, checksums.
mkdir -p "$TMP/site/releases/v0.0.1" "$TMP/build" "$TMP/bin"
echo "0.0.1" > "$TMP/site/releases/latest.txt"
cat > "$TMP/build/ry" <<'FAKE'
#!/bin/sh
# Fake ry: record every invocation and whether stdin was a terminal.
if [ -t 0 ]; then tty=tty; else tty=notty; fi
echo "args=$* stdin=$tty" >> "${RY_FAKE_LOG:?}"
case "$1" in --version) echo "ry 0.0.1 (fake)";; esac
exit 0
FAKE
chmod +x "$TMP/build/ry"
tar czf "$TMP/site/releases/v0.0.1/ry_${OS}_${ARCH}.tar.gz" -C "$TMP/build" ry
( cd "$TMP/site/releases/v0.0.1" && sha256sum "ry_${OS}_${ARCH}.tar.gz" > checksums.txt )

PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
( cd "$TMP/site" && python3 -m http.server "$PORT" --bind 127.0.0.1 >/dev/null 2>&1 ) & SERVER_PID=$!
for _ in $(seq 1 30); do curl -fsS "http://127.0.0.1:$PORT/releases/latest.txt" >/dev/null 2>&1 && break; sleep 0.2; done

run_installer() { # $1 = log file; remaining = env assignments
  local log=$1; shift
  : > "$log"
  env RYSH_BASE_URL="http://127.0.0.1:$PORT" RYSH_INSTALL_DIR="$TMP/bin" RY_FAKE_LOG="$log" "$@" sh "$INSTALLER" < /dev/null
}

pass=0; fail() { echo "FAIL: $1" >&2; exit 1; }

# 1. RYSH_CONNECT with RYSH_CONNECT_OPEN=0 → connect --no-open, plus device and dir.
run_installer "$TMP/log1" RYSH_CONNECT="https://letchat.ai rysh_ct_abc" RYSH_CONNECT_OPEN=0 RYSH_CONNECT_DEVICE="test box" RYSH_CONNECT_DIR="$TMP/company" >/dev/null
grep -q '^args=connect https://letchat.ai rysh_ct_abc --no-open --device test box --dir '"$TMP"'/company stdin=notty$' "$TMP/log1" || fail "no-open hand-off: $(cat "$TMP/log1")"
grep -q '^args=--version' "$TMP/log1" || fail "installer skipped the version check"
[ -x "$TMP/bin/ry" ] || fail "binary not installed"
pass=$((pass+1))

# 2. Without RYSH_CONNECT nothing is connected and the quick start prints.
out=$(run_installer "$TMP/log2")
grep -q 'connect' "$TMP/log2" && fail "connect ran without RYSH_CONNECT: $(cat "$TMP/log2")"
echo "$out" | grep -q 'Quick start' || fail "quick start missing without RYSH_CONNECT"
pass=$((pass+1))

# 3. A malformed RYSH_CONNECT fails after install, before touching ry connect.
if run_installer "$TMP/log3" RYSH_CONNECT="onlyserver" >/dev/null 2>"$TMP/err3"; then fail "malformed RYSH_CONNECT was accepted"; fi
grep -q 'RYSH_CONNECT must be' "$TMP/err3" || fail "malformed RYSH_CONNECT: wrong message: $(cat "$TMP/err3")"
grep -q 'connect' "$TMP/log3" && fail "ry connect ran with a malformed RYSH_CONNECT"
pass=$((pass+1))

# 4. OPEN=auto: with a controlling terminal, stdin is re-attached to /dev/tty and
#    the session opens; without one (CI, a subprocess) the installer must fall
#    back to --no-open rather than die on the redirect after installing.
run_installer "$TMP/log4" RYSH_CONNECT="https://letchat.ai rysh_ct_abc" >/dev/null 2>&1 || true
if ( : < /dev/tty ) 2>/dev/null; then
  grep -q '^args=connect https://letchat.ai rysh_ct_abc stdin=tty$' "$TMP/log4" || fail "tty hand-off: $(cat "$TMP/log4")"
  echo "note: a controlling terminal is present — the open path was exercised"
else
  grep -q '^args=connect https://letchat.ai rysh_ct_abc --no-open stdin=notty$' "$TMP/log4" || fail "no-terminal fallback: $(cat "$TMP/log4")"
  echo "note: no controlling terminal here — the --no-open fallback was exercised"
fi
pass=$((pass+1))

echo "ok: $pass installer checks passed"
