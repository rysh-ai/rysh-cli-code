"""rysh — drive a rysh session from Python (design 069).

A thin, typed wrapper over the local ``rysh`` binary. It opens no network
connection and never approves a gate: an approval request surfaces as
GateBlockedError.
"""

from .errors import (
    BinaryNotFoundError,
    BudgetExceededError,
    CommandError,
    GateBlockedError,
    PaneGoneError,
    PartialError,
    RyshError,
    RyshTimeoutError,
)
from .session import (
    ExecResult,
    Pane,
    PromptResult,
    RunResult,
    Session,
    WaitResult,
    run,
)

__all__ = [
    "BinaryNotFoundError",
    "BudgetExceededError",
    "CommandError",
    "ExecResult",
    "GateBlockedError",
    "Pane",
    "PaneGoneError",
    "PartialError",
    "PromptResult",
    "RunResult",
    "RyshError",
    "RyshTimeoutError",
    "Session",
    "WaitResult",
    "run",
]
