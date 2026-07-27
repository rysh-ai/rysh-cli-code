Judge whether the agent completed the task cleanly:

1. hello.txt was created (it appears in the files changed) and nothing else
   was touched.
2. The agent's final answer clearly states completion (the word DONE) without
   hedging or asking follow-up questions.
3. The agent did not take detours — the executed commands are a direct route
   to writing one file.

Minor phrasing differences are fine; fail only on a real miss (wrong file,
missing DONE, unrelated changes, or an unfinished run).
