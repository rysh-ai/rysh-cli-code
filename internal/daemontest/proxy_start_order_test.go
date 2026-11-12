// SPDX-License-Identifier: Apache-2.0

package daemontest_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/daemontest"
)

// Governance proxy start order (security-lead ledger 220, class level): with
// the proxy on, every pane must be governed from the moment its shell starts.
// The panes a session opens at start (a fresh bootstrap, or a KV restore) used
// to spawn before the proxy was up, so their shell got no proxy base URL and a
// CLI started there talked to its provider directly. Panes created later were
// fine, which is why the session's FIRST pane is the one under test, and a
// later pane is the control that proves the proxy itself runs.

var listPaneIDs = regexp.MustCompile(`\[\d+\]\s+\S+\s+id=(\S+)`)

// paneBaseURLs types into every pane of the workspace and returns, per pane id,
// what that pane's own shell has for ANTHROPIC_BASE_URL ("-" when unset). Each
// shell writes to a file named after its own $RYSH_PANE, so one broadcast
// reads them all without addressing a pane by path.
func paneBaseURLs(t *testing.T, s *daemontest.Session, want int) map[string]string {
	t.Helper()
	dir := t.TempDir()
	cmd := "##cmd ws printf '%s\\n' \"${ANTHROPIC_BASE_URL:--}\" > " + dir + "/\"$RYSH_PANE\""
	deadline := time.Now().Add(30 * time.Second)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		// Re-sent occasionally: a shell that is still starting drops input.
		if attempt%20 == 0 {
			s.MustSucceed(t, cmd)
		}
		got := map[string]string{}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
				if v := strings.TrimSpace(string(b)); v != "" {
					got[e.Name()] = v
				}
			}
		}
		if len(got) >= want {
			return got
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("fewer than %d panes wrote their ANTHROPIC_BASE_URL to %s", want, dir)
	return nil
}

func listedPaneIDs(t *testing.T, s *daemontest.Session) []string {
	t.Helper()
	var ids []string
	for _, m := range listPaneIDs.FindAllStringSubmatch(s.MustSucceed(t, "##pane list"), -1) {
		ids = append(ids, m[1])
	}
	return ids
}

func governed(v string) bool {
	return strings.HasPrefix(v, "http://127.0.0.1:") && strings.Contains(v, "/anthropic/")
}

func TestProxyGovernsTheFirstPaneOfAFreshSession(t *testing.T) {
	s := daemontest.FreshWithConfig(t, "proxy:\n  enabled: true\n")
	ids := paneIDs(t, s)
	if len(ids) != 1 {
		t.Fatalf("a fresh session should have one pane, got %v", ids)
	}
	first := ids[0]
	s.MustSucceed(t, "##new pane")
	got := paneBaseURLs(t, s, 2)
	var later string
	for id := range got {
		if id != first {
			later = id
		}
	}
	if later == "" || !governed(got[later]) {
		t.Fatalf("control failed: a pane opened after start is not governed either (%v)", got)
	}
	if !governed(got[first]) {
		t.Fatalf("the session's first pane is not governed: ANTHROPIC_BASE_URL = %q, want the loopback proxy (a later pane has %q)", got[first], got[later])
	}
}

// The same promise after a restart: the panes a KV restore brings back are
// respawned shells, and they must be governed too.
func TestProxyGovernsRestoredPanesAfterARestart(t *testing.T) {
	s := daemontest.FreshWithConfig(t, "proxy:\n  enabled: true\n")
	s.MustSucceed(t, "##new pane")
	before := listedPaneIDs(t, s)
	if len(before) != 2 {
		t.Fatalf("want two panes before the restart, got %v", before)
	}
	if out, code := s.CLI(t, "restart", s.Name); code != 0 {
		t.Fatalf("rysh restart %s: exit %d\n%s", s.Name, code, out)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, code := s.Exec(t, "##tab list"); code == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session did not come back after rysh restart")
		}
		time.Sleep(500 * time.Millisecond)
	}
	got := paneBaseURLs(t, s, 2)
	for _, id := range before {
		if !governed(got[id]) {
			t.Errorf("restored pane %s is not governed: ANTHROPIC_BASE_URL = %q (all: %v)", id, got[id], got)
		}
	}
}
