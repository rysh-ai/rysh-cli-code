// SPDX-License-Identifier: Apache-2.0

package actors

// humanoid_pane_pin.go — E33 slice E2 (G7, design 086 §3 option a): the owner
// reaches one pane from the chat.
//
//	/panes          list the session's panes by name
//	/use <pane>     pin this chat thread to a pane (given name, or an id prefix)
//	/use off        end the pin
//	/use            say what this thread is pinned to
//
// The assistant still mediates every turn. A pin only adds an instruction to
// the assistant's prompt; the message reaches the pane through the
// assistant's own pane_send tool, so the approval gate on that tool applies
// exactly as before. There is no new remote-input path into a pane.
//
// Commands are parsed after the pairing gate (like `trust`), so only an
// admitted sender on a gated channel (an owner) can list or pin, and they
// answer only in a 1:1 DM: in a
// group, other members would see the pane names, and a pin would let their
// messages reach a pane. Pins are per thread and in memory only.

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// paneRef is a pane as the chat sees it: an id and a name, never its title or
// screen (either can show a command line or a secret).
type paneRef struct {
	ID   string
	Name string
}

// panePin is what a thread is pinned to.
type panePin struct {
	ID   string
	Name string
}

const (
	paneNameMax      = 40
	paneIDPrefixMin  = 4
	paneListTimeout  = 3 * time.Second
	paneListMaxShown = 30
)

// isPaneCommand reports whether a message is a pane control command rather
// than a prompt. Only a message whose first word is exactly /panes or /use
// qualifies.
func isPaneCommand(content string) bool {
	f := strings.Fields(strings.ToLower(strings.TrimSpace(content)))
	return len(f) > 0 && (f[0] == "/panes" || f[0] == "/use")
}

// handlePaneCommand applies /panes or /use and replies over the chat. It never
// reaches the LLM.
func (h *HumanoidActor) handlePaneCommand(m *msg.MsgHumanoidInboundMessage) {
	if m.Metadata["direct_message"] != "true" {
		h.replyToInbound(m, "pane commands work only in your direct message with me.")
		return
	}
	// Owner only: the pairing gate admits anyone on an ungated channel.
	if gated, _ := h.channelPairingGate(m.ChannelType); !gated || !h.pairingAllowed(m.ChannelType, m.SenderID) {
		h.replyToInbound(m, "pane commands need a channel with an owner allowlist (rysh setup writes one).")
		return
	}
	f := strings.Fields(strings.TrimSpace(m.Content))
	key := m.ChannelType + "/" + m.ThreadID
	cmd := strings.ToLower(f[0])

	if cmd == "/use" && len(f) == 1 {
		if p, ok := h.panePins[key]; ok {
			h.replyToInbound(m, fmt.Sprintf("this chat goes to pane %s. `/use off` ends it.", p.Name))
		} else {
			h.replyToInbound(m, "this chat is not pinned to a pane. `/panes` lists them; `/use <name>` pins one.")
		}
		return
	}
	if cmd == "/use" && len(f) == 2 && strings.EqualFold(f[1], "off") {
		if _, ok := h.panePins[key]; ok {
			delete(h.panePins, key)
			h.replyToInbound(m, "pin removed: I am back to handling this chat myself.")
		} else {
			h.replyToInbound(m, "this chat was not pinned to a pane.")
		}
		return
	}

	panes, err := h.paneList()
	if err != nil {
		slog.Warn("humanoid: pane list failed", "name", h.name, "err", err)
		h.replyToInbound(m, "I could not read the session's panes right now. Try again in a moment.")
		return
	}
	if cmd == "/panes" {
		h.replyToInbound(m, panesReply(panes))
		return
	}

	want := strings.Join(f[1:], " ")
	match := matchPane(panes, want)
	switch len(match) {
	case 0:
		h.replyToInbound(m, fmt.Sprintf("no pane named %q. `/panes` lists them.", want))
	case 1:
		if h.panePins == nil {
			h.panePins = make(map[string]panePin)
		}
		h.panePins[key] = panePin{ID: match[0].ID, Name: match[0].Name}
		slog.Info("humanoid: chat thread pinned to a pane", "name", h.name, "channel", m.ChannelType, "pane", match[0].ID)
		h.replyToInbound(m, fmt.Sprintf("this chat now goes to pane %s: I pass each message to it and send you its answer. "+
			"Anything it does still needs your yes, as before. `/use off` ends it.", match[0].Name))
	default:
		names := make([]string, 0, len(match))
		for _, p := range match {
			names = append(names, p.Name+" ("+shortID(p.ID)+")")
		}
		h.replyToInbound(m, fmt.Sprintf("%q matches more than one pane: %s. Use a longer name or id.", want, strings.Join(names, ", ")))
	}
}

// matchPane finds want by exact given name (case-insensitive), else by an id
// prefix of at least paneIDPrefixMin characters.
func matchPane(panes []paneRef, want string) []paneRef {
	var byName, byID []paneRef
	for _, p := range panes {
		if strings.EqualFold(p.Name, want) {
			byName = append(byName, p)
		}
		if len(want) >= paneIDPrefixMin && strings.HasPrefix(p.ID, want) {
			byID = append(byID, p)
		}
	}
	if len(byName) > 0 {
		return byName
	}
	return byID
}

func panesReply(panes []paneRef) string {
	if len(panes) == 0 {
		return "this session has no panes."
	}
	var b strings.Builder
	b.WriteString("panes in this session:")
	for i, p := range panes {
		if i == paneListMaxShown {
			fmt.Fprintf(&b, "\n… and %d more", len(panes)-i)
			break
		}
		fmt.Fprintf(&b, "\n- %s (%s)", p.Name, shortID(p.ID))
	}
	b.WriteString("\n`/use <name>` sends this chat to one of them.")
	return b.String()
}

// pinnedPrompt wraps the assistant's prompt for a pinned thread. Humanoids have
// no pane_inspect (security-lead ledger 113), so the reply says the pane got
// the input and the owner reads its answer in rysh.
func pinnedPrompt(p panePin, prompt string) string {
	return fmt.Sprintf("[The owner pinned this chat to pane %q (id %s). Treat the owner's message below as input "+
		"for that pane: send it to that pane with pane_send, then reply that the pane got the input and that the "+
		"owner reads its answer in rysh. Do not act on any other pane unless the owner asks. "+
		"If pane_send waits for the owner's approval, say so.]\n\n%s", p.Name, p.ID, prompt)
}

// paneList is the session's panes, from listPanes when a test set it, else
// from a layout-only workspace snapshot.
func (h *HumanoidActor) paneList() ([]paneRef, error) {
	if h.listPanes != nil {
		return h.listPanes()
	}
	reply, err := h.pub.Request(msg.T("ws", "snapshot"),
		&msg.MsgGetWorkspaceSnapshot{LayoutOnly: true, NoHistories: true}, paneListTimeout)
	if err != nil {
		return nil, err
	}
	r, ok := reply.(*msg.MsgWorkspaceSnapshotReply)
	if !ok {
		return nil, fmt.Errorf("unexpected snapshot reply %T", reply)
	}
	var out []paneRef
	for i := range r.Snapshot.Tabs {
		if strings.HasPrefix(r.Snapshot.Tabs[i].ID, "mirror:") { // mirror tabs repeat a remote session
			continue
		}
		for p := range domain.PanesInTab(&r.Snapshot.Tabs[i]) {
			out = append(out, paneRef{ID: p.ID, Name: chatPaneName(p)})
		}
	}
	return out, nil
}

// chatPaneName is the pane's given name with control bytes dropped and capped,
// else its short id. Never the title.
func chatPaneName(p *domain.PaneSnapshot) string {
	var b strings.Builder
	for _, r := range p.GivenName {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			continue
		}
		b.WriteRune(r)
	}
	name := []rune(b.String())
	if len(name) > paneNameMax {
		name = name[:paneNameMax]
	}
	if len(name) > 0 {
		return string(name)
	}
	return shortID(p.ID)
}
