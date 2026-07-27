package actors

// Phase 3 (bash-shell-mode): persistent shell history file tests.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

func histCfg(t *testing.T) config.Config {
	t.Helper()
	return config.Config{RyshDir: t.TempDir(), SessionName: "testsess"}
}

func TestHistoryEscapeRoundTrip(t *testing.T) {
	cases := []string{
		"ls -la",
		`echo "a\nb"`,
		"for i in 1 2 3; do\n  echo $i\ndone",
		`printf '%s\\n' hi`,
		`back\slash and
newline mix \\n`,
	}
	for _, c := range cases {
		if got := unescapeHistoryEntry(escapeHistoryEntry(c)); got != c {
			t.Errorf("round trip failed:\n in: %q\nout: %q", c, got)
		}
	}
}

func TestHistoryAppendAndLoad(t *testing.T) {
	cfg := histCfg(t)
	appendHistoryFile(cfg, "echo one")
	appendHistoryFile(cfg, "echo two")
	appendHistoryFile(cfg, "for i in 1 2; do\n  echo $i\ndone")

	got := loadHistoryFile(cfg, 100)
	want := []string{"echo one", "echo two", "for i in 1 2; do\n  echo $i\ndone"}
	if len(got) != len(want) {
		t.Fatalf("loaded %d entries, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHistoryLoadTrimsToLimit(t *testing.T) {
	cfg := histCfg(t)
	for _, c := range []string{"a", "b", "c", "d", "e"} {
		appendHistoryFile(cfg, "cmd "+c)
	}
	got := loadHistoryFile(cfg, 2)
	if len(got) != 2 || got[0] != "cmd d" || got[1] != "cmd e" {
		t.Fatalf("trim load = %#v, want newest 2", got)
	}
	// The file itself must have been rewritten trimmed.
	data, err := os.ReadFile(filepath.Join(cfg.RyshDir, "history", "testsess.history"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "cmd d\ncmd e\n" {
		t.Errorf("file after trim = %q", string(data))
	}
}

func TestHistoryNoDirConfigured(t *testing.T) {
	// No RyshDir → no file, no panic, empty load.
	cfg := config.Config{SessionName: "x"}
	appendHistoryFile(cfg, "echo hi")
	if got := loadHistoryFile(cfg, 10); got != nil {
		t.Errorf("expected nil for unconfigured dir, got %#v", got)
	}
}
