#!/bin/sh
# Rysh one-line installer
# Usage: curl -fsSL https://rysh.ai/install.sh | sh
#
# Installs the latest Rysh binary to /usr/local/bin (or ~/.local/bin if not writable).
# Supports: macOS (darwin/arm64, darwin/amd64), Linux (linux/amd64, linux/arm64)
#
# Artifacts are served from packages.rysh.ai (Cloudflare R2), NOT from GitHub
# Releases -- the rysh-cli repository is private, so the GitHub Releases API is
# not reachable for end users.
#
# Environment overrides:
#   RYSH_VERSION      pin a specific version (e.g. "0.1.25" or "v0.1.25")
#   RYSH_BASE_URL     artifact host (default: https://packages.rysh.ai)
#   RYSH_INSTALL_DIR  install target (default: /usr/local/bin, else ~/.local/bin)

set -e

RYSH_BASE_URL="${RYSH_BASE_URL:-https://packages.rysh.ai}"
RYSH_INSTALL_DIR="${RYSH_INSTALL_DIR:-/usr/local/bin}"
BINARY_NAME="rysh"

fail() {
  echo "ERROR: $1" >&2
  [ -n "$2" ] && echo "$2" >&2
  exit 1
}

# ── Detect OS ────────────────────────────────────────────────────────────────
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in
  darwin) OS="darwin" ;;
  linux)  OS="linux"  ;;
  mingw* | msys* | cygwin*)
    fail "Windows is not supported by this script." \
         "Run rysh under WSL2, or see https://rysh.ai/docs/#installation"
    ;;
  *)
    fail "Unsupported operating system: $OS" \
         "See https://rysh.ai/docs/#installation for manual install options."
    ;;
esac

# ── Detect architecture ──────────────────────────────────────────────────────
ARCH=$(uname -m)
case "$ARCH" in
  x86_64 | amd64)  ARCH="amd64" ;;
  aarch64 | arm64) ARCH="arm64" ;;
  *)
    fail "Unsupported architecture: $ARCH" \
         "See https://rysh.ai/docs/#installation for manual install options."
    ;;
esac

command -v curl >/dev/null 2>&1 || fail "curl is required but not installed."
command -v tar  >/dev/null 2>&1 || fail "tar is required but not installed."

# ── Resolve version ──────────────────────────────────────────────────────────
# Priority: explicit RYSH_VERSION -> releases/latest.txt -> apt Packages index.
# The apt fallback exists so the installer keeps working against releases that
# were published before latest.txt was introduced.
resolve_version() {
  if [ -n "$RYSH_VERSION" ]; then
    echo "$RYSH_VERSION"
    return 0
  fi

  v=$(curl -fsSL "${RYSH_BASE_URL}/releases/latest.txt" 2>/dev/null | tr -d ' \t\r\n')
  if [ -n "$v" ]; then
    echo "$v"
    return 0
  fi

  v=$(curl -fsSL "${RYSH_BASE_URL}/apt/dists/stable/main/binary-amd64/Packages" 2>/dev/null \
    | awk '/^Version:/ {print $2}' \
    | sort -V \
    | tail -1)
  if [ -n "$v" ]; then
    echo "$v"
    return 0
  fi

  return 1
}

echo "Resolving latest Rysh release..."
LATEST=$(resolve_version) || fail \
  "Could not determine the latest Rysh version." \
  "Check your connection, or pin one: RYSH_VERSION=0.1.25 curl -fsSL https://rysh.ai/install.sh | sh"

# Normalise: strip any leading "v", then rebuild the tagged directory name.
VERSION="${LATEST#v}"
TAG="v${VERSION}"
echo "Latest version: ${TAG}"

# ── Build download URLs ──────────────────────────────────────────────────────
ARCHIVE="rysh_${OS}_${ARCH}.tar.gz"
RELEASE_URL="${RYSH_BASE_URL}/releases/${TAG}"
URL="${RELEASE_URL}/${ARCHIVE}"
CHECKSUM_URL="${RELEASE_URL}/checksums.txt"

# ── Download ─────────────────────────────────────────────────────────────────
TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

echo "Downloading ${ARCHIVE}..."
curl -fsSL -o "${TMP_DIR}/${ARCHIVE}" "$URL" || fail \
  "Download failed: $URL" \
  "That platform may not be published for ${TAG}. See https://rysh.ai/docs/#installation"

# ── Verify checksum ──────────────────────────────────────────────────────────
# Compute the digest and string-compare it. Deliberately avoids `sha256sum -c`:
# the BSD/macOS sha256sum does not accept GNU's --status, so the flag-based
# form silently differs across platforms.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 "$1" | awk '{print $NF}'
  else
    return 1
  fi
}

echo "Verifying checksum..."
if curl -fsSL -o "${TMP_DIR}/checksums.txt" "$CHECKSUM_URL" 2>/dev/null; then
  EXPECTED=$(awk -v f="$ARCHIVE" '$2 == f {print $1}' "${TMP_DIR}/checksums.txt")
  [ -n "$EXPECTED" ] || fail "No checksum entry for ${ARCHIVE} in checksums.txt"

  if ACTUAL=$(sha256_of "${TMP_DIR}/${ARCHIVE}"); then
    if [ "$ACTUAL" != "$EXPECTED" ]; then
      fail "Checksum verification FAILED for ${ARCHIVE}" \
           "expected ${EXPECTED}, got ${ACTUAL}. Refusing to install."
    fi
    echo "Checksum OK."
  else
    echo "WARNING: no sha256 tool found; skipping checksum verification." >&2
  fi
else
  echo "WARNING: could not fetch checksums.txt; skipping verification." >&2
fi

# ── Extract ──────────────────────────────────────────────────────────────────
tar xzf "${TMP_DIR}/${ARCHIVE}" -C "$TMP_DIR"
[ -f "${TMP_DIR}/${BINARY_NAME}" ] || fail "Archive did not contain a '${BINARY_NAME}' binary."
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
  echo ""
  echo "Installed to ${INSTALLED_PATH} (no write access to ${RYSH_INSTALL_DIR})."
  case ":${PATH}:" in
    *":${USER_BIN}:"*) ;;
    *) echo "Add it to your PATH: export PATH=\"\$HOME/.local/bin:\$PATH\"" ;;
  esac
fi

# ── macOS quarantine fix ─────────────────────────────────────────────────────
if [ "$OS" = "darwin" ]; then
  xattr -d com.apple.quarantine "$INSTALLED_PATH" 2>/dev/null || true
fi

# ── Verify ───────────────────────────────────────────────────────────────────
echo ""
"$INSTALLED_PATH" --version 2>&1 || true
echo ""
echo "✓ Rysh ${TAG} installed at ${INSTALLED_PATH}"
echo ""
echo "Quick start:"
echo "  rysh                       # start the default session"
echo "  rysh my-project            # start a named session"
echo ""
echo "Set your Anthropic API key in rysh.config.yaml in your project directory"
echo "(config and state are project-local; there is no global location):"
echo "  provider:"
echo "    api_key: \"sk-ant-...\""
echo ""
echo "Documentation: https://rysh.ai/docs"
