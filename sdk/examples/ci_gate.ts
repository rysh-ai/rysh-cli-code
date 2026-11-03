// CI: a headless rysh run, gated on its exit code (TypeScript twin of ci_gate.py).
//   npx tsx sdk/examples/ci_gate.ts "make the failing test pass"
import { BudgetExceededError, GateBlockedError, RyshError, run } from "../typescript/src/index.js";

async function main(): Promise<void> {
  const prompt = process.argv.slice(2).join(" ") || "run the test suite and fix the first failure";
  try {
    const r = await run({ prompt, timeout: 900, budget: "$2", worktree: true });
    console.log(`done in ${r.durationMs} ms; changed: ${r.filesChanged.join(", ") || "nothing"}`);
  } catch (e) {
    if (e instanceof GateBlockedError) console.error(`BLOCKED at an approval gate: ${e.message}`);
    else if (e instanceof BudgetExceededError) console.error(`over budget: ${e.message}`);
    else if (e instanceof RyshError) console.error(`run failed (exit ${e.exitCode}): ${e.message}`);
    else throw e;
    process.exitCode = e instanceof RyshError ? e.exitCode ?? 1 : 1;
  }
}

void main();
