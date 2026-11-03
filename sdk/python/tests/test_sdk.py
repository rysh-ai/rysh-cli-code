"""SDK tests against a fake rysh binary (design 069 S1).

unittest.TestCase classes: pytest collects them as they are, and
``python3 -m unittest`` runs them with nothing installed.
"""

import json
import os
import stat
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "src"))

import rysh  # noqa: E402
from rysh import (  # noqa: E402
    BinaryNotFoundError,
    BudgetExceededError,
    CommandError,
    GateBlockedError,
    PaneGoneError,
    PartialError,
    RyshTimeoutError,
    Session,
)


def j(v):
    return json.dumps(v) + "\n"


def exec_ok(output, command="x"):
    return j({"ok": True, "status": 0, "command": command, "output": output, "status_aware": True})


def exec_fail(error):
    return j({"ok": False, "status": 1, "command": "x", "output": "\n[rysh] " + error + "\n", "error": error})


PANE_A = {"id": "pane-a", "title": "gentle-otter", "tab_id": "t1", "tab_title": "T", "lane_id": "l1",
          "stack_id": "g1", "status": "idle", "mode": "shell", "program": "claude", "needs_you": False,
          "active": False, "meta": {"claude.session_id": "sid"}}


class FakeRyshCase(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        d = self.dir.name
        self.log = os.path.join(d, "argv.log")
        self.scenario = os.path.join(d, "scenario.json")
        self.bin = os.path.join(d, "fake-rysh-bin")
        with open(self.bin, "w") as f:
            f.write("#!%s\n" % sys.executable)
            with open(os.path.join(HERE, "fake_rysh.py")) as src:
                f.write(src.read())
        os.chmod(self.bin, os.stat(self.bin).st_mode | stat.S_IXUSR)
        self.env = {"FAKE_RYSH_LOG": self.log, "FAKE_RYSH_SCENARIO": self.scenario}
        self.rules([])

    def tearDown(self):
        self.dir.cleanup()

    def rules(self, rules):
        with open(self.scenario, "w") as f:
            json.dump(rules, f)

    def calls(self):
        if not os.path.exists(self.log):
            return []
        with open(self.log) as f:
            return [json.loads(l) for l in f if l.strip()]

    def session(self, name="s1"):
        return Session(name, binary=self.bin, env=self.env)


class TestExec(FakeRyshCase):
    def test_argv_is_a_list_and_text_is_one_element(self):
        marker = os.path.join(self.dir.name, "pwned")
        self.rules([{"has": ["exec"], "stdout": exec_ok("hi")}])
        res = self.session().exec("pane meta set note $(touch %s); echo x" % marker)
        self.assertTrue(res.ok)
        self.assertEqual(self.calls(), [["exec", "--json", "--session", "s1", "--",
                                         "##pane meta set note $(touch %s); echo x" % marker]])
        self.assertFalse(os.path.exists(marker), "text reached a shell")

    def test_failure_raises_command_error(self):
        self.rules([{"has": ["exec"], "stdout": exec_fail("pane not found: zz"), "exit": 1}])
        with self.assertRaises(CommandError) as cm:
            self.session().pane("zz")
        self.assertIn("pane not found", str(cm.exception))
        self.assertEqual(cm.exception.exit_code, 1)

    def test_whitespace_ref_is_refused_before_running(self):
        with self.assertRaises(ValueError):
            self.session().pane("two words")
        self.assertEqual(self.calls(), [])

    def test_missing_binary(self):
        with self.assertRaises(BinaryNotFoundError):
            Session("s", binary=os.path.join(self.dir.name, "nope")).panes()


class TestPanes(FakeRyshCase):
    def test_panes_and_pane(self):
        self.rules([
            {"has": ["--", "##pane list --all --json"], "stdout": exec_ok(j({"panes": [PANE_A]}))},
            {"has": ["--", "##pane info pane-a --json"], "stdout": exec_ok(j(PANE_A))},
        ])
        s = self.session()
        panes = s.panes()
        self.assertEqual([p.id for p in panes], ["pane-a"])
        self.assertEqual(panes[0].program, "claude")
        self.assertEqual(panes[0].meta, {"claude.session_id": "sid"})
        self.assertEqual(panes[0].refresh().title, "gentle-otter")

    def test_unknown_record_fields_are_ignored(self):
        rec = dict(PANE_A, future_field=1)
        self.rules([{"has": ["exec"], "stdout": exec_ok(j(rec))}])
        self.assertEqual(self.session().pane("pane-a").id, "pane-a")

    def test_spawn_claude_resolves_the_alias(self):
        self.rules([
            {"has": ["--", "##pane new --json --claude fix the tests"],
             "stdout": exec_ok(j({"alias": "gentle-otter", "tab_id": "t1", "session_id": "sid"}))},
            {"has": ["--", "##pane info gentle-otter --json"], "stdout": exec_fail("pane not found: gentle-otter"),
             "exit": 1, "times": 1},
            {"has": ["--", "##pane info gentle-otter --json"], "stdout": exec_ok(j(dict(PANE_A, program=""))),
             "times": 1},
            {"has": ["--", "##pane info gentle-otter --json"], "stdout": exec_ok(j(PANE_A))},
        ])
        p = self.session().spawn(agent="claude", prompt="fix   the\ntests")
        self.assertEqual((p.id, p.program), ("pane-a", "claude"))
        self.assertEqual(len(self.calls()), 4, "spawn returned before claude was running")

    def test_spawn_agent_that_never_starts_times_out(self):
        self.rules([
            {"has": ["--", "##pane new --json --claude"], "stdout": exec_ok(j({"alias": "a1", "tab_id": "t1"}))},
            {"has": ["--", "##pane info a1 --json"], "stdout": exec_ok(j(dict(PANE_A, program="")))},
        ])
        with self.assertRaises(RyshTimeoutError):
            self.session().spawn(agent="claude", resolve_timeout=0.5)

    def test_spawn_shell_returns_without_a_program(self):
        self.rules([
            {"has": ["--", "##pane new --json"], "stdout": exec_ok(j({"alias": "a1", "tab_id": "t1"}))},
            {"has": ["--", "##pane info a1 --json"], "stdout": exec_ok(j(dict(PANE_A, program="")))},
        ])
        self.assertEqual(self.session().spawn().program, "")

    def test_spawn_rejects_unknown_agent(self):
        with self.assertRaises(ValueError):
            self.session().spawn(agent="codex")


class TestPromptExitCodes(FakeRyshCase):
    def check(self, code, cls):
        self.rules([{"has": ["prompt"], "stdout": j({"status": "x", "exit_code": code, "detail": "why"}),
                     "exit": code}])
        with self.assertRaises(cls) as cm:
            self.session().prompt("do it", pane="pane-a", timeout=5)
        self.assertEqual(cm.exception.exit_code, code)
        self.assertEqual(cm.exception.result["exit_code"], code)
        self.assertEqual(len(self.calls()), 1, "the SDK retried")

    def test_1(self):
        self.check(1, CommandError)

    def test_2(self):
        self.check(2, PartialError)

    def test_3_gate_blocked_is_raised_never_retried(self):
        self.check(3, GateBlockedError)

    def test_4(self):
        self.check(4, BudgetExceededError)

    def test_5(self):
        self.check(5, RyshTimeoutError)

    def test_done(self):
        self.rules([{"has": ["prompt"], "stdout": j({"status": "done", "exit_code": 0, "output": "ok"})}])
        r = self.session().prompt("-- tricky", pane="pane-a", timeout=5)
        self.assertEqual(r.output, "ok")
        self.assertEqual(self.calls()[0], ["prompt", "--json", "--quiet", "--session", "s1", "--pane-id",
                                           "pane-a", "--timeout", "5.0s", "--", "-- tricky"])


class TestWait(FakeRyshCase):
    def test_reached(self):
        self.rules([{"has": ["wait"], "stdout": j({"status": "reached", "exit_code": 0, "pane_id": "pane-a",
                                                   "until": "idle", "state": "idle", "activity": "idle",
                                                   "needs_you": False, "waited_ms": 10})}])
        r = self.session().wait_for("pane-a", "idle", timeout=30, interval=0.5)
        self.assertEqual(r.state, "idle")
        self.assertEqual(self.calls()[0], ["wait", "--json", "--session", "s1", "--pane-id", "pane-a",
                                           "--until", "idle", "--timeout", "30.0s", "--interval", "0.5s"])

    def test_timeout(self):
        self.rules([{"has": ["wait"], "stdout": j({"status": "timeout", "exit_code": 5, "state": "working"}),
                     "exit": 5}])
        with self.assertRaises(RyshTimeoutError):
            self.session().wait_for("pane-a", "idle", timeout=1)

    def test_gone(self):
        self.rules([{"has": ["wait"], "stdout": j({"status": "gone", "exit_code": 1, "state": "exited"}),
                     "exit": 1}])
        with self.assertRaises(PaneGoneError):
            self.session().wait_for("pane-a", "idle", timeout=1)

    def test_bad_until(self):
        with self.assertRaises(ValueError):
            self.session().wait_for("pane-a", "done")


class TestRun(FakeRyshCase):
    def test_done(self):
        self.rules([{"has": ["run"], "stdout": "agent text\n" + j({
            "status": "done", "exit_code": 0, "duration_ms": 5, "session": "run-1",
            "result": {"files_changed": ["a.go"], "commands": ["go test"], "output": "ok", "tokens_used": 7}})}])
        r = rysh.run("fix the test", timeout=60, budget="$2", binary=self.bin, env=self.env)
        self.assertEqual((r.status, r.files_changed, r.tokens_used), ("done", ["a.go"], 7))
        self.assertEqual(self.calls()[0], ["run", "--json", "--timeout", "60.0s", "--budget", "$2", "fix the test"])

    def test_gate_blocked(self):
        self.rules([{"has": ["run"], "stdout": j({"status": "gate_blocked", "exit_code": 3,
                                                  "result": {"files_changed": []}}), "exit": 3}])
        with self.assertRaises(GateBlockedError) as cm:
            rysh.run("deploy", binary=self.bin, env=self.env)
        self.assertEqual(cm.exception.result["status"], "gate_blocked")
        self.assertEqual(len(self.calls()), 1)

    def test_dash_prompt_goes_through_prompt_flag(self):
        self.rules([{"has": ["run"], "stdout": j({"status": "done", "exit_code": 0})}])
        rysh.run("--keep everything", binary=self.bin, env=self.env)
        self.assertEqual(self.calls()[0], ["run", "--json", "--prompt", "--keep everything"])

    def test_skill_with_task(self):
        self.rules([{"has": ["run"], "stdout": j({"status": "done", "exit_code": 0})}])
        rysh.run(skill="reviewer.md", task="review HEAD", binary=self.bin, env=self.env)
        self.assertEqual(self.calls()[0], ["run", "--json", "--prompt", "review HEAD", "reviewer.md"])


class TestOthers(FakeRyshCase):
    def test_send(self):
        self.rules([{"has": ["send"], "stdout": "sent to pane pane-a\n"}])
        self.session().send("pane-a", "ls -la; echo $HOME")
        self.assertEqual(self.calls()[0], ["send", "s1", "ls -la; echo $HOME", "--pane", "pane-a", "--mode", "shell"])

    def test_send_needs_a_session_name(self):
        env_backup = os.environ.pop("RYSH_SESSION", None)
        try:
            with self.assertRaises(ValueError):
                Session(binary=self.bin, env=self.env).send("pane-a", "ls")
        finally:
            if env_backup is not None:
                os.environ["RYSH_SESSION"] = env_backup

    def test_screen(self):
        self.rules([{"has": ["screen"], "stdout": j({"pane_id": "pane-a", "rows": ["$ ls", "a b"]})}])
        self.assertEqual(self.session().screen("pane-a"), ["$ ls", "a b"])

    def test_board_post(self):
        self.rules([{"has": ["board", "post"]}])
        self.session().board_post("done: 3/3", kind="status")
        self.assertEqual(self.calls()[0], ["board", "post", "--session", "s1", "--kind", "status", "--", "done: 3/3"])


if __name__ == "__main__":
    unittest.main()
