// SPDX-License-Identifier: Apache-2.0

package main

// ORDER-82 (rr-msg-324, rr-msg-302 §1): `rysh board send --file` records the
// file in the board command (its File, and the default "Read … in full." text)
// RELATIVE TO THE WORKSPACE ROOT (the session record's Path), never as an
// absolute path; a file OUTSIDE the workspace is refused.
//
// Every generated record is checked for 0 "/Users/", 0 occurrences of the
// username and 0 of the real home directory. These are COUNTED here and never
// printed: a failure shows a label and counts only. rr-msg-239 controls: a
// needle the counter must find, and a non-empty record.

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// leakCounts -> ("/Users/", username, real home) occurrence counts in s.
func leakCounts(t *testing.T, s string) (int, int, int) {
	t.Helper()
	u, err := user.Current()
	if err != nil || u.Username == "" || len(u.HomeDir) < 2 {
		t.Fatalf("cannot read the current user (needed for the counts)")
	}
	return strings.Count(s, "/Users/"), strings.Count(s, u.Username), strings.Count(s, u.HomeDir)
}

func assertNoAbsolute(t *testing.T, label, s, ws string) {
	t.Helper()
	if strings.TrimSpace(s) == "" {
		t.Fatalf("%s: generated text is empty (vacuous)", label)
	}
	if a, b, c := leakCounts(t, s); a != 0 || b != 0 || c != 0 {
		t.Errorf("%s: '/Users/', username, real-home counts = %d, %d, %d (want 0)", label, a, b, c)
	}
	// the fake workspace holds no username, so its own absolute path is the
	// proxy that proves the path was made relative, not merely username-free
	for _, w := range []string{ws, evalOr(ws)} {
		if n := strings.Count(s, w); n != 0 {
			t.Errorf("%s: absolute workspace path count = %d (want 0)", label, n)
		}
	}
}

func evalOr(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// tempWorkspace: a fake workspace under $TMPDIR with one message body in a
// worktree's out/msgs, as fleetctl writes it.
func tempWorkspace(t *testing.T) (ws, rel, body string) {
	t.Helper()
	ws, err := os.MkdirTemp("", "e9-o82.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(ws) })
	if a, b, c := leakCounts(t, ws+evalOr(ws)); a != 0 || b != 0 || c != 0 {
		t.Fatalf("the fake workspace path must carry no username/home (counts %d, %d, %d)", a, b, c)
	}
	rel = filepath.Join("worktrees", "e9-o82-wt", "out", "msgs", "msg-0001.md")
	body = "# an order body\nline two\n"
	if err := os.MkdirAll(filepath.Dir(filepath.Join(ws, rel)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws, rel, body
}

func TestBoardSendFileNeedleControl(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	planted := "x /Users/" + u.Username + "/w " + u.HomeDir + "/z"
	if a, b, c := leakCounts(t, planted); a < 1 || b < 1 || c < 1 {
		t.Fatalf("needle: the counter missed a planted occurrence (%d, %d, %d)", a, b, c)
	}
}

func TestBoardSendFileAbsoluteInputIsRecordedRelative(t *testing.T) {
	ws, rel, _ := tempWorkspace(t)
	req := &msg.MsgCLIBoardPost{Kind: msg.BoardKindCmd, To: "w"}
	if err := attachSendFile(req, filepath.Join(ws, rel), ws); err != nil {
		t.Fatalf("attachSendFile: %v", err)
	}
	if req.File != rel {
		t.Errorf("File = %q, want the workspace-relative %q", req.File, rel)
	}
	if want := "Read " + rel + " in full."; req.Text != want {
		t.Errorf("default text = %q, want %q", req.Text, want)
	}
	j, _ := json.Marshal(req)
	assertNoAbsolute(t, "board record (json)", string(j), ws)
}

// fleetctl's form since ORDER-80: a workspace-relative --file, run with
// cwd = the workspace root, and its own text (kept as given).
func TestBoardSendFileRelativeInputFromTheWorkspaceRoot(t *testing.T) {
	ws, rel, _ := tempWorkspace(t)
	t.Chdir(ws)
	req := &msg.MsgCLIBoardPost{Kind: msg.BoardKindCmd, To: "w", Text: "[FLEET f | WORK ORDER | msg-0001] Read " + rel + " in full."}
	text := req.Text
	if err := attachSendFile(req, rel, ws); err != nil {
		t.Fatalf("attachSendFile: %v", err)
	}
	if req.File != rel || req.Text != text {
		t.Errorf("File = %q (want %q); caller text changed: %v", req.File, rel, req.Text != text)
	}
	j, _ := json.Marshal(req)
	assertNoAbsolute(t, "board record from a relative --file", string(j), ws)
}

func TestBoardSendFileRelativePathResolvesFromTheWorkspaceRoot(t *testing.T) {
	ws, rel, body := tempWorkspace(t)
	req := &msg.MsgCLIBoardPost{Kind: msg.BoardKindCmd, To: "w"}
	if err := attachSendFile(req, filepath.Join(ws, rel), ws); err != nil {
		t.Fatalf("attachSendFile: %v", err)
	}
	if filepath.IsAbs(req.File) {
		t.Fatalf("File is absolute")
	}
	got, err := os.ReadFile(filepath.Join(ws, req.File))
	if err != nil || string(got) != body {
		t.Fatalf("the recorded path does not resolve from the workspace root to the body")
	}
}

// RED at the base: the file was recorded as an absolute path, whatever it was.
func TestBoardSendFileOutsideTheWorkspaceIsRefused(t *testing.T) {
	ws, _, _ := tempWorkspace(t)
	out, err := os.MkdirTemp("", "e9-o82-out.")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(out) })
	f := filepath.Join(out, "order.md")
	if err := os.WriteFile(f, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := &msg.MsgCLIBoardPost{Kind: msg.BoardKindCmd, To: "w"}
	err = attachSendFile(req, f, ws)
	if err == nil {
		t.Fatalf("a --file outside the workspace was accepted (File recorded: %v)", req.File != "")
	}
	for _, want := range []string{"rr-msg-302", "outside the workspace"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q", want)
		}
	}
	if strings.Contains(err.Error(), out) || strings.Contains(err.Error(), evalOr(out)) {
		t.Errorf("the refusal prints the absolute path (it must name the base name only)")
	}
	if req.File != "" || req.Text != "" {
		t.Errorf("a refused file still set File/Text")
	}
}

// No workspace (the session record could not be read): refuse, never fall back
// to an absolute path.
func TestBoardSendFileWithoutAKnownWorkspaceIsRefused(t *testing.T) {
	ws, rel, _ := tempWorkspace(t)
	req := &msg.MsgCLIBoardPost{Kind: msg.BoardKindCmd, To: "w"}
	if err := attachSendFile(req, filepath.Join(ws, rel), ""); err == nil {
		t.Fatalf("accepted with no workspace root (File recorded: %v)", req.File != "")
	}
	if req.File != "" {
		t.Errorf("File set although the workspace is unknown")
	}
}
