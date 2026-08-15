// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func newSwitchTestWS(names []string, idx int) *WorkspaceActor {
	return &WorkspaceActor{workspaceNames: names, workspaceIdx: idx}
}

func TestResolveSwitchTarget(t *testing.T) {
	names := []string{"a", "b", "c"}
	cases := []struct {
		name   string
		idx    int
		m      *msg.MsgSwitchWorkspace
		want   int
		wantOK bool
	}{
		{"next from middle", 1, &msg.MsgSwitchWorkspace{Direction: msg.DirNext}, 2, true},
		{"next wraps at end", 2, &msg.MsgSwitchWorkspace{Direction: msg.DirNext}, 0, true},
		{"prev from middle", 1, &msg.MsgSwitchWorkspace{Direction: msg.DirPrev}, 0, true},
		{"prev wraps at start", 0, &msg.MsgSwitchWorkspace{Direction: msg.DirPrev}, 2, true},
		{"index in range", 0, &msg.MsgSwitchWorkspace{Index: 2}, 2, true},
		{"index out of range", 0, &msg.MsgSwitchWorkspace{Index: 9}, 0, false},
		{"negative index", 0, &msg.MsgSwitchWorkspace{Index: -1}, 0, false},
		{"direction beats index", 1, &msg.MsgSwitchWorkspace{Index: 5, Direction: msg.DirNext}, 2, true},
	}
	for _, c := range cases {
		w := newSwitchTestWS(names, c.idx)
		got, ok := w.resolveSwitchTarget(c.m)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("%s: got (%d,%v), want (%d,%v)", c.name, got, ok, c.want, c.wantOK)
		}
	}
}

func TestResolveSwitchTargetNoWorkspaces(t *testing.T) {
	w := newSwitchTestWS(nil, 0)
	if _, ok := w.resolveSwitchTarget(&msg.MsgSwitchWorkspace{Direction: msg.DirNext}); ok {
		t.Errorf("expected !ok when no workspaces are known")
	}
}

func TestWorkspaceKVKey(t *testing.T) {
	// Named workspaces use a '_' separator (NOT ':', which is invalid in a NATS
	// KV key and made every Put silently fail — dropping layout persistence).
	if got := (&WorkspaceActor{workspaceName: "main"}).kvKey(); got != "ws_main" {
		t.Errorf("kvKey()=%q, want %q", got, "ws_main")
	}
	// Disallowed characters in the name are sanitized to '_' so the key stays
	// valid per NATS KV's validKeyRe (^[-/_=.a-zA-Z0-9]+$).
	if got := (&WorkspaceActor{workspaceName: "a:b c/d"}).kvKey(); got != "ws_a_b_c_d" {
		t.Errorf("kvKey()=%q, want %q", got, "ws_a_b_c_d")
	}
	// Empty name falls back to the legacy fixed key.
	if got := (&WorkspaceActor{}).kvKey(); got != "state" {
		t.Errorf("kvKey()=%q, want %q", got, "state")
	}
}
