#!/usr/bin/env bash
#
# setup-rysh run step (design 009 §3.3): map action inputs onto exactly one of
#
#   rysh run --prompt <task>            (INPUT_TASK)
#   rysh run <skill.md>                 (INPUT_SKILL_FILE)
#   rysh eval <dirs> --live             (INPUT_EVAL)
#   rysh eval <dirs> --result <file>    (INPUT_EVAL + INPUT_EVAL_RESULT)
#
# then surface the outcome honestly: the status/exit-code outputs are emitted
# BEFORE the step fails, and when it fails it exits with rysh's REAL exit code
# (0 done · 1 error · 2 partial-saved · 3 gate-blocked · 4 budget-exhausted ·
# 5 timeout; eval: 0 all-pass · 1 otherwise). INPUT_FAIL_ON decides which
# non-zero codes fail the step: any (default) / error (1 and 5 only) / none.
#
# Env contract (set by action.yml; functions are sourceable for tests):
#   INPUT_TASK INPUT_SKILL_FILE INPUT_EVAL INPUT_EVAL_RESULT
#   INPUT_PROVIDER INPUT_BUDGET INPUT_TIMEOUT INPUT_WORKTREE INPUT_FAIL_ON
#   RYSH_BIN        rysh binary to execute (default: "rysh" from PATH)
#   RYSH_OUT_DIR    directory for result.json / eval.tap (default: ./rysh-out)
#   GITHUB_OUTPUT   step-output file; when unset, outputs are only logged
#
# Provider API keys are NOT handled here on purpose: rysh reads them from the
# process environment (RYSH_API_KEY / ANTHROPIC_API_KEY / GEMINI_API_KEY),
# which the workflow supplies from secrets. This script never echoes env vars.

set -euo pipefail

log()  { echo "[setup-rysh] $*"; }
fail() { echo "[setup-rysh] ERROR: $*" >&2; exit 1; }

# validate_inputs enforces the input contract before anything executes:
# exactly one mode, and no flag that its mode would silently ignore.
validate_inputs() {
  local modes=0
  [[ -n "${INPUT_TASK:-}" ]] && modes=$((modes + 1))
  [[ -n "${INPUT_SKILL_FILE:-}" ]] && modes=$((modes + 1))
  [[ -n "${INPUT_EVAL:-}" ]] && modes=$((modes + 1))
  if [[ "$modes" -ne 1 ]]; then
    fail "exactly one of the 'task', 'skill-file' or 'eval' inputs must be set — they are mutually exclusive (got task='${INPUT_TASK:-}', skill-file='${INPUT_SKILL_FILE:-}', eval='${INPUT_EVAL:-}')"
  fi
  if [[ -n "${INPUT_EVAL_RESULT:-}" && -z "${INPUT_EVAL:-}" ]]; then
    fail "'eval-result' requires the 'eval' input (it switches the eval case dirs to --result grading mode)"
  fi
  if [[ -n "${INPUT_EVAL:-}" && -n "${INPUT_BUDGET:-}" ]]; then
    fail "'budget' is a rysh run flag; rysh eval does not support it — remove 'budget' or switch to task/skill-file mode"
  fi
  if [[ -n "${INPUT_EVAL_RESULT:-}" ]]; then
    # --result grading is pure and local; reject flags it would silently ignore.
    [[ -z "${INPUT_PROVIDER:-}" ]] || fail "'provider' has no effect in eval --result grading mode — remove it"
    [[ -z "${INPUT_TIMEOUT:-}" ]] || fail "'timeout' has no effect in eval --result grading mode — remove it"
    [[ "${INPUT_WORKTREE:-false}" != "true" ]] || fail "'worktree' has no effect in eval --result grading mode — remove it"
  fi
  case "${INPUT_FAIL_ON:-any}" in
    any | error | none) ;;
    *) fail "invalid 'fail-on' value '${INPUT_FAIL_ON:-}' (want: any, error, or none)" ;;
  esac
}

# build_argv fills the globals from the INPUT_* env:
#   RYSH_ARGV    argv for the rysh binary
#   RUN_MODE     "run" | "eval"
#   RESULT_PATH  where rysh run writes the Result JSON ("" in eval mode)
#   TAP_PATH     where eval TAP output is captured ("" in run mode)
build_argv() {
  local out="${RYSH_OUT_DIR:-rysh-out}"
  RYSH_ARGV=()
  RESULT_PATH=""
  TAP_PATH=""

  if [[ -n "${INPUT_EVAL:-}" ]]; then
    RUN_MODE="eval"
    TAP_PATH="${out}/eval.tap"
    RYSH_ARGV=(eval "${INPUT_EVAL}")
    if [[ -n "${INPUT_EVAL_RESULT:-}" ]]; then
      RYSH_ARGV+=(--result "${INPUT_EVAL_RESULT}")
    else
      RYSH_ARGV+=(--live)
      [[ -n "${INPUT_PROVIDER:-}" ]] && RYSH_ARGV+=(--provider "${INPUT_PROVIDER}")
      [[ -n "${INPUT_TIMEOUT:-}" ]] && RYSH_ARGV+=(--timeout "${INPUT_TIMEOUT}")
      [[ "${INPUT_WORKTREE:-false}" == "true" ]] && RYSH_ARGV+=(--worktree)
    fi
    return 0
  fi

  RUN_MODE="run"
  RESULT_PATH="${out}/result.json"
  RYSH_ARGV=(run --json --result-out "${RESULT_PATH}")
  [[ -n "${INPUT_PROVIDER:-}" ]] && RYSH_ARGV+=(--provider "${INPUT_PROVIDER}")
  [[ -n "${INPUT_BUDGET:-}" ]] && RYSH_ARGV+=(--budget "${INPUT_BUDGET}")
  [[ -n "${INPUT_TIMEOUT:-}" ]] && RYSH_ARGV+=(--timeout "${INPUT_TIMEOUT}")
  [[ "${INPUT_WORKTREE:-false}" == "true" ]] && RYSH_ARGV+=(--worktree)
  if [[ -n "${INPUT_SKILL_FILE:-}" ]]; then
    RYSH_ARGV+=("${INPUT_SKILL_FILE}")
  else
    RYSH_ARGV+=(--prompt "${INPUT_TASK}")
  fi
  return 0
}

# map_status <mode> <exit-code> prints the status string for the outputs.
# Run-mode codes mirror run_cmd.go's exit table; anything unexpected is an
# honest "error", never invented as success.
map_status() {
  local mode="$1" code="$2"
  if [[ "$mode" == "eval" ]]; then
    if [[ "$code" -eq 0 ]]; then echo "eval_passed"; else echo "eval_failed"; fi
    return 0
  fi
  case "$code" in
    0) echo "done" ;;
    2) echo "partial" ;;
    3) echo "gate_blocked" ;;
    4) echo "budget_exhausted" ;;
    5) echo "timeout" ;;
    *) echo "error" ;;
  esac
}

# should_fail <fail-on> <exit-code>: exit status 0 == "fail the step".
should_fail() {
  local policy="$1" code="$2"
  [[ "$code" -ne 0 ]] || return 1
  case "$policy" in
    none) return 1 ;;
    error) [[ "$code" -eq 1 || "$code" -eq 5 ]] ;;
    *) return 0 ;; # any (default): every non-zero code fails
  esac
}

# emit_output writes one step output (and logs it — values here are statuses
# and file paths, never secrets).
emit_output() {
  local key="$1" val="$2"
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    echo "${key}=${val}" >>"${GITHUB_OUTPUT}"
  fi
  log "output ${key}=${val}"
}

main() {
  validate_inputs

  local out="${RYSH_OUT_DIR:-rysh-out}"
  mkdir -p "$out"
  out=$(cd "$out" && pwd) # absolute: outputs must be usable from any later step
  RYSH_OUT_DIR="$out"

  build_argv
  local bin="${RYSH_BIN:-rysh}"
  log "exec: rysh ${RYSH_ARGV[*]}"

  local rc=0
  if [[ "$RUN_MODE" == "eval" ]]; then
    # TAP goes to the log AND to TAP_PATH for the tap-path output / artifact.
    set +e
    "$bin" "${RYSH_ARGV[@]}" | tee "${TAP_PATH}"
    rc=${PIPESTATUS[0]}
    set -e
  else
    set +e
    "$bin" "${RYSH_ARGV[@]}"
    rc=$?
    set -e
  fi

  local status
  status=$(map_status "$RUN_MODE" "$rc")

  # Only advertise files that exist (a boot failure can leave no result.json).
  [[ -n "$RESULT_PATH" && -f "$RESULT_PATH" ]] || RESULT_PATH=""
  [[ -n "$TAP_PATH" && -f "$TAP_PATH" ]] || TAP_PATH=""

  emit_output "status" "$status"
  emit_output "exit-code" "$rc"
  emit_output "result-path" "$RESULT_PATH"
  emit_output "tap-path" "$TAP_PATH"

  log "finished: status=${status} exit=${rc} (fail-on=${INPUT_FAIL_ON:-any})"
  if should_fail "${INPUT_FAIL_ON:-any}" "$rc"; then
    log "failing the step with rysh's exit code ${rc}"
    exit "$rc"
  fi
  if [[ "$rc" -ne 0 ]]; then
    log "fail-on=${INPUT_FAIL_ON:-any} tolerates exit ${rc}; step succeeds — gate on the status/exit-code outputs downstream"
  fi
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
