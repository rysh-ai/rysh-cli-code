#!/usr/bin/env bash
#
# check-embedded-bundle.sh — is internal/web/static what the renderer source
# actually builds to? (backlog E-50)
#
# There is one renderer. rysh-cli-app/vite.web.config.ts builds it straight into
# rysh-cli/internal/web/static, which internal/web embeds with //go:embed. So a
# change to the app is only real once the rebuilt bundle is committed HERE, in a
# different repository — and nothing ever checked that it was. It went stale
# twice in one afternoon (app 0b0a971 and cce9ed3), each time caught only by a
# human rebuilding and diffing by hand.
#
# This is the standard generated-artifact check, and it works because the build
# is deterministic: the same source built twice produces a byte-identical tree
# (measured 2026-08-13, two builds, `diff -r` clean).
#
# Two deliberate design choices, both of which cost something:
#
#   1. It builds into a TEMPORARY directory and compares, instead of rebuilding
#      internal/web/static in place and asking git whether the tree went dirty.
#      The in-place shape is the more common one, but this working tree is
#      shared with sibling agents: a check that rewrites a tracked directory as
#      a side effect can destroy or race another agent's uncommitted work, and
#      one that answers "is this stale?" should not be able to change the answer.
#      It also means the check needs no git at all, so a clean CI clone and a
#      developer's dirty tree get the same verdict.
#
#   2. It is NOT wired into `go test`. It needs node and npm, and the Go suite
#      must not acquire a JavaScript toolchain dependency. This is a check
#      somebody runs — `make bundle-check` — or that CI runs, never a hidden
#      side effect of testing.
#
# Why this does not just call the canonical root target, which already has good
# guards (Makefile.internal_web `web` — it exits 1 on a missing config and
# installs node_modules itself):
#
#   that target sets ROOT from its own path (`$(dir $(abspath $(lastword
#   $(MAKEFILE_LIST))))`), so APP, CLI and STATIC are pinned to the MAIN
#   CHECKOUT no matter where you invoke it from, and it builds with no --outDir
#   so the config's relative outDir lands there too. Run it from a worktree and
#   it rebuilds the main checkout's bundle from the main checkout's sources,
#   reports success, ignores everything you changed, and leaves the shared main
#   checkout dirty for every other agent working in it. Deferring to it would
#   replace a command that does nothing with a command that does damage.
#
# So this script resolves its tree from its OWN location: run the copy in your
# worktree and you check your worktree; run the one in the main checkout and you
# check that. It prints which tree it used, every time, because "succeeded
# against the wrong tree" is the same failure as E-50 itself.
#
# Usage:
#   scripts/check-embedded-bundle.sh            # check; non-zero when stale
#   scripts/check-embedded-bundle.sh --write    # rebuild the bundle in place
#   RYSH_APP_DIR=/path/to/rysh-cli-app scripts/check-embedded-bundle.sh

set -euo pipefail

CLI_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATIC_DIR="$CLI_DIR/internal/web/static"
WRITE=0

for arg in "$@"; do
	case "$arg" in
	--write) WRITE=1 ;;
	-h | --help)
		sed -n '2,36p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "unknown argument: $arg" >&2
		exit 2
		;;
	esac
done

die() {
	echo >&2
	echo "FAIL: $*" >&2
	exit 1
}

# ---------------------------------------------------------------------------
# Locate the renderer source.
#
# This is the step that has to be loud. `make build-frontend` tested for
# ../rysh-cli-app and, when it was missing, printed a note and exited 0 — so
# from any worktree (worktrees/rysh-cli-<branch>/../rysh-cli-app does not
# exist) it rebuilt nothing and reported success. Every fleet agent works in a
# worktree. A check that cannot find its input must fail, never pass quietly.
# ---------------------------------------------------------------------------
find_app_dir() {
	if [ -n "${RYSH_APP_DIR:-}" ]; then
		echo "$RYSH_APP_DIR"
		return
	fi
	# ../rysh-cli-app is the live checkout beside rysh-cli; ../../rysh-cli-app
	# is that same checkout seen from worktrees/rysh-cli-<branch>.
	for candidate in "$CLI_DIR/../rysh-cli-app" "$CLI_DIR/../../rysh-cli-app"; do
		if [ -f "$candidate/vite.web.config.ts" ]; then
			(cd "$candidate" && pwd)
			return
		fi
	done
	echo ""
}

APP_DIR="$(find_app_dir)"
if [ -z "$APP_DIR" ] || [ ! -f "$APP_DIR/vite.web.config.ts" ]; then
	die "cannot find the renderer source (rysh-cli-app/vite.web.config.ts).

  Looked in:
    \$RYSH_APP_DIR              ${RYSH_APP_DIR:-<unset>}
    $CLI_DIR/../rysh-cli-app
    $CLI_DIR/../../rysh-cli-app

  If you are in a git worktree, the sibling ../rysh-cli-app does not exist
  there. Point at the live checkout explicitly:

    RYSH_APP_DIR=/path/to/rysh-cli-app make bundle-check"
fi

if [ ! -d "$STATIC_DIR" ]; then
	die "no embedded bundle at $STATIC_DIR — this does not look like a rysh-cli tree."
fi

# ---------------------------------------------------------------------------
# Dependencies. A fresh worktree has no node_modules, and letting npx improvise
# produces either a mystery failure or a silent download of a different vite.
# ---------------------------------------------------------------------------
command -v node >/dev/null 2>&1 || die "node is not installed, and this check builds the renderer.
  Install node, or run this on a machine that has it. It is deliberately not part of 'go test'."
command -v npm >/dev/null 2>&1 || die "npm is not installed (node is). Install npm to build the renderer."

if [ ! -d "$APP_DIR/node_modules" ]; then
	die "the renderer's dependencies are not installed.

  Run this first, then try again:

    cd $APP_DIR && npm ci

  (Checked for $APP_DIR/node_modules. Without it npx would fetch some other
  vite and build something different from what anyone else builds.)"
fi

# ---------------------------------------------------------------------------
# Provenance. A verdict nobody can interpret is not a control: say which source
# tree was built, at which commit, and whether it had uncommitted changes.
# ---------------------------------------------------------------------------
app_head="$(git -C "$APP_DIR" log --oneline -1 2>/dev/null || echo "unknown (not a git checkout)")"
app_dirty=""
if git -C "$APP_DIR" rev-parse >/dev/null 2>&1; then
	if [ -n "$(git -C "$APP_DIR" status --porcelain -- src electron index.html 2>/dev/null)" ]; then
		app_dirty=" (UNCOMMITTED renderer changes present)"
	fi
fi

echo "renderer source : $APP_DIR"
echo "renderer commit : ${app_head}${app_dirty}"
echo "embedded bundle : $STATIC_DIR"
echo

BUILD_DIR="$(mktemp -d "${TMPDIR:-/tmp}/rysh-bundle-check.XXXXXX")"
# shellcheck disable=SC2329  # invoked by the EXIT trap below
cleanup() { rm -rf "$BUILD_DIR"; }
trap cleanup EXIT

echo "Building the renderer into a temporary directory (the bundle in this tree is not touched)…"
build_log="$BUILD_DIR/.build.log"
if ! (cd "$APP_DIR" && npx vite build --config vite.web.config.ts \
	--outDir "$BUILD_DIR" --emptyOutDir >"$build_log" 2>&1); then
	echo >&2
	echo "--- vite output ---" >&2
	cat "$build_log" >&2
	die "the renderer failed to build. That is the finding: the committed bundle cannot be
  reproduced from the current source, so nobody can tell whether it is stale."
fi
rm -f "$build_log"

# ---------------------------------------------------------------------------
# Compare. diff -r catches content changes, added files and removed files, which
# matters because vite names assets by content hash: a stale bundle usually
# shows up as one file present on each side rather than as a modified file.
# ---------------------------------------------------------------------------
if diff -r "$BUILD_DIR" "$STATIC_DIR" >"$BUILD_DIR/../bundle-diff.txt" 2>&1; then
	rm -f "$BUILD_DIR/../bundle-diff.txt"
	echo "OK: the embedded bundle matches a fresh build of the renderer."
	if [ -n "$app_dirty" ]; then
		echo
		echo "NOTE: the renderer tree has uncommitted changes, so this says the bundle matches"
		echo "      your WORKING COPY. It does not say the committed app and the committed"
		echo "      bundle agree — commit the renderer and re-run to check that."
	fi
	exit 0
fi

echo
echo "--- how the committed bundle differs from a fresh build ---"
sed 's|'"$BUILD_DIR"'|<fresh build>|g; s|'"$STATIC_DIR"'|<committed bundle>|g' \
	"$BUILD_DIR/../bundle-diff.txt" | head -40
rm -f "$BUILD_DIR/../bundle-diff.txt"

if [ "$WRITE" = "1" ]; then
	echo
	echo "--write given: replacing the embedded bundle with the fresh build."
	rm -rf "${STATIC_DIR:?}"/*
	cp -R "$BUILD_DIR"/. "$STATIC_DIR"/
	echo "Done. Commit $STATIC_DIR, and rebuild the rysh binary so //go:embed picks it up."
	exit 0
fi

die "the embedded bundle is STALE — internal/web/static is not what the renderer builds to.

  Every browser and mobile client is served this bundle, so a renderer change that
  is not rebuilt here is invisible in production while looking merged in git.

  Fix it:

    make bundle-rebuild        # rebuilds internal/web/static in place
    git add internal/web/static && git commit

  From a worktree, pass the live renderer checkout explicitly:

    RYSH_APP_DIR=/path/to/rysh-cli-app make bundle-rebuild"
