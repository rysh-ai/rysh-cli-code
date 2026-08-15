// SPDX-License-Identifier: Apache-2.0

package daemontest_test

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/daemontest"
)

// TestPaneMetaRoundTrip drives `##pane meta` against a live daemon: the value
// has to travel Workspace → Pane → KV → snapshot → back out of `##pane info`,
// and a unit test can only see the first hop.
func TestPaneMetaRoundTrip(t *testing.T) {
	s := daemontest.Fresh(t)

	s.MustSucceed(t, "##pane meta set claude.session_id 11111111-2222-3333-4444-555555555555")
	// The pane applies it on its own goroutine, so read back with a little
	// patience rather than assuming the next command sees it.
	var list string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		list = s.MustSucceed(t, "##pane meta list")
		if strings.Contains(list, "11111111-2222-3333-4444-555555555555") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(list, "claude.session_id") ||
		!strings.Contains(list, "11111111-2222-3333-4444-555555555555") {
		t.Fatalf("##pane meta list does not show what was set:\n%s", list)
	}

	// The same entry must be visible where someone would actually look for it.
	if info := s.MustSucceed(t, "##pane info"); !strings.Contains(info, "claude.session_id") {
		t.Errorf("##pane info does not show pane metadata:\n%s", info)
	}

	if got := s.MustSucceed(t, "##pane meta get claude.session_id"); !strings.Contains(got, "1111") {
		t.Errorf("##pane meta get: %s", got)
	}

	// A supervisor enumerating its panes reads them in ONE call; per-pane
	// round-trips are what the flag exists to avoid.
	if listed := s.MustSucceed(t, "##pane list --meta"); !strings.Contains(listed, "meta:claude.session_id=1111") {
		t.Errorf("##pane list --meta does not carry metadata:\n%s", listed)
	}
	// And the default listing stays clean.
	if plain := s.MustSucceed(t, "##pane list"); strings.Contains(plain, "meta:") {
		t.Errorf("##pane list should not show metadata unless asked:\n%s", plain)
	}

	s.MustSucceed(t, "##pane meta delete claude.session_id")
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		list = s.MustSucceed(t, "##pane meta list")
		if !strings.Contains(list, "11111111") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if strings.Contains(list, "11111111") {
		t.Errorf("deleted metadata is still listed:\n%s", list)
	}

	// Reading a key that was never set is a failure, not an empty success: a
	// script asking for a pane's session id must be able to tell "no such pane
	// meta" from "the value is empty".
	s.MustFail(t, "##pane meta get no-such-key")
}

// TestPaneListReportsRunningProgram is the end-to-end for the program field:
// only a live PTY with something in the foreground can produce it.
func TestPaneListReportsRunningProgram(t *testing.T) {
	s := daemontest.Fresh(t)

	// A long-lived, boring foreground program. `sleep` is in every PATH this
	// suite can run on, and holds the terminal without drawing anything.
	s.MustSucceed(t, "##cmd ws sleep 60")

	var list string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		list = s.MustSucceed(t, "##pane list")
		if strings.Contains(list, "running=sleep") {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !strings.Contains(list, "running=sleep") {
		t.Fatalf("##pane list never reported the foreground program:\n%s", list)
	}

	// The long-standing "[N] <title> id=<uuid>" prefix every parser keys on must
	// survive the new suffix.
	if !regexp.MustCompile(`\[\d+\]\s+\S+\s+id=\S{36}`).MatchString(list) {
		t.Errorf("pane line format changed shape:\n%s", list)
	}

	// And the filter that decides where a broadcast lands must agree with it.
	out := s.MustSucceed(t, "##cmd ws --running sleep true")
	if !strings.Contains(out, "in 1 pane(s)") {
		t.Errorf("--running sleep did not match the busy pane:\n%s", out)
	}
	// `--running shell` is the pane at its prompt; with the only pane busy,
	// nothing matches, and a fan-out that reaches nothing must report failure
	// rather than a cheerful zero.
	s.MustFail(t, "##cmd ws --running shell true")
}
