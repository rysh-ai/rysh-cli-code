#!/usr/bin/env bash
#
# setup-rysh install step (design 009 §3.3).
#
# Derived from the rysh.ai one-line installer (rysh-cli/scripts/install.sh,
# also hosted at https://rysh.ai/install.sh) — same artifact naming and layout
# as .goreleaser.yml publishes to packages.rysh.ai:
#
#   ${RYSH_BASE_URL}/releases/latest.txt
#   ${RYSH_BASE_URL}/releases/v<version>/rysh_<os>_<arch>.tar.gz
#   ${RYSH_BASE_URL}/releases/v<version>/checksums.txt        (sha256)
#
# CI-appropriate differences from install.sh:
#   - checksum verification is MANDATORY and fail-closed: a missing
#     checksums.txt, a missing entry, a mismatch, or no sha256 tool ABORTS the
#     install. CI must never execute an unverified binary (install.sh only
#     warns in those cases).
#   - installs into a caller-supplied tool dir (RYSH_INSTALL_DIR) and appends
#     it to $GITHUB_PATH; never touches /usr/local/bin.
#   - optional build-from-source mode (GOWORK=off go build) for hermetic CI
#     and unreleased code.
#
# Env contract (set by action.yml; every function is sourceable for tests):
#   RYSH_VERSION            "latest" (default) or a version: 0.1.28 / v0.1.28
#   RYSH_BASE_URL           artifact host (default https://packages.rysh.ai)
#   RYSH_INSTALL_DIR        REQUIRED: directory to install the binary into
#   RYSH_BUILD_FROM_SOURCE  "true" => go build from RYSH_SOURCE_DIR instead
#   RYSH_SOURCE_DIR         module dir for build-from-source (default ".")
#   GITHUB_PATH             appended to when non-empty (standard Actions file)
#
# Supported platforms: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64 —
# exactly the tar.gz targets .goreleaser.yml publishes. The windows zip is a
# CLI-only artifact and is not supported by this action.

set -euo pipefail

log()  { echo "[setup-rysh] $*"; }
fail() { echo "[setup-rysh] ERROR: $*" >&2; exit 1; }

# detect_platform prints "<os> <arch>" (goreleaser spellings) or fails.
detect_platform() {
  local os arch
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  case "$os" in
    linux | darwin) ;;
    *) fail "unsupported runner OS: $os (linux and macOS only; the Windows release is CLI-only and not supported by this action)" ;;
  esac
  arch=$(uname -m)
  case "$arch" in
    x86_64 | amd64)  arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) fail "unsupported runner architecture: $arch (amd64 and arm64 only)" ;;
  esac
  echo "$os $arch"
}

# resolve_version prints the bare version (no leading "v"). A pinned
# RYSH_VERSION wins; otherwise releases/latest.txt is consulted.
resolve_version() {
  local v="${RYSH_VERSION:-latest}"
  if [[ -n "$v" && "$v" != "latest" ]]; then
    echo "${v#v}"
    return 0
  fi
  v=$(curl -fsSL "${RYSH_BASE_URL}/releases/latest.txt" 2>/dev/null | tr -d ' \t\r\n') || true
  [[ -n "$v" ]] || fail "could not resolve the latest rysh version from ${RYSH_BASE_URL}/releases/latest.txt — pin one with the 'version' input"
  echo "${v#v}"
}

# sha256_of prints the hex digest of $1, trying the tools that exist across
# ubuntu/macos runners. Returns 1 when none exists (caller fails closed).
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

# verify_checksum <archive> <checksums.txt> <archive-name>: aborts unless the
# archive's sha256 matches its checksums.txt entry. Fail-closed on every path.
verify_checksum() {
  local archive="$1" checksums="$2" name="$3" expected actual
  expected=$(awk -v f="$name" '$2 == f {print $1}' "$checksums")
  [[ -n "$expected" ]] || fail "no checksum entry for ${name} in checksums.txt — refusing to install"
  actual=$(sha256_of "$archive") || fail "no sha256 tool available (sha256sum/shasum/openssl) — refusing to install an unverified binary"
  [[ "$actual" == "$expected" ]] || fail "checksum verification FAILED for ${name}: expected ${expected}, got ${actual} — refusing to install"
  log "checksum OK (${name})"
}

# install_release downloads the pinned/latest release archive, verifies it,
# and installs the binary into RYSH_INSTALL_DIR.
install_release() {
  command -v curl >/dev/null 2>&1 || fail "curl is required"
  command -v tar  >/dev/null 2>&1 || fail "tar is required"

  local os arch version tag archive base tmp
  read -r os arch <<<"$(detect_platform)"
  version=$(resolve_version)
  tag="v${version}"
  archive="rysh_${os}_${arch}.tar.gz"
  base="${RYSH_BASE_URL}/releases/${tag}"

  tmp=$(mktemp -d)
  # shellcheck disable=SC2064  # expand $tmp now: it is final and local
  trap "rm -rf '$tmp'" EXIT

  log "downloading ${base}/${archive}"
  curl -fsSL -o "${tmp}/${archive}" "${base}/${archive}" \
    || fail "download failed: ${base}/${archive} (is ${tag} published for ${os}/${arch}?)"
  curl -fsSL -o "${tmp}/checksums.txt" "${base}/checksums.txt" \
    || fail "could not fetch ${base}/checksums.txt — refusing to install an unverified binary"
  verify_checksum "${tmp}/${archive}" "${tmp}/checksums.txt" "${archive}"

  tar xzf "${tmp}/${archive}" -C "${tmp}"
  [[ -f "${tmp}/rysh" ]] || fail "archive did not contain a 'rysh' binary"
  install -m 0755 "${tmp}/rysh" "${RYSH_INSTALL_DIR}/rysh"
  if [[ "$os" == "darwin" ]]; then
    xattr -d com.apple.quarantine "${RYSH_INSTALL_DIR}/rysh" 2>/dev/null || true
  fi
  log "installed rysh ${tag} to ${RYSH_INSTALL_DIR}/rysh"
}

# build_from_source builds rysh with GOWORK=off from RYSH_SOURCE_DIR — the
# same invocation the repo's own CI uses (rysh-cli is outside go.work).
build_from_source() {
  command -v go >/dev/null 2>&1 || fail "build-from-source requires Go on the runner (add actions/setup-go before this action)"
  local src="${RYSH_SOURCE_DIR:-.}" dest
  [[ -f "${src}/go.mod" ]] || fail "build-from-source: no go.mod in '${src}' — point the source-dir input at the rysh-cli checkout"
  if [[ -n "${RYSH_VERSION:-}" && "${RYSH_VERSION}" != "latest" ]]; then
    log "note: version input '${RYSH_VERSION}' is ignored with build-from-source (building whatever is checked out)"
  fi
  dest=$(cd "${RYSH_INSTALL_DIR}" && pwd)
  log "building rysh from source in ${src} (GOWORK=off go build)"
  (cd "$src" && GOWORK=off go build -o "${dest}/rysh" ./cmd/rysh)
  log "built rysh into ${dest}/rysh"
}

main() {
  [[ -n "${RYSH_INSTALL_DIR:-}" ]] || fail "RYSH_INSTALL_DIR is required"
  mkdir -p "${RYSH_INSTALL_DIR}"
  RYSH_BASE_URL="${RYSH_BASE_URL:-https://packages.rysh.ai}"

  if [[ "${RYSH_BUILD_FROM_SOURCE:-false}" == "true" ]]; then
    build_from_source
  else
    install_release
  fi

  if [[ -n "${GITHUB_PATH:-}" ]]; then
    echo "${RYSH_INSTALL_DIR}" >>"${GITHUB_PATH}"
    log "added ${RYSH_INSTALL_DIR} to GITHUB_PATH"
  fi
  "${RYSH_INSTALL_DIR}/rysh" --version || true
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
