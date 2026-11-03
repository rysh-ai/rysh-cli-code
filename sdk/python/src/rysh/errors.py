"""Typed errors for the rysh SDK.

The exit codes are ``rysh run``'s, shared by ``rysh prompt`` and ``rysh wait``:

    0 done   1 error   2 partial   3 gate-blocked   4 budget exhausted   5 timeout

Every error carries what the binary said, so nothing is lost by raising.
A gate-blocked run is never retried or approved by the SDK: it is raised.
"""

from __future__ import annotations

from typing import Any, Optional, Sequence


class RyshError(Exception):
    """A rysh command did not succeed.

    Attributes:
        exit_code: the binary's exit status (None when it never ran).
        argv: the argument list that was run (never a shell string).
        stdout / stderr: what the binary printed.
        result: the parsed ``--json`` line, when there was one.
    """

    def __init__(
        self,
        message: str,
        *,
        exit_code: Optional[int] = None,
        argv: Sequence[str] = (),
        stdout: str = "",
        stderr: str = "",
        result: Optional[dict[str, Any]] = None,
    ) -> None:
        super().__init__(message)
        self.exit_code = exit_code
        self.argv = list(argv)
        self.stdout = stdout
        self.stderr = stderr
        self.result = result


class BinaryNotFoundError(RyshError):
    """The ``rysh`` binary could not be started."""


class CommandError(RyshError):
    """Exit 1: the command ran and failed, or the session could not be reached."""


class PaneGoneError(CommandError):
    """``wait_for`` on a pane that no longer exists."""


class PartialError(RyshError):
    """Exit 2: the run paused with a resumable partial and nobody to resume it."""


class GateBlockedError(RyshError):
    """Exit 3: an approval gate fired. The SDK never approves; a human must."""


class BudgetExceededError(RyshError):
    """Exit 4: the ``budget`` ceiling was crossed."""


class RyshTimeoutError(RyshError, TimeoutError):
    """Exit 5: the deadline passed before the run, turn or wait finished."""


_BY_CODE: dict[int, type[RyshError]] = {
    1: CommandError,
    2: PartialError,
    3: GateBlockedError,
    4: BudgetExceededError,
    5: RyshTimeoutError,
}


def error_for_exit(code: int) -> type[RyshError]:
    """The error class for a non-zero exit code (unknown codes are RyshError)."""
    return _BY_CODE.get(code, RyshError)
