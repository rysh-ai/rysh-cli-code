#!/usr/bin/env bash
#
# install-rysh-test.sh: the open-source installer fails closed (sprint 0.2.15
# R0-E4b; security-lead ledger 145 I1-I11 and ledger 148). install-rysh.sh must
# verify the detached signature checksums.txt.sig with the pinned release key
# (VALIDSIG primary fingerprint, field 10), then match the archive against
# exactly one checksums.txt line, and refuse everything else. The one opt-out
# (RYSH_INSTALL_INSECURE_SKIP_VERIFY=1) skips only the signature.
#
# Offline. Throwaway keys (no passphrase) are made at test time in a scratch
# GNUPGHOME with its own agent, stopped at the end; no private key is committed
# and no key or signature bytes are printed. A test key reaches the installer
# only through a DERIVED copy that differs from install-rysh.sh in the pin line
# and the embedded key block alone (checked), never through an environment
# seam. Downloads come from file:// release dirs inside the marked harness.
# On macOS every installer run is inside sandbox-exec with (deny network*).
#
# Usage: install-rysh-test.sh [--script <install-rysh.sh>] [--release-key <armored public key file>]
#   --script        the installer under test (default: beside this file); a RED
#                   run points it at an older copy
#   --release-key   root scripts/oss-assets/<key>.asc: the embedded key block
#                   must equal it byte for byte. Without it the drift check is
#                   NOT RUN and counts as a failure (a skip is not a pass).
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
SCRIPT="$HERE/install-rysh.sh"; RELKEY=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --script) SCRIPT="$2"; shift 2 ;;
    --release-key) RELKEY="$2"; shift 2 ;;
    *) echo "usage: $0 [--script <path>] [--release-key <file>]" >&2; exit 2 ;;
  esac
done
unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_DEFAULT_REGION AWS_SESSION_TOKEN S3_ENDPOINT_URL \
      GEMINI_API_KEY GIT_USERNAME TANGOLIFE_BLOG_AUTOMATION_TOKEN TANGOLIFE_INSTAGRAM_PASSWORD \
      TANGOLIFE_INSTAGRAM_USERNAME HOMEBREW_TAP_TOKEN NPM_TOKEN NODE_AUTH_TOKEN RYSH_CLI_PAT GH_TOKEN \
      GPG_FINGERPRINT GPG_KEY_FILE NFPM_PASSPHRASE GORELEASER_GITHUB_TOKEN GITHUB_TOKEN PERALABS_CO_UK_PEM \
      RYSH_INSTALL_INSECURE_SKIP_VERIFY RYSH_INSTALL_TEST_HARNESS RYSH_INSTALL_TEST_BASE RYSH_INSTALL_TEST_HIDE \
      RYSH_VERSION RYSH_REPO RYSH_INSTALL_DIR
TMP="$(cd "$(mktemp -d)" && pwd -P)"
export GNUPGHOME="$TMP/gnupg"; mkdir -m 700 "$GNUPGHOME"
trap 'gpgconf --kill gpg-agent >/dev/null 2>&1; chmod -R u+w "$TMP" 2>/dev/null; rm -rf "$TMP"' EXIT
pass=0 failn=0
ok()  { echo "ok   $1"; pass=$((pass+1)); }
bad() { echo "FAIL $1"; [[ -n "${2:-}" ]] && sed 's/^/     | /' <<<"$2"; failn=$((failn+1)); }
[[ -f "$SCRIPT" ]] || { echo "FAIL no installer at $SCRIPT"; exit 1; }
command -v gpg >/dev/null 2>&1 || { echo "FAIL gpg is required to run this test (not a skip)"; exit 1; }

SANDBOX=()
if command -v sandbox-exec >/dev/null 2>&1; then
  SANDBOX=(sandbox-exec -p '(version 1)(allow default)(deny network*)')
fi
H="$TMP/harness"; mkdir -p "$H"; : >"$H/.rysh-install-test-harness"
VER=0.0.99; TAG="v$VER"

# The platform the installer will pick, computed the way it does.
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in x86_64|amd64) ARCH=amd64 ;; arm64|aarch64) ARCH=arm64 ;; *) ARCH="$(uname -m)" ;; esac
[[ "$OS" == darwin && "$ARCH" == amd64 && "$(sysctl -n hw.optional.arm64 2>/dev/null)" == 1 ]] && ARCH=arm64
ARCHIVE="rysh_${OS}_${ARCH}.tar.gz"

sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi; }

# ── keys (throwaway; primary certifies, a subkey signs, like the release key) ──
genkey() {   # genkey <name> [extra gpg args...] -> primary fingerprint on stdout
  local name="$1"; shift
  gpg --batch --quiet --passphrase '' "$@" --quick-gen-key "$name <test@invalid>" ed25519 cert 1d >/dev/null 2>&1 || return 1
  local fpr; fpr="$(gpg --batch --with-colons --list-keys "$name <test@invalid>" 2>/dev/null | awk -F: '$1=="fpr"{print $10; exit}')"
  gpg --batch --quiet --passphrase '' "$@" --quick-add-key "$fpr" ed25519 sign 1d >/dev/null 2>&1 || return 1
  echo "$fpr"
}
A="$(genkey 'rysh test release key')"
B="$(genkey 'rysh test other key')"
E="$(genkey 'rysh test expired key' --faked-system-time 20200101T000000!)"
R="$(genkey 'rysh test revoked key')"
[[ -n "$A" && -n "$B" && -n "$E" && -n "$R" ]] && ok "setup: four throwaway keys in the scratch GNUPGHOME" || bad "setup: key generation failed"
gpg --batch --armor --export "$A" >"$TMP/A.asc"
gpg --batch --armor --export "$B" >"$TMP/B.asc"
gpg --batch --armor --export "$E" >"$TMP/E.asc"

# ── a release dir: <dir>/<tag>/{archives, checksums.txt[, checksums.txt.sig]} ──
mkarchive() {   # mkarchive <file>: a tar.gz holding an executable "rysh"
  local d; d="$(mktemp -d "$TMP/a.XXXX")"
  printf '#!/bin/sh\necho "rysh test build"\n' >"$d/rysh"; chmod +x "$d/rysh"
  tar -czf "$1" -C "$d" rysh
}
mkrelease() {   # mkrelease <name> <signing key fpr|none> [sign args...] -> the release dir
  local r="$H/rel/$1" k="$2"; shift 2
  mkdir -p "$r/$TAG"
  local p
  for p in darwin_amd64 darwin_arm64 linux_amd64 linux_arm64; do mkarchive "$r/$TAG/rysh_$p.tar.gz"; done
  ( cd "$r/$TAG" && for f in rysh_*.tar.gz; do printf '%s  %s\n' "$(sha "$f")" "$f"; done ) >"$r/$TAG/checksums.txt"
  if [[ "$k" != none ]]; then
    gpg --batch --quiet --yes "$@" --local-user "$(subkey_of "$k")!" --armor --detach-sign \
        --output "$r/$TAG/checksums.txt.sig" "$r/$TAG/checksums.txt" >/dev/null 2>&1
  fi
  echo "$r"
}
resign() {   # resign <release dir> <signing key fpr>
  gpg --batch --quiet --yes --local-user "$(subkey_of "$2")!" --armor --detach-sign \
      --output "$1/$TAG/checksums.txt.sig" "$1/$TAG/checksums.txt" >/dev/null 2>&1
}
subkey_of() { gpg --batch --with-colons --list-keys "$1" 2>/dev/null | awk -F: '$1=="sub"{s=1} s&&$1=="fpr"{print $10; exit}'; }

# R signs a release while still valid; then it is revoked with the certificate
# gpg made at generation (its guard colon stripped)
REL_R="$(mkrelease revoked "$R")"
sed 's/^:-----/-----/' "$GNUPGHOME/openpgp-revocs.d/$R.rev" >"$TMP/R.rev" 2>/dev/null
gpg --batch --quiet --import "$TMP/R.rev" >/dev/null 2>&1
gpg --batch --armor --export "$R" >"$TMP/R.asc"
gpg --batch --with-colons --list-keys "$R" 2>/dev/null | grep -q '^pub:r:' && ok "setup: key R is revoked" || bad "setup: key R is not revoked"

# ── derived copies: only the pin line and the embedded key block change ──────
PIN_RE='^RYSH_RELEASE_KEY_FPR="[0-9A-F]{40}"$'
KEY_OPEN="cat <<'RYSH_RELEASE_PUBLIC_KEY'"; KEY_CLOSE='RYSH_RELEASE_PUBLIC_KEY'
strip_pin_and_key() {   # the installer without its pin line and key block
  awk -v o="$KEY_OPEN" -v c="$KEY_CLOSE" '
    $0 ~ /^RYSH_RELEASE_KEY_FPR="/ {next}
    in_k && $0 == c {in_k=0; print; next}
    in_k {next}
    { print } $0 ~ o {in_k=1}' "$1"
}
embedded_key() { awk -v o="$KEY_OPEN" -v c="$KEY_CLOSE" 'in_k && $0 == c {exit} in_k {print} index($0,o) {in_k=1}' "$1"; }
derive() {   # derive <out> <pin fpr> <armored key file>
  awk -v pin="$2" -v kf="$3" -v o="$KEY_OPEN" -v c="$KEY_CLOSE" '
    $0 ~ /^RYSH_RELEASE_KEY_FPR="/ { print "RYSH_RELEASE_KEY_FPR=\"" pin "\""; next }
    in_k && $0 == c { while ((getline l < kf) > 0) print l; in_k=0; print; next }
    in_k {next}
    { print } index($0,o) {in_k=1}' "$SCRIPT" >"$1"
}
DA="$TMP/derived-A.sh"; derive "$DA" "$A" "$TMP/A.asc"
if [[ "$(grep -c -E "$PIN_RE" "$SCRIPT")" == 1 && "$(grep -c -F "$KEY_OPEN" "$SCRIPT")" == 1 ]] \
   && diff <(strip_pin_and_key "$SCRIPT") <(strip_pin_and_key "$DA") >/dev/null \
   && [[ "$(grep -c -E "$PIN_RE" "$DA")" == 1 ]] && cmp -s <(embedded_key "$DA") "$TMP/A.asc"; then
  ok "derived copy differs from install-rysh.sh only in the pin line and the key block"
else
  bad "install-rysh.sh has no single pin line + key block, or a derived copy changes more than those"
fi

# ── running the installer ─────────────────────────────────────────────────────
OUT="" RC=0 BIN=""
run() {   # run <script> <release dir> [VAR=value...]: installs into a fresh dir
  local s="$1" r="$2"; shift 2
  BIN="$(mktemp -d "$TMP/bin.XXXX")"
  RC=0
  OUT="$(env RYSH_VERSION="$VER" RYSH_INSTALL_DIR="$BIN" RYSH_INSTALL_TEST_HARNESS="$H" \
             RYSH_INSTALL_TEST_BASE="file://$r" "$@" ${SANDBOX[@]+"${SANDBOX[@]}"} sh "$s" 2>&1)" || RC=$?
}
installed() { [[ -x "$BIN/rysh" ]]; }
nothing()   { [[ -z "$(ls -A "$BIN")" ]]; }
refused() {   # refused <label> <message ERE>
  if [[ "$RC" -ne 0 ]] && nothing && grep -q -E -- "$2" <<<"$OUT"; then ok "$1 (exit $RC, nothing installed)"
  else bad "$1: exit $RC, installed: $(nothing && echo no || echo yes)" "$OUT"; fi
}

# 1. good: signed by A's signing subkey; the pin is A's primary (I1: a subkey counts through its primary)
REL="$(mkrelease good "$A")"
run "$DA" "$REL"
if [[ "$RC" -eq 0 ]] && installed && grep -q 'Signature OK' <<<"$OUT" && grep -q 'Checksum OK' <<<"$OUT"; then
  ok "a release signed by the pinned key's signing subkey installs (signature + checksum OK)"
else bad "good release: exit $RC, installed: $(installed && echo yes || echo no)" "$OUT"; fi
SUBA="$(subkey_of "$A")"
[[ -n "$SUBA" && "$SUBA" != "$A" ]] && ok "the good case really signed with a subkey (VALIDSIG field 10 is the primary)" || bad "setup: A has no separate signing subkey"

# 2. the shipped pin: the unmodified installer refuses a release signed by a throwaway key
run "$SCRIPT" "$REL"
refused "the shipped installer refuses a release not signed by the rysh release key" 'not signed by the rysh release key|could not be verified'

# 3. tampered archive
REL="$(mkrelease tamper-archive "$A")"; printf 'x' >>"$REL/$TAG/$ARCHIVE"
run "$DA" "$REL"; refused "a tampered archive is refused" 'Checksum mismatch'

# 4. tampered checksums.txt after signing (a forged line)
REL="$(mkrelease tamper-sums "$A")"
printf 'x' >>"$REL/$TAG/$ARCHIVE"
( cd "$REL/$TAG" && for f in rysh_*.tar.gz; do printf '%s  %s\n' "$(sha "$f")" "$f"; done ) >"$REL/$TAG/checksums.txt"
run "$DA" "$REL"; refused "a checksums.txt changed after signing is refused (bad signature)" 'not signed by the rysh release key|could not be verified'

# 5. signed by another key that the installer does not hold
REL="$(mkrelease other-key "$B")"
run "$DA" "$REL"; refused "a signature by another key is refused" 'not signed by the rysh release key|could not be verified'

# 6. GOODSIG by a subkey of another primary: the embedded key is B, the pin is A
DB="$TMP/derived-B-pinA.sh"; derive "$DB" "$A" "$TMP/B.asc"
REL="$(mkrelease other-primary "$B")"
run "$DB" "$REL"; refused "a good signature whose VALIDSIG primary is not the pin is refused" 'not signed by the rysh release key'

# 7. no signature published: an older (<= v0.2.14) release refuses without the opt-out (I7)
REL="$(mkrelease unsigned none)"
run "$DA" "$REL"; refused "a release with no checksums.txt.sig is refused, naming the opt-out" 'checksums\.txt\.sig.*not published|RYSH_INSTALL_INSECURE_SKIP_VERIFY'

# 8. no checksums.txt
REL="$(mkrelease no-sums "$A")"; rm -f "$REL/$TAG/checksums.txt"
run "$DA" "$REL"; refused "a release with no checksums.txt is refused" 'checksums\.txt.*not published'
run "$DA" "$REL" RYSH_INSTALL_INSECURE_SKIP_VERIFY=1; refused "no checksums.txt is refused even with the opt-out" 'checksums\.txt.*not published'

# 9. gpg missing
REL="$(mkrelease no-gpg "$A")"
run "$DA" "$REL" RYSH_INSTALL_TEST_HIDE="gpg"; refused "no gpg is refused and says how to install it" 'gpg.*required|gnupg'

# 10. sha256 tool missing (with and without the opt-out)
run "$DA" "$REL" RYSH_INSTALL_TEST_HIDE="sha256sum shasum"; refused "no sha256 tool is refused" 'sha256'
run "$DA" "$REL" RYSH_INSTALL_TEST_HIDE="sha256sum shasum gpg" RYSH_INSTALL_INSECURE_SKIP_VERIFY=1
refused "no sha256 tool is refused even with the opt-out" 'sha256'

# 11. the filename-substring trick: only "<archive>.evil" is listed
REL="$(mkrelease substring none)"
h="$(sha "$REL/$TAG/$ARCHIVE")"; printf '%s  %s.evil\n%s  x%s\n' "$h" "$ARCHIVE" "$h" "$ARCHIVE" >"$REL/$TAG/checksums.txt"; resign "$REL" "$A"
run "$DA" "$REL"; refused "an archive listed only under a longer or prefixed name is refused" 'not listed'

# 12. a duplicate line for the archive
REL="$(mkrelease duplicate none)"
# Read first, then append: GNU grep refuses an input that is also its output
# (BSD grep allows it), which on Linux left one line and tested nothing.
line="$(grep -F "  $ARCHIVE" "$REL/$TAG/checksums.txt")"; printf '%s\n' "$line" >>"$REL/$TAG/checksums.txt"
[[ "$(grep -c -F "  $ARCHIVE" "$REL/$TAG/checksums.txt")" -eq 2 ]] || bad "duplicate fixture: checksums.txt does not list the archive twice" "$(cat "$REL/$TAG/checksums.txt")"
resign "$REL" "$A"
run "$DA" "$REL"; refused "two checksums.txt lines for the archive are refused" 'listed [0-9]+ times|more than once'

# 13. the opt-out: banner before and after, the checksum still enforced
REL="$(mkrelease optout-good "$A")"
run "$DA" "$REL" RYSH_INSTALL_TEST_HIDE="gpg" RYSH_INSTALL_INSECURE_SKIP_VERIFY=1
if [[ "$RC" -eq 0 ]] && installed && [[ "$(grep -c 'RYSH_INSTALL_INSECURE_SKIP_VERIFY' <<<"$OUT")" -ge 2 ]] \
   && grep -q 'NOT verified' <<<"$OUT" && grep -q 'Checksum OK' <<<"$OUT"; then
  ok "the opt-out installs without gpg, with a banner before and after, and still checks the checksum"
else bad "opt-out good: exit $RC" "$OUT"; fi
REL="$(mkrelease optout-tamper "$A")"; printf 'x' >>"$REL/$TAG/$ARCHIVE"
run "$DA" "$REL" RYSH_INSTALL_INSECURE_SKIP_VERIFY=1; refused "the opt-out never skips the checksum (tampered archive refused)" 'Checksum mismatch'
REL="$(mkrelease optout-old none)"
run "$DA" "$REL" RYSH_INSTALL_INSECURE_SKIP_VERIFY=1
[[ "$RC" -eq 0 ]] && installed && grep -q 'NOT verified' <<<"$OUT" \
  && ok "an unsigned older release installs only with the opt-out, loudly" || bad "opt-out old release: exit $RC" "$OUT"
run "$DA" "$REL" RYSH_INSTALL_INSECURE_SKIP_VERIFY=yes; refused "the opt-out takes exactly 1, nothing else" 'not published|RYSH_INSTALL_INSECURE_SKIP_VERIFY'

# 14. expired and revoked keys (as gpg reports them)
DE="$TMP/derived-E.sh"; derive "$DE" "$E" "$TMP/E.asc"
REL="$(mkrelease expired "$E" --faked-system-time 20200101T120000!)"
run "$DE" "$REL"; refused "a signature by an expired key is refused" 'not signed by the rysh release key|expired|could not be verified'
DR="$TMP/derived-R.sh"; derive "$DR" "$R" "$TMP/R.asc"
run "$DR" "$REL_R"; refused "a signature by a revoked key is refused" 'not signed by the rysh release key|revoked|could not be verified'

# 15. test seams are refused outside a marked harness under the temp dir (I8)
REL="$H/rel/good"
BIN="$(mktemp -d "$TMP/bin.XXXX")"; RC=0
OUT="$(env RYSH_VERSION="$VER" RYSH_INSTALL_DIR="$BIN" RYSH_INSTALL_TEST_BASE="file://$REL" ${SANDBOX[@]+"${SANDBOX[@]}"} sh "$DA" 2>&1)" || RC=$?
refused "a test seam without the harness is refused" 'outside the test harness'
NH="$TMP/unmarked"; mkdir -p "$NH"
run "$DA" "$REL" RYSH_INSTALL_TEST_HARNESS="$NH"; refused "an unmarked harness dir is refused" 'harness'
run "$DA" "$REL" RYSH_INSTALL_TEST_BASE="https://example.invalid/rel"; refused "a non-file:// test base is refused" 'file://'
mkdir -p "$TMP/outside/$TAG"; cp "$REL/$TAG/"* "$TMP/outside/$TAG/"
run "$DA" "$TMP/outside"; refused "a test base outside the harness is refused" 'inside the harness'

# 16. static (code lines, comments excluded): https only, no -k/--insecure, no pipe to sh, no keyserver
code="$(grep -v -E '^[[:space:]]*#' "$SCRIPT")"
# curl in command position (line start, $(, if/then/else, ;, &&, ||), not the
# usage hints that quote "VAR=1 curl ... | sh" inside a message string
CURL_RE='(^[[:space:]]*|\$\(|;[[:space:]]*|&&[[:space:]]*|\|\|[[:space:]]*|(if|then|else)[[:space:]]+)curl[[:space:]]+-'
n_curl="$(grep -c -E "$CURL_RE" <<<"$code" || true)"
n_proto="$(grep -E "$CURL_RE" <<<"$code" | grep -c -F -- "--proto '=https'" || true)"
[[ "$n_curl" -ge 1 && "$n_curl" == "$n_proto" ]] && ok "every curl call is --proto '=https' ($n_proto/$n_curl)" || bad "curl calls without --proto '=https': $n_proto of $n_curl"
n_redir="$(grep -E "$CURL_RE" <<<"$code" | grep -c -F -- "--proto-redir '=https'" || true)"
[[ "$n_curl" == "$n_redir" ]] && ok "every curl call refuses a redirect off https ($n_redir/$n_curl)" || bad "curl calls without --proto-redir '=https': $n_redir of $n_curl"
if grep -q -E -- '(^|[[:space:]])(-k|--insecure)([[:space:]]|$)|\|[[:space:]]*(ba)?sh([[:space:]]|$)|keyserver|--auto-key-retrieve|--recv-key' <<<"$code"; then
  bad "the installer has -k/--insecure, a pipe into a shell, or a keyserver path"
else ok "no -k/--insecure, no pipe into a shell, no keyserver or key retrieval"; fi
v="$(grep -n -E 'verify_signature$|verify_signature[[:space:]]*$' <<<"$code" | tail -1 | cut -d: -f1)"
c="$(grep -n -F 'Checksum OK' <<<"$code" | tail -1 | cut -d: -f1)"
x="$(grep -n -E 'tar -xzf' <<<"$code" | tail -1 | cut -d: -f1)"
[[ -n "$v" && -n "$c" && -n "$x" && "$v" -lt "$x" && "$c" -lt "$x" ]] && ok "verify comes before extract" || bad "verify/extract order: verify ${v:-?} checksum ${c:-?} extract ${x:-?}"

# 17. the embedded key IS the pinned key, and the pin is release's fingerprint (I1)
pin="$(grep -E "$PIN_RE" "$SCRIPT" | head -1 | cut -d'"' -f2)"
[[ "$pin" == 7516CA5CD0B1A64E5B4B9F0D6815300AB56A1364 ]] && ok "the pin is the full rysh release primary fingerprint" || bad "the pin is not 7516CA5C…1364: ${pin:-none}"
KH="$TMP/khome"; mkdir -m 700 "$KH"
embedded_key "$SCRIPT" >"$TMP/embedded.asc"
gpg --homedir "$KH" --batch --quiet --import "$TMP/embedded.asc" >/dev/null 2>&1
kf="$(gpg --homedir "$KH" --batch --with-colons --list-keys 2>/dev/null | awk -F: '$1=="pub"{p=1;n++} p&&$1=="fpr"{print $10; p=0}' | tr '\n' ' ')"
[[ -n "$pin" && "$kf" == "$pin " ]] && ok "the embedded public key block is exactly one key, the pinned one" || bad "the embedded key block is not exactly the pinned key"
gpg --homedir "$KH" --batch --with-colons --list-secret-keys 2>/dev/null | grep -q '^sec' && bad "the embedded block holds a SECRET key" || ok "the embedded block holds no secret key"
gpgconf --homedir "$KH" --kill gpg-agent >/dev/null 2>&1

# 18. drift: the embedded block == root scripts/oss-assets/<release key> (ledger 148)
if [[ -n "$RELKEY" ]]; then
  cmp -s "$TMP/embedded.asc" "$RELKEY" && ok "the embedded key block equals $(basename "$RELKEY") byte for byte" || bad "the embedded key block differs from $(basename "$RELKEY")"
else
  bad "NOT RUN: key drift check (pass --release-key <root scripts/oss-assets key file>)"
fi

echo
echo "install-rysh-test: $pass passed, $failn failed"
[[ "$failn" -eq 0 ]]
