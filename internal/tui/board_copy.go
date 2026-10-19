// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"encoding/json"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The TUI's half of `##board copy` (the board-copy task).
//
// The daemon runs the command — asks the recorder, selects, formats — because
// that is where `##` commands run and where every caller (this TUI, the web
// app, `rysh send`) can reach it. It cannot do the last hop: OSC 52 has to go
// down THIS terminal's tty to reach the user's local clipboard, which over SSH
// is the only clipboard that matters. So the daemon asks, on
// msgpkg.ClipboardWriteSubject, and this file answers.
//
// It writes and answers from the subscription goroutine rather than via
// Update: the daemon is blocked on the answer, and routing it through the
// event loop would make a busy frame look like "no terminal attached", sending
// the daemon to its fallback for a copy this TUI could have made. The status
// flash is the only part that needs the Model, and it goes through a channel.

// clipboardFlashMsg carries a daemon-requested copy's outcome into Update.
type clipboardFlashMsg struct{ text string }

// setupClipboardWriteSubscription answers the daemon's clipboard requests.
// Returns a never-delivering channel when there is no bus.
func setupClipboardWriteSubscription(b *bus.Bus) chan clipboardFlashMsg {
	ch := make(chan clipboardFlashMsg, 4)
	if b == nil || b.Conn() == nil {
		return ch
	}
	codecs := b.Codecs()
	pub := b.Publisher()
	_, _ = b.Conn().Subscribe(msgpkg.ClipboardWriteSubject(), func(n *nats.Msg) {
		var env msgpkg.NATSEnvelope
		if json.Unmarshal(n.Data, &env) != nil {
			return
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		req, ok := decoded.(*msgpkg.MsgClipboardWrite)
		if !ok {
			return
		}
		resp, flash := answerClipboardWrite(req)
		if env.ReplyTo != "" && pub != nil {
			_ = pub.Send(env.ReplyTo, resp)
		}
		select {
		case ch <- clipboardFlashMsg{text: flash}:
		default:
		}
	})
	return ch
}

// answerClipboardWrite performs one daemon-requested copy through the TUI's
// one clipboard path and builds the answer. Split out so it is testable
// without a bus.
func answerClipboardWrite(req *msgpkg.MsgClipboardWrite) (*msgpkg.MsgCLIResponse, string) {
	if req.Text == "" {
		// The daemon never sends an empty copy; refusing one here keeps a
		// future caller from clearing somebody's clipboard by accident.
		return &msgpkg.MsgCLIResponse{OK: false, Error: "empty copy refused"}, ""
	}
	if err := writeClipboard(req.Text); err != nil {
		return &msgpkg.MsgCLIResponse{OK: false, Error: err.Error()}, "clipboard error: " + err.Error()
	}
	return &msgpkg.MsgCLIResponse{OK: true, Output: "terminal clipboard (OSC 52)"},
		"[board] copied to clipboard"
}

// listenClipboardFlashCmd re-arms the flash channel, the listenAttentionEvents
// idiom.
func (m Model) listenClipboardFlashCmd() tea.Cmd {
	ch := m.clipboardFlashCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg { return <-ch }
}
