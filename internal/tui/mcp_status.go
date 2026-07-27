package tui

// Live MCP restart-state surface (follow-up 6b).
//
// The MCP manager fires a StatusEvent on every server state transition; the
// daemon forwards it as MsgMCPStatus on the session-global T("mcp","status")
// subject. The TUI subscribes here, folds each transition into a per-server
// map, and renders a compact footer segment (e.g. "[mcp:github] reconnecting
// 2/20"). Healthy ("connected") and removed servers are dropped from the map
// so the footer only ever shows servers that need attention.
//
// Like the content plane, subscription callbacks never touch Model state
// directly — they push onto a buffered channel that a listen command turns
// into an mcpStatusMsg processed in Update (single-threaded tea loop), so the
// map is never raced against View().

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// mcpServerStatus is the TUI's latest known state for one MCP server.
type mcpServerStatus struct {
	phase   string // reconnecting | given_up | disconnected (connected/removed are dropped)
	attempt int
	max     int
	detail  string
	updated time.Time
}

// mcpStatusMsg carries one decoded MCP status transition into Update.
type mcpStatusMsg struct{ st msgpkg.MsgMCPStatus }

// setupMCPStatusSubscription subscribes to the session-global MCP status topic
// and forwards decoded transitions onto a buffered channel (drop-on-full; the
// manager re-emits on the next transition, so a dropped intermediate frame
// self-heals).
func setupMCPStatusSubscription(b *bus.Bus) chan msgpkg.MsgMCPStatus {
	ch := make(chan msgpkg.MsgMCPStatus, 64)
	codecs := b.Codecs()
	conn := b.Conn()
	_, _ = conn.Subscribe(msgpkg.T("mcp", "status"), func(natMsg *nats.Msg) {
		var env msgpkg.NATSEnvelope
		if json.Unmarshal(natMsg.Data, &env) != nil {
			return
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		st, ok := decoded.(*msgpkg.MsgMCPStatus)
		if !ok || st == nil {
			return
		}
		select {
		case ch <- *st:
		default: // dropped — self-heals on next transition
		}
	})
	return ch
}

// listenMCPStatusCmd blocks for the next MCP status transition and yields an
// mcpStatusMsg. Re-armed after each signal (see Update).
func (m Model) listenMCPStatusCmd() tea.Cmd {
	ch := m.mcpStatusCh
	return func() tea.Msg {
		st := <-ch
		return mcpStatusMsg{st: st}
	}
}

// applyMCPStatus folds one transition into the per-server map. Connected and
// removed servers are healthy/gone, so they are dropped (nothing to show);
// every other phase is stored for the footer.
func (m *Model) applyMCPStatus(st msgpkg.MsgMCPStatus) {
	if st.Server == "" {
		return
	}
	switch st.Phase {
	case "connected", "removed":
		delete(m.mcpServers, st.Server)
	default:
		m.mcpServers[st.Server] = mcpServerStatus{
			phase:   st.Phase,
			attempt: st.Attempt,
			max:     st.Max,
			detail:  st.Detail,
			updated: time.Now(),
		}
	}
}

var (
	mcpReconnectingStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))           // yellow
	mcpGaveUpStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true) // red
	mcpDisconnectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))            // red
	mcpOtherStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))          // grey
)

// mcpStatus renders the footer indicator for any MCP server not in the healthy
// "connected" state, or "" when every server is healthy. Servers are listed in
// name order so the segment is stable frame-to-frame.
func (m Model) mcpStatus() string {
	if len(m.mcpServers) == 0 {
		return ""
	}
	names := make([]string, 0, len(m.mcpServers))
	for n := range m.mcpServers {
		names = append(names, n)
	}
	sort.Strings(names)

	segs := make([]string, 0, len(names))
	for _, n := range names {
		s := m.mcpServers[n]
		switch s.phase {
		case "reconnecting":
			if s.max > 0 {
				segs = append(segs, mcpReconnectingStyle.Render(fmt.Sprintf("[mcp:%s] reconnecting %d/%d", n, s.attempt, s.max)))
			} else {
				segs = append(segs, mcpReconnectingStyle.Render(fmt.Sprintf("[mcp:%s] reconnecting", n)))
			}
		case "given_up":
			segs = append(segs, mcpGaveUpStyle.Render(fmt.Sprintf("[mcp:%s] ✗ gave up", n)))
		case "disconnected":
			segs = append(segs, mcpDisconnectedStyle.Render(fmt.Sprintf("[mcp:%s] disconnected", n)))
		default:
			segs = append(segs, mcpOtherStyle.Render(fmt.Sprintf("[mcp:%s] %s", n, s.phase)))
		}
	}
	return strings.Join(segs, " ")
}
