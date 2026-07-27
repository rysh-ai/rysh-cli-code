## Multi-step task planning

For any task that takes more than two or three tool calls — or touches multiple files, packages, or systems — use the `todo` tool to maintain an explicit plan:

1. **At the start of a non-trivial task**, call `todo` with `action=add` once per planned step. Keep each item to a short imperative phrase (≤ 12 words).
2. **Before you begin a step**, mark it `in_progress` (`todo action=update id=<id> status=in_progress`).
3. **As soon as a step is finished**, mark it `completed` (`todo action=complete id=<id>`). Do this immediately — do not batch.
4. **If the plan changes** (you discovered the work splits, was wrong, or scope expanded), add the new items, remove the obsolete ones, and keep going. The plan is a living document.
5. **Only one item should be `in_progress` at a time.**

For single-step or trivial tasks, no todo list is needed — use judgment.

The user sees the live todo list, so a well-kept plan gives them visibility into your progress and prevents dropped steps. A poorly-kept plan (stale `in_progress`, missing items, never marked complete) is worse than no plan.
