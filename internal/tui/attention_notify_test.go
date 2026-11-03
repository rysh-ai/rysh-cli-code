// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestAttentionNotifyBytesForAPrompt(t *testing.T) {
	cases := []struct {
		mode string
		want string
	}{
		{config.NotifyTerminalOff, ""},
		{"", ""},
		{config.NotifyTerminalBell, "\a"},
		{config.NotifyTerminalOSC9, "\x1b]9;rysh: builder needs you\a"},
	}
	for _, c := range cases {
		if got := attentionNotifyBytes(c.mode, msgpkg.AttentionPrompt, "builder"); got != c.want {
			t.Errorf("mode %q: got %q, want %q", c.mode, got, c.want)
		}
	}
}

// The existing categories (approval, channels) keep their bell and iTerm2
// request whatever the new setting says: P2 must not change them.
func TestAttentionNotifyBytesLeavesOtherCategoriesAlone(t *testing.T) {
	want := "\a\x1b]1337;RequestAttention=yes\a"
	for _, mode := range []string{config.NotifyTerminalOff, config.NotifyTerminalBell, config.NotifyTerminalOSC9} {
		if got := attentionNotifyBytes(mode, msgpkg.AttentionApproval, "x"); got != want {
			t.Errorf("approval under %q: got %q, want %q", mode, got, want)
		}
	}
}

// The label goes into a terminal escape: control bytes in it could end the
// sequence early or start a new one, so they are removed, and it is capped.
func TestAttentionNotifyLabelIsSanitised(t *testing.T) {
	got := attentionNotifyBytes(config.NotifyTerminalOSC9, msgpkg.AttentionPrompt, "a\x07b\x1b]9;evil\nc"+strings.Repeat("z", 200))
	if !strings.HasPrefix(got, "\x1b]9;rysh: a") || !strings.HasSuffix(got, "\a") {
		t.Fatalf("not an OSC 9 notification for the label: %q", got)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(got, "\x1b]9;"), "\a")
	if strings.ContainsAny(body, "\x07\x1b\n\r") {
		t.Fatalf("control bytes left in the notification: %q", got)
	}
	if len(body) > 80 {
		t.Fatalf("label not capped: %d bytes", len(body))
	}
}

func attnModel(mode string, out *bytes.Buffer) Model {
	return Model{
		cfg:               config.Config{NotifyTerminal: mode},
		attentionState:    map[string]*attentionInfo{},
		attentionLastBell: map[string]time.Time{},
		attentionOut:      out,
		snapshot: domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{{ID: "t", Lanes: []domain.LaneSnapshot{{
			ID: "l", PaneGroups: []domain.PaneGroupSnapshot{{ID: "g", Panes: []domain.PaneSnapshot{{ID: "p1", GivenName: "builder"}}}},
		}}}}},
	}
}

func TestPromptEventMarksThePaneAndAckClearsIt(t *testing.T) {
	var out bytes.Buffer
	m := attnModel(config.NotifyTerminalOSC9, &out)

	next, _ := m.Update(attentionEventMsg{Event: &msgpkg.MsgAttentionEvent{PaneID: "p1", Category: msgpkg.AttentionPrompt, Priority: msgpkg.AttentionPriorityHigh}})
	m = next.(Model)
	if info := m.attentionState["p1"]; info == nil || info.Count != 1 || info.Category != msgpkg.AttentionPrompt {
		t.Fatalf("attention state = %+v", m.attentionState["p1"])
	}
	if got := out.String(); got != "\x1b]9;rysh: builder needs you\a" {
		t.Fatalf("terminal output = %q", got)
	}

	next, _ = m.Update(attentionEventMsg{Ack: &msgpkg.MsgAttentionAck{PaneID: "p1"}})
	m = next.(Model)
	if _, ok := m.attentionState["p1"]; ok {
		t.Fatal("ack did not clear the pane's attention")
	}
}

func TestPromptEventIsSilentByDefault(t *testing.T) {
	var out bytes.Buffer
	m := attnModel(config.NotifyTerminalOff, &out)
	next, _ := m.Update(attentionEventMsg{Event: &msgpkg.MsgAttentionEvent{PaneID: "p1", Category: msgpkg.AttentionPrompt}})
	m = next.(Model)
	if out.Len() != 0 {
		t.Fatalf("notify off wrote %q", out.String())
	}
	if m.attentionState["p1"] == nil {
		t.Fatal("the pane is still marked even with notify off")
	}
}

func TestPromptHasItsOwnIcon(t *testing.T) {
	var m Model
	if m.attentionIcon(msgpkg.AttentionPrompt) == m.attentionIcon(msgpkg.AttentionCategory("unknown")) {
		t.Fatal("prompt uses the default icon")
	}
}
