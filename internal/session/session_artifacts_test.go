package session

import (
	"os"
	"path/filepath"
	"testing"
)

// workspaceState is the .rysh content that belongs to the workspace or the user
// rather than to any one session. None of it may be removed when a session is
// deleted.
func workspaceState(t *testing.T, work string) []string {
	t.Helper()
	rysh := filepath.Join(work, ".rysh")
	paths := []string{
		filepath.Join(rysh, "secrets", "default", "ANTHROPIC_API_KEY"),
		filepath.Join(rysh, "humanoids", "whatsapp-assistant", "SKILL.md"),
		filepath.Join(rysh, "channel-state", "whatsapp", "1018266588047184.json"),
		filepath.Join(rysh, "channel-state", "whatsapp-relay", "conn-1.json"),
		filepath.Join(rysh, "agents", "reviewer", "SKILL.md"),
		filepath.Join(rysh, "pipelines", "ci.build.yaml"),
		filepath.Join(rysh, "policy.yaml"),
		filepath.Join(rysh, "mcp.json"),
		filepath.Join(rysh, "rysh.lock"),
		filepath.Join(rysh, "forge", "spec.json"),
		filepath.Join(rysh, "nats", "jetstream", "$G", "streams", "KV_rysh-panes-other", "meta.inf"),
		filepath.Join(rysh, "sessions", "other-session.json"),
		filepath.Join(rysh, "browser-instances", "work", "cookies"),
		filepath.Join(rysh, "history", "other-session.history"),
		filepath.Join(work, ".rysh-notes.md"),
	}
	for _, p := range paths {
		write(t, p, "keep me")
	}
	return paths
}

func assertAllExist(t *testing.T, paths []string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("deleting a session destroyed workspace state: %s (%v)", p, err)
		}
	}
}

// The regression: Delete removed every entry under .rysh except
// browser-instances, so deleting one session wiped the workspace's secrets,
// humanoid artefacts, channel state, installed packages, the shared NATS data
// directory, and the records of every other session rooted there.
func TestDeleteRemovesOnlyTheSessionsOwnArtifacts(t *testing.T) {
	st := newTestStore(t)
	work := t.TempDir()
	keep := workspaceState(t, work)

	ownHistory := filepath.Join(work, ".rysh", "history", "test-session.history")
	write(t, ownHistory, "ls -la")

	writeRecord(t, st, Record{Name: "test-session", Path: work})
	if err := st.Delete("test-session"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := os.Stat(ownHistory); !os.IsNotExist(err) {
		t.Errorf("the session's own history file should be removed, err=%v", err)
	}
	assertAllExist(t, keep)
}

// `rysh run` boots a throwaway session in whatever directory it is invoked from
// and tears it down on exit. That teardown must leave the host workspace exactly
// as it found it — a headless CI run used to delete the credentials and channel
// state of the workspace it ran in.
func TestDeleteThrowawayRunSessionLeavesWorkspaceIntact(t *testing.T) {
	st := newTestStore(t)
	work := t.TempDir()
	keep := workspaceState(t, work)

	// A run-* session writes no history file of its own.
	writeRecord(t, st, Record{Name: "run-1784935936594051127", Path: work})
	if err := st.Delete("run-1784935936594051127"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	assertAllExist(t, keep)
	if _, err := os.Stat(filepath.Join(work, ".rysh")); err != nil {
		t.Fatalf(".rysh itself should survive: %v", err)
	}
}

// A session recreated under the same name must not inherit the old one's shell
// history — and must never remove another session's.
func TestDeleteLeavesOtherSessionsHistory(t *testing.T) {
	work := t.TempDir()
	mine := filepath.Join(work, ".rysh", "history", "mine.history")
	theirs := filepath.Join(work, ".rysh", "history", "theirs.history")
	write(t, mine, "mine")
	write(t, theirs, "theirs")

	deleteSessionArtifacts(work, "mine")

	if _, err := os.Stat(mine); !os.IsNotExist(err) {
		t.Errorf("own history should be removed, err=%v", err)
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Errorf("another session's history should survive: %v", err)
	}
}

// An empty working directory or session name must be a no-op, rather than
// resolving to a relative path and deleting something under the process's cwd.
func TestDeleteSessionArtifactsIgnoresEmptyArguments(t *testing.T) {
	work := t.TempDir()
	h := filepath.Join(work, ".rysh", "history", "s.history")
	write(t, h, "x")

	deleteSessionArtifacts("", "s")
	deleteSessionArtifacts(work, "")
	deleteSessionArtifacts("  ", "  ")

	if _, err := os.Stat(h); err != nil {
		t.Errorf("no-op calls should not remove anything: %v", err)
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
