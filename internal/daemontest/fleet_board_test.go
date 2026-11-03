// SPDX-License-Identifier: Apache-2.0

package daemontest_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/daemontest"
	"github.com/rysh-ai/rysh-cli-code/internal/fleetboard"
)

// TestFleetBoardThroughTheDaemon: the daemon keeps a fleet board row for a
// registered fleet, and a member's milestone post on the fleet's own board
// reaches it (design 030 W2). The row appears within the actor's 20 s
// registry poll, so the wait is bounded at 40 s.
func TestFleetBoardThroughTheDaemon(t *testing.T) {
	s := daemontest.Fresh(t)
	m := listPaneID.FindStringSubmatch(s.MustSucceed(t, "##pane list"))
	if m == nil {
		t.Fatal("no pane id in ##pane list")
	}
	pane := m[1]

	cli := func(args ...string) string {
		t.Helper()
		out, code := s.CLI(t, args...)
		if code != 0 {
			t.Fatalf("rysh %q: exit %d\n%s", args, code, out)
		}
		return out
	}
	cli("fleet", "register", "dt-fleet", "--session", s.Name)
	cli("fleet", "member", "add", "dt-fleet", "--pane", pane, "--role", "ceo", "--session", s.Name)

	read := func() (fleetboard.ReplyRow, bool) {
		var reply fleetboard.Reply
		if json.Unmarshal([]byte(cli("fleet", "board", "--json", "--session", s.Name)), &reply) != nil {
			return fleetboard.ReplyRow{}, false
		}
		for _, r := range reply.Rows {
			if r.Fleet == "dt-fleet" {
				return r, true
			}
		}
		return fleetboard.ReplyRow{}, false
	}
	deadline := time.Now().Add(40 * time.Second)
	for {
		if r, ok := read(); ok && r.Members == 1 && r.HeadPane == pane {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no fleet board row for dt-fleet:\n%s", cli("fleet", "board", "--session", s.Name))
		}
		time.Sleep(500 * time.Millisecond)
	}

	cli("board", "post", "--board", "dt-fleet", "--as", pane, "--kind", "milestone", "--session", s.Name, "--", "M1 shipped")
	deadline = time.Now().Add(20 * time.Second)
	for {
		if r, ok := read(); ok && r.LastStatus != nil && r.LastStatus.Text == "M1 shipped" {
			if r.Head == nil || r.Health != fleetboard.HealthActive {
				t.Fatalf("row after the milestone: %+v", r)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the milestone never reached the row:\n%s", cli("fleet", "board", "--session", s.Name))
		}
		time.Sleep(300 * time.Millisecond)
	}
	if out := cli("fleet", "board", "--session", s.Name); !strings.Contains(out, "MILESTONE: M1 shipped") {
		t.Fatalf("plain list:\n%s", out)
	}
}
