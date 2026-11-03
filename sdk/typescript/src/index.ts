// @rysh/sdk — drive a rysh session from TypeScript (design 069).
// A thin, typed wrapper over the local rysh binary. It opens no network
// connection and never approves a gate.
export * from "./errors.js";
export { Session, Pane, run, UNTIL } from "./session.js";
export type { Until, PaneRecord, PaneRef, ExecResult, PromptResult, WaitResult, RunResult, RunOptions, SessionOptions } from "./session.js";
