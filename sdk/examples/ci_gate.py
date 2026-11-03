#!/usr/bin/env python3
"""CI: a headless rysh run, gated on its exit code.

    python3 sdk/examples/ci_gate.py "make the failing test pass"

Exit 0 only when the run is done. A gate-blocked run fails the job: CI has
no human to approve, and the SDK never approves on anyone's behalf.
"""

import sys

sys.path.insert(0, __file__.rsplit("/", 2)[0] + "/python/src")  # run from a checkout

import rysh


def main() -> int:
    prompt = " ".join(sys.argv[1:]) or "run the test suite and fix the first failure"
    try:
        r = rysh.run(prompt, timeout=900, budget="$2", worktree=True)
    except rysh.GateBlockedError as e:
        print(f"BLOCKED at an approval gate: {e}", file=sys.stderr)
        return 3
    except rysh.BudgetExceededError as e:
        print(f"over budget: {e}", file=sys.stderr)
        return 4
    except rysh.RyshError as e:  # 1 error, 2 partial, 5 timeout
        print(f"run failed (exit {e.exit_code}): {e}", file=sys.stderr)
        return e.exit_code or 1
    print(f"done in {r.duration_ms} ms; changed: {', '.join(r.files_changed) or 'nothing'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
