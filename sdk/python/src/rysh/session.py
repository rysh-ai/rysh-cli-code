"""Session, Pane and run(): typed wrappers over the rysh CLI (design 069).

Each method is one documented CLI call:

    Session.exec        rysh exec --json -- '##<cmd>'
    Session.panes       ##pane list --all --json
    Session.pane        ##pane info <ref> --json
    Session.spawn       ##pane new --json [--claude <prompt> | --worktree [branch]]
    Session.send        rysh send <session> <text> --pane <id> --mode shell|prompt
    Session.prompt      rysh prompt --json
    Session.wait_for    rysh wait --json --until idle|needs-you|exited|any
    Session.screen      rysh screen --json
    Session.board_post  rysh board post -- <text>
    run                 rysh run --json
"""

from __future__ import annotations

import json
import os
import time
from dataclasses import dataclass, field
from typing import Any, Mapping, Optional, Sequence, Union

from ._runner import Completed, Runner, raise_for
from .errors import CommandError, PaneGoneError, RyshError, RyshTimeoutError

UNTIL = ("idle", "needs-you", "exited", "any")


def _dur(seconds: float) -> str:
    """A Go duration string for seconds (rysh parses time.ParseDuration)."""
    if seconds < 0:
        raise ValueError("a duration cannot be negative")
    return f"{float(seconds)}s"


def _one_word(ref: str, what: str) -> str:
    # A ## line is tokenised on whitespace, so a ref with a space would be
    # read as two arguments. Refuse rather than address the wrong pane.
    if not ref or any(c.isspace() for c in ref):
        raise ValueError(f"{what} must be one word with no whitespace: {ref!r}")
    return ref


@dataclass
class ExecResult:
    ok: bool
    status: int
    command: str
    output: str = ""
    error: str = ""
    status_aware: bool = False


@dataclass
class PromptResult:
    status: str
    exit_code: int
    session: str = ""
    output: str = ""
    detail: str = ""


@dataclass
class WaitResult:
    status: str
    pane_id: str
    until: str
    state: str
    activity: str = ""
    needs_you: bool = False
    attention: str = ""
    program: str = ""
    waited_ms: int = 0


@dataclass
class RunResult:
    status: str
    exit_code: int
    duration_ms: int = 0
    session: str = ""
    files_changed: list[str] = field(default_factory=list)
    commands: list[str] = field(default_factory=list)
    output: str = ""
    tokens_used: int = 0


@dataclass
class Pane:
    """One pane, as ##pane info --json reports it. Methods act on it by id."""

    id: str
    title: str = ""
    given_name: str = ""
    tab_id: str = ""
    tab_title: str = ""
    lane_id: str = ""
    stack_id: str = ""
    status: str = ""
    mode: str = ""
    program: str = ""
    needs_you: bool = False
    attention: str = ""
    active: bool = False
    meta: dict[str, str] = field(default_factory=dict)
    _session: Optional["Session"] = field(default=None, repr=False, compare=False)

    @classmethod
    def from_record(cls, rec: Mapping[str, Any], session: Optional["Session"] = None) -> "Pane":
        names = {f for f in cls.__dataclass_fields__ if not f.startswith("_")}
        p = cls(**{k: v for k, v in rec.items() if k in names})
        p.meta = dict(p.meta or {})
        p._session = session
        return p

    def _s(self) -> "Session":
        if self._session is None:
            raise RyshError("this Pane is not bound to a Session")
        return self._session

    def refresh(self) -> "Pane":
        return self._s().pane(self.id)

    def send(self, text: str, mode: str = "shell") -> None:
        self._s().send(self.id, text, mode=mode)

    def prompt(self, text: str, timeout: Optional[float] = None) -> PromptResult:
        return self._s().prompt(text, pane=self.id, timeout=timeout)

    def wait_for(self, until: str, timeout: float = 600.0, interval: Optional[float] = None) -> WaitResult:
        return self._s().wait_for(self.id, until, timeout=timeout, interval=interval)

    def screen(self) -> list[str]:
        return self._s().screen(self.id)


PaneRef = Union[str, Pane]


def _pane_id(p: PaneRef) -> str:
    return p.id if isinstance(p, Pane) else _one_word(p, "pane")


class Session:
    """A running rysh session, driven through the local ``rysh`` binary.

    ``name`` defaults to the binary's own default session (rysh config /
    ``$RYSH_SESSION`` inside a pane). ``binary`` is the rysh executable;
    ``env`` adds to the environment it runs with.
    """

    def __init__(
        self,
        name: Optional[str] = None,
        *,
        binary: str = "rysh",
        env: Optional[Mapping[str, str]] = None,
    ) -> None:
        self.name = name
        self.runner = Runner(binary, env)

    # -- plumbing ---------------------------------------------------------

    def _session_args(self) -> list[str]:
        return ["--session", self.name] if self.name else []

    def exec(self, command: str, *, pane: Optional[PaneRef] = None, tab: Optional[str] = None,
             check: bool = True) -> ExecResult:
        """Run one ## command. ``command`` may start with ## or not."""
        body = command.strip()
        if not body.startswith("#"):
            body = "##" + body
        args = ["exec", "--json", *self._session_args()]
        if pane is not None:
            args += ["--pane-id", _pane_id(pane)]
        if tab is not None:
            args += ["--tab-id", _one_word(tab, "tab")]
        args += ["--", body]
        c = self.runner.run(args)
        rec = c.json_line()
        if rec is None:
            if c.exit_code != 0:
                raise_for(c)
            raise CommandError("rysh exec printed no JSON result", exit_code=c.exit_code,
                               argv=c.argv, stdout=c.stdout, stderr=c.stderr)
        res = ExecResult(
            ok=bool(rec.get("ok")), status=int(rec.get("status", c.exit_code)),
            command=str(rec.get("command", "")), output=str(rec.get("output", "")),
            error=str(rec.get("error", "")), status_aware=bool(rec.get("status_aware")),
        )
        if check and (c.exit_code != 0 or not res.ok):
            msg = (res.error or res.output).strip() or f"##{res.command} failed"
            raise CommandError(msg, exit_code=c.exit_code or 1, argv=c.argv,
                               stdout=c.stdout, stderr=c.stderr, result=rec)
        return res

    def _exec_json(self, command: str) -> Any:
        res = self.exec(command)
        try:
            return json.loads(res.output.strip())
        except ValueError as e:
            raise CommandError(f"##{command}: output is not JSON", result={"output": res.output}) from e

    # -- panes ------------------------------------------------------------

    def panes(self, *, tab: Optional[str] = None) -> list[Pane]:
        """Every pane (all tabs), or one tab's panes."""
        cmd = "pane list --all --json" if tab is None else f"pane list --tab {_one_word(tab, 'tab')} --json"
        doc = self._exec_json(cmd)
        return [Pane.from_record(r, self) for r in doc.get("panes", [])]

    def pane(self, ref: str) -> Pane:
        """One pane by id, title or given-name."""
        return Pane.from_record(self._exec_json(f"pane info {_one_word(ref, 'pane')} --json"), self)

    def spawn(
        self,
        *,
        agent: Optional[str] = None,
        prompt: Optional[str] = None,
        worktree: Union[bool, str] = False,
        resolve_timeout: float = 30.0,
    ) -> Pane:
        """Open a pane: a shell, ``agent="claude"`` (with an optional launch
        prompt), or ``worktree=True|"<branch>"``. Returns once the pane exists
        and, for an agent, once its program is running — before that the pane
        is a quiet shell, and ``wait_for("idle")`` would fire at once.

        A ## line is tokenised on whitespace, so runs of spaces and newlines in
        ``prompt`` collapse to single spaces.
        """
        cmd = ["pane", "new", "--json"]
        if agent is not None:
            if agent != "claude":
                raise ValueError(f"unsupported agent {agent!r} (supported: 'claude')")
            if worktree:
                raise ValueError("agent and worktree cannot be combined by ##pane new")
            cmd.append("--claude")
            if prompt:
                cmd.append(" ".join(prompt.split()))
        elif prompt:
            raise ValueError("prompt needs agent='claude'")
        elif worktree:
            cmd.append("--worktree")
            if isinstance(worktree, str):
                cmd.append(_one_word(worktree, "branch"))
        made = self._exec_json(" ".join(cmd))
        alias = made.get("alias", "")
        if not alias:
            raise CommandError("##pane new --json reported no alias", result=made)
        deadline = time.monotonic() + resolve_timeout
        while True:
            try:
                p = self.pane(alias)
                if agent is None or p.program:
                    return p
                if time.monotonic() >= deadline:
                    raise RyshTimeoutError(
                        f"pane {p.id} exists but {agent} did not start within {resolve_timeout}s",
                        result={"alias": alias, "pane_id": p.id})
            except CommandError as e:
                if "pane not found" not in str(e) or time.monotonic() >= deadline:
                    raise
            time.sleep(0.25)

    # -- input and output ---------------------------------------------------

    def send(self, pane: PaneRef, text: str, mode: str = "shell") -> None:
        """Type input into a pane (shell command, or a prompt for a rysh pane)."""
        if mode not in ("shell", "prompt"):
            raise ValueError("mode must be 'shell' or 'prompt'")
        name = self.name or os.environ.get("RYSH_SESSION", "")
        if not name:
            raise ValueError("send needs a session name: Session(name=...) or $RYSH_SESSION")
        self.runner.check(["send", name, text, "--pane", _pane_id(pane), "--mode", mode])

    def prompt(self, text: str, *, pane: Optional[PaneRef] = None,
               timeout: Optional[float] = None) -> PromptResult:
        """Send a prompt to a rysh pane and wait for the turn to end.
        Raises GateBlockedError (3), PartialError (2), BudgetExceededError (4),
        RyshTimeoutError (5) or CommandError (1)."""
        args = ["prompt", "--json", "--quiet", *self._session_args()]
        if pane is not None:
            args += ["--pane-id", _pane_id(pane)]
        if timeout is not None:
            args += ["--timeout", _dur(timeout)]
        args += ["--", text]
        c = self.runner.run(args, timeout=timeout)
        if c.exit_code != 0:
            raise_for(c)
        rec = c.json_line() or {}
        return PromptResult(
            status=str(rec.get("status", "done")), exit_code=int(rec.get("exit_code", 0)),
            session=str(rec.get("session", "")), output=str(rec.get("output", "")),
            detail=str(rec.get("detail", "")),
        )

    def wait_for(self, pane: PaneRef, until: str, *, timeout: float = 600.0,
                 interval: Optional[float] = None) -> WaitResult:
        """Block until the pane is idle / needs-you / exited ("any": one look).
        Raises RyshTimeoutError on timeout and PaneGoneError if the pane is gone."""
        if until not in UNTIL:
            raise ValueError(f"until must be one of {UNTIL}")
        args = ["wait", "--json", *self._session_args(), "--pane-id", _pane_id(pane),
                "--until", until, "--timeout", _dur(timeout)]
        if interval is not None:
            args += ["--interval", _dur(interval)]
        c = self.runner.run(args, timeout=timeout)
        rec = c.json_line() or {}
        if c.exit_code == 1 and rec.get("status") == "gone":
            raise PaneGoneError(f"pane {_pane_id(pane)} is gone", exit_code=1, argv=c.argv,
                                stdout=c.stdout, stderr=c.stderr, result=rec)
        if c.exit_code != 0:
            raise_for(c)
        return WaitResult(**{k: rec[k] for k in WaitResult.__dataclass_fields__ if k in rec})

    def screen(self, pane: PaneRef) -> list[str]:
        """The pane's current screen rows."""
        c = self.runner.check(["screen", "--json", *self._session_args(), "--pane-id", _pane_id(pane)])
        rec = c.json_line() or {}
        return [str(r) for r in rec.get("rows", [])]

    def board_post(self, text: str, *, as_pane: Optional[str] = None, kind: Optional[str] = None,
                   thread: Optional[str] = None) -> None:
        """Post to the agents board."""
        args = ["board", "post", *self._session_args()]
        if as_pane:
            args += ["--as", _one_word(as_pane, "as_pane")]
        if kind:
            args += ["--kind", _one_word(kind, "kind")]
        if thread:
            args += ["--thread", _one_word(thread, "thread")]
        args += ["--", text]
        self.runner.check(args)


def run(
    prompt: Optional[str] = None,
    *,
    skill: Optional[str] = None,
    task: Optional[str] = None,
    timeout: Optional[float] = None,
    provider: Optional[str] = None,
    budget: Optional[str] = None,
    worktree: bool = False,
    keep: bool = False,
    result_out: Optional[str] = None,
    json_audit: Optional[str] = None,
    binary: str = "rysh",
    env: Optional[Mapping[str, str]] = None,
    extra_args: Sequence[str] = (),
) -> RunResult:
    """``rysh run``: a headless one-shot run in a throwaway session.

    Pass ``prompt`` text or a ``skill`` file (with an optional ``task``).
    A non-zero exit raises its typed error; ``err.result`` holds the run's
    JSON line (status, files_changed, …). A gate-blocked run (exit 3) is
    raised as GateBlockedError and never retried.
    """
    if (prompt is None) == (skill is None):
        raise ValueError("pass exactly one of prompt or skill")
    positional = skill if skill is not None else prompt
    assert positional is not None
    if positional.startswith("-"):
        # rysh run reads a leading dash as a flag; --prompt carries such text.
        if skill is not None:
            raise ValueError("a skill path cannot start with '-'")
        if task is not None:
            raise ValueError("a prompt starting with '-' cannot be combined with task")
    args = ["run", "--json"]
    if timeout is not None:
        args += ["--timeout", _dur(timeout)]
    for flag, val in (("--provider", provider), ("--budget", budget),
                      ("--result-out", result_out), ("--json-audit", json_audit)):
        if val is not None:
            args += [flag, val]
    if worktree:
        args.append("--worktree")
    if keep:
        args.append("--keep")
    args += list(extra_args)
    if positional.startswith("-"):
        args += ["--prompt", positional]
    else:
        if task is not None:
            args += ["--prompt", task]
        args.append(positional)
    c = Runner(binary, env).run(args, timeout=timeout)
    if c.exit_code != 0:
        raise_for(c)
    return _run_result(c)


def _run_result(c: Completed) -> RunResult:
    rec = c.json_line() or {}
    res = rec.get("result") or {}
    return RunResult(
        status=str(rec.get("status", "done")), exit_code=int(rec.get("exit_code", c.exit_code)),
        duration_ms=int(rec.get("duration_ms", 0)), session=str(rec.get("session", "")),
        files_changed=list(res.get("files_changed") or []), commands=list(res.get("commands") or []),
        output=str(res.get("output", "")), tokens_used=int(res.get("tokens_used", 0)),
    )
