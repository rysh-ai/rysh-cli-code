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

set -e

RYSH_REPO="${RYSH_REPO:-rysh-ai/rysh-cli-code}"
RYSH_INSTALL_DIR="${RYSH_INSTALL_DIR:-/usr/local/bin}"
BINARY_NAME="rysh"

fail() {
  echo "ERROR: $1" >&2
  [ -n "$2" ] && echo "$2" >&2
  exit 1
}

command -v curl >/dev/null 2>&1 || fail "curl is required."

# ── Detect platform ──────────────────────────────────────────────────────────
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  darwin|linux) ;;
  mingw*|msys*|cygwin*)
    fail "Native Windows is not supported by this installer." \
         "rysh needs a PTY. Run it under WSL2, or take the cli-only zip from
    https://github.com/${RYSH_REPO}/releases/latest" ;;
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
  url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
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

TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

echo "Downloading ${ARCHIVE}..."
curl -fsSL -o "${TMP_DIR}/${ARCHIVE}" "${BASE}/${ARCHIVE}" || fail \
  "Download failed: ${BASE}/${ARCHIVE}" \
  "That platform may not be published for ${TAG}."

# ── Verify the checksum ──────────────────────────────────────────────────────
# Computed and string-compared rather than `sha256sum -c`: BSD/macOS sha256sum
# does not accept GNU's --status, so the flag-based form differs across
# platforms. Same reasoning as install.sh.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    echo ""
  fi
}

echo "Verifying checksum..."
if curl -fsSL -o "${TMP_DIR}/checksums.txt" "${BASE}/checksums.txt" 2>/dev/null; then
  want=$(awk -v f="$ARCHIVE" '$2 == f || $2 == "*"f {print $1}' "${TMP_DIR}/checksums.txt" | head -1)
  got=$(sha256_of "${TMP_DIR}/${ARCHIVE}")
  if [ -z "$got" ]; then
    echo "WARNING: no sha256 tool found; skipping verification." >&2
  elif [ -z "$want" ]; then
    echo "WARNING: ${ARCHIVE} is not listed in checksums.txt; skipping." >&2
  elif [ "$want" != "$got" ]; then
    fail "Checksum mismatch for ${ARCHIVE}." "expected ${want}, got ${got}"
  else
    echo "Checksum OK."
  fi
else
  echo "WARNING: checksums.txt not published for ${TAG}; skipping verification." >&2
fi

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

echo ""
"$INSTALLED_PATH" --version || true
echo ""
echo "✓ rysh ${TAG} installed at ${INSTALLED_PATH}"
echo ""
echo "This is the open-source (Apache-2.0) build. Source:"
echo "  https://github.com/${RYSH_REPO}"
