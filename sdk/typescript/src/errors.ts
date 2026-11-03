// Typed errors for the rysh SDK (design 069).
//
// Exit codes are `rysh run`'s, shared by `rysh prompt` and `rysh wait`:
//   0 done  1 error  2 partial  3 gate-blocked  4 budget exhausted  5 timeout
// A gate-blocked run is raised, never retried or approved.

export interface RyshErrorInit {
  exitCode?: number | null;
  argv?: string[];
  stdout?: string;
  stderr?: string;
  result?: Record<string, unknown> | null;
}

export class RyshError extends Error {
  readonly exitCode: number | null;
  readonly argv: string[];
  readonly stdout: string;
  readonly stderr: string;
  readonly result: Record<string, unknown> | null;

  constructor(message: string, init: RyshErrorInit = {}) {
    super(message);
    this.name = new.target.name;
    this.exitCode = init.exitCode ?? null;
    this.argv = init.argv ?? [];
    this.stdout = init.stdout ?? "";
    this.stderr = init.stderr ?? "";
    this.result = init.result ?? null;
  }
}

/** The rysh binary could not be started. */
export class BinaryNotFoundError extends RyshError {}
/** Exit 1: the command failed, or the session could not be reached. */
export class CommandError extends RyshError {}
/** waitFor on a pane that no longer exists. */
export class PaneGoneError extends CommandError {}
/** Exit 2: paused with a resumable partial and nobody to resume it. */
export class PartialError extends RyshError {}
/** Exit 3: an approval gate fired. The SDK never approves. */
export class GateBlockedError extends RyshError {}
/** Exit 4: the budget ceiling was crossed. */
export class BudgetExceededError extends RyshError {}
/** Exit 5: the deadline passed. */
export class RyshTimeoutError extends RyshError {}

const BY_CODE: Record<number, new (m: string, i?: RyshErrorInit) => RyshError> = {
  1: CommandError,
  2: PartialError,
  3: GateBlockedError,
  4: BudgetExceededError,
  5: RyshTimeoutError,
};

export function errorForExit(code: number): new (m: string, i?: RyshErrorInit) => RyshError {
  return BY_CODE[code] ?? RyshError;
}
