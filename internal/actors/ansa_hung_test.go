// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Tests for `rysh ansa hung` / `##ansa hung` (D-67 (a), founder 2026-11-01).
//
// The properties under test, in the order they matter:
//  1. the check reads the transcript's FILE TIME only, never its content: it
//     still gives a verdict when the transcript cannot be opened at all;
//  2. a claude.session_id that is not a session id never becomes a path;
//  3. HUNG needs BOTH signals: the working marker on screen and a transcript
//     that has not changed for N minutes. Either one alone is not HUNG;
//  4. a missing input is UNKNOWN, never a guess either way.

const (
	hungSID1 = "0f0f0f0f-1111-2222-3333-444444444444"
	hungSID2 = "1e1e1e1e-5555-6666-7777-888888888888"
)

var (
	hungWorking = []string{"✻ Thinking… (12s · esc to interrupt)", "> "}
	hungIdle    = []string{"> ", "  ⏵⏵ bypass permissions on"}
)

// hungFake serves one screen per pane.
type hungFake struct {
	*fakeAnsaTransport
	mu        sync.Mutex
	screenBy  map[string][]string
	screenErr map[string]error
}

func newHungFake(panes []ansaPane) *hungFake {
	return &hungFake{
		fakeAnsaTransport: &fakeAnsaTransport{panes: panes},
		screenBy:          map[string][]string{},
		screenErr:         map[string]error{},
	}
}

func (f *hungFake) Screen(paneID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.screenErr[paneID]; err != nil {
		return nil, err
	}
	return f.screenBy[paneID], nil
}

func hungPane(id, name, sid string) ansaPane {
	meta := map[string]string{}
	if sid != "" {
		meta[metaClaudeSessionID] = sid
	}
	return ansaPane{ID: id, GivenName: name, Meta: meta}
}

// writeTranscript creates <home>/projects/<project>/<sid>.jsonl with the given
// content and modification time, and returns its path.
func writeTranscript(t *testing.T, home, project, sid, content string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(home, "projects", project)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return p
}

var hungNow = time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC)

func hungEnv(home string) ansaHungEnv {
	return ansaHungEnv{ClaudeHome: home, ClaudeHomeLabel: "~/.claude", Now: func() time.Time { return hungNow }}
}

func oneRow(t *testing.T, tr ansaTransport, req ansaHungRequest, env ansaHungEnv) ansaHungRow {
	t.Helper()
	rows, res := ansaHungCheck(tr, req, env)
	if res != nil {
		t.Fatalf("refused: [%s] %s", res.Code, res.Error)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	return rows[0]
}

func TestAnsaHungFlagsAWorkingScreenOverAStillTranscript(t *testing.T) {
	home := t.TempDir()
	writeTranscript(t, home, "-proj", hungSID1, "{}\n", hungNow.Add(-15*time.Minute))
	f := newHungFake([]ansaPane{hungPane("p1", "worker", hungSID1)})
	f.screenBy["p1"] = hungWorking

	row := oneRow(t, f, ansaHungRequest{To: "@worker", Minutes: 10}, hungEnv(home))
	if row.Verdict != ansaHungHung {
		t.Fatalf("verdict %s, want %s: %+v", row.Verdict, ansaHungHung, row)
	}
	if row.Age != 15*time.Minute || row.PaneID != "p1" || row.SessionID != hungSID1 {
		t.Fatalf("row = %+v", row)
	}
}

func TestAnsaHungIsBusyWhileTheTranscriptMoves(t *testing.T) {
	home := t.TempDir()
	writeTranscript(t, home, "-proj", hungSID1, "{}\n", hungNow.Add(-30*time.Second))
	f := newHungFake([]ansaPane{hungPane("p1", "worker", hungSID1)})
	f.screenBy["p1"] = hungWorking

	row := oneRow(t, f, ansaHungRequest{To: "p1", Minutes: 10}, hungEnv(home))
	if row.Verdict != ansaHungBusy {
		t.Fatalf("verdict %s, want %s", row.Verdict, ansaHungBusy)
	}
}

// No marker on screen is never HUNG, however old the transcript: an idle
// claude's transcript stops moving too. The verdict does not say "idle"
// either, because a narrow pane hides the marker (ansaScreenIsWorking).
func TestAnsaHungNoMarkerIsNeverHung(t *testing.T) {
	home := t.TempDir()
	writeTranscript(t, home, "-proj", hungSID1, "{}\n", hungNow.Add(-5*time.Hour))
	f := newHungFake([]ansaPane{hungPane("p1", "worker", hungSID1)})
	f.screenBy["p1"] = hungIdle

	row := oneRow(t, f, ansaHungRequest{To: "p1"}, hungEnv(home))
	if row.Verdict != ansaHungNoMarker {
		t.Fatalf("verdict %s, want %s", row.Verdict, ansaHungNoMarker)
	}
}

// D-67 (a): file times only, never content. The transcript is made unreadable
// (mode 000) and carries a marker string; the check must still reach HUNG
// (so it never needed to open the file) and the marker must appear nowhere.
func TestAnsaHungNeverOpensTheTranscript(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can open a mode-000 file, so this proof needs a normal user")
	}
	home := t.TempDir()
	const secret = "TRANSCRIPT-CONTENT-MUST-NOT-BE-READ"
	p := writeTranscript(t, home, "-proj", hungSID1, `{"type":"user","text":"`+secret+`"}`+"\n",
		hungNow.Add(-20*time.Minute))
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	if fh, err := os.Open(p); err == nil {
		fh.Close()
		t.Skip("this filesystem lets the owner open a mode-000 file; the proof would be vacuous")
	}

	f := newHungFake([]ansaPane{hungPane("p1", "worker", hungSID1)})
	f.screenBy["p1"] = hungWorking
	rows, res := ansaHungCheck(f, ansaHungRequest{To: "p1", Minutes: 10}, hungEnv(home))
	if res != nil {
		t.Fatalf("refused: %s", res.Error)
	}
	if len(rows) != 1 || rows[0].Verdict != ansaHungHung {
		t.Fatalf("rows = %+v, want one HUNG row (reached from the file time alone)", rows)
	}
	if out := ansaHungReport(rows, 10); strings.Contains(out, secret) {
		t.Fatalf("report carries transcript content:\n%s", out)
	}
}

// A claude.session_id is pane meta: anything that can reach the bus can set
// it. It is used as a file name only when it has the exact shape of a session
// id, so "../../x" cannot point the stat anywhere else.
func TestAnsaHungNeverTurnsAMetaValueIntoAPath(t *testing.T) {
	home := t.TempDir()
	// home/evil.jsonl is what projects/<dir>/../../evil.jsonl would resolve to.
	evil := filepath.Join(home, "evil.jsonl")
	if err := os.WriteFile(evil, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := hungNow.Add(-time.Hour)
	_ = os.Chtimes(evil, old, old)
	writeTranscript(t, home, "-proj", hungSID1, "{}\n", old)

	for _, sid := range []string{"../../evil", "*", hungSID1 + "/../" + hungSID1, " ", "evil"} {
		f := newHungFake([]ansaPane{hungPane("p1", "worker", sid)})
		f.screenBy["p1"] = hungWorking
		row := oneRow(t, f, ansaHungRequest{To: "p1"}, hungEnv(home))
		if row.Verdict != ansaHungUnknown {
			t.Errorf("claude.session_id %q: verdict %s, want %s (%+v)", sid, row.Verdict, ansaHungUnknown, row)
		}
	}
}

func TestAnsaHungMissingInputsAreUnknown(t *testing.T) {
	home := t.TempDir()
	writeTranscript(t, home, "-proj", hungSID1, "{}\n", hungNow.Add(-time.Hour))

	cases := []struct {
		name   string
		pane   ansaPane
		screen error
		want   string // word the note must carry
	}{
		{"no session meta", hungPane("p1", "w", ""), nil, "claude.session_id"},
		{"no transcript file", hungPane("p1", "w", hungSID2), nil, "no transcript"},
		{"screen unreadable", hungPane("p1", "w", hungSID1), errors.New("nats: timeout"), "screen"},
	}
	for _, tc := range cases {
		f := newHungFake([]ansaPane{tc.pane})
		f.screenBy["p1"] = hungWorking
		if tc.screen != nil {
			f.screenErr["p1"] = tc.screen
		}
		row := oneRow(t, f, ansaHungRequest{To: "p1"}, hungEnv(home))
		if row.Verdict != ansaHungUnknown || !strings.Contains(row.Note, tc.want) {
			t.Errorf("%s: verdict %s note %q, want %s with %q", tc.name, row.Verdict, row.Note, ansaHungUnknown, tc.want)
		}
	}
}

// The newest file wins when one session id has a transcript in two project
// folders (a resume from another directory).
func TestAnsaHungUsesTheNewestTranscriptOfTheSession(t *testing.T) {
	home := t.TempDir()
	writeTranscript(t, home, "-old", hungSID1, "{}\n", hungNow.Add(-2*time.Hour))
	writeTranscript(t, home, "-new", hungSID1, "{}\n", hungNow.Add(-time.Minute))
	f := newHungFake([]ansaPane{hungPane("p1", "w", hungSID1)})
	f.screenBy["p1"] = hungWorking
	row := oneRow(t, f, ansaHungRequest{To: "p1", Minutes: 10}, hungEnv(home))
	if row.Verdict != ansaHungBusy || row.Age != time.Minute {
		t.Fatalf("row = %+v, want BUSY at 1m (the newer file)", row)
	}
}

func TestAnsaHungAllChecksEveryClaudePaneOnly(t *testing.T) {
	home := t.TempDir()
	writeTranscript(t, home, "-proj", hungSID1, "{}\n", hungNow.Add(-time.Hour))
	writeTranscript(t, home, "-proj", hungSID2, "{}\n", hungNow.Add(-10*time.Second))
	f := newHungFake([]ansaPane{
		hungPane("p1", "stuck", hungSID1),
		hungPane("p2", "shell", ""),
		hungPane("p3", "fine", hungSID2),
	})
	f.screenBy["p1"] = hungWorking
	f.screenBy["p3"] = hungWorking

	rows, res := ansaHungCheck(f, ansaHungRequest{All: true}, hungEnv(home))
	if res != nil {
		t.Fatalf("refused: %s", res.Error)
	}
	if len(rows) != 2 || rows[0].PaneID != "p1" || rows[1].PaneID != "p3" {
		t.Fatalf("rows = %+v, want p1 and p3 in pane order (p2 has no claude.session_id)", rows)
	}
	if rows[0].Verdict != ansaHungHung || rows[1].Verdict != ansaHungBusy {
		t.Fatalf("verdicts %s/%s, want HUNG/BUSY", rows[0].Verdict, rows[1].Verdict)
	}
}

func TestAnsaHungTargetRules(t *testing.T) {
	home := t.TempDir()
	f := newHungFake([]ansaPane{hungPane("p1", "w", hungSID1), hungPane("p2", "dup", ""), hungPane("p3", "dup", "")})
	cases := []struct {
		req  ansaHungRequest
		code string
	}{
		{ansaHungRequest{}, msg.AnsaErrNoTarget},
		{ansaHungRequest{To: "p1", All: true}, msg.AnsaErrNoTarget},
		{ansaHungRequest{To: "p1", ClaudeSession: hungSID1}, msg.AnsaErrNoTarget},
		{ansaHungRequest{To: "nope"}, msg.AnsaErrUnknownTarget},
		{ansaHungRequest{To: "@dup"}, msg.AnsaErrAmbiguousTarget},
		{ansaHungRequest{ClaudeSession: hungSID2}, msg.AnsaErrUnknownTarget},
		{ansaHungRequest{To: "p1", Minutes: -1}, msg.AnsaErrBadMinutes},
		{ansaHungRequest{To: "p1", Minutes: ansaHungMaxMinutes + 1}, msg.AnsaErrBadMinutes},
	}
	for _, tc := range cases {
		_, res := ansaHungCheck(f, tc.req, hungEnv(home))
		if res == nil || res.Code != tc.code {
			t.Errorf("%+v: got %+v, want refusal %s", tc.req, res, tc.code)
		}
	}
	f.panesErr = errors.New("tab did not answer")
	if _, res := ansaHungCheck(f, ansaHungRequest{All: true}, hungEnv(home)); res == nil || res.Code != msg.AnsaErrDirectory {
		t.Errorf("panes error: got %+v, want %s", res, msg.AnsaErrDirectory)
	}
}

func TestAnsaHungReport(t *testing.T) {
	rows := []ansaHungRow{
		{PaneID: "p1", Persona: "stuck", SessionID: hungSID1, Verdict: ansaHungHung, Working: true, Found: true, Age: 14 * time.Minute},
		{PaneID: "p3", Persona: "fine", SessionID: hungSID2, Verdict: ansaHungBusy, Working: true, Found: true, Age: 20 * time.Second},
		{PaneID: "p4", Persona: "quiet", SessionID: hungSID2, Verdict: ansaHungNoMarker, Found: true, Age: 3*time.Hour + 5*time.Minute},
		{PaneID: "p5", Persona: "odd", Verdict: ansaHungUnknown, Note: "no claude.session_id meta on this pane"},
	}
	out := ansaHungReport(rows, 10)
	for _, want := range []string{
		"4 claude panes checked: 1 hung, 1 busy, 1 no marker, 1 unknown",
		"HUNG       stuck (pane p1): working marker on screen; transcript unchanged for 14m",
		"BUSY       fine (pane p3): working marker on screen; transcript written 20s ago",
		"NO-MARKER  quiet (pane p4): no working marker on screen; transcript written 3h05m ago",
		"UNKNOWN    odd (pane p5): no claude.session_id meta on this pane",
		"never transcript content",
		"rysh ansa keys <pane-id> -- esc",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if got := ansaHungReport(nil, 10); !strings.Contains(got, "no pane in this session carries claude.session_id") {
		t.Errorf("empty report:\n%s", got)
	}
}

func TestParseAnsaHungArgs(t *testing.T) {
	ok := []struct {
		args []string
		want ansaHungRequest
	}{
		{[]string{"@w"}, ansaHungRequest{To: "@w"}},
		{[]string{"p1", "--minutes", "5"}, ansaHungRequest{To: "p1", Minutes: 5}},
		{[]string{"--all", "--minutes=3"}, ansaHungRequest{All: true, Minutes: 3}},
		{[]string{"--claude-session", hungSID1}, ansaHungRequest{ClaudeSession: hungSID1}},
	}
	for _, tc := range ok {
		got, err := parseAnsaHungArgs(tc.args)
		if err != nil || got != tc.want {
			t.Errorf("parseAnsaHungArgs(%q) = %+v, %v; want %+v", tc.args, got, err, tc.want)
		}
	}
	for _, args := range [][]string{
		{"p1", "--minutes", "abc"},
		{"p1", "--minutes"},
		{"p1", "p2"},
		{"--fleet", "x"},
		{"--claude-session"},
	} {
		if _, err := parseAnsaHungArgs(args); err == nil {
			t.Errorf("parseAnsaHungArgs(%q) succeeded, want an error", args)
		}
	}
}

// LEAD note N1: a note never prints the absolute claude home (it carries the
// username); it names the folder by its label.
func TestAnsaHungNotesNeverPrintTheAbsoluteHome(t *testing.T) {
	home := t.TempDir()
	f := newHungFake([]ansaPane{hungPane("p1", "w", hungSID1)})
	f.screenBy["p1"] = hungWorking

	// No transcript: the note names the folder.
	row := oneRow(t, f, ansaHungRequest{To: "p1"}, hungEnv(home))
	if strings.Contains(row.Note, home) || !strings.Contains(row.Note, "~/.claude/projects") {
		t.Errorf("no-transcript note %q: want ~/.claude/projects, never %s", row.Note, home)
	}

	// projects/ is a file, so listing it fails: the error must not carry the path.
	if err := os.WriteFile(filepath.Join(home, "projects"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	row = oneRow(t, f, ansaHungRequest{To: "p1"}, hungEnv(home))
	if row.Verdict != ansaHungUnknown || strings.Contains(row.Note, home) {
		t.Errorf("listing-error note %q (verdict %s): must be UNKNOWN and never print %s", row.Note, row.Verdict, home)
	}
	if out := ansaHungReport([]ansaHungRow{row}, 10); strings.Contains(out, home) {
		t.Errorf("report prints the absolute home:\n%s", out)
	}
}

func TestAnsaClaudeHomeLabel(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if _, label := ansaClaudeHome(); label != "~/.claude" {
		t.Errorf("label %q, want ~/.claude", label)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if dir, label := ansaClaudeHome(); label != "$CLAUDE_CONFIG_DIR" || dir != os.Getenv("CLAUDE_CONFIG_DIR") {
		t.Errorf("got %q / %q, want the env dir labelled $CLAUDE_CONFIG_DIR", dir, label)
	}
}
