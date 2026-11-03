"""The one door to rysh: run the local binary with an argument list.

Never a shell string, never ``shell=True``: text from a caller (a prompt, a
pane name) is one argv element and cannot become a second command. The SDK
opens no socket and reads no session file; the binary does all of that.
"""

from __future__ import annotations

import json
import os
import subprocess
from dataclasses import dataclass
from typing import Any, Mapping, Optional, Sequence

from .errors import BinaryNotFoundError, RyshTimeoutError, error_for_exit

# Extra seconds the process is given beyond the command's own --timeout, so
# the binary reports its own timeout (exit 5, with a result line) before the
# SDK has to kill it.
_GRACE = 30.0


@dataclass
class Completed:
    argv: list[str]
    exit_code: int
    stdout: str
    stderr: str

    def json_line(self) -> Optional[dict[str, Any]]:
        """The last stdout line that parses as a JSON object, if any."""
        for line in reversed(self.stdout.splitlines()):
            line = line.strip()
            if line.startswith("{"):
                try:
                    v = json.loads(line)
                except ValueError:
                    continue
                if isinstance(v, dict):
                    return v
        return None


class Runner:
    """Runs ``binary`` with argument lists."""

    def __init__(self, binary: str = "rysh", env: Optional[Mapping[str, str]] = None) -> None:
        self.binary = binary
        self.env = dict(env) if env is not None else None

    def run(self, args: Sequence[str], *, timeout: Optional[float] = None) -> Completed:
        argv = [self.binary, *[str(a) for a in args]]
        env = None
        if self.env is not None:
            env = {**os.environ, **self.env}
        try:
            p = subprocess.run(
                argv,
                shell=False,
                stdin=subprocess.DEVNULL,
                capture_output=True,
                text=True,
                env=env,
                timeout=None if timeout is None else timeout + _GRACE,
            )
        except FileNotFoundError as e:
            raise BinaryNotFoundError(
                f"rysh binary not found: {self.binary!r}", argv=argv
            ) from e
        except PermissionError as e:
            raise BinaryNotFoundError(
                f"rysh binary not executable: {self.binary!r}", argv=argv
            ) from e
        except subprocess.TimeoutExpired as e:
            raise RyshTimeoutError(
                f"rysh did not exit within {timeout}s (+{_GRACE}s grace)",
                exit_code=None,
                argv=argv,
                stdout=_text(e.stdout),
                stderr=_text(e.stderr),
            ) from e
        return Completed(argv=argv, exit_code=p.returncode, stdout=p.stdout, stderr=p.stderr)

    def check(self, args: Sequence[str], *, timeout: Optional[float] = None) -> Completed:
        """run(), raising the typed error for a non-zero exit."""
        c = self.run(args, timeout=timeout)
        if c.exit_code != 0:
            raise_for(c)
        return c


def raise_for(c: Completed, message: Optional[str] = None) -> None:
    result = c.json_line()
    if message is None:
        message = _message(c, result)
    cls = error_for_exit(c.exit_code)
    raise cls(
        message,
        exit_code=c.exit_code,
        argv=c.argv,
        stdout=c.stdout,
        stderr=c.stderr,
        result=result,
    )


def _message(c: Completed, result: Optional[dict[str, Any]]) -> str:
    if result:
        for k in ("error", "detail"):
            if result.get(k):
                return f"rysh exited {c.exit_code}: {result[k]}"
        if result.get("status"):
            return f"rysh exited {c.exit_code}: {result['status']}"
    tail = (c.stderr.strip() or c.stdout.strip()).splitlines()
    return f"rysh exited {c.exit_code}" + (f": {tail[-1]}" if tail else "")


def _text(b: Any) -> str:
    if b is None:
        return ""
    if isinstance(b, bytes):
        return b.decode("utf-8", "replace")
    return str(b)
