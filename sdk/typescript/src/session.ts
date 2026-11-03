// Session, Pane and run(): typed wrappers over the rysh CLI (design 069).
// Same API as the Python package; every method is one documented CLI call
// (see sdk/python/src/rysh/session.py for the table).

import { CommandError, PaneGoneError, RyshError, RyshTimeoutError } from "./errors.js";
import { Completed, errorFor, jsonLine, Runner } from "./runner.js";

export const UNTIL = ["idle", "needs-you", "exited", "any"] as const;
export type Until = (typeof UNTIL)[number];

export interface PaneRecord {
  id: string;
  title: string;
  given_name?: string;
  tab_id: string;
  tab_title: string;
  lane_id: string;
  stack_id: string;
  status: string;
  mode: string;
  program?: string;
  needs_you: boolean;
  attention?: string;
  active: boolean;
  meta?: Record<string, string>;
}

export interface ExecResult {
  ok: boolean;
  status: number;
  command: string;
  output: string;
  error: string;
  statusAware: boolean;
}

export interface PromptResult {
  status: string;
  exitCode: number;
  session: string;
  output: string;
  detail: string;
}

export interface WaitResult {
  status: string;
  paneId: string;
  until: string;
  state: string;
  activity: string;
  needsYou: boolean;
  attention: string;
  program: string;
  waitedMs: number;
}

export interface RunResult {
  status: string;
  exitCode: number;
  durationMs: number;
  session: string;
  filesChanged: string[];
  commands: string[];
  output: string;
  tokensUsed: number;
}

/** A Go duration string for seconds (rysh parses time.ParseDuration). */
function dur(seconds: number): string {
  if (!(seconds >= 0)) throw new RangeError("a duration cannot be negative");
  return `${seconds}s`;
}

/** A ## line is tokenised on whitespace: a ref with a space would address
 * the wrong thing, so refuse it. */
function oneWord(ref: string, what: string): string {
  if (!ref || /\s/.test(ref)) throw new RangeError(`${what} must be one word with no whitespace: ${JSON.stringify(ref)}`);
  return ref;
}

export type PaneRef = string | Pane;

function paneId(p: PaneRef): string {
  return typeof p === "string" ? oneWord(p, "pane") : p.id;
}

export class Pane {
  readonly id: string;
  readonly record: PaneRecord;

  constructor(record: PaneRecord, private readonly session: Session) {
    this.record = { ...record, meta: { ...(record.meta ?? {}) } };
    this.id = record.id;
  }

  get title() { return this.record.title; }
  get givenName() { return this.record.given_name ?? ""; }
  get program() { return this.record.program ?? ""; }
  get status() { return this.record.status; }
  get needsYou() { return this.record.needs_you; }
  get attention() { return this.record.attention ?? ""; }
  get meta() { return this.record.meta ?? {}; }

  refresh() { return this.session.pane(this.id); }
  send(text: string, mode: "shell" | "prompt" = "shell") { return this.session.send(this.id, text, mode); }
  prompt(text: string, opts: { timeout?: number } = {}) { return this.session.prompt(text, { ...opts, pane: this.id }); }
  waitFor(until: Until, opts: { timeout?: number; interval?: number } = {}) { return this.session.waitFor(this.id, until, opts); }
  screen() { return this.session.screen(this.id); }
}

export interface SessionOptions {
  binary?: string;
  env?: Record<string, string>;
}

export class Session {
  readonly runner: Runner;

  /** name defaults to the binary's own default session. */
  constructor(readonly name?: string, opts: SessionOptions = {}) {
    this.runner = new Runner(opts.binary ?? "rysh", opts.env);
  }

  private sessionArgs(): string[] {
    return this.name ? ["--session", this.name] : [];
  }

  /** Run one ## command (with or without its leading ##). */
  async exec(command: string, opts: { pane?: PaneRef; tab?: string; check?: boolean } = {}): Promise<ExecResult> {
    let body = command.trim();
    if (!body.startsWith("#")) body = "##" + body;
    const args = ["exec", "--json", ...this.sessionArgs()];
    if (opts.pane !== undefined) args.push("--pane-id", paneId(opts.pane));
    if (opts.tab !== undefined) args.push("--tab-id", oneWord(opts.tab, "tab"));
    args.push("--", body);
    const c = await this.runner.run(args);
    const rec = jsonLine(c.stdout);
    if (!rec) {
      if (c.exitCode !== 0) throw errorFor(c);
      throw new CommandError("rysh exec printed no JSON result", { exitCode: c.exitCode, argv: c.argv, stdout: c.stdout, stderr: c.stderr });
    }
    const res: ExecResult = {
      ok: Boolean(rec.ok), status: Number(rec.status ?? c.exitCode), command: String(rec.command ?? ""),
      output: String(rec.output ?? ""), error: String(rec.error ?? ""), statusAware: Boolean(rec.status_aware),
    };
    if ((opts.check ?? true) && (c.exitCode !== 0 || !res.ok)) {
      const msg = (res.error || res.output).trim() || `##${res.command} failed`;
      throw new CommandError(msg, { exitCode: c.exitCode || 1, argv: c.argv, stdout: c.stdout, stderr: c.stderr, result: rec });
    }
    return res;
  }

  private async execJSON(command: string): Promise<any> {
    const res = await this.exec(command);
    try {
      return JSON.parse(res.output.trim());
    } catch {
      throw new CommandError(`##${command}: output is not JSON`, { result: { output: res.output } });
    }
  }

  /** Every pane (all tabs), or one tab's panes. */
  async panes(opts: { tab?: string } = {}): Promise<Pane[]> {
    const cmd = opts.tab === undefined ? "pane list --all --json" : `pane list --tab ${oneWord(opts.tab, "tab")} --json`;
    const doc = await this.execJSON(cmd);
    return ((doc.panes ?? []) as PaneRecord[]).map((r) => new Pane(r, this));
  }

  /** One pane by id, title or given-name. */
  async pane(ref: string): Promise<Pane> {
    return new Pane(await this.execJSON(`pane info ${oneWord(ref, "pane")} --json`), this);
  }

  /** Open a pane: a shell, agent "claude" (optional launch prompt), or a
   * worktree. Resolves once the pane exists and, for an agent, its program
   * is running. Whitespace in the prompt
   * collapses to single spaces (a ## line is tokenised on whitespace). */
  async spawn(opts: { agent?: "claude"; prompt?: string; worktree?: boolean | string; resolveTimeout?: number } = {}): Promise<Pane> {
    const cmd = ["pane", "new", "--json"];
    if (opts.agent !== undefined) {
      if (opts.agent !== "claude") throw new RangeError(`unsupported agent ${JSON.stringify(opts.agent)} (supported: "claude")`);
      if (opts.worktree) throw new RangeError("agent and worktree cannot be combined by ##pane new");
      cmd.push("--claude");
      if (opts.prompt) cmd.push(opts.prompt.split(/\s+/).filter(Boolean).join(" "));
    } else if (opts.prompt) {
      throw new RangeError("prompt needs agent: \"claude\"");
    } else if (opts.worktree) {
      cmd.push("--worktree");
      if (typeof opts.worktree === "string") cmd.push(oneWord(opts.worktree, "branch"));
    }
    const made = await this.execJSON(cmd.join(" "));
    const alias = String(made.alias ?? "");
    if (!alias) throw new CommandError("##pane new --json reported no alias", { result: made });
    // For an agent, also wait for its program: until claude starts the pane
    // is a quiet shell, and waitFor("idle") would fire at once.
    const timeoutS = opts.resolveTimeout ?? 30;
    const deadline = Date.now() + timeoutS * 1000;
    for (;;) {
      try {
        const p = await this.pane(alias);
        if (opts.agent === undefined || p.program) return p;
        if (Date.now() >= deadline) {
          throw new RyshTimeoutError(`pane ${p.id} exists but ${opts.agent} did not start within ${timeoutS}s`,
            { result: { alias, pane_id: p.id } });
        }
      } catch (e) {
        if (!(e instanceof CommandError) || !e.message.includes("pane not found") || Date.now() >= deadline) throw e;
      }
      await new Promise((r) => setTimeout(r, 250));
    }
  }

  /** Type input into a pane. */
  async send(pane: PaneRef, text: string, mode: "shell" | "prompt" = "shell"): Promise<void> {
    if (mode !== "shell" && mode !== "prompt") throw new RangeError("mode must be shell or prompt");
    const name = this.name || process.env.RYSH_SESSION || "";
    if (!name) throw new RangeError("send needs a session name: new Session(name) or $RYSH_SESSION");
    await this.runner.check(["send", name, text, "--pane", paneId(pane), "--mode", mode]);
  }

  /** Prompt a rysh pane and wait for the turn. Throws the typed error for 1–5. */
  async prompt(text: string, opts: { pane?: PaneRef; timeout?: number } = {}): Promise<PromptResult> {
    const args = ["prompt", "--json", "--quiet", ...this.sessionArgs()];
    if (opts.pane !== undefined) args.push("--pane-id", paneId(opts.pane));
    if (opts.timeout !== undefined) args.push("--timeout", dur(opts.timeout));
    args.push("--", text);
    const c = await this.runner.run(args, opts.timeout);
    if (c.exitCode !== 0) throw errorFor(c);
    const rec = jsonLine(c.stdout) ?? {};
    return {
      status: String(rec.status ?? "done"), exitCode: Number(rec.exit_code ?? 0), session: String(rec.session ?? ""),
      output: String(rec.output ?? ""), detail: String(rec.detail ?? ""),
    };
  }

  /** Block until the pane is idle / needs-you / exited ("any": one look).
   * Throws RyshTimeoutError on timeout and PaneGoneError if the pane is gone. */
  async waitFor(pane: PaneRef, until: Until, opts: { timeout?: number; interval?: number } = {}): Promise<WaitResult> {
    if (!UNTIL.includes(until)) throw new RangeError(`until must be one of ${UNTIL.join(", ")}`);
    const timeout = opts.timeout ?? 600;
    const args = ["wait", "--json", ...this.sessionArgs(), "--pane-id", paneId(pane), "--until", until, "--timeout", dur(timeout)];
    if (opts.interval !== undefined) args.push("--interval", dur(opts.interval));
    const c = await this.runner.run(args, timeout);
    const rec = jsonLine(c.stdout) ?? {};
    if (c.exitCode === 1 && rec.status === "gone") {
      throw new PaneGoneError(`pane ${paneId(pane)} is gone`, { exitCode: 1, argv: c.argv, stdout: c.stdout, stderr: c.stderr, result: rec });
    }
    if (c.exitCode !== 0) throw errorFor(c);
    return {
      status: String(rec.status ?? ""), paneId: String(rec.pane_id ?? ""), until: String(rec.until ?? until),
      state: String(rec.state ?? ""), activity: String(rec.activity ?? ""), needsYou: Boolean(rec.needs_you),
      attention: String(rec.attention ?? ""), program: String(rec.program ?? ""), waitedMs: Number(rec.waited_ms ?? 0),
    };
  }

  /** The pane's current screen rows. */
  async screen(pane: PaneRef): Promise<string[]> {
    const c = await this.runner.check(["screen", "--json", ...this.sessionArgs(), "--pane-id", paneId(pane)]);
    return ((jsonLine(c.stdout) ?? {}).rows ?? []).map(String);
  }

  /** Post to the agents board. */
  async boardPost(text: string, opts: { asPane?: string; kind?: string; thread?: string } = {}): Promise<void> {
    const args = ["board", "post", ...this.sessionArgs()];
    if (opts.asPane) args.push("--as", oneWord(opts.asPane, "asPane"));
    if (opts.kind) args.push("--kind", oneWord(opts.kind, "kind"));
    if (opts.thread) args.push("--thread", oneWord(opts.thread, "thread"));
    args.push("--", text);
    await this.runner.check(args);
  }
}

export interface RunOptions {
  prompt?: string;
  skill?: string;
  task?: string;
  timeout?: number;
  provider?: string;
  budget?: string;
  worktree?: boolean;
  keep?: boolean;
  resultOut?: string;
  jsonAudit?: string;
  binary?: string;
  env?: Record<string, string>;
  extraArgs?: string[];
}

/** `rysh run`: a headless one-shot run in a throwaway session. A non-zero
 * exit throws its typed error (err.result is the run's JSON line); a
 * gate-blocked run (3) throws GateBlockedError and is never retried. */
export async function run(opts: RunOptions): Promise<RunResult> {
  if ((opts.prompt === undefined) === (opts.skill === undefined)) throw new RangeError("pass exactly one of prompt or skill");
  const positional = (opts.skill ?? opts.prompt) as string;
  const dash = positional.startsWith("-");
  if (dash && opts.skill !== undefined) throw new RangeError("a skill path cannot start with '-'");
  if (dash && opts.task !== undefined) throw new RangeError("a prompt starting with '-' cannot be combined with task");
  const args = ["run", "--json"];
  if (opts.timeout !== undefined) args.push("--timeout", dur(opts.timeout));
  const flags: [string, string | undefined][] = [
    ["--provider", opts.provider], ["--budget", opts.budget], ["--result-out", opts.resultOut], ["--json-audit", opts.jsonAudit],
  ];
  for (const [f, v] of flags) if (v !== undefined) args.push(f, v);
  if (opts.worktree) args.push("--worktree");
  if (opts.keep) args.push("--keep");
  args.push(...(opts.extraArgs ?? []));
  if (dash) args.push("--prompt", positional);
  else {
    if (opts.task !== undefined) args.push("--prompt", opts.task);
    args.push(positional);
  }
  const c: Completed = await new Runner(opts.binary ?? "rysh", opts.env).run(args, opts.timeout);
  if (c.exitCode !== 0) throw errorFor(c);
  const rec = jsonLine(c.stdout) ?? {};
  const res = rec.result ?? {};
  return {
    status: String(rec.status ?? "done"), exitCode: Number(rec.exit_code ?? 0), durationMs: Number(rec.duration_ms ?? 0),
    session: String(rec.session ?? ""), filesChanged: [...(res.files_changed ?? [])], commands: [...(res.commands ?? [])],
    output: String(res.output ?? ""), tokensUsed: Number(res.tokens_used ?? 0),
  };
}

export { RyshError };
