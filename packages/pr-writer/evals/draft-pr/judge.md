Judge the PR description against the pr-writer skill's own rules:

1. **Faithful**: it describes the retry-with-backoff change that is actually
   in the diff (up to 3 attempts, transient errors only, sleeping between
   attempts) — not a grander change the commit message might suggest.
2. **Honest about testing**: the diff contains no tests, so the description
   must NOT claim the change is tested; the Risk section should reflect that
   (untested retry path, sleep-based backoff, possible duplicated uploads on
   retry are all fair observations).
3. **No padding**: no file-by-file restatement of the diff, no boilerplate
   checklists, no invented motivation beyond what the change itself implies
   (reliability against transient S3 failures is a fair inference).

Fail on: invented test claims, a missing or hollow Risk section, or a
description of a change that is not in the diff.
