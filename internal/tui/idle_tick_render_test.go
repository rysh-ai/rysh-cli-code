// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"
	"time"
)

// F-100 (E9): an idle `rysh attach` burned 30-40% of one core once its panes
// held text. Measured with a real attach against a test daemon (branch
// e9-f100-idle-attach-probe): the TUI handled ~238 tickMsg a minute while idle
// and EVERY one forced a full re-render, although the tick's only job is to flip
// attentionBlink and nothing on screen blinks unless some pane has attention.
// Each full render re-measures and re-allocates every visible line, so the cost
// grew with what the panes showed. With no attention, the tick must serve the
// cached frame.

func TestF100IdleTickServesTheCachedFrame(t *testing.T) {
	m := Model{}
	next, cmd := m.Update(tickMsg(time.Now()))
	got := next.(Model)
	if !got.viewSkip {
		t.Fatal("a tick with no attention anywhere re-rendered the whole view (F-100)")
	}
	if cmd == nil {
		t.Fatal("the tick stopped re-arming; the blink could never resume")
	}
}

func TestF100TickStillBlinksWhenSomethingNeedsAttention(t *testing.T) {
	m := Model{attentionState: map[string]*attentionInfo{"p1": {Count: 1}}}
	next, _ := m.Update(tickMsg(time.Now()))
	got := next.(Model)
	if got.viewSkip {
		t.Fatal("a pane needs attention but the tick served the cached frame: its marker would not blink")
	}
	if got.attentionBlink == m.attentionBlink {
		t.Fatal("the blink did not toggle")
	}
}

func TestF100ClearedAttentionDoesNotKeepRendering(t *testing.T) {
	// A pane whose attention was acknowledged keeps an entry with Count 0.
	m := Model{attentionState: map[string]*attentionInfo{"p1": {Count: 0}}}
	next, _ := m.Update(tickMsg(time.Now()))
	if !next.(Model).viewSkip {
		t.Fatal("an attention entry with Count 0 still forced a render on every tick")
	}
}
