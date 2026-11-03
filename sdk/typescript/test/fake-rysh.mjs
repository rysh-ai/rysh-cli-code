// A fake `rysh` binary for the SDK tests: same contract as
// sdk/python/tests/fake_rysh.py. Records argv to $FAKE_RYSH_LOG and answers
// from the rules in $FAKE_RYSH_SCENARIO. It never runs anything it is given.
import { appendFileSync, readFileSync, writeFileSync } from "node:fs";

const argv = process.argv.slice(2);
appendFileSync(process.env.FAKE_RYSH_LOG, JSON.stringify(argv) + "\n");
const path = process.env.FAKE_RYSH_SCENARIO;
const rules = JSON.parse(readFileSync(path, "utf8"));
const i = rules.findIndex((r) => (r.has ?? []).every((h) => argv.includes(h)));
if (i < 0) {
  process.stderr.write(`fake rysh: no rule for ${JSON.stringify(argv)}\n`);
  process.exit(99);
}
const r = rules[i];
if (r.times !== undefined) {
  r.times -= 1;
  if (r.times <= 0) rules.splice(i, 1);
  writeFileSync(path, JSON.stringify(rules));
}
process.stdout.write(r.stdout ?? "");
process.stderr.write(r.stderr ?? "");
process.exitCode = r.exit ?? 0;
