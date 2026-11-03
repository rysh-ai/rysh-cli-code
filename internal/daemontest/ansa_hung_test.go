// SPDX-License-Identifier: Apache-2.0

package daemontest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/daemontest"
)

// TestAnsaHungThroughTheDaemon runs `rysh ansa hung` and `##ansa hung` against
// a test daemon whose claude home is a temp dir (CLAUDE_CONFIG_DIR, inherited
// by the daemon), so the real ~/.claude is never read.
//
//  1. a quiet shell over a 30-minute-old transcript is NO-MARKER, not HUNG;
//  2. once the working marker is on screen, the same pane is HUNG;
//  3. a fresh transcript write turns it BUSY;
//  4. the ## door and --all see the same pane.
func TestAnsaHungThroughTheDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	s := daemontest.Fresh(t)

	list := s.MustSucceed(t, "##pane list")
	m := listPaneID.FindStringSubmatch(list)
	if m == nil {
		t.Fatalf("no pane id in ##pane list:\n%s", list)
	}
	pane := m[1]
	const sid = "0f0f0f0f-1111-2222-3333-555555555555"
	s.MustSucceed(t, "##pane meta set claude.session_id "+sid)

	dir := filepath.Join(home, "projects", "-daemontest")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * time.Minute)
	if err := os.Chtimes(transcript, old, old); err != nil {
		t.Fatal(err)
	}

	hung := func(args ...string) string {
		t.Helper()
		out, code := s.CLI(t, append([]string{"ansa", "hung", "--session", s.Name}, args...)...)
		if code != 0 {
			t.Fatalf("rysh ansa hung %q: exit %d\n%s", args, code, out)
		}
		return out
	}

	// 1. Quiet screen.
	if out := hung(pane); !strings.Contains(out, "NO-MARKER") {
		t.Fatalf("quiet pane: want NO-MARKER\n%s", out)
	}

	// 2. Put claude's working marker on the screen. The pane serves its screen
	// only while a foreground program owns it (as claude does), so the marker
	// is printed and then `sleep` holds the pane. A fresh pane's shell may not
	// take the first line yet, so it is re-sent until sleep runs (as
	// startRawReader does).
	deadline := time.Now().Add(40 * time.Second)
	for attempt := 0; ; attempt++ {
		if attempt%15 == 0 && !strings.Contains(s.MustSucceed(t, "##pane list"), "running=sleep") {
			s.MustSucceed(t, "##cmd ws echo esc to interrupt; sleep 120")
		}
		out := hung(pane, "--minutes", "10")
		if strings.Contains(out, "HUNG ") && strings.Contains(out, "unchanged for 30m") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pane never read HUNG with the marker on screen:\n%s", out)
		}
		time.Sleep(300 * time.Millisecond)
	}

	// 3. A fresh transcript write is BUSY, not HUNG.
	now := time.Now()
	if err := os.Chtimes(transcript, now, now); err != nil {
		t.Fatal(err)
	}
	if out := hung("--claude-session", sid); !strings.Contains(out, "BUSY ") {
		t.Fatalf("fresh transcript: want BUSY\n%s", out)
	}

	// 4. The ## door and --all.
	if out := s.MustSucceed(t, "##ansa hung --all"); !strings.Contains(out, "1 claude panes checked") ||
		!strings.Contains(out, pane) {
		t.Fatalf("##ansa hung --all:\n%s", out)
	}

	// A refusal exits non-zero.
	if out, code := s.CLI(t, "ansa", "hung", "--session", s.Name, "no-such-pane"); code == 0 ||
		!strings.Contains(out, "unknown_target") {
		t.Fatalf("unknown target: exit %d\n%s", code, out)
	}
}
