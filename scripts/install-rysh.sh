#!/bin/sh
# rysh one-line installer — THE OPEN-SOURCE BUILD
# Usage: curl -fsSL https://packages.rysh.ai/install-rysh.sh | sh
#
# Installs the Apache-2.0 `rysh` binary from the PUBLIC repository's GitHub
# Releases: https://github.com/rysh-ai/rysh-cli-code
#
# This is not the same program as `ry`. `install.sh` beside this file installs
# `ry`, the proprietary build, from packages.rysh.ai. The two are different
# binaries with different licences and they install side by side:
#   /usr/bin/rysh   open source, this script
#   /usr/bin/ry     proprietary, install.sh
#
# Artifacts come from GitHub Releases here, not from packages.rysh.ai, because
# rysh-cli-code is public and its releases are reachable without a token — the
# reason install.sh cannot use them is that ITS repository is private.
#
# Supports: macOS (darwin/arm64, darwin/amd64), Linux (linux/amd64, linux/arm64)
#
# Environment overrides:
#   RYSH_VERSION      pin a version (e.g. "0.2.7" or "v0.2.7")
#   RYSH_REPO         source repository (default: rysh-ai/rysh-cli-code)
#   RYSH_INSTALL_DIR  install target (default: /usr/local/bin, else ~/.local/bin)
#   RYSH_INSTALL_INSECURE_SKIP_VERIFY=1
#                     skip ONLY the signature check, for a host without gpg or
#                     an unsigned release (v0.2.14 and older). Loud; the
#                     checksum is still checked.
#
# Verification fails closed. Before anything is unpacked:
#   1. checksums.txt.sig must be a valid detached signature over checksums.txt
#      by the rysh release key: gpg runs in a throwaway GNUPGHOME holding only
#      the public key embedded below, and the signature counts only when gpg's
#      VALIDSIG names the pinned PRIMARY fingerprint (a signing subkey counts
#      through its primary). No keyserver, never your own keyring.
#   2. the archive's sha256 must equal the one line of checksums.txt whose
#      file name is exactly the archive's.
# A missing checksums.txt, signature, gpg or sha256 tool refuses the install.
#
# The RYSH_INSTALL_TEST_* variables exist for scripts/install-rysh-test.sh
# only: they are refused unless RYSH_INSTALL_TEST_HARNESS names a marked
# directory under the temp dir. They can point downloads at local files or
# hide a tool, never change the key or the pin, so even a misused one cannot
# make an unverified archive pass.

set -e

RYSH_REPO="${RYSH_REPO:-rysh-ai/rysh-cli-code}"
RYSH_INSTALL_DIR="${RYSH_INSTALL_DIR:-/usr/local/bin}"
BINARY_NAME="rysh"

fail() {
  echo "ERROR: $1" >&2
  [ -n "$2" ] && echo "$2" >&2
  exit 1
}

# The rysh release signing key: the full primary fingerprint, and the armored
# public key (the same bytes as scripts/oss-assets in the release tree).
RYSH_RELEASE_KEY_FPR="7516CA5CD0B1A64E5B4B9F0D6815300AB56A1364"
release_public_key() {
  cat <<'RYSH_RELEASE_PUBLIC_KEY'
-----BEGIN PGP PUBLIC KEY BLOCK-----

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
5oCPSvT6s8W2l4Zjuw==
=LdBp
-----END PGP PUBLIC KEY BLOCK-----
RYSH_RELEASE_PUBLIC_KEY
}

# ── Test harness (see the header) ────────────────────────────────────────────
HARNESS=""
if [ -n "${RYSH_INSTALL_TEST_HARNESS:-}" ]; then
  h=$(cd "$RYSH_INSTALL_TEST_HARNESS" 2>/dev/null && pwd -P) || h=""
  t=$(cd "${TMPDIR:-/tmp}" 2>/dev/null && pwd -P) || t=""
  case "$h/" in
    "$t"/?*) ;;
    *) h="" ;;
  esac
  [ -n "$h" ] && [ -n "$t" ] && [ -f "$h/.rysh-install-test-harness" ] \
    || fail "RYSH_INSTALL_TEST_HARNESS is not a marked test harness under the temp dir."
  HARNESS="$h"
elif [ -n "${RYSH_INSTALL_TEST_BASE:-}${RYSH_INSTALL_TEST_HIDE:-}" ]; then
  fail "RYSH_INSTALL_TEST_* is set outside the test harness; unset it."
fi
TEST_BASE_DIR=""
if [ -n "$HARNESS" ] && [ -n "${RYSH_INSTALL_TEST_BASE:-}" ]; then
  case "$RYSH_INSTALL_TEST_BASE" in
    file://*) ;;
    *) fail "RYSH_INSTALL_TEST_BASE must be a file:// URL." ;;
  esac
  TEST_BASE_DIR=$(cd "${RYSH_INSTALL_TEST_BASE#file://}" 2>/dev/null && pwd -P) || TEST_BASE_DIR=""
  case "$TEST_BASE_DIR/" in
    "$HARNESS"/?*) ;;
    *) fail "RYSH_INSTALL_TEST_BASE must be inside the harness." ;;
  esac
fi

# have <tool>: on PATH, and not hidden by the harness.
have() {
  if [ -n "$HARNESS" ]; then
    case " ${RYSH_INSTALL_TEST_HIDE:-} " in
      *" $1 "*) return 1 ;;
    esac
  fi
  command -v "$1" >/dev/null 2>&1
}

have curl || fail "curl is required."

# ── Detect platform ──────────────────────────────────────────────────────────
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  darwin|linux) ;;
  mingw*|msys*|cygwin*)
    fail "Native Windows is not supported by this installer." \
         "Run it under WSL2 (the tested path), or take the Windows zip from
    https://github.com/${RYSH_REPO}/releases/latest (native panes, not yet tested on real Windows)" ;;
  *) fail "Unsupported OS: $OS" ;;
esac

ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) fail "Unsupported architecture: $ARCH" ;;
esac

# `uname -m` reports x86_64 for a TRANSLATED process on Apple Silicon, so a user
# running this from a Rosetta shell would silently get the Intel build. Ask the
# hardware instead. Observed on an M-series mac where `uname -m` says x86_64 and
# `hw.optional.arm64` says 1.
if [ "$OS" = "darwin" ] && [ "$ARCH" = "amd64" ]; then
  if [ "$(sysctl -n hw.optional.arm64 2>/dev/null)" = "1" ]; then
    ARCH="arm64"
    echo "Apple Silicon detected behind a translated shell; installing the arm64 build."
  fi
fi

# ── Resolve the version ──────────────────────────────────────────────────────
# The /releases/latest redirect rather than the API: it needs no token and has
# no rate limit, so a shared IP cannot turn the installer into a 403.
resolve_version() {
  if [ -n "$RYSH_VERSION" ]; then
    echo "$RYSH_VERSION"
    return 0
  fi
  [ -z "$TEST_BASE_DIR" ] || return 1
  url=$(curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSLI -o /dev/null -w '%{url_effective}' \
        "https://github.com/${RYSH_REPO}/releases/latest" 2>/dev/null) || return 1
  case "$url" in
    */tag/*) echo "${url##*/tag/}" ;;
    *) return 1 ;;
  esac
}

echo "Resolving latest rysh release..."
LATEST=$(resolve_version) || fail \
  "Could not determine the latest rysh version." \
  "Check your connection, or pin one:
    RYSH_VERSION=0.2.7 curl -fsSL https://packages.rysh.ai/install-rysh.sh | sh"

VERSION="${LATEST#v}"
TAG="v${VERSION}"
echo "Latest version: ${TAG}"

ARCHIVE="${BINARY_NAME}_${OS}_${ARCH}.tar.gz"
BASE="https://github.com/${RYSH_REPO}/releases/download/${TAG}"

SKIP_SIG=0
[ "${RYSH_INSTALL_INSECURE_SKIP_VERIFY:-}" = "1" ] && SKIP_SIG=1

# ── Tools: refuse before downloading anything ────────────────────────────────
if have sha256sum; then
  SHA256="sha256sum"
elif have shasum; then
  SHA256="shasum -a 256"
else
  fail "A sha256 tool is required to verify the download (sha256sum or shasum)." \
       "Install coreutils (Linux) or perl's shasum (macOS ships it), then re-run."
fi
if [ "$SKIP_SIG" = 0 ] && ! have gpg; then
  fail "gpg is required to verify the release signature." \
       "Install GnuPG and re-run:
    macOS:          brew install gnupg
    Debian/Ubuntu:  sudo apt-get install gnupg
    Fedora/RHEL:    sudo dnf install gnupg2
Or, at your own risk, skip ONLY the signature check (the checksum is still checked):
    RYSH_INSTALL_INSECURE_SKIP_VERIFY=1 curl -fsSL https://packages.rysh.ai/install-rysh.sh | sh"
fi

insecure_banner() {
  cat >&2 <<EOF

!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!
!! RYSH_INSTALL_INSECURE_SKIP_VERIFY=1: the release SIGNATURE is NOT verified.
!! $1
!! Only the sha256 checksum protects this install.
!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!

EOF
}

TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

# fetch <asset> <out>: one release asset into the temp dir, https only.
fetch() {
  if [ -n "$TEST_BASE_DIR" ]; then
    cp "${TEST_BASE_DIR}/${TAG}/$1" "$2" 2>/dev/null
  else
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL -o "$2" "${BASE}/$1"
  fi
}

echo "Downloading ${ARCHIVE}..."
fetch "$ARCHIVE" "${TMP_DIR}/${ARCHIVE}" || fail \
  "Download failed: ${BASE}/${ARCHIVE}" \
  "That platform may not be published for ${TAG}."
fetch checksums.txt "${TMP_DIR}/checksums.txt" || fail \
  "checksums.txt is not published for ${TAG}; refusing to install an unverifiable download."

# ── Verify the signature of checksums.txt ────────────────────────────────────
# Accept only when gpg's VALIDSIG line names the pinned primary fingerprint
# (argument 10, the primary key's). Exit codes and GOODSIG alone are not enough.
verify_signature() {
  gh="${TMP_DIR}/gnupg"
  mkdir -m 700 "$gh"
  : > "$gh/common.conf"
  release_public_key | gpg --homedir "$gh" --batch --quiet --no-autostart --import >/dev/null 2>&1 \
    || fail "Could not load the rysh release public key."
  n=$(gpg --homedir "$gh" --batch --no-autostart --with-colons --list-keys 2>/dev/null | grep -c '^pub:') || n=0
  [ "$n" = 1 ] || fail "The embedded rysh release key is not exactly one key."
  rc=0
  status=$(gpg --homedir "$gh" --batch --no-autostart --no-auto-key-retrieve --status-fd 1 \
             --verify "${TMP_DIR}/checksums.txt.sig" "${TMP_DIR}/checksums.txt" 2>/dev/null) || rc=$?
  if printf '%s\n' "$status" | grep -q -E '^\[GNUPG:\] (BADSIG|ERRSIG|EXPSIG|EXPKEYSIG|REVKEYSIG|NO_PUBKEY)( |$)'; then
    rc=1
  fi
  good=$(printf '%s\n' "$status" | grep -c '^\[GNUPG:\] GOODSIG ') || good=0
  valid=$(printf '%s\n' "$status" | awk '$1 == "[GNUPG:]" && $2 == "VALIDSIG" { n++ } END { print n + 0 }')
  pinned=$(printf '%s\n' "$status" | awk -v pin="$RYSH_RELEASE_KEY_FPR" \
             '$1 == "[GNUPG:]" && $2 == "VALIDSIG" && toupper($12) == pin { n++ } END { print n + 0 }')
  if [ "$rc" != 0 ] || [ "$good" -lt 1 ] || [ "$valid" -lt 1 ] || [ "$pinned" != "$valid" ]; then
    fail "checksums.txt for ${TAG} is not signed by the rysh release key; refusing to install." \
         "Expected a valid signature by ${RYSH_RELEASE_KEY_FPR}."
  fi
  echo "Signature OK (rysh release key ${RYSH_RELEASE_KEY_FPR})."
}

if [ "$SKIP_SIG" = 1 ]; then
  insecure_banner "Skipping the signature check of checksums.txt for ${TAG}."
else
  echo "Verifying signature..."
  fetch checksums.txt.sig "${TMP_DIR}/checksums.txt.sig" || fail \
    "checksums.txt.sig is not published for ${TAG}; refusing to install an unsigned release." \
    "Releases up to v0.2.14 were not signed. To install one anyway, at your own risk
(the checksum is still checked):
    RYSH_INSTALL_INSECURE_SKIP_VERIFY=1 RYSH_VERSION=${VERSION} curl -fsSL https://packages.rysh.ai/install-rysh.sh | sh"
  verify_signature
fi

# ── Verify the checksum ──────────────────────────────────────────────────────
# Exactly one line whose file name field is exactly the archive's (GNU's
# "*name" binary marker allowed). Computed and string-compared rather than
# `sha256sum -c`: BSD/macOS sha256sum does not accept GNU's --status, so the
# flag-based form differs across platforms. Same reasoning as install.sh.
echo "Verifying checksum..."
lines=$(awk -v f="$ARCHIVE" 'NF == 2 && ($2 == f || $2 == "*" f)' "${TMP_DIR}/checksums.txt" | wc -l | tr -d ' ')
case "$lines" in
  1) ;;
  0) fail "${ARCHIVE} is not listed in checksums.txt for ${TAG}; refusing to install." ;;
  *) fail "${ARCHIVE} is listed ${lines} times in checksums.txt for ${TAG}; refusing to install." ;;
esac
want=$(awk -v f="$ARCHIVE" 'NF == 2 && ($2 == f || $2 == "*" f) { print tolower($1) }' "${TMP_DIR}/checksums.txt")
got=$($SHA256 "${TMP_DIR}/${ARCHIVE}" | awk '{ print tolower($1) }')
case "$want" in
  ""|*[!0-9a-f]*) fail "The checksums.txt line for ${ARCHIVE} is not a sha256." ;;
esac
[ "${#want}" = 64 ] || fail "The checksums.txt line for ${ARCHIVE} is not a sha256."
[ "$want" = "$got" ] || fail "Checksum mismatch for ${ARCHIVE}." "expected ${want}, got ${got}"
echo "Checksum OK."

tar -xzf "${TMP_DIR}/${ARCHIVE}" -C "$TMP_DIR" || fail "Could not unpack ${ARCHIVE}."
[ -f "${TMP_DIR}/${BINARY_NAME}" ] || fail "${BINARY_NAME} not found inside ${ARCHIVE}."
chmod +x "${TMP_DIR}/${BINARY_NAME}"

# ── Install ──────────────────────────────────────────────────────────────────
if [ -w "$RYSH_INSTALL_DIR" ]; then
  mv "${TMP_DIR}/${BINARY_NAME}" "${RYSH_INSTALL_DIR}/${BINARY_NAME}"
  INSTALLED_PATH="${RYSH_INSTALL_DIR}/${BINARY_NAME}"
else
  USER_BIN="${HOME}/.local/bin"
  mkdir -p "$USER_BIN"
  mv "${TMP_DIR}/${BINARY_NAME}" "${USER_BIN}/${BINARY_NAME}"
  INSTALLED_PATH="${USER_BIN}/${BINARY_NAME}"
  echo "Installed to ${INSTALLED_PATH} (no write access to ${RYSH_INSTALL_DIR})."
  case ":${PATH}:" in
    *":${USER_BIN}:"*) ;;
    *) echo "Add it to your PATH: export PATH=\"\$HOME/.local/bin:\$PATH\"" ;;
  esac
fi

[ "$SKIP_SIG" = 1 ] && insecure_banner "rysh ${TAG} was installed WITHOUT a signature check."

echo ""
"$INSTALLED_PATH" --version || true
echo ""
echo "✓ rysh ${TAG} installed at ${INSTALLED_PATH}"
echo ""
echo "This is the open-source (Apache-2.0) build. Source:"
echo "  https://github.com/${RYSH_REPO}"
