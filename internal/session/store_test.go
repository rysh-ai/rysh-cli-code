// SPDX-License-Identifier: Apache-2.0

package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// newTestStore creates a Store backed by a temporary session directory.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	sessionDir := t.TempDir()
	cfg := config.Config{SessionDir: sessionDir}
	st, err := NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return st
}

// writeRecord writes a session Record JSON directly to the store directory.
func writeRecord(t *testing.T, st *Store, rec Record) {
	t.Helper()
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	if err := os.WriteFile(st.pathFor(rec.Name), data, 0o644); err != nil {
		t.Fatalf("write record: %v", err)
	}
}

// Delete clears the session's own shell history so a session recreated under the
// same name starts clean, and leaves the workspace's own files alone. (What must
// survive is pinned in detail by TestDeleteRemovesOnlyTheSessionsOwnArtifacts.)
func TestDelete_CleansUpItsOwnHistoryOnly(t *testing.T) {
	st := newTestStore(t)

	// Create a temporary working directory to simulate where the session ran.
	workDir := t.TempDir()

	ryshDir := filepath.Join(workDir, ".rysh")
	pipelinesDir := filepath.Join(ryshDir, "pipelines")
	if err := os.MkdirAll(pipelinesDir, 0o755); err != nil {
		t.Fatalf("MkdirAll .rysh/pipelines: %v", err)
	}
	historyFile := filepath.Join(ryshDir, "history", "test-session.history")
	if err := os.MkdirAll(filepath.Dir(historyFile), 0o755); err != nil {
		t.Fatalf("MkdirAll .rysh/history: %v", err)
	}
	pipelineFile := filepath.Join(pipelinesDir, "ci.build.yaml")
	for _, name := range []string{historyFile, pipelineFile} {
		if err := os.WriteFile(name, []byte("test"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	notesFile := filepath.Join(workDir, ".rysh-notes.md")
	if err := os.WriteFile(notesFile, []byte("# notes"), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	// Write a session record pointing at the working directory.
	writeRecord(t, st, Record{Name: "test-session", Path: workDir})

	// Delete the session.
	if err := st.Delete("test-session"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := os.Stat(historyFile); !os.IsNotExist(err) {
		t.Errorf("the session's shell history should be removed, err=%v", err)
	}
	// Authored pipeline definitions and shared notes are the workspace's, not
	// this session's — deleting a session must not take them with it.
	for _, p := range []string{ryshDir, pipelineFile, notesFile} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s should survive Delete: %v", p, err)
		}
	}
}

func TestDelete_NoWorkingDirFiles(t *testing.T) {
	st := newTestStore(t)

	// Working directory exists but contains none of the session artifacts.
	workDir := t.TempDir()
	writeRecord(t, st, Record{Name: "clean-session", Path: workDir})

	// Delete should succeed without errors.
	if err := st.Delete("clean-session"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestDelete_EmptyPath(t *testing.T) {
	st := newTestStore(t)

	// Session record with an empty Path field -- nothing to clean up.
	writeRecord(t, st, Record{Name: "no-path-session", Path: ""})

	if err := st.Delete("no-path-session"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestDelete_NonexistentSession(t *testing.T) {
	st := newTestStore(t)

	// Deleting a session that does not exist should be a no-op.
	if err := st.Delete("ghost"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

// TestUpsertGet_PreservesSource verifies the front-end provenance tag survives
// a JSON round-trip through Upsert and Get, and through List. This is what lets
// the desktop app label a session's origin in its picker and lets EnsureCanOpen
// work out which render surfaces to warn about.
func TestUpsertGet_PreservesSource(t *testing.T) {
	st := newTestStore(t)

	if _, err := st.Upsert(Record{Name: "app-session", State: "stopped", Source: "app"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := st.Get("app-session")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Source != "app" {
		t.Errorf("Source after Get = %q, want %q", got.Source, "app")
	}

	records, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 1 || records[0].Source != "app" {
		t.Errorf("Source after List = %+v, want one record with Source=app", records)
	}
}

// TestEnsureCanOpen covers the open decision table. Nothing is refused — both
// front-ends drive the same daemon — so what the table pins is which
// combinations produce DEGRADATION NOTES, and specifically that the asymmetry
// runs one way: the desktop app is a superset of the terminal, so it opens a
// terminal session with nothing lost, while the terminal opens an app session
// with the app-only render surfaces called out.
func TestEnsureCanOpen(t *testing.T) {
	cases := []struct {
		name      string
		recSource string
		want      string
		wantNotes bool
	}{
		{"same-app", "app", "app", false},
		{"same-cli", "cli", "cli", false},
		{"cli-opens-app", "app", "cli", true},
		{"app-opens-cli", "cli", "app", false},
		{"legacy-blank-cli", "", "cli", false},
		{"legacy-blank-app", "", "app", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := Record{Name: tc.name, Source: tc.recSource}
			notes, err := EnsureCanOpen(rec, tc.want)
			if err != nil {
				t.Fatalf("EnsureCanOpen(%q, %q) = %v, want no error — no front-end pair is refused",
					tc.recSource, tc.want, err)
			}
			if tc.wantNotes && len(notes) == 0 {
				t.Errorf("EnsureCanOpen(%q, %q) returned no notes, want degradations",
					tc.recSource, tc.want)
			}
			if !tc.wantNotes && len(notes) > 0 {
				t.Errorf("EnsureCanOpen(%q, %q) returned notes %v, want none",
					tc.recSource, tc.want, notes)
			}
		})
	}
}

// TestEnsureCanOpenEmailParity guards a real asymmetry inside the asymmetry:
// the terminal has its own three-column email client (internal/tui/email_view.go)
// speaking the same NATS request/reply as the app's, so opening an app session
// must NOT claim email panes are degraded. Only the surfaces the terminal truly
// cannot paint belong in the notes.
func TestEnsureCanOpenEmailParity(t *testing.T) {
	notes, err := EnsureCanOpen(Record{Name: "app-session", Source: SourceApp}, SourceCLI)
	if err != nil {
		t.Fatalf("EnsureCanOpen: %v", err)
	}
	for _, n := range notes {
		if strings.Contains(n, "email") {
			t.Errorf("note %q claims email degrades, but the TUI has a full email client", n)
		}
	}
	if len(notes) == 0 {
		t.Fatal("expected at least the web-pane degradation")
	}
}

// TestDegradationSummary checks the rendered block names the creating front-end
// and lists every note, since this is what the user actually reads.
func TestDegradationSummary(t *testing.T) {
	rec := Record{Name: "designs", Source: SourceApp}
	notes, err := EnsureCanOpen(rec, SourceCLI)
	if err != nil {
		t.Fatalf("EnsureCanOpen: %v", err)
	}
	summary := DegradationSummary(rec, notes)
	if !strings.Contains(summary, "designs") || !strings.Contains(summary, "rysh desktop app") {
		t.Errorf("summary = %q; want it to name the session and its creator", summary)
	}
	for _, n := range notes {
		if !strings.Contains(summary, n) {
			t.Errorf("summary dropped note %q", n)
		}
	}
	if got := DegradationSummary(rec, nil); got != "" {
		t.Errorf("DegradationSummary with no notes = %q, want \"\"", got)
	}
}
