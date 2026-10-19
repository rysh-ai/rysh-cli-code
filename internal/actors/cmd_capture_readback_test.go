// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// These two fixes answer one incident. An agent ran `##cmd --capture`, then
// polled the result the only way rysh offered — by broadcasting
// `cat <capture file>` back across the same scope. The panes running claude
// took the shell command as a prompt, never produced the sentinel, and the
// poller retried: 64 pane-writes in two minutes, shredding what the operator
// was typing at the time.
//
// Fix 1: reading a capture is a daemon-side read that touches no pane.
// Fix 2: --capture only ever targets panes at a shell, and says when that
//        dropped something.

// writeCapture drops a capture file straight into the directory the reader
// looks in, which is the contract --capture writes to.
func writeCapture(t *testing.T, baseDir, paneID, content string) {
	t.Helper()
	dir := captureDir(baseDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, paneID+".out"), []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func captureTestWorkspace(t *testing.T) (*WorkspaceActor, string) {
	t.Helper()
	root := t.TempDir()
	// pub is deliberately nil. handleCmdCapture must not reach a publisher at
	// all — that is the fix — so a read that tried to send would panic here
	// rather than quietly going back to typing into panes.
	return &WorkspaceActor{cfg: config.Config{WorkingDirectory: root}}, root
}

// ---------------------------------------------------------------------------
// Fix 1 — the read-back
// ---------------------------------------------------------------------------

// TestParseCapture covers the sentinel's two real shapes. `echo` supplies its
// own newline, but a command whose output does NOT end in one leaves the
// sentinel glued to the last line — and a reader that only understands the
// tidy case calls a finished run "still running" forever.
func TestParseCapture(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wBody   string
		wDone   bool
		wStatus int
	}{
		{"own line", "hello\n" + captureSentinel + "0\n", "hello\n", true, 0},
		{"no trailing newline before sentinel", "hello" + captureSentinel + "3\n", "hello", true, 3},
		{"no final newline at all", "hi\n" + captureSentinel + "7", "hi\n", true, 7},
		{"empty output, finished", captureSentinel + "0\n", "", true, 0},
		{"still running", "partial output\n", "partial output\n", false, 0},
		{"nonzero status", "boom\n" + captureSentinel + "127\n", "boom\n", true, 127},
		// The command's own output can contain the sentinel text; the real one
		// is still the last, so the scan must not stop at the first.
		{"sentinel echoed in output", "saw " + captureSentinel + "0 in a log\n" +
			captureSentinel + "0\n", "saw " + captureSentinel + "0 in a log\n", true, 0},
		// A half-written sentinel is not a finished run.
		{"garbled status", "x\n" + captureSentinel + "notanumber\n", "x\n" + captureSentinel + "notanumber\n", false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, done, status := parseCapture(c.content)
			if body != c.wBody || done != c.wDone || status != c.wStatus {
				t.Errorf("parseCapture(%q) = (%q, %v, %d), want (%q, %v, %d)",
					c.content, body, done, status, c.wBody, c.wDone, c.wStatus)
			}
		})
	}
}

// TestReadCaptureTruncatesToTheTail pins the cap. A captured `make test` is
// megabytes and the reader is usually an LLM; printing all of it is how a fix
// for a flooding bug becomes a flooding bug.
func TestReadCaptureTruncatesToTheTail(t *testing.T) {
	_, root := captureTestWorkspace(t)
	body := strings.Repeat("a", captureReadLimit) + "THE-END\n"
	writeCapture(t, root, "p1", body+captureSentinel+"0\n")

	c, err := readCapture(capturePath(root, "p1"), "p1")
	if err != nil {
		t.Fatalf("readCapture: %v", err)
	}
	if c.truncated == 0 {
		t.Fatalf("a file over the limit reported no truncation")
	}
	if len(c.body) > captureReadLimit {
		t.Errorf("body is %d bytes, over the %d limit", len(c.body), captureReadLimit)
	}
	if !strings.HasSuffix(c.body, "THE-END\n") {
		t.Errorf("kept the head, not the tail: a failure reports itself at the END")
	}
}

func TestResolveCapturePaneID(t *testing.T) {
	captures := []capture{
		{paneID: "276a479b-42fa-4fdb-8ca9-f2f88c78e2ec"},
		{paneID: "276a0000-0000-0000-0000-000000000000"},
		{paneID: "d51c54b9-ea6a-48b7-84b5-ffe259ca9f16"},
	}
	if got, err := resolveCapturePaneID(captures, "d51c54b9-ea6a-48b7-84b5-ffe259ca9f16"); err != nil || got != captures[2].paneID {
		t.Errorf("exact id = (%q, %v)", got, err)
	}
	// A unique prefix resolves: the ids are UUIDs and every other rysh selector
	// takes a short form.
	if got, err := resolveCapturePaneID(captures, "d51c"); err != nil || got != captures[2].paneID {
		t.Errorf("unique prefix = (%q, %v)", got, err)
	}
	if _, err := resolveCapturePaneID(captures, "276a"); err == nil {
		t.Errorf("an ambiguous prefix must be refused, not silently resolved to one of them")
	}
	if _, err := resolveCapturePaneID(captures, "zzz"); err == nil {
		t.Errorf("an unknown pane must be an error")
	}
}

// TestCmdCaptureReadsWithoutTouchingAPane is the regression guard for the
// incident: the read-back path must produce the captured output with no
// publisher wired up at all.
func TestCmdCaptureReadsWithoutTouchingAPane(t *testing.T) {
	w, root := captureTestWorkspace(t)
	writeCapture(t, root, "pane-a", "ubuntu\nip-172-31-45-229\n"+captureSentinel+"0\n")

	var out strings.Builder
	w.handleCmdCapture(&out, []string{"pane-a"})
	got := out.String()

	for _, want := range []string{"ubuntu", "ip-172-31-45-229", "exit 0", "pane-a"} {
		if !strings.Contains(got, want) {
			t.Errorf("read-back missing %q:\n%s", want, got)
		}
	}
	if w.ryshFail != nil {
		t.Errorf("a successful read reported failure: %v", w.ryshFail)
	}
}

func TestCmdCaptureReportsStillRunning(t *testing.T) {
	w, root := captureTestWorkspace(t)
	writeCapture(t, root, "pane-a", "half a line")

	var out strings.Builder
	w.handleCmdCapture(&out, []string{"pane-a"})
	got := out.String()

	if !strings.Contains(got, "running") {
		t.Errorf("an unfinished capture must say so, not pass the partial file off as the result:\n%s", got)
	}
	// Still running is an answer, not a failure — the caller polls again.
	if w.ryshFail != nil {
		t.Errorf("still-running was reported as a command failure: %v", w.ryshFail)
	}
}

func TestCmdCaptureListsAndRefusesUnknown(t *testing.T) {
	w, root := captureTestWorkspace(t)
	writeCapture(t, root, "pane-a", "a\n"+captureSentinel+"0\n")
	writeCapture(t, root, "pane-b", "still going")

	var list strings.Builder
	w.handleCmdCapture(&list, nil)
	for _, want := range []string{"pane-a", "pane-b", "exit 0", "running"} {
		if !strings.Contains(list.String(), want) {
			t.Errorf("listing missing %q:\n%s", want, list.String())
		}
	}

	w2, _ := captureTestWorkspace(t)
	var empty strings.Builder
	w2.handleCmdCapture(&empty, nil)
	if w2.ryshFail == nil {
		t.Errorf("an empty capture dir must be a non-zero exit, so a script can tell")
	}
}

// TestCmdCaptureIsNotAScope proves the subcommand is reachable: `capture` was
// previously rejected by parseCmdArgs as an unknown scope.
func TestCmdCaptureIsNotAScope(t *testing.T) {
	if _, _, _, err := parseCmdArgs([]string{"capture", "pane-a"}); err == nil {
		t.Fatalf("parseCmdArgs accepted 'capture' as a scope; the interception in " +
			"handleCmdBroadcast is what makes the read-back reachable")
	}
}

// ---------------------------------------------------------------------------
// Fix 2 — --capture never types into an agent
// ---------------------------------------------------------------------------

// TestCaptureImpliesRunningShell is the fix for the broadcast half. A captured
// command is shell syntax; sent to a pane running claude it is a prompt, and
// no capture file is ever written.
func TestCaptureImpliesRunningShell(t *testing.T) {
	_, sel, _, err := parseCmdArgs([]string{"ws", "--capture", "pwd"})
	if err != nil {
		t.Fatalf("parseCmdArgs: %v", err)
	}
	if sel.running != "shell" || !sel.runningImplied {
		t.Errorf("--capture gave running=%q implied=%v; want shell/true — without this "+
			"a --capture fan-out types into every agent in the scope",
			sel.running, sel.runningImplied)
	}
}

// TestExplicitRunningBeatsTheImpliedOne keeps `--running claude --capture`
// expressible for anyone who means it.
func TestExplicitRunningBeatsTheImpliedOne(t *testing.T) {
	_, sel, _, err := parseCmdArgs([]string{"ws", "--running", "claude", "--capture", "pwd"})
	if err != nil {
		t.Fatalf("parseCmdArgs: %v", err)
	}
	if sel.running != "claude" || sel.runningImplied {
		t.Errorf("explicit --running was overridden: running=%q implied=%v", sel.running, sel.runningImplied)
	}
}

// TestUncapturedBroadcastIsUnfiltered: the implied filter is scoped to
// --capture. A plain `##cmd ws <command>` still reaches every pane, agents
// included, which is what fan-out to claudes depends on.
func TestUncapturedBroadcastIsUnfiltered(t *testing.T) {
	_, sel, _, err := parseCmdArgs([]string{"ws", "echo hi"})
	if err != nil {
		t.Fatalf("parseCmdArgs: %v", err)
	}
	if sel.running != "" || sel.runningImplied {
		t.Errorf("a plain broadcast gained a filter: running=%q implied=%v", sel.running, sel.runningImplied)
	}
}

// TestFilterPanesByProgramCountsWhatItDropped: the filter now runs without
// being asked for, so a pane missing from the fan-out has to be countable. A
// silent drop is the one outcome the caller cannot detect.
func TestFilterPanesByProgramCountsWhatItDropped(t *testing.T) {
	snap := &domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{{
		Lanes: []domain.LaneSnapshot{{PaneGroups: []domain.PaneGroupSnapshot{{Panes: []domain.PaneSnapshot{
			{ID: "a", Program: "claude"},
			{ID: "b"},
			{ID: "c", Program: "vim"},
		}}}}},
	}}}
	all := []string{"a", "b", "c"}

	if kept, dropped := filterPanesByProgram(snap, all, "shell"); len(kept) != 1 || dropped != 2 {
		t.Errorf("--running shell kept %v dropped %d; want 1 kept / 2 dropped", kept, dropped)
	}
	if kept, dropped := filterPanesByProgram(snap, all, ""); len(kept) != 3 || dropped != 0 {
		t.Errorf("no filter kept %v dropped %d; want everything", kept, dropped)
	}
}

// TestSkipNoteExplainsTheImpliedFilter: when --capture drops panes the caller
// never asked to drop, the summary has to say why.
func TestSkipNoteExplainsTheImpliedFilter(t *testing.T) {
	implied := cmdSelectors{capture: true, running: "shell", runningImplied: true}
	note := skipNote(cmdSkips{busy: 2}, implied)
	if !strings.Contains(note, "2 not at a shell") || !strings.Contains(note, "--capture") {
		t.Errorf("implied skip note = %q; it must name the count AND the rule the caller did not type", note)
	}

	explicit := cmdSelectors{running: "shell"}
	if note := skipNote(cmdSkips{busy: 2}, explicit); strings.Contains(note, "--capture") {
		t.Errorf("an explicitly asked-for filter should not be explained as --capture's doing: %q", note)
	}

	if note := skipNote(cmdSkips{}, implied); note != "" {
		t.Errorf("nothing skipped must produce no note, got %q", note)
	}

	both := skipNote(cmdSkips{shared: 1, pipeline: 2, busy: 3}, implied)
	for _, want := range []string{"1 shared", "2 pipeline", "3 not at a shell"} {
		if !strings.Contains(both, want) {
			t.Errorf("combined note %q is missing %q", both, want)
		}
	}
}

// TestCaptureCommandStillWritesTheSentinel guards the half of the contract the
// reader depends on, now that writer and reader live in one file.
func TestCaptureCommandStillWritesTheSentinel(t *testing.T) {
	got := captureCommand("/ws", "pane-1", "git status")
	if !strings.Contains(got, capturePath("/ws", "pane-1")) {
		t.Errorf("capture path missing from %q", got)
	}
	// Round-trip: what the writer emits is what the reader understands.
	body, done, status := parseCapture("out\n" + captureSentinel + "0\n")
	if !done || status != 0 || body != "out\n" {
		t.Errorf("writer and reader disagree on the sentinel: (%q, %v, %d)", body, done, status)
	}
	if !strings.Contains(got, fmt.Sprintf("%s$?", captureSentinel)) {
		t.Errorf("sentinel does not carry the exit status: %q", got)
	}
}
