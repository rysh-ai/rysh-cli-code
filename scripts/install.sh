#!/bin/sh
# Rysh one-line installer
# Usage: curl -fsSL https://rysh.ai/install.sh | sh
#
# Installs the latest Rysh binary to /usr/local/bin (or ~/.local/bin if not writable),
# and its licence files (EULA, LICENSE, NOTICE) to the matching share/doc/<binary>.
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
#   RYSH_BINARY_NAME  binary/artifact name (default: ry; the open-source build is rysh)
#
# Company connect (design 037) — the dashboard's one-line install-and-connect:
#   RYSH_CONNECT         "<server> <code>": after installing, run `ry connect` with
#                        the company page's one-time code; writes ~/Rysh/<slug>/
#                        and opens the session there when a terminal is available
#   RYSH_CONNECT_OPEN    "auto" (default) opens the session; "0" only writes it
#   RYSH_CONNECT_DEVICE  device label shown on the dashboard (default: user@host)
#   RYSH_CONNECT_DIR     company workspace directory (default: ~/Rysh/<slug>)

set -e

RYSH_BASE_URL="${RYSH_BASE_URL:-https://packages.rysh.ai}"
RYSH_INSTALL_DIR="${RYSH_INSTALL_DIR:-/usr/local/bin}"
BINARY_NAME="${RYSH_BINARY_NAME:-ry}"

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
ARCHIVE="${BINARY_NAME}_${OS}_${ARCH}.tar.gz"
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

# ── Licence files ────────────────────────────────────────────────────────────
# The ry tarball ships EULA.txt, LICENSE, NOTICE and vt10x-LICENSE (the EULA,
# Apache-2.0 §4 and the vendored vt10x MIT notice). Keep them next to the
# binary's prefix — <parent of the bin dir used>/share/doc/<binary>, e.g.
# /usr/local/share/doc/ry or ~/.local/share/doc/ry — instead of deleting them
# with TMP_DIR. If that is not writable, fall back to ~/.local/share/doc/<binary>.
# Never fails the install: an older archive without them gets one warning.
# Test: .github/scripts/check-install-notices-test.sh
NOTICE_FILES=""
for f in EULA.txt LICENSE NOTICE vt10x-LICENSE; do
  [ -f "${TMP_DIR}/${f}" ] && NOTICE_FILES="${NOTICE_FILES} ${f}"
done
if [ -z "$NOTICE_FILES" ]; then
  echo "WARNING: the ${TAG} archive carries no licence files (EULA.txt, LICENSE, NOTICE); installed the binary only." >&2
else
  DOC_DIR="$(dirname "$(dirname "$INSTALLED_PATH")")/share/doc/${BINARY_NAME}"
  if ! { mkdir -p "$DOC_DIR" 2>/dev/null && [ -w "$DOC_DIR" ]; }; then
    DOC_DIR="${HOME}/.local/share/doc/${BINARY_NAME}"
    mkdir -p "$DOC_DIR" 2>/dev/null || DOC_DIR=""
  fi
  if [ -n "$DOC_DIR" ]; then
    for f in $NOTICE_FILES; do
      cp "${TMP_DIR}/${f}" "${DOC_DIR}/${f}" 2>/dev/null || echo "WARNING: could not copy ${f} to ${DOC_DIR}." >&2
    done
    echo "Licence files (EULA, LICENSE, NOTICE): ${DOC_DIR}"
  else
    echo "WARNING: no writable place for the licence files; they are in the archive ${ARCHIVE}." >&2
  fi
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

# ── Company connect (design 037) ─────────────────────────────────────────────
# RYSH_CONNECT="<server> <code>" is what the company page's install line sets.
# Installing is done; hand over to `ry connect`, which claims the code, writes
# ~/Rysh/<slug>/rysh.config.yaml and opens the session on the company view.
# `curl | sh` leaves stdin on the pipe, so the session is opened with stdin
# re-attached to the terminal; with no terminal (CI, a script) or with
# RYSH_CONNECT_OPEN=0 the workspace is written and not opened.
if [ -n "${RYSH_CONNECT:-}" ]; then
  set -- $RYSH_CONNECT
  CONNECT_SERVER="${1:-}"
  CONNECT_CODE="${2:-}"
  [ -n "$CONNECT_SERVER" ] && [ -n "$CONNECT_CODE" ] || fail \
    "RYSH_CONNECT must be \"<server> <code>\", e.g. RYSH_CONNECT=\"https://letchat.ai rysh_ct_...\"" \
    "Copy the whole install line from the company page on the dashboard."
  set --
  [ -n "${RYSH_CONNECT_DEVICE:-}" ] && set -- "$@" --device "$RYSH_CONNECT_DEVICE"
  [ -n "${RYSH_CONNECT_DIR:-}" ] && set -- "$@" --dir "$RYSH_CONNECT_DIR"
  echo ""
  echo "Connecting this machine to your company..."
  rm -rf "$TMP_DIR"
  case "${RYSH_CONNECT_OPEN:-auto}" in
    0|no|false)
      exec "$INSTALLED_PATH" connect "$CONNECT_SERVER" "$CONNECT_CODE" --no-open "$@"
      ;;
  esac
  # Open /dev/tty for real: its permission bits say nothing about whether this
  # process has a controlling terminal (a CI job or a subprocess has none, and
  # the redirect would fail after the install already succeeded).
  if ( : < /dev/tty ) 2>/dev/null; then
    exec "$INSTALLED_PATH" connect "$CONNECT_SERVER" "$CONNECT_CODE" "$@" < /dev/tty
  fi
  echo "No terminal here: the company workspace is written and not opened."
  exec "$INSTALLED_PATH" connect "$CONNECT_SERVER" "$CONNECT_CODE" --no-open "$@"
fi

echo ""
echo "Quick start:"
echo "  ${BINARY_NAME}                         # start the default session"
echo "  ${BINARY_NAME} my-project              # start a named session"
echo ""
echo "Set your Anthropic API key in rysh.config.yaml in your project directory"
echo "(config and state are project-local; there is no global location):"
echo "  provider:"
echo "    api_key: \"sk-ant-...\""
echo ""
echo "Documentation: https://rysh.ai/docs"
