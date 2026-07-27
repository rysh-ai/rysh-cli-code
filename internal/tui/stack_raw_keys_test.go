package tui

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// freePort returns an OS-assigned free TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// startTestBus spins up a real *bus.Bus backed by a fresh embedded NATS server
// (own ephemeral port + temp data dir) so both sendMsg and refreshCmd resolve.
func startTestBus(t *testing.T) *bus.Bus {
	t.Helper()
	b, err := bus.New(bus.Config{
		Mode:        "embedded",
		SessionName: "tui-stack-test",
		Port:        freePort(t),
		DataDir:     t.TempDir(),
	})
	if err != nil {
		t.Fatalf("bus.New: %v", err)
	}
	t.Cleanup(b.Close)
	return b
}

// buildStackedRawModel builds a Model with ONE lane / ONE pane group that holds
// TWO stacked panes. The active pane has RawMode=true (claude on the alt
// screen). The model starts in modeRaw, mimicking syncRawMode's transition for
// a stacked interactive pane. No relay is started (stacked layouts never relay).
func buildStackedRawModel(b *bus.Bus) Model {
	var pub *msgpkg.NATSPublisher
	if b != nil {
		pub = b.Publisher()
	}
	p1 := domain.PaneSnapshot{ID: "pane-1", Title: "claude", Status: "running", ProviderName: "claude", Flex: 1, RawMode: true}
	p2 := domain.PaneSnapshot{ID: "pane-2", Title: "shell", Status: "ready", ProviderName: "shell", Flex: 1}
	tab := domain.TabSnapshot{
		ID:           "tab-1",
		Title:        "t1",
		ActivePaneID: "pane-1",
		Lanes: []domain.LaneSnapshot{
			{
				ID:           "lane-1",
				Flex:         1,
				ActivePaneID: "pane-1",
				PaneGroups: []domain.PaneGroupSnapshot{
					{ID: "g-1", RowFlex: 1, ActivePaneID: "pane-1", Panes: []domain.PaneSnapshot{p1, p2}},
				},
			},
		},
	}
	m := Model{
		snapshot: domain.WorkspaceSnapshot{
			Tabs:         []domain.TabSnapshot{tab},
			ActiveTabID:  "tab-1",
			ActivePaneID: "pane-1",
		},
		mode:              modeRaw,
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

// TestRawModeCtrlSEntersStackMode is the load-bearing claim: while a stacked
// pane runs claude (RawMode=true) and the TUI is in modeRaw, pressing Ctrl+S
// must switch to modeStack. This path requires NO NATS (no send happens).
func TestRawModeCtrlSEntersStackMode(t *testing.T) {
	m := buildStackedRawModel(nil)
	if m.mode != modeRaw {
		t.Fatalf("precondition: expected modeRaw, got %q", m.mode)
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	got := updated.(Model)
	if got.mode != modeStack {
		t.Fatalf("after ctrl+s in modeRaw: expected mode %q, got %q", modeStack, got.mode)
	}
}

// TestStackModeUpRotatesPrev verifies that, once in modeStack, pressing Up
// publishes MsgStackedPaneRotate{DirPrev} to the workspace inbox. Uses an
// in-process NATS publisher so the real send path runs.
func TestStackModeUpRotatesPrev(t *testing.T) {
	b := startTestBus(t)
	nc := b.Conn()
	codecs := b.Codecs()

	sub, err := nc.SubscribeSync(msgpkg.T("ws", "inbox"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	m := buildStackedRawModel(b)

	// Step 1: Ctrl+S from modeRaw -> modeStack.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = updated.(Model)
	if m.mode != modeStack {
		t.Fatalf("after ctrl+s: expected modeStack, got %q", m.mode)
	}

	// Step 2: Up -> publish MsgStackedPaneRotate{DirPrev}. updateStackMode
	// synchronously sends the rotate (model_update.go:723) and then builds a
	// refreshCmd (model_update.go:724) which reads m.bus — hence the model is
	// backed by a real *bus.Bus here.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = updated.(Model)
	if m.mode != modeStack {
		t.Fatalf("after up: expected to stay in modeStack, got %q", m.mode)
	}

	// Drain the inbox and assert a MsgStackedPaneRotate{DirPrev} was published.
	raw, err := sub.NextMsg(time.Second)
	if err != nil {
		t.Fatalf("expected a published message on ws.inbox, got err: %v", err)
	}
	var env msgpkg.NATSEnvelope
	if err := json.Unmarshal(raw.Data, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	decoded, err := codecs.Decode(env.TypeTag, env.Payload)
	if err != nil {
		t.Fatalf("decode %q: %v", env.TypeTag, err)
	}
	rot, ok := decoded.(*msgpkg.MsgStackedPaneRotate)
	if !ok {
		t.Fatalf("expected *MsgStackedPaneRotate, got %T", decoded)
	}
	if rot.Direction != msgpkg.DirPrev {
		t.Fatalf("expected DirPrev, got %q", rot.Direction)
	}
}

// TestStackModeDigitSelectsByIndex verifies that, once in modeStack, pressing a
// digit publishes MsgStackedPaneSelect with the 0-based index (digit - 1) and
// stays in stack mode so further keys can be pressed.
func TestStackModeDigitSelectsByIndex(t *testing.T) {
	b := startTestBus(t)
	nc := b.Conn()
	codecs := b.Codecs()

	sub, err := nc.SubscribeSync(msgpkg.T("ws", "inbox"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	m := buildStackedRawModel(b)

	// Ctrl+S from modeRaw -> modeStack.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	m = updated.(Model)
	if m.mode != modeStack {
		t.Fatalf("after ctrl+s: expected modeStack, got %q", m.mode)
	}

	// Press "3" -> publish MsgStackedPaneSelect{Index: 2}, staying in stack mode.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	m = updated.(Model)
	if m.mode != modeStack {
		t.Fatalf("after digit: expected to stay in modeStack, got %q", m.mode)
	}

	raw, err := sub.NextMsg(time.Second)
	if err != nil {
		t.Fatalf("expected a published message on ws.inbox, got err: %v", err)
	}
	var env msgpkg.NATSEnvelope
	if err := json.Unmarshal(raw.Data, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	decoded, err := codecs.Decode(env.TypeTag, env.Payload)
	if err != nil {
		t.Fatalf("decode %q: %v", env.TypeTag, err)
	}
	sel, ok := decoded.(*msgpkg.MsgStackedPaneSelect)
	if !ok {
		t.Fatalf("expected *MsgStackedPaneSelect, got %T", decoded)
	}
	if sel.Index != 2 {
		t.Fatalf("expected Index 2 (digit 3 -> 0-based 2), got %d", sel.Index)
	}
}
