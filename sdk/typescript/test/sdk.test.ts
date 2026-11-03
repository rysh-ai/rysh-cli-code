// SDK tests against a fake rysh binary (design 069 S2). Run with
// `vitest run --globals` (describe/it/expect are globals; no import).
import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import {
  BinaryNotFoundError, BudgetExceededError, CommandError, GateBlockedError, PaneGoneError,
  PartialError, RyshTimeoutError, Session, run,
} from "../src/index.js";

const HERE = dirname(fileURLToPath(import.meta.url));
const j = (v: unknown) => JSON.stringify(v) + "\n";
const execOk = (output: string) => j({ ok: true, status: 0, command: "x", output, status_aware: true });
const execFail = (error: string) => j({ ok: false, status: 1, command: "x", output: `\n[rysh] ${error}\n`, error });

const PANE_A = {
  id: "pane-a", title: "gentle-otter", tab_id: "t1", tab_title: "T", lane_id: "l1", stack_id: "g1",
  status: "idle", mode: "shell", program: "claude", needs_you: false, active: false, meta: { "claude.session_id": "sid" },
};

let dir: string, log: string, scenario: string, bin: string, env: Record<string, string>;

function rules(r: unknown[]) {
  writeFileSync(scenario, JSON.stringify(r));
}
function calls(): string[][] {
  if (!existsSync(log)) return [];
  return readFileSync(log, "utf8").split("\n").filter(Boolean).map((l) => JSON.parse(l));
}
const session = (name = "s1") => new Session(name, { binary: bin, env });

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), "rysh-sdk-test-"));
  log = join(dir, "argv.log");
  scenario = join(dir, "scenario.json");
  bin = join(dir, "fake-rysh-bin");
  writeFileSync(bin, `#!${process.execPath}\n` + readFileSync(join(HERE, "fake-rysh.mjs"), "utf8"));
  chmodSync(bin, 0o755);
  env = { FAKE_RYSH_LOG: log, FAKE_RYSH_SCENARIO: scenario };
  rules([]);
});
afterEach(() => rmSync(dir, { recursive: true, force: true }));

describe("exec", () => {
  it("passes an argument list; text is one element and never reaches a shell", async () => {
    const marker = join(dir, "pwned");
    rules([{ has: ["exec"], stdout: execOk("hi") }]);
    const res = await session().exec(`pane meta set note $(touch ${marker}); echo x`);
    expect(res.ok).toBe(true);
    expect(calls()).toEqual([["exec", "--json", "--session", "s1", "--", `##pane meta set note $(touch ${marker}); echo x`]]);
    expect(existsSync(marker)).toBe(false);
  });
  it("throws CommandError on failure", async () => {
    rules([{ has: ["exec"], stdout: execFail("pane not found: zz"), exit: 1 }]);
    await expect(session().pane("zz")).rejects.toThrow(CommandError);
  });
  it("refuses a whitespace ref before running", async () => {
    await expect(session().pane("two words")).rejects.toThrow(RangeError);
    expect(calls()).toEqual([]);
  });
  it("missing binary", async () => {
    await expect(new Session("s", { binary: join(dir, "nope") }).panes()).rejects.toThrow(BinaryNotFoundError);
  });
});

describe("panes", () => {
  it("lists and resolves", async () => {
    rules([
      { has: ["--", "##pane list --all --json"], stdout: execOk(j({ panes: [PANE_A] })) },
      { has: ["--", "##pane info pane-a --json"], stdout: execOk(j(PANE_A)) },
    ]);
    const panes = await session().panes();
    expect(panes.map((p) => p.id)).toEqual(["pane-a"]);
    expect(panes[0].program).toBe("claude");
    expect((await panes[0].refresh()).title).toBe("gentle-otter");
  });
  it("spawn claude resolves the alias once the pane exists", async () => {
    rules([
      { has: ["--", "##pane new --json --claude fix the tests"], stdout: execOk(j({ alias: "gentle-otter", tab_id: "t1" })) },
      { has: ["--", "##pane info gentle-otter --json"], stdout: execFail("pane not found: gentle-otter"), exit: 1, times: 1 },
      { has: ["--", "##pane info gentle-otter --json"], stdout: execOk(j({ ...PANE_A, program: "" })), times: 1 },
      { has: ["--", "##pane info gentle-otter --json"], stdout: execOk(j(PANE_A)) },
    ]);
    const p = await session().spawn({ agent: "claude", prompt: "fix   the\ntests" });
    expect([p.id, p.program]).toEqual(["pane-a", "claude"]);
    expect(calls().length).toBe(4);
  });
  it("spawn: an agent that never starts times out", async () => {
    rules([
      { has: ["--", "##pane new --json --claude"], stdout: execOk(j({ alias: "a1", tab_id: "t1" })) },
      { has: ["--", "##pane info a1 --json"], stdout: execOk(j({ ...PANE_A, program: "" })) },
    ]);
    await expect(session().spawn({ agent: "claude", resolveTimeout: 0.5 })).rejects.toThrow(RyshTimeoutError);
  });
  it("spawn: a shell pane returns without a program", async () => {
    rules([
      { has: ["--", "##pane new --json"], stdout: execOk(j({ alias: "a1", tab_id: "t1" })) },
      { has: ["--", "##pane info a1 --json"], stdout: execOk(j({ ...PANE_A, program: "" })) },
    ]);
    expect((await session().spawn()).program).toBe("");
  });
});

describe("prompt exit codes", () => {
  const cases: [number, Function][] = [
    [1, CommandError], [2, PartialError], [3, GateBlockedError], [4, BudgetExceededError], [5, RyshTimeoutError],
  ];
  for (const [code, Cls] of cases) {
    it(`exit ${code} throws ${Cls.name}, once`, async () => {
      rules([{ has: ["prompt"], stdout: j({ status: "x", exit_code: code }), exit: code }]);
      const err = await session().prompt("do it", { pane: "pane-a", timeout: 5 }).catch((e) => e);
      expect(err).toBeInstanceOf(Cls);
      expect(err.exitCode).toBe(code);
      expect(err.result.exit_code).toBe(code);
      expect(calls().length).toBe(1);
    });
  }
  it("done", async () => {
    rules([{ has: ["prompt"], stdout: j({ status: "done", exit_code: 0, output: "ok" }) }]);
    const r = await session().prompt("-- tricky", { pane: "pane-a", timeout: 5 });
    expect(r.output).toBe("ok");
    expect(calls()[0]).toEqual(["prompt", "--json", "--quiet", "--session", "s1", "--pane-id", "pane-a", "--timeout", "5s", "--", "-- tricky"]);
  });
});

describe("waitFor", () => {
  it("reached", async () => {
    rules([{ has: ["wait"], stdout: j({ status: "reached", exit_code: 0, pane_id: "pane-a", until: "idle", state: "idle", activity: "idle", needs_you: false, waited_ms: 10 }) }]);
    const r = await session().waitFor("pane-a", "idle", { timeout: 30, interval: 0.5 });
    expect(r.state).toBe("idle");
    expect(calls()[0]).toEqual(["wait", "--json", "--session", "s1", "--pane-id", "pane-a", "--until", "idle", "--timeout", "30s", "--interval", "0.5s"]);
  });
  it("timeout", async () => {
    rules([{ has: ["wait"], stdout: j({ status: "timeout", exit_code: 5 }), exit: 5 }]);
    await expect(session().waitFor("pane-a", "idle", { timeout: 1 })).rejects.toThrow(RyshTimeoutError);
  });
  it("gone", async () => {
    rules([{ has: ["wait"], stdout: j({ status: "gone", exit_code: 1 }), exit: 1 }]);
    await expect(session().waitFor("pane-a", "idle", { timeout: 1 })).rejects.toThrow(PaneGoneError);
  });
});

describe("run", () => {
  it("done", async () => {
    rules([{ has: ["run"], stdout: "agent text\n" + j({ status: "done", exit_code: 0, duration_ms: 5, session: "run-1", result: { files_changed: ["a.go"], commands: ["go test"], output: "ok", tokens_used: 7 } }) }]);
    const r = await run({ prompt: "fix the test", timeout: 60, budget: "$2", binary: bin, env });
    expect([r.status, r.filesChanged, r.tokensUsed]).toEqual(["done", ["a.go"], 7]);
    expect(calls()[0]).toEqual(["run", "--json", "--timeout", "60s", "--budget", "$2", "fix the test"]);
  });
  it("gate-blocked throws once, never retried", async () => {
    rules([{ has: ["run"], stdout: j({ status: "gate_blocked", exit_code: 3 }), exit: 3 }]);
    const err = await run({ prompt: "deploy", binary: bin, env }).catch((e) => e);
    expect(err).toBeInstanceOf(GateBlockedError);
    expect(err.result.status).toBe("gate_blocked");
    expect(calls().length).toBe(1);
  });
  it("dash prompt goes through --prompt", async () => {
    rules([{ has: ["run"], stdout: j({ status: "done", exit_code: 0 }) }]);
    await run({ prompt: "--keep everything", binary: bin, env });
    expect(calls()[0]).toEqual(["run", "--json", "--prompt", "--keep everything"]);
  });
});

describe("others", () => {
  it("send", async () => {
    rules([{ has: ["send"], stdout: "sent\n" }]);
    await session().send("pane-a", "ls -la; echo $HOME");
    expect(calls()[0]).toEqual(["send", "s1", "ls -la; echo $HOME", "--pane", "pane-a", "--mode", "shell"]);
  });
  it("screen", async () => {
    rules([{ has: ["screen"], stdout: j({ pane_id: "pane-a", rows: ["$ ls", "a b"] }) }]);
    expect(await session().screen("pane-a")).toEqual(["$ ls", "a b"]);
  });
  it("boardPost", async () => {
    rules([{ has: ["board", "post"] }]);
    await session().boardPost("done: 3/3", { kind: "status" });
    expect(calls()[0]).toEqual(["board", "post", "--session", "s1", "--kind", "status", "--", "done: 3/3"]);
  });
});
