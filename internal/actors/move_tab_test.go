package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func newMoveTestWS(ids string, active int) *WorkspaceActor {
	w := &WorkspaceActor{activeTabIdx: active}
	for _, c := range ids {
		w.tabs = append(w.tabs, &tabInfo{id: string(c)})
	}
	return w
}

func wsTabOrder(w *WorkspaceActor) string {
	var b strings.Builder
	for _, t := range w.tabs {
		b.WriteString(t.id)
	}
	return b.String()
}

func TestMoveActiveTab(t *testing.T) {
	cases := []struct {
		name      string
		ids       string
		active    int
		dir       msg.Direction
		wantOrder string
		wantIdx   int
		wantMoved bool
	}{
		{"right from middle", "abc", 1, msg.DirRight, "acb", 2, true},
		{"left from middle", "abc", 1, msg.DirLeft, "bac", 0, true},
		{"right at end is noop", "abc", 2, msg.DirRight, "abc", 2, false},
		{"left at start is noop", "abc", 0, msg.DirLeft, "abc", 0, false},
		{"right from first", "abc", 0, msg.DirRight, "bac", 1, true},
		{"left from last", "abc", 2, msg.DirLeft, "acb", 1, true},
		{"unsupported dir is noop", "abc", 1, msg.DirUp, "abc", 1, false},
	}
	for _, c := range cases {
		w := newMoveTestWS(c.ids, c.active)
		moved := w.moveActiveTab(c.dir)
		if moved != c.wantMoved {
			t.Errorf("%s: moved=%v, want %v", c.name, moved, c.wantMoved)
		}
		if got := wsTabOrder(w); got != c.wantOrder {
			t.Errorf("%s: order=%q, want %q", c.name, got, c.wantOrder)
		}
		if w.activeTabIdx != c.wantIdx {
			t.Errorf("%s: activeTabIdx=%d, want %d", c.name, w.activeTabIdx, c.wantIdx)
		}
	}
}
