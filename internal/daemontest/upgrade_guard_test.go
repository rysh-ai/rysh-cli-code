package daemontest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/daemontest"
)

// TestUpgradeRefusesToKillRunningPanes drives the guard the only way that can
// actually fail: through the CLI, against a daemon with something live in it.
//
// The unit test covers the decision (force / interactive / refuse). It cannot
// cover the QUESTION — asking the daemon what is running — and that is where
// the bug was: the snapshot reply was asserted to the wrong type, so every
// query came back empty and the guard approved killing a pane running
// `sleep 300` while reporting nothing amiss. A guard that fails open is only
// safe if something checks that it can still see.
func TestUpgradeRefusesToKillRunningPanes(t *testing.T) {
	s := daemontest.Fresh(t)

	s.MustSucceed(t, "##cmd ws sleep 300")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(s.MustSucceed(t, "##pane list"), "running=sleep") {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	out, code := s.CLI(t, "attach", s.Name, "--upgrade")
	if code == 0 {
		t.Errorf("--upgrade proceeded past a pane running sleep (exit 0):\n%s", out)
	}
	if !strings.Contains(out, "refusing to restart") || !strings.Contains(out, "--force") {
		t.Errorf("refusal should name the panes and say how to override:\n%s", out)
	}
	if !strings.Contains(out, "sleep") {
		t.Errorf("refusal should say WHAT is running:\n%s", out)
	}

	// The daemon must still be there: a guard that refuses and then takes the
	// session down anyway would be worse than no guard.
	s.MustSucceed(t, "##tab list")
}
