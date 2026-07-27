#!/usr/bin/env bash
#
# Hermetic tests for the setup-rysh action scripts (no network, no real rysh,
# no API key). TAP-ish output; exit 0 iff every check passed.
#
# What is covered and why (each guard was verified to fail when its subject is
# broken — see the PR description):
#   install.sh  version pinning + v-prefix normalisation + latest.txt
#               resolution (served over file:// so curl runs for real),
#               MANDATORY checksum verification (mismatch / missing entry /
#               missing checksums.txt all abort and install nothing),
#               build-from-source, GITHUB_PATH append.
#   run.sh      mode mutual exclusion (task/skill-file/eval), eval-result and
#               budget contract errors, exact argv mapping per mode, TAP
#               capture, status mapping, fail-on any/error/none semantics,
#               GITHUB_OUTPUT emission (present even when the step fails).
#
# Run directly:  bash action/test/run_tests.sh
# Or via Go:     GOWORK=off go test -run TestSetupRyshScripts .

set -u

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ACTION_DIR=$(dirname "$HERE")
INSTALL_SH="${ACTION_DIR}/scripts/install.sh"
RUN_SH="${ACTION_DIR}/scripts/run.sh"

PASS=0
FAIL=0

t_ok()   { PASS=$((PASS + 1)); echo "ok - $1"; }
t_fail() { FAIL=$((FAIL + 1)); echo "not ok - $1"; shift; local l; for l in "$@"; do echo "  # $l"; done; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# ── shellcheck (when available — preinstalled on ubuntu-latest runners) ──────
if command -v shellcheck >/dev/null 2>&1; then
  if shellcheck "$INSTALL_SH" "$RUN_SH" "${BASH_SOURCE[0]}"; then
    t_ok "shellcheck clean"
  else
    t_fail "shellcheck clean" "see findings above"
  fi
else
  echo "# shellcheck not installed; skipping lint"
fi

# ── install.sh helpers ───────────────────────────────────────────────────────

# make_release <root> <version> [checksum-override]
# Builds a fake packages.rysh.ai tree under <root> with the REAL layout:
# releases/latest.txt + releases/v<version>/rysh_<os>_<arch>.tar.gz +
# checksums.txt. The "binary" is a stub script that prints a marker.
make_release() {
  local root="$1" ver="$2" sum_override="${3:-}"
  local os arch archive stage rel sum
  read -r os arch <<<"$(bash -c "source '$INSTALL_SH'; detect_platform")"
  archive="rysh_${os}_${arch}.tar.gz"
  rel="${root}/releases/v${ver}"
  stage="${root}/.stage"
  mkdir -p "$rel" "$stage"
  printf '#!/usr/bin/env bash\necho "fake-rysh %s"\n' "$ver" >"${stage}/rysh"
  chmod +x "${stage}/rysh"
  tar czf "${rel}/${archive}" -C "$stage" rysh
  sum=$(sha256sum "${rel}/${archive}" | awk '{print $1}')
  [[ -n "$sum_override" ]] && sum="$sum_override"
  printf '%s  %s\n' "$sum" "$archive" >"${rel}/checksums.txt"
  printf 'v%s\n' "$ver" >"${root}/releases/latest.txt"
}

# run_install <install-dir> [K=V ...]  — runs install.sh with a clean env
# contract; captures combined output in INSTALL_LOG and exit code in INSTALL_RC.
run_install() {
  local dir="$1"
  shift
  INSTALL_RC=0
  INSTALL_LOG=$(env GITHUB_PATH= RYSH_INSTALL_DIR="$dir" "$@" bash "$INSTALL_SH" 2>&1) || INSTALL_RC=$?
}

# ── install.sh: version pinning ──────────────────────────────────────────────
root="${WORK}/rel1" && make_release "$root" 0.9.9
dir="${WORK}/bin1"
run_install "$dir" RYSH_BASE_URL="file://${root}" RYSH_VERSION=0.9.9
if [[ $INSTALL_RC -eq 0 && -x "${dir}/rysh" ]] && "${dir}/rysh" | grep -q "fake-rysh 0.9.9"; then
  t_ok "install: pinned version downloads, verifies and installs"
else
  t_fail "install: pinned version downloads, verifies and installs" "rc=$INSTALL_RC" "$INSTALL_LOG"
fi

dir="${WORK}/bin2"
run_install "$dir" RYSH_BASE_URL="file://${root}" RYSH_VERSION=v0.9.9
if [[ $INSTALL_RC -eq 0 && -x "${dir}/rysh" ]]; then
  t_ok "install: v-prefixed pin normalises to the same release"
else
  t_fail "install: v-prefixed pin normalises to the same release" "rc=$INSTALL_RC" "$INSTALL_LOG"
fi

# ── install.sh: latest resolution via releases/latest.txt ────────────────────
dir="${WORK}/bin3"
run_install "$dir" RYSH_BASE_URL="file://${root}" RYSH_VERSION=latest
if [[ $INSTALL_RC -eq 0 ]] && "${dir}/rysh" | grep -q "fake-rysh 0.9.9"; then
  t_ok "install: 'latest' resolves through releases/latest.txt"
else
  t_fail "install: 'latest' resolves through releases/latest.txt" "rc=$INSTALL_RC" "$INSTALL_LOG"
fi

# ── install.sh: checksum failure aborts, installs nothing ────────────────────
root="${WORK}/rel-bad" && make_release "$root" 0.9.9 "$(printf 'deadbeef%.0s' 1 2 3 4 5 6 7 8)"
dir="${WORK}/bin4"
run_install "$dir" RYSH_BASE_URL="file://${root}" RYSH_VERSION=0.9.9
if [[ $INSTALL_RC -ne 0 && ! -e "${dir}/rysh" ]] && grep -q "checksum verification FAILED" <<<"$INSTALL_LOG"; then
  t_ok "install: checksum mismatch aborts and installs nothing"
else
  t_fail "install: checksum mismatch aborts and installs nothing" "rc=$INSTALL_RC" "$INSTALL_LOG"
fi

# ── install.sh: missing checksum entry aborts ────────────────────────────────
root="${WORK}/rel-noentry" && make_release "$root" 0.9.9
printf '%s  %s\n' "0000000000000000000000000000000000000000000000000000000000000000" "some_other_file.tar.gz" \
  >"${root}/releases/v0.9.9/checksums.txt"
dir="${WORK}/bin5"
run_install "$dir" RYSH_BASE_URL="file://${root}" RYSH_VERSION=0.9.9
if [[ $INSTALL_RC -ne 0 && ! -e "${dir}/rysh" ]] && grep -q "no checksum entry" <<<"$INSTALL_LOG"; then
  t_ok "install: missing checksums.txt entry aborts"
else
  t_fail "install: missing checksums.txt entry aborts" "rc=$INSTALL_RC" "$INSTALL_LOG"
fi

# ── install.sh: missing checksums.txt aborts (fail-closed, unlike install.sh's warn) ──
root="${WORK}/rel-nosums" && make_release "$root" 0.9.9
rm "${root}/releases/v0.9.9/checksums.txt"
dir="${WORK}/bin6"
run_install "$dir" RYSH_BASE_URL="file://${root}" RYSH_VERSION=0.9.9
if [[ $INSTALL_RC -ne 0 && ! -e "${dir}/rysh" ]] && grep -q "refusing to install an unverified binary" <<<"$INSTALL_LOG"; then
  t_ok "install: absent checksums.txt aborts (fail closed)"
else
  t_fail "install: absent checksums.txt aborts (fail closed)" "rc=$INSTALL_RC" "$INSTALL_LOG"
fi

# ── install.sh: GITHUB_PATH append ───────────────────────────────────────────
root="${WORK}/rel-path" && make_release "$root" 0.9.9
dir="${WORK}/bin7"
ghpath="${WORK}/github_path"
: >"$ghpath"
INSTALL_RC=0
INSTALL_LOG=$(env GITHUB_PATH="$ghpath" RYSH_INSTALL_DIR="$dir" RYSH_BASE_URL="file://${root}" RYSH_VERSION=0.9.9 bash "$INSTALL_SH" 2>&1) || INSTALL_RC=$?
if [[ $INSTALL_RC -eq 0 ]] && grep -qx "$dir" "$ghpath"; then
  t_ok "install: install dir appended to GITHUB_PATH"
else
  t_fail "install: install dir appended to GITHUB_PATH" "rc=$INSTALL_RC" "path file: $(cat "$ghpath" 2>/dev/null)" "$INSTALL_LOG"
fi

# ── install.sh: build-from-source ────────────────────────────────────────────
if command -v go >/dev/null 2>&1; then
  src="${WORK}/src"
  mkdir -p "${src}/cmd/rysh"
  printf 'module fakerysh\n\ngo 1.21\n' >"${src}/go.mod"
  # The main package lives in cmd/rysh, mirroring the real repo layout.
  printf 'package main\n\nimport "fmt"\n\nfunc main() { fmt.Println("fake source build") }\n' >"${src}/cmd/rysh/main.go"
  dir="${WORK}/bin8"
  run_install "$dir" RYSH_BUILD_FROM_SOURCE=true RYSH_SOURCE_DIR="$src" RYSH_VERSION=0.0.1
  if [[ $INSTALL_RC -eq 0 ]] && "${dir}/rysh" | grep -q "fake source build" \
    && grep -q "is ignored with build-from-source" <<<"$INSTALL_LOG"; then
    t_ok "install: build-from-source builds and notes the ignored version pin"
  else
    t_fail "install: build-from-source builds and notes the ignored version pin" "rc=$INSTALL_RC" "$INSTALL_LOG"
  fi
else
  echo "# go not installed; skipping build-from-source test"
fi

# ── run.sh helpers ───────────────────────────────────────────────────────────

FAKE_RYSH="${WORK}/fake-rysh"
cat >"$FAKE_RYSH" <<'FAKE'
#!/usr/bin/env bash
# Fake rysh: records argv, honours --result-out, prints FAKE_RYSH_TAP, exits
# FAKE_RYSH_EXIT. Lets run.sh be tested with every exit code and no daemon.
set -u
printf '%s\n' "$@" > "${FAKE_RYSH_ARGV_FILE}"
args=("$@")
for ((i = 0; i < ${#args[@]} - 1; i++)); do
  if [[ "${args[i]}" == "--result-out" ]]; then
    echo '{"files_changed":[],"commands":[],"output":"","tokens_used":0}' > "${args[i+1]}"
  fi
done
if [[ -n "${FAKE_RYSH_TAP:-}" ]]; then printf '%s' "${FAKE_RYSH_TAP}"; fi
exit "${FAKE_RYSH_EXIT:-0}"
FAKE
chmod +x "$FAKE_RYSH"

# run_runsh <case-dir> [K=V ...] — runs run.sh with a clean contract env.
# Captures RUN_RC / RUN_LOG; the case dir gets gh_output + argv files.
run_runsh() {
  local cdir="$1"
  shift
  mkdir -p "$cdir"
  RUN_RC=0
  RUN_LOG=$(env \
    GITHUB_OUTPUT="${cdir}/gh_output" \
    FAKE_RYSH_ARGV_FILE="${cdir}/argv" \
    RYSH_BIN="$FAKE_RYSH" \
    RYSH_OUT_DIR="${cdir}/out" \
    "$@" bash "$RUN_SH" 2>&1) || RUN_RC=$?
}

# got_output <case-dir> <key>=<value> — asserts an emitted step output.
got_output() { grep -qx "$2" "${1}/gh_output" 2>/dev/null; }

argv_is() { # argv_is <case-dir> <expected...>
  local cdir="$1"
  shift
  local expected
  expected=$(printf '%s\n' "$@")
  [[ "$(cat "${cdir}/argv" 2>/dev/null)" == "$expected" ]]
}

# ── run.sh: mode mutual exclusion ────────────────────────────────────────────
c="${WORK}/c-mx2"
run_runsh "$c" INPUT_TASK="fix it" INPUT_SKILL_FILE="reviewer.md"
if [[ $RUN_RC -ne 0 ]] && grep -q "mutually exclusive" <<<"$RUN_LOG" && [[ ! -e "${c}/argv" ]]; then
  t_ok "run: task + skill-file rejected as mutually exclusive (nothing executed)"
else
  t_fail "run: task + skill-file rejected as mutually exclusive (nothing executed)" "rc=$RUN_RC" "$RUN_LOG"
fi

c="${WORK}/c-mx0"
run_runsh "$c"
if [[ $RUN_RC -ne 0 ]] && grep -q "mutually exclusive" <<<"$RUN_LOG"; then
  t_ok "run: no mode input rejected"
else
  t_fail "run: no mode input rejected" "rc=$RUN_RC" "$RUN_LOG"
fi

c="${WORK}/c-mx3"
run_runsh "$c" INPUT_TASK="x" INPUT_EVAL="evals"
if [[ $RUN_RC -ne 0 ]] && grep -q "mutually exclusive" <<<"$RUN_LOG"; then
  t_ok "run: task + eval rejected"
else
  t_fail "run: task + eval rejected" "rc=$RUN_RC" "$RUN_LOG"
fi

# ── run.sh: contract errors ──────────────────────────────────────────────────
c="${WORK}/c-er"
run_runsh "$c" INPUT_TASK="x" INPUT_EVAL_RESULT="r.json"
if [[ $RUN_RC -ne 0 ]] && grep -q "'eval-result' requires the 'eval' input" <<<"$RUN_LOG"; then
  t_ok "run: eval-result without eval rejected"
else
  t_fail "run: eval-result without eval rejected" "rc=$RUN_RC" "$RUN_LOG"
fi

c="${WORK}/c-bg"
# shellcheck disable=SC2016  # literal "$2" is a valid budget spelling
run_runsh "$c" INPUT_EVAL="evals" INPUT_BUDGET='$2'
if [[ $RUN_RC -ne 0 ]] && grep -q "rysh eval does not support it" <<<"$RUN_LOG"; then
  t_ok "run: budget in eval mode rejected"
else
  t_fail "run: budget in eval mode rejected" "rc=$RUN_RC" "$RUN_LOG"
fi

c="${WORK}/c-fo-bad"
run_runsh "$c" INPUT_TASK="x" INPUT_FAIL_ON="whatever"
if [[ $RUN_RC -ne 0 ]] && grep -q "invalid 'fail-on' value" <<<"$RUN_LOG"; then
  t_ok "run: invalid fail-on rejected"
else
  t_fail "run: invalid fail-on rejected" "rc=$RUN_RC" "$RUN_LOG"
fi

# ── run.sh: argv mapping ─────────────────────────────────────────────────────
c="${WORK}/c-task"
run_runsh "$c" INPUT_TASK="fix the flaky test"
if [[ $RUN_RC -eq 0 ]] \
  && argv_is "$c" run --json --result-out "${c}/out/result.json" --prompt "fix the flaky test" \
  && got_output "$c" "status=done" && got_output "$c" "exit-code=0" \
  && got_output "$c" "result-path=${c}/out/result.json" && got_output "$c" "tap-path="; then
  t_ok "run: task maps to rysh run --json --result-out --prompt (+outputs)"
else
  t_fail "run: task maps to rysh run --json --result-out --prompt (+outputs)" "rc=$RUN_RC" "argv: $(tr '\n' '|' <"${c}/argv" 2>/dev/null)" "$RUN_LOG"
fi

c="${WORK}/c-flags"
# shellcheck disable=SC2016  # literal "$2" is a valid budget spelling
run_runsh "$c" INPUT_TASK="t" INPUT_PROVIDER="anthropic" INPUT_BUDGET='$2' INPUT_TIMEOUT="5m" INPUT_WORKTREE="true"
# shellcheck disable=SC2016
if [[ $RUN_RC -eq 0 ]] \
  && argv_is "$c" run --json --result-out "${c}/out/result.json" \
    --provider anthropic --budget '$2' --timeout 5m --worktree --prompt "t"; then
  t_ok "run: provider/budget/timeout/worktree map to run flags"
else
  t_fail "run: provider/budget/timeout/worktree map to run flags" "rc=$RUN_RC" "argv: $(tr '\n' '|' <"${c}/argv" 2>/dev/null)"
fi

c="${WORK}/c-skill"
run_runsh "$c" INPUT_SKILL_FILE="agents/reviewer.md"
if [[ $RUN_RC -eq 0 ]] \
  && argv_is "$c" run --json --result-out "${c}/out/result.json" agents/reviewer.md; then
  t_ok "run: skill-file is passed positionally (no --prompt)"
else
  t_fail "run: skill-file is passed positionally (no --prompt)" "rc=$RUN_RC" "argv: $(tr '\n' '|' <"${c}/argv" 2>/dev/null)"
fi

c="${WORK}/c-live"
run_runsh "$c" INPUT_EVAL="evals" INPUT_PROVIDER="anthropic" INPUT_TIMEOUT="90s" INPUT_WORKTREE="true" \
  FAKE_RYSH_TAP=$'TAP version 13\nok - case\n1..1\n'
if [[ $RUN_RC -eq 0 ]] \
  && argv_is "$c" eval evals --live --provider anthropic --timeout 90s --worktree \
  && grep -q "ok - case" "${c}/out/eval.tap" \
  && got_output "$c" "status=eval_passed" && got_output "$c" "tap-path=${c}/out/eval.tap" \
  && got_output "$c" "result-path="; then
  t_ok "run: eval maps to rysh eval --live and captures TAP"
else
  t_fail "run: eval maps to rysh eval --live and captures TAP" "rc=$RUN_RC" "argv: $(tr '\n' '|' <"${c}/argv" 2>/dev/null)" "$RUN_LOG"
fi

c="${WORK}/c-result"
run_runsh "$c" INPUT_EVAL="evals" INPUT_EVAL_RESULT="res.json" FAKE_RYSH_EXIT=1 \
  FAKE_RYSH_TAP=$'TAP version 13\nnot ok - case\n1..1\n'
if [[ $RUN_RC -eq 1 ]] \
  && argv_is "$c" eval evals --result res.json \
  && got_output "$c" "status=eval_failed" && got_output "$c" "exit-code=1"; then
  t_ok "run: eval-result maps to --result; failure propagates under default fail-on"
else
  t_fail "run: eval-result maps to --result; failure propagates under default fail-on" "rc=$RUN_RC" "argv: $(tr '\n' '|' <"${c}/argv" 2>/dev/null)" "$RUN_LOG"
fi

# ── run.sh: status mapping + fail-on semantics ───────────────────────────────
# fail-on=any (default): the step exits with rysh's REAL code, outputs emitted first.
c="${WORK}/c-gate"
run_runsh "$c" INPUT_TASK="x" FAKE_RYSH_EXIT=3
if [[ $RUN_RC -eq 3 ]] && got_output "$c" "status=gate_blocked" && got_output "$c" "exit-code=3"; then
  t_ok "run: fail-on=any propagates exit 3 as gate_blocked with outputs emitted"
else
  t_fail "run: fail-on=any propagates exit 3 as gate_blocked with outputs emitted" "rc=$RUN_RC" "$RUN_LOG"
fi

c="${WORK}/c-none"
run_runsh "$c" INPUT_TASK="x" FAKE_RYSH_EXIT=4 INPUT_FAIL_ON="none"
if [[ $RUN_RC -eq 0 ]] && got_output "$c" "status=budget_exhausted" && got_output "$c" "exit-code=4"; then
  t_ok "run: fail-on=none tolerates exit 4 (budget_exhausted) but reports it"
else
  t_fail "run: fail-on=none tolerates exit 4 (budget_exhausted) but reports it" "rc=$RUN_RC" "$RUN_LOG"
fi

c="${WORK}/c-err-gate"
run_runsh "$c" INPUT_TASK="x" FAKE_RYSH_EXIT=3 INPUT_FAIL_ON="error"
s1=$RUN_RC
c2="${WORK}/c-err-partial"
run_runsh "$c2" INPUT_TASK="x" FAKE_RYSH_EXIT=2 INPUT_FAIL_ON="error"
s2=$RUN_RC
if [[ $s1 -eq 0 && $s2 -eq 0 ]] && got_output "$c" "status=gate_blocked" && got_output "$c2" "status=partial"; then
  t_ok "run: fail-on=error tolerates gate-blocked (3) and partial (2)"
else
  t_fail "run: fail-on=error tolerates gate-blocked (3) and partial (2)" "rc(3)=$s1 rc(2)=$s2"
fi

c="${WORK}/c-err-hard1"
run_runsh "$c" INPUT_TASK="x" FAKE_RYSH_EXIT=1 INPUT_FAIL_ON="error"
s1=$RUN_RC
c="${WORK}/c-err-hard5"
run_runsh "$c" INPUT_TASK="x" FAKE_RYSH_EXIT=5 INPUT_FAIL_ON="error"
s5=$RUN_RC
if [[ $s1 -eq 1 && $s5 -eq 5 ]] && got_output "$c" "status=timeout"; then
  t_ok "run: fail-on=error still fails on error (1) and timeout (5) with real codes"
else
  t_fail "run: fail-on=error still fails on error (1) and timeout (5) with real codes" "rc(1)=$s1 rc(5)=$s5"
fi

c="${WORK}/c-partial-any"
run_runsh "$c" INPUT_TASK="x" FAKE_RYSH_EXIT=2
if [[ $RUN_RC -eq 2 ]] && got_output "$c" "status=partial"; then
  t_ok "run: exit 2 maps to status=partial and fails under default fail-on"
else
  t_fail "run: exit 2 maps to status=partial and fails under default fail-on" "rc=$RUN_RC"
fi

echo "1..$((PASS + FAIL))"
echo "# ${PASS} passed, ${FAIL} failed"
[[ $FAIL -eq 0 ]]
