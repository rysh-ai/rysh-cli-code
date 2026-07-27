package session

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// TestUpsertGet_PreservesSource verifies the front-end ownership tag survives a
// JSON round-trip through Upsert and Get, and through List. This is what lets
// the desktop app filter to its own "app" sessions and the open guards reject a
// session created by the other front-end.
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

// TestEnsureSourceMatch covers the open-guard decision table: same source is
// allowed, a mismatch is refused, and a blank (legacy) record source is
// compatible with either front-end.
func TestEnsureSourceMatch(t *testing.T) {
	cases := []struct {
		name      string
		recSource string
		want      string
		ok        bool
	}{
		{"same-app", "app", "app", true},
		{"same-cli", "cli", "cli", true},
		{"cli-opens-app", "app", "cli", false},
		{"app-opens-cli", "cli", "app", false},
		{"legacy-blank-cli", "", "cli", true},
		{"legacy-blank-app", "", "app", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := Record{Name: tc.name, Source: tc.recSource}
			err := EnsureSourceMatch(rec, tc.want)
			if tc.ok && err != nil {
				t.Errorf("ensureSourceMatch(%q, %q) = %v, want nil", tc.recSource, tc.want, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("ensureSourceMatch(%q, %q) = nil, want error", tc.recSource, tc.want)
			}
		})
	}
}
