// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// `rysh board tail` parity with `##board copy`: the copy grammar, the shared
// formatter, --copy, and session resolution from anywhere under the workspace.

func TestBoardCopyAndOutputArgs(t *testing.T) {
	a, err := parseBoardArgs([]string{"copy", "3", "issue-314", "--board", "etl-fix", "--print", "--session", "s"})
	if err != nil {
		t.Fatal(err)
	}
	if a.items.Worker != "issue-314" || a.items.N != 3 || a.board != "etl-fix" || !a.print || a.sess != "s" {
		t.Fatalf("copy = %+v", a)
	}
	b, err := parseBoardArgs([]string{"output", "pdf", "5", "--out", "/x", "--no-open"})
	if err != nil || b.items.Format != "pdf" || b.items.N != 5 || b.items.Out != "/x" || !b.items.NoOpen {
		t.Fatalf("output = %+v, %v", b, err)
	}
	if _, err := parseBoardArgs([]string{"output", "5"}); err == nil || !strings.Contains(err.Error(), "needs a format") {
		t.Fatalf("missing format = %v", err)
	}
	if _, err := parseBoardArgs([]string{"output", "5", "docx", "x"}); err == nil {
		t.Fatal("unknown format accepted")
	}
	if _, err := parseBoardArgs([]string{"copy", "--nope"}); err == nil {
		t.Fatal("unknown flag accepted")
	}
}

func TestBoardTailAgentAndLastAreItemBased(t *testing.T) {
	p, err := parseBoardArgs([]string{"tail", "--last", "2", "--agent", "issue-314", "--json"})
	if err != nil || p.agent != "issue-314" || p.last != 2 || !p.jsonOut {
		t.Fatalf("tail = %+v, %v", p, err)
	}
	if _, err := parseBoardArgs([]string{"tail", "--last", "two"}); err == nil {
		t.Fatal("--last two accepted")
	}

	p, _ = parseBoardArgs([]string{"tail", "--agent", "issue-314", "--last", "1"})
	out, err := renderBoardTailItems(tailFleet(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "● issue-314  [milestone]") || !strings.Contains(out, strings.Repeat("z", 190)) ||
		strings.Contains(out, "BLOCKED") {
		t.Fatalf("tail --agent --last =\n%s", out)
	}
	p, _ = parseBoardArgs([]string{"tail", "--agent", "issue-31", "--json"})
	js, _ := renderBoardTailItems(tailFleet(), p)
	var got board.QueryReply
	if err := json.Unmarshal([]byte(js), &got); err != nil || len(got.Threads) != 1 || got.Threads[0].Root.Persona != "issue-31" {
		t.Fatalf("tail --agent --json = %s", js)
	}
	p, _ = parseBoardArgs([]string{"tail", "--agent", "nobody"})
	if out, _ := renderBoardTailItems(tailFleet(), p); !strings.Contains(out, "the recorder answered: no messages from nobody") {
		t.Fatalf("empty = %q", out)
	}
}

func tailFleet() *board.QueryReply {
	st := board.New(0)
	root := func(pane, persona, text string, n int, ts int64) {
		p := msg.NewBoardPost(pane, persona, msg.BoardKindMilestone, text, ts)
		p.ThreadID = msg.MintThreadID(pane, n)
		st.Apply(p)
	}
	root("p314", "issue-314", "STATUS: green\nRUN: https://ci.example.com/"+strings.Repeat("z", 190), 1, 1_000)
	root("p31", "issue-31", "BLOCKED", 1, 2_000)
	r := board.Answer(st, board.Query{})
	return &r
}

func TestBoardCopyCLIWritesTheLocalClipboard(t *testing.T) {
	var clip []string
	p, _ := parseBoardArgs([]string{"copy", "issue-314", "1"})
	out, err := boardItemsRun(tailFleet(), p, func(s string) error { clip = append(clip, s); return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := board.BuildCopy(tailFleet(), "issue-314", 1)
	if len(clip) != 1 || clip[0] != want.Text {
		t.Fatalf("clipboard %q, want the shared text", clip)
	}
	if out != "[board] copied 1 of 1 messages from issue-314 to the clipboard" {
		t.Fatalf("confirmation = %q", out)
	}
	p, _ = parseBoardArgs([]string{"copy", "--print"})
	if out, _ := boardItemsRun(tailFleet(), p, func(string) error { return nil }, nil); !strings.HasPrefix(out, "● issue-314") {
		t.Fatalf("--print printed %q", out)
	}
	p, _ = parseBoardArgs([]string{"copy", "nobody"})
	if _, err := boardItemsRun(tailFleet(), p, func(string) error { t.Fatal("copied"); return nil }, nil); err == nil ||
		err.Error() != "[board] no messages from nobody" {
		t.Fatalf("no match = %v", err)
	}
	p, _ = parseBoardArgs([]string{"copy"})
	if _, err := boardItemsRun(tailFleet(), p, func(string) error { return errors.New("no pbcopy") }, nil); err == nil {
		t.Fatal("a failed clipboard write was reported as success")
	}
}

func TestBoardOutputCLI(t *testing.T) {
	var got board.OutputRequest
	p, _ := parseBoardArgs([]string{"output", "html", "--board", "etl-fix", "--no-open", "--out", "/o"})
	out, err := boardItemsRun(tailFleet(), p, nil, func(r board.OutputRequest) (string, error) { got = r; return "/o/x.html", nil })
	if err != nil {
		t.Fatal(err)
	}
	if got.Page.Board != "etl-fix" || got.Dir != "/o" || got.Open || len(got.Page.Picked) != 2 {
		t.Fatalf("request = %+v", got)
	}
	if out != "[board] wrote 2 of 2 messages → /o/x.html" {
		t.Fatalf("confirmation = %q", out)
	}
}

// writeSessionRecord puts a session record where the registry keeps it.
func writeSessionRecord(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(`{"name":"`+name+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// TestBoardSessionResolvesFromUnderTheWorkspace: FOUND LIVE — from
// worktrees/<name>, `rysh board post|tail` failed with `session "datalake" not
// found` although $RYSH_SESSION was set. A subdirectory and a git worktree
// under the workspace must both find the workspace's session.
func TestBoardSessionResolvesFromUnderTheWorkspace(t *testing.T) {
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(ws, ".rysh", "sessions")
	writeSessionRecord(t, sessions, "datalake")
	t.Setenv("RYSH_DIR", "")
	t.Setenv("RYSH_SESSION_DIR", "")
	t.Setenv("RYSH_SESSION", "datalake")

	sub := filepath.Join(ws, "src", "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	dirs := []string{sub}
	if _, err := exec.LookPath("git"); err == nil {
		git := func(args ...string) {
			cmd := exec.Command("git", append([]string{"-C", ws}, args...)...)
			cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		git("init", "-q")
		git("commit", "-q", "--allow-empty", "-m", "init")
		git("worktree", "add", "-q", filepath.Join(ws, "worktrees", "issue-314"), "-b", "issue-314")
		dirs = append(dirs, filepath.Join(ws, "worktrees", "issue-314"))
	}

	for _, dir := range dirs {
		chdir(t, dir)
		cfg := config.Load() // cwd-derived: <dir>/.rysh/sessions, which is empty
		store, name, err := resolveBoardSession(cfg, "")
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if name != "datalake" {
			t.Fatalf("%s: session name = %q, want $RYSH_SESSION", dir, name)
		}
		if _, err := store.Get(name); err != nil {
			t.Fatalf("%s: the workspace's session was not found: %v", dir, err)
		}
		if _, err := os.Stat(filepath.Join(dir, ".rysh")); err == nil {
			t.Fatalf("%s: resolving littered the directory with a .rysh", dir)
		}
	}
}

// TestBoardSessionHonoursAnExplicitRegistry: RYSH_SESSION_DIR is obeyed, not
// walked past.
func TestBoardSessionHonoursAnExplicitRegistry(t *testing.T) {
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	writeSessionRecord(t, filepath.Join(ws, ".rysh", "sessions"), "datalake")
	explicit := filepath.Join(t.TempDir(), "elsewhere")
	sub := filepath.Join(ws, "deep")
	_ = os.MkdirAll(sub, 0o755)
	chdir(t, sub)
	t.Setenv("RYSH_SESSION_DIR", explicit)
	t.Setenv("RYSH_SESSION", "datalake")

	store, _, err := resolveBoardSession(config.Load(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("datalake"); err == nil {
		t.Fatal("an explicit RYSH_SESSION_DIR was walked past")
	}
}
