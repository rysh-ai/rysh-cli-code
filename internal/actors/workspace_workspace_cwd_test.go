package actors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// newWsCwdTestWorkspace builds a minimal active WorkspaceActor (workspace "main"
// at index 0) plus a seeded session registry record. cmdWorkspaceCwd only touches
// w.cfg, w.tabs (left empty so the broadcast is a no-op), w.workspaceName/Idx, and
// the on-disk config file — crucially NOT the session registry.
func newWsCwdTestWorkspace(t *testing.T) (*WorkspaceActor, *session.Store, config.Config) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Config{
		SessionName:      "cwdsess",
		SessionDir:       filepath.Join(root, ".rysh", "sessions"),
		WorkingDirectory: filepath.Join(root, "start"),
	}
	if err := os.MkdirAll(cfg.WorkingDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(cfg)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := store.Upsert(session.Record{
		Name:  cfg.SessionName,
		Path:  cfg.WorkingDirectory,
		State: "detached",
	}); err != nil {
		t.Fatalf("seed record: %v", err)
	}
	return &WorkspaceActor{cfg: cfg, workspaceName: "main", workspaceIdx: 0}, store, cfg
}

// TestCmdWorkspaceCwdSetsLive verifies ##workspace cwd updates the live working
// directory and does NOT mutate the session registry record (working dir is
// per-workspace; the registry Path stays the project root).
func TestCmdWorkspaceCwdSetsLive(t *testing.T) {
	w, store, cfg := newWsCwdTestWorkspace(t)
	target := filepath.Join(filepath.Dir(cfg.WorkingDirectory), "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	w.cmdWorkspaceCwd(&out, target)

	if w.cfg.WorkingDirectory != target {
		t.Errorf("w.cfg.WorkingDirectory = %q, want %q", w.cfg.WorkingDirectory, target)
	}
	if !strings.Contains(out.String(), target) {
		t.Errorf("output %q does not mention new dir %q", out.String(), target)
	}
	rec, err := store.Get(cfg.SessionName)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.Path != cfg.WorkingDirectory {
		t.Errorf("registry Path = %q, want it unchanged %q (workspace cwd must not touch the registry)", rec.Path, cfg.WorkingDirectory)
	}
}

// TestCmdWorkspaceCwdWritesConfig verifies that with a config file loaded,
// ##workspace cwd writes the active workspace's working_directory back to its
// entry so it survives a daemon restart.
func TestCmdWorkspaceCwdWritesConfig(t *testing.T) {
	w, _, cfg := newWsCwdTestWorkspace(t)
	cfgRoot := filepath.Dir(filepath.Dir(cfg.SessionDir)) // the temp root
	cfgPath := filepath.Join(cfgRoot, "rysh.config.yaml")
	if err := os.WriteFile(cfgPath, []byte("workspace:\n  - name: \"main\"\n    working_directory: \"old\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.cfg.ConfigFile = cfgPath

	target := filepath.Join(cfgRoot, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	w.cmdWorkspaceCwd(&out, target)

	raw, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(raw), "working_directory") || !strings.Contains(string(raw), target) {
		t.Errorf("config not updated with workspace working_directory:\n%s", raw)
	}
	if !strings.Contains(out.String(), "written to") {
		t.Errorf("output %q does not confirm config write", out.String())
	}
}

// TestCmdWorkspaceCwdRelative verifies a relative path resolves against the
// active workspace's current dir.
func TestCmdWorkspaceCwdRelative(t *testing.T) {
	w, _, cfg := newWsCwdTestWorkspace(t)
	sub := filepath.Join(cfg.WorkingDirectory, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	w.cmdWorkspaceCwd(&out, "sub")
	if w.cfg.WorkingDirectory != sub {
		t.Errorf("relative resolve: w.cfg.WorkingDirectory = %q, want %q", w.cfg.WorkingDirectory, sub)
	}
}

// TestCmdWorkspaceCwdRejectsMissing verifies a non-existent dir is rejected and
// leaves both the live cwd and the registry record unchanged.
func TestCmdWorkspaceCwdRejectsMissing(t *testing.T) {
	w, store, cfg := newWsCwdTestWorkspace(t)
	before := w.cfg.WorkingDirectory

	var out strings.Builder
	w.cmdWorkspaceCwd(&out, filepath.Join(cfg.WorkingDirectory, "does-not-exist"))

	if w.cfg.WorkingDirectory != before {
		t.Errorf("working dir changed to %q on invalid input; want unchanged %q", w.cfg.WorkingDirectory, before)
	}
	if !strings.Contains(out.String(), "not a directory") {
		t.Errorf("output %q does not report the error", out.String())
	}
	rec, err := store.Get(cfg.SessionName)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.Path != before {
		t.Errorf("registry Path = %q, want unchanged %q", rec.Path, before)
	}
}

// TestCmdWorkspaceCwdQuery verifies that ##workspace cwd with no argument prints
// the current working directory without changing it.
func TestCmdWorkspaceCwdQuery(t *testing.T) {
	w, _, _ := newWsCwdTestWorkspace(t)
	before := w.cfg.WorkingDirectory

	var out strings.Builder
	w.cmdWorkspaceCwd(&out, "")

	if w.cfg.WorkingDirectory != before {
		t.Errorf("query changed working dir to %q", w.cfg.WorkingDirectory)
	}
	if !strings.Contains(out.String(), before) {
		t.Errorf("query output %q does not show current dir %q", out.String(), before)
	}
}
