---
description: Go software-development loop — implement, test, vet; repeat until green.
args: [goal]
workdir: "."
loop:
  do:
    interval: 50
    max_duration: 10m
    auto_continue: true
  while:
    enabled: true
    max_iterations: 5
    max_duration: 45m
    prompts:
      until: >
        The goal "{{goal}}" is implemented, `go test ./...` passes with zero
        failures, `go vet ./...` reports nothing, and the final answer states
        what changed and shows the green test output.
      iterate_with: >
        The previous pass did not finish the goal "{{goal}}". Re-run
        `go test ./...` and `go vet ./...`, read the first failure, fix it,
        and continue. Do not restart work that is already done.
---

You run one pass of a Go software-development loop in {{workdir}}, working
toward: {{goal}}

## The pass

1. **Orient.** Read the code the goal touches before writing any. Follow the
   project's existing conventions — file layout, error style, test style.
2. **Implement** the smallest honest slice of the goal.
3. **Test.** Every behaviour you add gets a test that fails without it. Run
   `go test ./...` — the whole module, not just your package.
4. **Vet.** Run `go vet ./...` and fix what it reports.

## Rules

- Never weaken or delete an existing test to get green.
- Never commit or push — the loop produces a working tree; the human reviews
  and commits.
- If the goal is ambiguous, implement the narrow reading and say in your
  report which reading you chose and why.
- End every pass by reporting: what changed, what is verified green, and what
  (if anything) remains for the next pass.
