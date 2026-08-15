// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Key routing for the dedicated replay pane (design 006 v2): while the
// focused pane has PaneType == "replay", the TUI claims space/←/→/+/-/q as
// playback controls (published as MsgReplayControl / MsgClosePane to the
// workspace inbox) instead of letting them fall through to the input line.
// Modeled on stack_raw_keys_test.go: a hand-built Model over a real embedded
// NATS bus, asserting on the published messages.

// buildReplayModel builds a Model whose active pane is a replay pane
// (PaneType "replay", no shell) in modeNormal.
func buildReplayModel(b *bus.Bus) Model {
	var pub *msgpkg.NATSPublisher
	if b != nil {
		pub = b.Publisher()
	}
	rp := domain.PaneSnapshot{ID: "replay-1", Title: "replay", PaneType: "replay", Status: "REPLAY 00:00/01:00", Flex: 1}
	tab := domain.TabSnapshot{
		ID:           "tab-1",
		Title:        "t1",
		ActivePaneID: "replay-1",
		Lanes: []domain.LaneSnapshot{
			{
				ID:           "lane-1",
				Flex:         1,
				ActivePaneID: "replay-1",
				PaneGroups: []domain.PaneGroupSnapshot{
					{ID: "g-1", RowFlex: 1, ActivePaneID: "replay-1", Panes: []domain.PaneSnapshot{rp}},
				},
			},
		},
	}
	m := Model{
		snapshot: domain.WorkspaceSnapshot{
			Tabs:         []domain.TabSnapshot{tab},
			ActiveTabID:  "tab-1",
			ActivePaneID: "replay-1",
		},
		mode:              modeNormal,
		width:             120,
		height:            40,
		inputs:            map[string]textinput.Model{},
		paneInputModes:    map[string]string{},
		panePastedText:    map[string]string{},
		paneHistoryIdx:    map[string]int{},
		paneHistorySaved:  map[string]string{},
		paneScrollOffsets: map[string]int{},
		pipelineOutputs:   map[string]string{},
		attentionState:    map[string]*attentionInfo{},
		pub:               pub,
		bus:               b,
		workspaceInbox:    msgpkg.T("ws", "inbox"),
	}
	m.recomputePaneRects()
	return m
}

// wsInboxNext reads and decodes the next envelope from a SubscribeSync
// subscription on the workspace inbox.
func wsInboxNext(t *testing.T, b *bus.Bus, subj string, run func()) interface{} {
	t.Helper()
	nc := b.Conn()
	sub, err := nc.SubscribeSync(subj)
	if err != nil {
		t.Fatalf("subscribe %s: %v", subj, err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	run()
	raw, err := sub.NextMsg(time.Second)
	if err != nil {
		t.Fatalf("expected a message on %s, got err: %v", subj, err)
	}
	var env msgpkg.NATSEnvelope
	if err := json.Unmarshal(raw.Data, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	decoded, err := b.Codecs().Decode(env.TypeTag, env.Payload)
	if err != nil {
		t.Fatalf("decode %q: %v", env.TypeTag, err)
	}
	return decoded
}

// TestReplayPaneControlKeysPublishControls: space / ←/→ / +/- in a focused
// replay pane publish the corresponding MsgReplayControl, stay in modeNormal,
// and never reach the pane's input line.
func TestReplayPaneControlKeysPublishControls(t *testing.T) {
	b := startTestBus(t)

	cases := []struct {
		key        tea.KeyMsg
		wantAction string
		wantDelta  int64
	}{
		{tea.KeyMsg{Type: tea.KeySpace}, "pause", 0},
		{tea.KeyMsg{Type: tea.KeyLeft}, "seek", -10_000},
		{tea.KeyMsg{Type: tea.KeyRight}, "seek", 10_000},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'+'}}, "faster", 0},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'-'}}, "slower", 0},
	}
	for _, tc := range cases {
		m := buildReplayModel(b)
		decoded := wsInboxNext(t, b, msgpkg.T("ws", "inbox"), func() {
			updated, _ := m.Update(tc.key)
			m = updated.(Model)
		})
		ctrl, ok := decoded.(*msgpkg.MsgReplayControl)
		if !ok {
			t.Fatalf("key %q: expected *MsgReplayControl, got %T", tc.key.String(), decoded)
		}
		if ctrl.Action != tc.wantAction || ctrl.DeltaMs != tc.wantDelta || ctrl.PaneID != "replay-1" {
			t.Fatalf("key %q: control = %+v, want action %q delta %d pane replay-1",
				tc.key.String(), ctrl, tc.wantAction, tc.wantDelta)
		}
		if m.mode != modeNormal {
			t.Fatalf("key %q: mode = %v, want modeNormal", tc.key.String(), m.mode)
		}
		// The key must not leak into the pane's input line.
		if got := m.inputs["replay-1"].Value(); got != "" {
			t.Fatalf("key %q leaked into the input line: %q", tc.key.String(), got)
		}
	}
}

// TestReplayPaneQClosesPane: q in a focused replay pane publishes MsgClosePane
// (the workspace stops the playback when the pane's actor stops).
func TestReplayPaneQClosesPane(t *testing.T) {
	b := startTestBus(t)
	m := buildReplayModel(b)

	decoded := wsInboxNext(t, b, msgpkg.T("ws", "inbox"), func() {
		updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
		m = updated.(Model)
	})
	if _, ok := decoded.(*msgpkg.MsgClosePane); !ok {
		t.Fatalf("expected *MsgClosePane, got %T", decoded)
	}
}

// TestReplayKeysOnlyClaimedForReplayPanes: the same keys on a normal pane are
// NOT intercepted — space types into the input line and publishes nothing.
func TestReplayKeysOnlyClaimedForReplayPanes(t *testing.T) {
	b := startTestBus(t)
	m := buildReplayModel(b)
	// Demote the focused pane to a normal pane.
	m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes[0].PaneType = ""

	nc := b.Conn()
	sub, err := nc.SubscribeSync(msgpkg.T("ws", "inbox"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	m = updated.(Model)
	if _, err := sub.NextMsg(150 * time.Millisecond); err == nil {
		t.Fatal("space on a normal pane must not publish a replay control")
	}

	// And a replay pane keeps the multiplexer chords: ctrl+p still enters
	// pane mode (never trap the user).
	m2 := buildReplayModel(b)
	updated, _ = m2.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
	m2 = updated.(Model)
	if m2.mode != modePane {
		t.Fatalf("ctrl+p on a replay pane: mode = %v, want modePane", m2.mode)
	}
}
