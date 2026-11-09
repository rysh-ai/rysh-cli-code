// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// security-lead (v0.2.16 landing read of a7f3ce80): an agent's ## command
// through the MCP server runs quiet (no echo, no history) only on the agent's
// OWN pane. On another pane it is echoed and recorded in that pane's
// histories as before, so the owner of that pane sees what the agent did.
func TestMCPRyshCommandQuietOnlyOnItsOwnPane(t *testing.T) {
	type sent struct {
		pane  string
		quiet bool
	}
	var got []sent
	oldQ, oldL := mcpRyshQuiet, mcpRyshLoud
	t.Cleanup(func() { mcpRyshQuiet, mcpRyshLoud = oldQ, oldL })
	mcpRyshQuiet = func(_ liveSession, pane, _ string) (*msg.MsgCLIResponse, error) {
		got = append(got, sent{pane, true})
		return &msg.MsgCLIResponse{OK: true}, nil
	}
	mcpRyshLoud = func(_ liveSession, pane, _ string) (*msg.MsgCLIResponse, error) {
		got = append(got, sent{pane, false})
		return &msg.MsgCLIResponse{OK: true}, nil
	}

	own := liveSession{name: "s1", self: "pane-own"}
	for _, pane := range []string{"pane-own", "pane-other", ""} {
		if _, err := own.RyshCommand(context.Background(), pane, "new pane"); err != nil {
			t.Fatal(err)
		}
	}
	none := liveSession{name: "s1"} // not started from a pane of this session
	if _, err := none.RyshCommand(context.Background(), "pane-own", "new pane"); err != nil {
		t.Fatal(err)
	}
	want := []sent{{"pane-own", true}, {"pane-other", false}, {"", false}, {"pane-own", false}}
	if len(got) != len(want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d sent %+v, want %+v (all: %v)", i, got[i], want[i], got)
		}
	}
}

// The MCP server's own pane is $RYSH_PANE only when $RYSH_SESSION is unset
// or names the session it serves, as for `rysh exec` (execTarget).
func TestMCPSelfPane(t *testing.T) {
	cases := []struct{ pane, envSess, sess, want string }{
		{"p1", "", "s1", "p1"},
		{"p1", "s1", "s1", "p1"},
		{"p1", "s2", "s1", ""},
		{"", "s1", "s1", ""},
		{"  p1 ", "", "s1", "p1"},
	}
	for _, c := range cases {
		if got := mcpSelfPane(c.pane, c.envSess, c.sess); got != c.want {
			t.Errorf("mcpSelfPane(%q, %q, %q) = %q, want %q", c.pane, c.envSess, c.sess, got, c.want)
		}
	}
}
