// The one door to rysh: execFile the local binary with an argument list.
// Never a shell string: caller text is one argv element. No socket, no
// session files — the binary does all of that.

import { execFile } from "node:child_process";
import { BinaryNotFoundError, errorForExit, RyshTimeoutError } from "./errors.js";

/** Seconds beyond a command's own --timeout before the SDK kills it, so the
 * binary reports its own timeout (exit 5, with a result line) first. */
const GRACE_S = 30;

export interface Completed {
  argv: string[];
  exitCode: number;
  stdout: string;
  stderr: string;
}

/** The last stdout line that parses as a JSON object. */
export function jsonLine(stdout: string): Record<string, any> | null {
  const lines = stdout.split("\n").reverse();
  for (const raw of lines) {
    const line = raw.trim();
    if (!line.startsWith("{")) continue;
    try {
      const v = JSON.parse(line);
      if (v && typeof v === "object" && !Array.isArray(v)) return v;
    } catch {
      // not JSON; keep looking
    }
  }
  return null;
}

export class Runner {
  constructor(
    readonly binary: string = "rysh",
    readonly env?: Record<string, string>,
  ) {}

  run(args: string[], timeoutS?: number): Promise<Completed> {
    const argv = [this.binary, ...args.map(String)];
    return new Promise((resolve, reject) => {
      execFile(
        this.binary,
        args.map(String),
        {
          shell: false,
          env: this.env ? { ...process.env, ...this.env } : process.env,
          timeout: timeoutS === undefined ? 0 : (timeoutS + GRACE_S) * 1000,
          maxBuffer: 64 * 1024 * 1024,
          encoding: "utf8",
        },
        (err, stdout, stderr) => {
          if (err && (err as NodeJS.ErrnoException).code === "ENOENT") {
            reject(new BinaryNotFoundError(`rysh binary not found: ${this.binary}`, { argv }));
            return;
          }
          if (err && (err as NodeJS.ErrnoException).code === "EACCES") {
            reject(new BinaryNotFoundError(`rysh binary not executable: ${this.binary}`, { argv }));
            return;
          }
          if (err && (err as any).killed) {
            reject(new RyshTimeoutError(`rysh did not exit within ${timeoutS}s (+${GRACE_S}s grace)`,
              { argv, stdout, stderr }));
            return;
          }
          const code = err ? (typeof (err as any).code === "number" ? (err as any).code : 1) : 0;
          resolve({ argv, exitCode: code, stdout, stderr });
        },
      );
    });
  }

  async check(args: string[], timeoutS?: number): Promise<Completed> {
    const c = await this.run(args, timeoutS);
    if (c.exitCode !== 0) throw errorFor(c);
    return c;
  }
}

export function errorFor(c: Completed, message?: string) {
  const result = jsonLine(c.stdout);
  if (message === undefined) {
    message = `rysh exited ${c.exitCode}`;
    const why = result && (result.error || result.detail || result.status);
    if (why) message += `: ${why}`;
    else {
      const tail = (c.stderr.trim() || c.stdout.trim()).split("\n").pop();
      if (tail) message += `: ${tail}`;
    }
  }
  const Cls = errorForExit(c.exitCode);
  return new Cls(message, { exitCode: c.exitCode, argv: c.argv, stdout: c.stdout, stderr: c.stderr, result });
}
