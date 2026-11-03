// SPDX-License-Identifier: Apache-2.0

package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeSettings_Shape(t *testing.T) {
	b, err := ClaudeSettings("/opt/rysh bin/rysh", "/w/it's/.rysh", "sess")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"PreToolUse", "PostToolUse", "Notification", "Stop", "UserPromptSubmit"} {
		if len(s.Hooks[name]) != 1 || len(s.Hooks[name][0].Hooks) != 1 {
			t.Fatalf("%s missing: %s", name, b)
		}
		h := s.Hooks[name][0].Hooks[0]
		if h.Type != "command" || !strings.Contains(h.Command, "'/opt/rysh bin/rysh' hook emit") ||
			!strings.Contains(h.Command, `'/w/it'\''s/.rysh'`) || h.Timeout*1e9 < int(MaxTimeout) {
			t.Errorf("%s hook: %+v", name, h)
		}
		// Only the gate turns any failure of rysh itself into a block; for
		// Stop, exit 2 would mean "keep going", so the others must not.
		gate := strings.HasSuffix(h.Command, "|| exit 2")
		if !strings.Contains(h.Command, " --event "+name) {
			t.Errorf("%s: the command must name its event: %q", name, h.Command)
		}
		if gate != (name == "PreToolUse") {
			t.Errorf("%s: fail-closed suffix = %v", name, gate)
		}
	}
	if s.Hooks["PreToolUse"][0].Matcher != "*" {
		t.Error("PreToolUse must match every tool")
	}
}

func TestShellQuoteRoundTrips(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	for _, s := range []string{"plain", "with space", "it's", `"dq"`, "$HOME", "a\\b"} {
		out, err := exec.Command("bash", "-c", "printf %s "+shellQuote(s)).Output()
		if err != nil || string(out) != s {
			t.Errorf("%q -> %q (%v)", s, out, err)
		}
	}
}

func TestEmit_PreToolBlocksWithExit2(t *testing.T) {
	needBash(t)
	ryshDir := writeConfig(t, `
hooks:
  pre-tool:
    - run: 'grep -q "rm -rf" && { echo "refused: rm -rf" >&2; exit 1; }; exit 0'
      match: "^Bash$"
`)
	in := `{"hook_event_name":"PreToolUse","session_id":"s1","cwd":"/tmp","tool_name":"Bash","tool_input":{"command":"rm -rf /"}}`
	var stdout, stderr bytes.Buffer
	code := Emit(context.Background(), strings.NewReader(in), &stdout, &stderr, EmitOptions{RyshDir: ryshDir, PaneID: "p1"})
	if code != 2 || !strings.Contains(stderr.String(), "refused: rm -rf") {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("emit must never write stdout (claude reads it): %q", stdout.String())
	}
	in = `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}`
	stderr.Reset()
	if code := Emit(context.Background(), strings.NewReader(in), &stdout, &stderr, EmitOptions{RyshDir: ryshDir, PaneID: "p1"}); code != 0 {
		t.Fatalf("ls blocked: %d %q", code, stderr.String())
	}
}

func TestEmit_BrokenInputAndConfigFailClosedOnlyForPreTool(t *testing.T) {
	broken := writeConfig(t, "hookz: {}\n")
	var out, errb bytes.Buffer
	if code := Emit(context.Background(), strings.NewReader("not json"), &out, &errb, EmitOptions{RyshDir: t.TempDir()}); code != 2 {
		t.Fatalf("unreadable input of an unknown event must block: %d", code)
	}
	// The settings file names the event, so unreadable input is refused only
	// where refusing is what 2 means: on Stop, 2 would keep claude going.
	for ev, want := range map[string]int{"PreToolUse": 2, "Stop": 0, "PostToolUse": 0, "Notification": 0, "UserPromptSubmit": 0} {
		if code := Emit(context.Background(), strings.NewReader("not json"), &out, &errb, EmitOptions{RyshDir: t.TempDir(), Event: ev}); code != want {
			t.Errorf("unreadable input, --event %s: code %d, want %d", ev, code, want)
		}
	}
	// A flag and an input that disagree are unreadable too; the gating one wins.
	if code := Emit(context.Background(), strings.NewReader(`{"hook_event_name":"PreToolUse","tool_name":"Bash"}`), &out, &errb, EmitOptions{RyshDir: t.TempDir(), Event: "Stop"}); code != 2 {
		t.Fatalf("--event Stop with a PreToolUse input must block: %d", code)
	}
	pre := `{"hook_event_name":"PreToolUse","tool_name":"Read"}`
	if code := Emit(context.Background(), strings.NewReader(pre), &out, &errb, EmitOptions{RyshDir: broken}); code != 2 {
		t.Fatalf("broken hooks.yaml must block pre-tool: %d", code)
	}
	stop := `{"hook_event_name":"Stop"}`
	if code := Emit(context.Background(), strings.NewReader(stop), &out, &errb, EmitOptions{RyshDir: broken}); code != 0 {
		t.Fatalf("Stop must never be blocked (exit 2 would keep claude going): %d", code)
	}
	if code := Emit(context.Background(), strings.NewReader(`{"hook_event_name":"SessionEnd"}`), &out, &errb, EmitOptions{RyshDir: broken}); code != 0 {
		t.Fatalf("unknown events are ignored: %d", code)
	}
}

func TestEmit_NotifyEventsRunHooks(t *testing.T) {
	needBash(t)
	seen := filepath.Join(t.TempDir(), "seen")
	ryshDir := writeConfig(t, `
hooks:
  on-finish:
    - run: 'cat >> `+seen+`'
  on-needs-you:
    - run: 'cat >> `+seen+`; exit 7'
  post-tool:
    - run: 'cat >> `+seen+`'
`)
	var out, errb bytes.Buffer
	for _, ev := range []string{"Stop", "Notification", "PostToolUse"} {
		in := `{"hook_event_name":"` + ev + `","tool_name":"Edit","message":"Claude needs your permission"}`
		if code := Emit(context.Background(), strings.NewReader(in), &out, &errb, EmitOptions{RyshDir: ryshDir, PaneID: "p9"}); code != 0 {
			t.Fatalf("%s: code %d", ev, code)
		}
	}
	b, _ := os.ReadFile(seen)
	for _, want := range []string{`"event":"on-finish"`, `"event":"on-needs-you"`, `"reason":"Claude needs your permission"`, `"event":"post-tool"`, `"pane_id":"p9"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("hooks saw %q, want %q", b, want)
		}
	}
}

func TestEmit_UserPromptSubmitCheckpoints(t *testing.T) {
	t.Setenv("RYSH_PANE", "") // the test may itself run in a rysh pane
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	for _, a := range [][]string{{"init", "-q", "-b", "main"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, a...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]string{"hook_event_name": "UserPromptSubmit", "cwd": repo, "session_id": "sess-uuid", "prompt": "do it"})
	var out, errb bytes.Buffer
	if code := Emit(context.Background(), bytes.NewReader(in), &out, &errb, EmitOptions{RyshDir: t.TempDir()}); code != 0 {
		t.Fatalf("code %d %q", code, errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("UserPromptSubmit stdout becomes claude context; must be empty: %q", out.String())
	}
	refs, _ := exec.Command("git", "-C", repo, "for-each-ref", "--format=%(refname)", "refs/rysh/checkpoints/sess-uuid/").Output()
	if !strings.Contains(string(refs), "refs/rysh/checkpoints/sess-uuid/") {
		t.Fatalf("no checkpoint under the claude session id: %q (stderr %q)", refs, errb.String())
	}
	// checkpoints: off disables it.
	off := writeConfig(t, "checkpoints: off\n")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Emit(context.Background(), bytes.NewReader(in), &out, &errb, EmitOptions{RyshDir: off, PaneID: "other"}); code != 0 {
		t.Fatal(code)
	}
	refs, _ = exec.Command("git", "-C", repo, "for-each-ref", "refs/rysh/checkpoints/other/").Output()
	if len(refs) != 0 {
		t.Fatal("checkpoints: off must not checkpoint")
	}
}
