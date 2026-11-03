// SPDX-License-Identifier: Apache-2.0

package hooks

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func needBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
}

func TestLoad_MissingFileIsEmptyAndAutoCheckpoints(t *testing.T) {
	c := Load(t.TempDir())
	if c.Err != nil || c.Loaded || !c.AutoCheckpoints || c.ClaudeBridge {
		t.Fatalf("missing file: %+v", c)
	}
	if blocked, _ := c.Gate(context.Background(), PreTool, Payload{Tool: "Bash"}, Env{}); blocked {
		t.Fatal("no file means nothing gates")
	}
}

func TestLoad_ParsesEveryEvent(t *testing.T) {
	dir := writeConfig(t, `
claude_bridge: true
checkpoints: off
hooks:
  pre-tool:
    - run: ./check.sh
      match: "Bash|Edit"
      timeout: 5s
  post-tool:
    - run: ./fmt.sh
  on-needs-you:
    - run: ./notify.sh
  on-finish:
    - run: ./lint.sh
      timeout: 2m
`)
	c := Load(dir)
	if c.Err != nil || !c.Loaded || !c.ClaudeBridge || c.AutoCheckpoints {
		t.Fatalf("%+v", c)
	}
	for _, ev := range []Event{PreTool, PostTool, NeedsYou, Finish} {
		if len(c.For(ev)) != 1 {
			t.Errorf("%s: %d hooks", ev, len(c.For(ev)))
		}
	}
	if h := c.For(PreTool)[0]; h.timeout != 5*time.Second || !h.Matches("Bash") || h.Matches("Read") {
		t.Fatalf("pre-tool hook: %+v", h)
	}
	if h := c.For(PostTool)[0]; h.timeout != DefaultTimeout || !h.Matches("anything") {
		t.Fatalf("post-tool hook defaults: %+v", h)
	}
}

func TestLoad_RejectsBadFiles(t *testing.T) {
	for name, body := range map[string]string{
		"unknown key":      "hookz: {}\n",
		"unknown event":    "hooks:\n  on-boot:\n    - run: x\n",
		"empty run":        "hooks:\n  on-finish:\n    - run: \"\"\n",
		"bad timeout":      "hooks:\n  on-finish:\n    - run: x\n      timeout: soon\n",
		"timeout too long": "hooks:\n  on-finish:\n    - run: x\n      timeout: 11m\n",
		"bad match":        "hooks:\n  pre-tool:\n    - run: x\n      match: \"(\"\n",
		"bad checkpoints":  "checkpoints: sometimes\n",
		"not yaml":         ":::\n",
	} {
		c := Load(writeConfig(t, body))
		if c.Err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

// A file that does not parse is an unknown posture: every pre-tool call is
// blocked until it is fixed (design 013's rule for policy.yaml).
func TestGate_BrokenConfigFailsClosed(t *testing.T) {
	c := Load(writeConfig(t, "hookz: {}\n"))
	blocked, reason := c.Gate(context.Background(), PreTool, Payload{Tool: "Read"}, Env{})
	if !blocked || !strings.Contains(reason, FileName) {
		t.Fatalf("blocked=%v reason=%q", blocked, reason)
	}
	// Notify-only events never block, even then.
	if blocked, _ := c.Gate(context.Background(), Finish, Payload{}, Env{}); blocked {
		t.Fatal("on-finish must never block")
	}
}

func TestGate_ExitCodesAndTimeout(t *testing.T) {
	needBash(t)
	dir := writeConfig(t, `
hooks:
  pre-tool:
    - run: 'read -r line; case "$line" in *rm\ -rf*) echo "no rm -rf" >&2; exit 2;; esac; exit 0'
      match: "^Bash$"
    - run: 'sleep 5'
      match: "^Slow$"
      timeout: 200ms
    - run: 'exit 1'
      match: "^Fails$"
`)
	c := Load(dir)
	if c.Err != nil {
		t.Fatal(c.Err)
	}
	ctx := context.Background()
	env := Env{Dir: dir}
	in := func(cmd string) json.RawMessage { b, _ := json.Marshal(map[string]string{"command": cmd}); return b }

	if blocked, why := c.Gate(ctx, PreTool, Payload{Tool: "Bash", Input: in("ls")}, env); blocked {
		t.Fatalf("ls blocked: %s", why)
	}
	blocked, why := c.Gate(ctx, PreTool, Payload{Tool: "Bash", Input: in("rm -rf /")}, env)
	if !blocked || !strings.Contains(why, "no rm -rf") {
		t.Fatalf("rm -rf: blocked=%v why=%q", blocked, why)
	}
	start := time.Now()
	blocked, why = c.Gate(ctx, PreTool, Payload{Tool: "Slow"}, env)
	if !blocked || !strings.Contains(why, "timed out") || time.Since(start) > 4*time.Second {
		t.Fatalf("timeout: blocked=%v why=%q after %s", blocked, why, time.Since(start))
	}
	if blocked, _ := c.Gate(ctx, PreTool, Payload{Tool: "Fails"}, env); !blocked {
		t.Fatal("a non-zero exit blocks")
	}
	if blocked, _ := c.Gate(ctx, PreTool, Payload{Tool: "Unmatched"}, env); blocked {
		t.Fatal("no matching hook: not blocked")
	}
}

func TestRun_PayloadAndEnv(t *testing.T) {
	needBash(t)
	out := filepath.Join(t.TempDir(), "seen")
	dir := writeConfig(t, `
hooks:
  on-finish:
    - run: 'cat > `+out+`; echo "$RYSH_HOOK_EVENT $RYSH_PANE $PWD" >> `+out+`'
`)
	c := Load(dir)
	res := c.Run(context.Background(), Finish, Payload{PaneID: "p1", Program: "claude", Cwd: "/w"},
		Env{Dir: dir, Vars: []string{"RYSH_PANE=p1"}})
	if len(res) != 1 || res[0].Err != nil {
		t.Fatalf("results: %+v", res)
	}
	b, _ := os.ReadFile(out)
	got := string(b)
	real, _ := filepath.EvalSymlinks(dir)
	for _, want := range []string{`"event":"on-finish"`, `"pane_id":"p1"`, `"program":"claude"`, "on-finish p1 "} {
		if !strings.Contains(got, want) {
			t.Errorf("hook saw %q, want %q", got, want)
		}
	}
	if !strings.Contains(got, dir) && !strings.Contains(got, real) {
		t.Errorf("hook did not run in the workspace dir: %q", got)
	}
}

func TestRun_NotifyFailureIsReportedNotBlocking(t *testing.T) {
	needBash(t)
	c := Load(writeConfig(t, "hooks:\n  on-needs-you:\n    - run: 'echo boom; exit 3'\n"))
	res := c.Run(context.Background(), NeedsYou, Payload{}, Env{})
	if len(res) != 1 || res[0].ExitCode != 3 || !strings.Contains(res[0].Output, "boom") {
		t.Fatalf("%+v", res)
	}
	if blocked, _ := c.Gate(context.Background(), NeedsYou, Payload{}, Env{}); blocked {
		t.Fatal("notify events never block")
	}
}

func TestRun_OutputCapped(t *testing.T) {
	needBash(t)
	c := Load(writeConfig(t, "hooks:\n  on-finish:\n    - run: 'head -c 200000 /dev/zero | tr \"\\\\0\" x'\n"))
	res := c.Run(context.Background(), Finish, Payload{}, Env{})
	if len(res) != 1 || len(res[0].Output) > OutputLimit+64 {
		t.Fatalf("output not capped: %d", len(res[0].Output))
	}
}

// Hooks get an allow-listed environment, never the daemon's whole one: the
// pane environment carries credential names (lead ruling R8, rr-msg-193).
func TestRun_EnvironmentIsAllowListed(t *testing.T) {
	needBash(t)
	t.Setenv("GAP09_NOT_ON_THE_LIST", "plain")
	t.Setenv("RYSH_NOT_AN_EVENT_FIELD", "plain")
	t.Setenv("LANG", "C")
	out := filepath.Join(t.TempDir(), "env")
	dir := writeConfig(t, `
hooks:
  on-finish:
    - run: 'echo "${GAP09_NOT_ON_THE_LIST-unset} ${RYSH_NOT_AN_EVENT_FIELD-unset} ${PATH:+path} ${HOME:+home} ${LANG-nolang} $RYSH_PANE $RYSH_HOOK_EVENT" > `+out+`'
`)
	res := Load(dir).Run(context.Background(), Finish, Payload{}, Env{Dir: dir, Vars: []string{"RYSH_PANE=p1"}})
	if len(res) != 1 || res[0].Err != nil {
		t.Fatalf("results: %+v", res)
	}
	b, _ := os.ReadFile(out)
	if got, want := strings.TrimSpace(string(b)), "unset unset path home C p1 on-finish"; got != want {
		t.Fatalf("hook environment: got %q, want %q", got, want)
	}
}
