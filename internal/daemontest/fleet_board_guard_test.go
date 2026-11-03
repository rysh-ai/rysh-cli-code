// SPDX-License-Identifier: Apache-2.0

package daemontest_test

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/daemontest"
)

// TestFleetBoardGuardThroughTheDaemon (design 030 §9.3, W3): through the real
// CLI door, a worker's post to the fleet board is refused; once the same pane
// is the fleet's ceo it is accepted, recorded, and `board tail` does not mark
// it unverified.
func TestFleetBoardGuardThroughTheDaemon(t *testing.T) {
	s := daemontest.Fresh(t)
	m := listPaneID.FindStringSubmatch(s.MustSucceed(t, "##pane list"))
	if m == nil {
		t.Fatal("no pane id in ##pane list")
	}
	pane := m[1]
	cli := func(args ...string) (string, int) {
		return s.CLI(t, append(args, "--session", s.Name)...)
	}
	mustCLI := func(args ...string) string {
		t.Helper()
		out, code := cli(args...)
		if code != 0 {
			t.Fatalf("rysh %q: exit %d\n%s", args, code, out)
		}
		return out
	}
	mustCLI("fleet", "register", "dt-guard")
	mustCLI("fleet", "member", "add", "dt-guard", "--pane", pane, "--role", "worker")

	out, code := cli("board", "post", "--board", "fleet", "--as", pane, "--", "worker claims a milestone")
	if code == 0 || !strings.Contains(out, "only a fleet head") {
		t.Fatalf("a worker's fleet-board post: exit %d\n%s", code, out)
	}

	mustCLI("fleet", "member", "add", "dt-guard", "--pane", pane, "--role", "ceo")
	mustCLI("board", "post", "--board", "fleet", "--as", pane, "--kind", "milestone", "--", "[dt-guard] MILESTONE: M1")

	tail := ""
	for i := 0; i < 50; i++ {
		tail = mustCLI("board", "tail", "--board", "fleet")
		if strings.Contains(tail, "MILESTONE: M1") {
			break
		}
	}
	if !strings.Contains(tail, "MILESTONE: M1") {
		t.Fatalf("the head's post never reached the fleet board:\n%s", tail)
	}
	if strings.Contains(tail, "(unverified)") || strings.Contains(tail, "worker claims") {
		t.Fatalf("fleet board tail:\n%s", tail)
	}
}
