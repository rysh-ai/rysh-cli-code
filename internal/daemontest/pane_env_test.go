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

// `##pane list` prints the owning tab before the panes, and both carry an
// `id=`, so the two are matched separately rather than by position.
var (
	listTabID  = regexp.MustCompile(`panes in tab "[^"]*"\s+id=(\S+)`)
	listPaneID = regexp.MustCompile(`\[\d+\]\s+\S+\s+id=(\S+)`)
)

// readPaneIdentity types printenv into the session's pane and returns the five
// identity values that shell prints, in order.
//
// Through the pane rather than around it: what a pane's own shell prints is the
// environment programs in that pane actually get, which is the claim under
// test. A file rather than the screen, because a pane's output is a VT
// rendering and not a stream to parse.
func readPaneIdentity(t *testing.T, s *daemontest.Session, dump string) []string {
	t.Helper()
	// printf rather than `printenv`, and this is not a style preference: BSD
	// printenv takes ONE name and ignores the rest, so on macOS
	// `printenv A B C D E` prints only A. This test asked for five values, got
	// one, and reported "the pane never wrote all five" — which reads as a
	// missing environment when the environment was fine all along. It was red
	// on every Mac in the fleet for exactly that reason.
	//
	// printf '%s\n' with five arguments cycles the format and is POSIX, so it
	// prints five lines on BSD and GNU alike. An UNSET variable still yields an
	// empty line, which strings.Fields drops — so the original property holds:
	// a missing one shortens the output rather than hiding in a dump of the
	// daemon's whole environment.
	cmd := "##cmd ws printf '%s\\n' \"$RYSH_SESSION\" \"$RYSH_TAB\" \"$RYSH_LANE\" " +
		"\"$RYSH_STACK\" \"$RYSH_PANE\" > " + dump

	deadline := time.Now().Add(30 * time.Second)
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		// Re-sent occasionally: the command is typed into the pane's shell, and
		// a shell that is still starting up drops what it never read.
		if attempt%20 == 0 {
			s.MustSucceed(t, cmd)
		}
		if b, err := os.ReadFile(dump); err == nil {
			if got := strings.Fields(string(b)); len(got) == 5 {
				return got
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the pane never wrote all five identity variables to %s — "+
		"RYSH_SESSION/RYSH_TAB/RYSH_LANE/RYSH_STACK/RYSH_PANE are missing from its shell", dump)
	return nil
}

// TestPaneShellKnowsItsOwnPane checks the property paneIdentityEnv exists for,
// where it actually has to hold: inside a real pane's shell.
//
// A unit test can only prove the strings are built correctly. It cannot prove
// they survive the path that matters — pane actor, PTY spawn, interactive bash
// — and that path is exactly where an inherited RYSH_PANE or a lost append
// would leave a program addressing somebody else's pane while looking fine.
func TestPaneShellKnowsItsOwnPane(t *testing.T) {
	s := daemontest.Fresh(t)

	list := s.MustSucceed(t, "##pane list")
	tab := listTabID.FindStringSubmatch(list)
	pane := listPaneID.FindStringSubmatch(list)
	if tab == nil || pane == nil {
		t.Fatalf("could not read the tab and pane ids out of ##pane list:\n%s", list)
	}

	got := readPaneIdentity(t, s, filepath.Join(t.TempDir(), "identity.txt"))
	for _, tc := range []struct{ name, got, want string }{
		{"RYSH_SESSION", got[0], s.Name},
		{"RYSH_TAB", got[1], tab[1]},
		{"RYSH_PANE", got[4], pane[1]},
	} {
		if tc.got != tc.want {
			t.Errorf("%s in the pane = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	// The lane and stack ids have no counterpart in `##pane list`, which prints
	// positions (lane-1, group-1) rather than ids — so they are checked by what
	// they are FOR, in the test below.
	for i, name := range []string{"RYSH_LANE", "RYSH_STACK"} {
		if got[2+i] == "" {
			t.Errorf("%s is empty in the pane", name)
		}
	}
}

// TestPaneIdentitySelectsItsOwnPane is the point of exporting the ids rather
// than positions: they have to work as selectors.
//
// `--pane` on its own is resolved inside the ACTIVE pane's group, so a program
// addressing a pane outside that group needs the whole path — tab, lane, stack,
// pane. This builds exactly that command out of what the pane says it is, and
// requires it to resolve to one pane. An id that cannot be selected with would
// be a plausible-looking string that silently fails to route.
func TestPaneIdentitySelectsItsOwnPane(t *testing.T) {
	s := daemontest.Fresh(t)

	got := readPaneIdentity(t, s, filepath.Join(t.TempDir(), "identity.txt"))
	qualified := "##cmd pane --tab " + got[1] + " --lane " + got[2] +
		" --pg " + got[3] + " --pane " + got[4] + " true"

	out := s.MustSucceed(t, qualified)
	if !strings.Contains(out, "in 1 pane(s)") {
		t.Errorf("a command fully qualified with the pane's own ids did not reach "+
			"exactly one pane:\n%s\n%s", qualified, out)
	}
}
