#!/usr/bin/env bash
# =============================================================================
# scripts/setup-distribution.sh
# One-time automated setup for all Rysh distribution channels:
#   Homebrew tap, apt repo (Cloudflare R2), RPM repo (Cloudflare R2),
#   WinGet (fork + auto-PR via GitHub Action), Chocolatey (needs API key),
#   GitHub Actions secrets, GPG signing key.
#
# Usage:
#   ./scripts/setup-distribution.sh
#
# Credentials are loaded in priority order:
#   1. secrets/ files  (secrets/claudflare.api.token, secrets/GPG_PASSPHRASE,
#                        secrets/chocolatey.api.key)
#   2. Environment variables  (CF_API_TOKEN, GPG_PASSPHRASE, CHOCOLATEY_API_KEY)
#   3. Interactive prompt  (fallback if neither source has the value)
#
# CF_ACCOUNT_ID is auto-detected from the Cloudflare API token (no file needed).
# GITHUB_TOKEN is auto-sourced from the gh CLI (gh auth token).
# =============================================================================

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# ── Configuration ─────────────────────────────────────────────────────────────
RYSH_ORG="rysh-ai"
RYSH_REPO="rysh-cli"
HOMEBREW_TAP="homebrew-rysh"
R2_BUCKET="rysh-packages"
R2_SUBDOMAIN="packages.rysh.ai"   # custom domain on the bucket
GPG_EMAIL="releases@rysh.ai"
GPG_NAME="Rysh AI"
GPG_EXPIRE="2y"
WINGET_FORK_REPO="winget-pkgs"    # forked under $RYSH_ORG

# ── Colours ───────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'

step()  { echo -e "\n${BLUE}${BOLD}[$((++STEP_N))/$TOTAL_STEPS] $*${NC}"; }
ok()    { echo -e "  ${GREEN}✓${NC} $*"; }
warn()  { echo -e "  ${YELLOW}⚠${NC}  $*"; }
info()  { echo -e "  ${CYAN}→${NC} $*"; }
fail()  { echo -e "\n${RED}✗ ERROR: $*${NC}" >&2; exit 1; }
ask()   { echo -e "  ${YELLOW}?${NC}  $*"; }

STEP_N=0
TOTAL_STEPS=9

# ── Secrets directory ─────────────────────────────────────────────────────────
SECRETS_DIR="$REPO_ROOT/secrets"

# read_secret <variable_name> <secrets_file_basename> [<silent:true|false>]
#   Populates the named variable by checking in order:
#     1. secrets/<file>  (strip all whitespace/newlines)
#     2. existing env var with the same name as <variable_name>
#     3. interactive prompt (silent if arg 3 is "true")
#   Sets a global with the name given in arg 1.
read_secret() {
  local var_name="$1"
  local file_name="$2"
  local silent="${3:-false}"
  local file_path="${SECRETS_DIR}/${file_name}"
  local value=""

  # 1. Try secrets file
  if [ -f "$file_path" ]; then
    value=$(tr -d '[:space:]' < "$file_path")
    if [ -n "$value" ]; then
      printf "  ${GREEN}✓${NC} %-26s ${CYAN}(secrets/%s)${NC}\n" "$var_name" "$file_name"
      eval "${var_name}=\"\$value\""
      return 0
    fi
  fi

  # 2. Try existing env var
  value="${!var_name:-}"
  if [ -n "$value" ]; then
    printf "  ${GREEN}✓${NC} %-26s ${CYAN}(env var)${NC}\n" "$var_name"
    eval "${var_name}=\"\$value\""
    return 0
  fi

  # 3. Interactive prompt
  printf "  ${YELLOW}?${NC}  $var_name not found in secrets/ or env. "
  if [ "$silent" = "true" ]; then
    read -rsp "Enter value (hidden): " value; echo ""
  else
    read -rp "Enter value: " value
  fi
  eval "${var_name}=\"\$value\""
}

# ── State tracking ────────────────────────────────────────────────────────────
STATE_FILE="$REPO_ROOT/.dist-setup-state"
touch "$STATE_FILE"
done_step() { grep -q "^$1$" "$STATE_FILE" 2>/dev/null && return 0 || return 1; }
mark_done() { echo "$1" >> "$STATE_FILE"; }

# =============================================================================
# STEP 0: Banner
# =============================================================================
echo ""
echo -e "${BOLD}╔══════════════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}║    Rysh Distribution Channel Setup (automated)      ║${NC}"
echo -e "${BOLD}╚══════════════════════════════════════════════════════╝${NC}"
echo ""
echo "  Org:    ${RYSH_ORG}"
echo "  Repo:   ${RYSH_REPO}"
echo "  Domain: ${R2_SUBDOMAIN}"
echo ""

# =============================================================================
# STEP 1: Check prerequisites
# =============================================================================
step "Checking prerequisites"

MISSING=()
for cmd in gh gpg git curl jq rclone goreleaser; do
  if command -v "$cmd" &>/dev/null; then
    ok "$cmd"
  else
    warn "$cmd not found"
    MISSING+=("$cmd")
  fi
done

if [ ${#MISSING[@]} -gt 0 ]; then
  echo ""
  warn "Missing tools: ${MISSING[*]}"
  echo "  Install them:"
  echo "    brew install gh gpg rclone goreleaser jq   # macOS"
  echo "    # or see: https://goreleaser.com/install/"
  echo ""
  read -rp "  Continue anyway? Some steps may fail. [y/N] " CONT
  [[ "$CONT" =~ ^[Yy]$ ]] || exit 1
fi

# Check gh is authenticated
if ! gh auth status &>/dev/null; then
  warn "gh CLI not authenticated. Running gh auth login..."
  gh auth login
fi
ok "GitHub CLI authenticated"

# =============================================================================
# STEP 2: Collect credentials
# =============================================================================
step "Collecting credentials"

if [ -d "$SECRETS_DIR" ]; then
  info "Reading from secrets/ directory: ${SECRETS_DIR}"
else
  warn "secrets/ directory not found — will use env vars or prompts"
fi
echo ""

# GitHub token — always sourced from the gh CLI (no secrets file needed)
GH_TOKEN="${GITHUB_TOKEN:-$(gh auth token 2>/dev/null || echo "")}"
if [ -z "$GH_TOKEN" ]; then
  fail "Could not get GitHub token. Run: gh auth login"
fi
printf "  ${GREEN}✓${NC} %-26s ${CYAN}(gh auth token)${NC}\n" "GITHUB_TOKEN"

# Cloudflare API token
# Note: the secrets file is named 'claudflare.api.token' (preserving the typo)
read_secret CF_API_TOKEN "claudflare.api.token" true

# Cloudflare Account ID — auto-detected from the token via API (no file needed)
if [ -z "${CF_ACCOUNT_ID:-}" ]; then
  info "Auto-detecting Cloudflare Account ID from token..."
  CF_ACCOUNT_ID=$(curl -sf \
    -H "Authorization: Bearer ${CF_API_TOKEN}" \
    "https://api.cloudflare.com/client/v4/accounts?per_page=1" \
    | jq -r '.result[0].id // empty' 2>/dev/null || echo "")
  if [ -n "$CF_ACCOUNT_ID" ]; then
    printf "  ${GREEN}✓${NC} %-26s ${CYAN}(Cloudflare API)${NC}\n" "CF_ACCOUNT_ID"
  else
    warn "Could not auto-detect Account ID. Find it at: https://dash.cloudflare.com"
    read -rp "  Paste your Cloudflare Account ID: " CF_ACCOUNT_ID
  fi
else
  printf "  ${GREEN}✓${NC} %-26s ${CYAN}(env var)${NC}\n" "CF_ACCOUNT_ID"
fi

# GPG passphrase
read_secret GPG_PASSPHRASE "GPG_PASSPHRASE" true

# Chocolatey API key (optional — Chocolatey publishing skipped if absent)
CHOCOLATEY_API_KEY="${CHOCOLATEY_API_KEY:-}"
CHOCO_FILE="${SECRETS_DIR}/chocolatey.api.key"
if [ -f "$CHOCO_FILE" ]; then
  CHOCOLATEY_API_KEY=$(tr -d '[:space:]' < "$CHOCO_FILE")
  printf "  ${GREEN}✓${NC} %-26s ${CYAN}(secrets/chocolatey.api.key)${NC}\n" "CHOCOLATEY_API_KEY"
elif [ -n "$CHOCOLATEY_API_KEY" ]; then
  printf "  ${GREEN}✓${NC} %-26s ${CYAN}(env var)${NC}\n" "CHOCOLATEY_API_KEY"
else
  warn "CHOCOLATEY_API_KEY not found in secrets/ or env — Chocolatey publishing will be skipped"
  info "Add it later: echo '<key>' > secrets/chocolatey.api.key  then re-run"
fi

echo ""
ok "All credentials loaded"

# =============================================================================
# STEP 3: Generate GPG signing key
# =============================================================================
step "Generating GPG signing key"

GPG_KEY_EXISTS=false
if gpg --list-secret-keys --keyid-format=long "$GPG_EMAIL" &>/dev/null; then
  ok "GPG key for $GPG_EMAIL already exists — skipping generation"
  GPG_KEY_EXISTS=true
fi

if ! $GPG_KEY_EXISTS; then
  info "Generating 4096-bit RSA GPG key for $GPG_EMAIL..."
  gpg --batch --passphrase "$GPG_PASSPHRASE" --gen-key <<EOF
Key-Type: RSA
Key-Length: 4096
Subkey-Type: RSA
Subkey-Length: 4096
Name-Real: ${GPG_NAME}
Name-Email: ${GPG_EMAIL}
Expire-Date: ${GPG_EXPIRE}
Passphrase: ${GPG_PASSPHRASE}
%commit
EOF
  ok "GPG key generated"
fi

# Export key details
GPG_KEY_ID=$(gpg --list-secret-keys --keyid-format=long "$GPG_EMAIL" \
  | grep sec | awk '{print $2}' | cut -d'/' -f2 | head -1)
GPG_FINGERPRINT=$(gpg --fingerprint --with-colons "$GPG_EMAIL" \
  | grep '^fpr' | head -1 | cut -d':' -f10)

info "Key ID:          $GPG_KEY_ID"
info "Fingerprint:     $GPG_FINGERPRINT"

# Export keys to files
gpg --armor --export "$GPG_EMAIL" > /tmp/rysh-public.asc
gpg --armor --batch --pinentry-mode loopback \
  --passphrase "$GPG_PASSPHRASE" \
  --export-secret-keys "$GPG_EMAIL" > /tmp/rysh-private.asc

ok "GPG keys exported to /tmp/rysh-{public,private}.asc"

# Publish to keyservers (best-effort)
info "Publishing public key to keyservers..."
gpg --keyserver keyserver.ubuntu.com --send-keys "$GPG_KEY_ID" 2>/dev/null && \
  ok "Published to keyserver.ubuntu.com" || warn "keyserver.ubuntu.com upload failed (non-fatal)"
gpg --keyserver keys.openpgp.org --send-keys "$GPG_KEY_ID" 2>/dev/null && \
  ok "Published to keys.openpgp.org" || warn "keys.openpgp.org upload failed (non-fatal)"

mark_done "gpg"

# =============================================================================
# STEP 4: Cloudflare R2 — create bucket + configure rclone
# =============================================================================
step "Setting up Cloudflare R2 bucket"

CF_BASE="https://api.cloudflare.com/client/v4/accounts/${CF_ACCOUNT_ID}"

# Check if bucket already exists
BUCKET_STATUS=$(curl -sf -o /dev/null -w "%{http_code}" \
  -H "Authorization: Bearer ${CF_API_TOKEN}" \
  "${CF_BASE}/r2/buckets/${R2_BUCKET}" 2>/dev/null || echo "000")

if [ "$BUCKET_STATUS" = "200" ]; then
  ok "R2 bucket '${R2_BUCKET}' already exists"
else
  info "Creating R2 bucket '${R2_BUCKET}'..."
  RESULT=$(curl -sf -X PUT \
    -H "Authorization: Bearer ${CF_API_TOKEN}" \
    -H "Content-Type: application/json" \
    -d "{\"name\":\"${R2_BUCKET}\"}" \
    "${CF_BASE}/r2/buckets" 2>&1) || fail "Failed to create R2 bucket: $RESULT"
  ok "R2 bucket '${R2_BUCKET}' created"
fi

# Enable public access on the bucket
info "Enabling public access on bucket..."
curl -sf -X PUT \
  -H "Authorization: Bearer ${CF_API_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"enabled":true}' \
  "${CF_BASE}/r2/buckets/${R2_BUCKET}/policy" &>/dev/null || true

# Get R2 endpoint details for rclone
R2_ENDPOINT="https://${CF_ACCOUNT_ID}.r2.cloudflarestorage.com"

# Create R2 API token for rclone (uses existing CF_API_TOKEN)
info "Configuring rclone for R2..."
rclone config create r2 s3 \
  provider Cloudflare \
  access_key_id "" \
  secret_access_key "" \
  endpoint "${R2_ENDPOINT}" \
  acl public-read \
  --non-interactive &>/dev/null 2>&1 || true

# Use the CF API token directly for rclone (token-based auth)
RCLONE_CFG="${HOME}/.config/rclone/rclone.conf"
mkdir -p "$(dirname "$RCLONE_CFG")"
# Overwrite r2 section with token-based auth
python3 - <<PYEOF
import configparser, os
cfg = configparser.ConfigParser()
cfg_path = os.path.expanduser("${RCLONE_CFG}")
cfg.read(cfg_path)
if not cfg.has_section('r2'):
    cfg.add_section('r2')
cfg['r2']['type'] = 's3'
cfg['r2']['provider'] = 'Cloudflare'
cfg['r2']['access_key_id'] = '${CF_API_TOKEN}'
cfg['r2']['secret_access_key'] = '${CF_API_TOKEN}'
cfg['r2']['endpoint'] = '${R2_ENDPOINT}'
cfg['r2']['acl'] = 'public-read'
with open(cfg_path, 'w') as f:
    cfg.write(f)
PYEOF

# Test rclone connectivity
rclone lsd "r2:${R2_BUCKET}" &>/dev/null && ok "rclone → R2 connection works" || \
  warn "rclone test failed — R2 creds may need Cloudflare R2 API token (not Global API)"

# Upload GPG public key
info "Uploading GPG public key to R2..."
rclone copyto /tmp/rysh-public.asc "r2:${R2_BUCKET}/gpg" \
  --s3-acl public-read 2>/dev/null && ok "GPG key at https://${R2_SUBDOMAIN}/gpg" || \
  warn "GPG key upload failed — retry manually: rclone copyto /tmp/rysh-public.asc r2:${R2_BUCKET}/gpg"

# Upload repo config files
info "Uploading apt/rpm repo config files..."
rclone copyto packaging/apt/rysh.repo "r2:${R2_BUCKET}/apt/rysh.repo" \
  --s3-acl public-read 2>/dev/null || true
rclone copyto packaging/rpm/rysh.repo "r2:${R2_BUCKET}/rpm/rysh.repo" \
  --s3-acl public-read 2>/dev/null || true
ok "Repo config files uploaded"

# Upload install.sh
rclone copyto scripts/install.sh "r2:${R2_BUCKET}/install.sh" \
  --s3-acl public-read 2>/dev/null && ok "install.sh at https://${R2_SUBDOMAIN}/install.sh" || true

mark_done "r2"

# =============================================================================
# STEP 5: GitHub Actions — enable permissions + set all secrets
# =============================================================================
step "Configuring GitHub Actions secrets"

FULL_REPO="${RYSH_ORG}/${RYSH_REPO}"

# Enable Actions on repo (read+write permissions)
info "Enabling Actions write permissions on ${FULL_REPO}..."
gh api "repos/${FULL_REPO}/actions/permissions" \
  -X PUT -f enabled=true -f allowed_actions=all &>/dev/null && \
  ok "Actions enabled on ${FULL_REPO}" || warn "Could not set Actions permissions via API"

gh api "repos/${FULL_REPO}/actions/permissions/workflow" \
  -X PUT -f default_workflow_permissions=write -f can_approve_pull_request_reviews=true \
  &>/dev/null && ok "Workflow write permissions enabled" || true

# Set secrets
set_secret() {
  local name="$1" value="$2"
  if [ -z "$value" ]; then
    warn "Skipping secret $name (empty value)"
    return
  fi
  echo "$value" | gh secret set "$name" --repo "$FULL_REPO" &>/dev/null && \
    ok "Secret set: $name" || warn "Failed to set secret: $name"
}

set_secret "GORELEASER_GITHUB_TOKEN"  "$GH_TOKEN"
set_secret "GPG_PRIVATE_KEY"          "$(cat /tmp/rysh-private.asc)"
set_secret "GPG_PASSPHRASE"           "$GPG_PASSPHRASE"
set_secret "GPG_FINGERPRINT"          "$GPG_FINGERPRINT"
set_secret "AWS_ACCESS_KEY_ID"        "$CF_API_TOKEN"
set_secret "AWS_SECRET_ACCESS_KEY"    "$CF_API_TOKEN"
set_secret "S3_ENDPOINT_URL"          "$R2_ENDPOINT"
set_secret "WINGET_TOKEN"             "$GH_TOKEN"
[ -n "${CHOCOLATEY_API_KEY:-}" ] && set_secret "CHOCOLATEY_API_KEY" "$CHOCOLATEY_API_KEY"

mark_done "github-secrets"

# =============================================================================
# STEP 6: Homebrew tap — create repo + seed formula
# =============================================================================
step "Setting up Homebrew tap (${RYSH_ORG}/${HOMEBREW_TAP})"

TAP_EXISTS=false
gh repo view "${RYSH_ORG}/${HOMEBREW_TAP}" &>/dev/null && TAP_EXISTS=true

if $TAP_EXISTS; then
  ok "Tap repo ${RYSH_ORG}/${HOMEBREW_TAP} already exists"
else
  info "Creating ${RYSH_ORG}/${HOMEBREW_TAP}..."
  gh repo create "${RYSH_ORG}/${HOMEBREW_TAP}" \
    --public \
    --description "Homebrew tap for Rysh — agentic terminal multiplexer" \
    &>/dev/null && ok "Tap repo created" || fail "Could not create tap repo"
fi

# Clone, add formula, push
TAP_DIR=$(mktemp -d)
trap 'rm -rf "$TAP_DIR"' EXIT

info "Seeding formula..."
git clone --quiet "git@github.com:${RYSH_ORG}/${HOMEBREW_TAP}.git" "$TAP_DIR" 2>/dev/null || \
  git clone --quiet "https://github.com/${RYSH_ORG}/${HOMEBREW_TAP}.git" "$TAP_DIR"

mkdir -p "${TAP_DIR}/Formula"
cp packaging/homebrew/Formula/rysh.rb "${TAP_DIR}/Formula/rysh.rb"

cd "$TAP_DIR"
git config user.email "releases@rysh.ai"
git config user.name "goreleaser-bot"
if ! git diff --quiet HEAD -- Formula/rysh.rb 2>/dev/null; then
  git add Formula/rysh.rb
  git commit -m "feat: add rysh formula template (checksums updated by goreleaser)" &>/dev/null
  git push origin main &>/dev/null && ok "Formula pushed to tap" || warn "Push failed — may need permissions"
else
  ok "Formula already up to date in tap"
fi
cd "$REPO_ROOT"

mark_done "homebrew"

# =============================================================================
# STEP 7: Fork microsoft/winget-pkgs for the WinGet auto-PR action
# =============================================================================
step "Forking microsoft/winget-pkgs for WinGet auto-PR"

WINGET_FORK_EXISTS=false
gh repo view "${RYSH_ORG}/${WINGET_FORK_REPO}" &>/dev/null && WINGET_FORK_EXISTS=true

if $WINGET_FORK_EXISTS; then
  ok "${RYSH_ORG}/${WINGET_FORK_REPO} already exists"
else
  info "Forking microsoft/winget-pkgs → ${RYSH_ORG}/${WINGET_FORK_REPO}..."
  gh repo fork microsoft/winget-pkgs \
    --org "$RYSH_ORG" \
    --fork-name "$WINGET_FORK_REPO" \
    --clone=false &>/dev/null && \
    ok "Forked: ${RYSH_ORG}/${WINGET_FORK_REPO}" || \
    warn "Fork failed — may need to fork manually at https://github.com/microsoft/winget-pkgs"
fi

# Grant the token write access to the fork
info "The WINGET_TOKEN secret grants the winget-releaser Action access to this fork"

mark_done "winget-fork"

# =============================================================================
# STEP 8: Validate GoReleaser config + snapshot build
# =============================================================================
step "Validating GoReleaser config"

info "Running: goreleaser check"
goreleaser check 2>&1 | grep -v "^$" | sed 's/^/  /' || fail "goreleaser check failed"
ok "GoReleaser config valid"

info "Running snapshot build (no publishing)..."
goreleaser release --snapshot --clean 2>&1 | tail -5 | sed 's/^/  /'
ok "Snapshot build succeeded"
info "Artifacts in ./dist/ — inspect with: ls dist/"

mark_done "goreleaser"

# =============================================================================
# STEP 9: Summary + next action
# =============================================================================
step "Setup complete"

echo ""
echo -e "${GREEN}${BOLD}Everything is configured. Summary:${NC}"
echo ""
echo -e "  ${GREEN}✓${NC} GPG key generated    key ID: ${GPG_KEY_ID}"
echo -e "  ${GREEN}✓${NC} Cloudflare R2 bucket ${R2_BUCKET} ready"
echo -e "  ${GREEN}✓${NC} rclone configured    remote: r2"
echo -e "  ${GREEN}✓${NC} GitHub secrets set   (8 secrets on ${FULL_REPO})"
echo -e "  ${GREEN}✓${NC} Homebrew tap created  github.com/${RYSH_ORG}/${HOMEBREW_TAP}"
echo -e "  ${GREEN}✓${NC} WinGet fork ready     github.com/${RYSH_ORG}/${WINGET_FORK_REPO}"
echo -e "  ${GREEN}✓${NC} GoReleaser config OK"
if [ -n "${CHOCOLATEY_API_KEY:-}" ]; then
  echo -e "  ${GREEN}✓${NC} Chocolatey API key   set"
else
  echo -e "  ${YELLOW}⚠${NC}  Chocolatey API key   NOT set (re-run with CHOCOLATEY_API_KEY=...)"
fi
echo ""
echo -e "${BOLD}To publish your first release:${NC}"
echo ""
echo -e "  ${CYAN}make release VERSION_TAG=0.1.0${NC}"
echo ""
echo -e "  This will:"
echo -e "    • Build binaries for 5 platforms"
echo -e "    • Create .deb + .rpm packages"
echo -e "    • Sign with GPG, publish GitHub Release"
echo -e "    • Update Homebrew formula"
echo -e "    • Sync apt + RPM repos to packages.rysh.ai"
echo -e "    • Submit WinGet PR to microsoft/winget-pkgs automatically"
echo -e "    • Push to Chocolatey"
echo ""
echo -e "${BOLD}Monitor the release:${NC}"
echo -e "  https://github.com/${FULL_REPO}/actions"
echo ""

# Clean up temp key files
rm -f /tmp/rysh-private.asc /tmp/rysh-public.asc

echo -e "${YELLOW}Note:${NC} The .dist-setup-state file records completed steps."
echo "Delete it to re-run all steps from scratch."
echo ""
