// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/vterm"
)

// PaneSharedOutputListenerActor subscribes to another pane's shared output
// NATS topic and forwards the text to the owner pane. Lines starting with
// "##>event:print:" are intercepted and the payload is forwarded separately.
// Regular (non-event) lines are prefixed with "[<targetAlias>] " so the user
// knows where the output is from.
//
// It is spawned as a child of PaneActor when the user runs ##pane listen.
type PaneSharedOutputListenerActor struct {
	ownerPaneID   string // the pane doing the listening (pane B)
	targetPaneID  string // the pane being listened to (pane A)
	targetAlias   string // display alias for the target pane
	pub           *msg.NATSPublisher
	nc            *nats.Conn
	br            *bridge.NATSBridge // bridge subscription to sharedOutput
	contextBuf    strings.Builder    // accumulated output from source pane for softdev context
	maxContextBuf int                // max size of context buffer (default 51200 = 50KB)
	remoteVTerm   *vterm.VTerm       // VTerm for interactive mode from target pane
	isInteractive bool               // target pane is in interactive mode
	// lastForwardedEvicted tracks how many of remoteVTerm's evicted scrollback
	// lines have been forwarded to the owner pane (so each is sent once).
	lastForwardedEvicted int64
}

// NewPaneSharedOutputListenerActor creates a PaneSharedOutputListenerActor.
func NewPaneSharedOutputListenerActor(
	ownerPaneID, targetPaneID, targetAlias string,
	pub *msg.NATSPublisher,
	nc *nats.Conn,
) *PaneSharedOutputListenerActor {
	return &PaneSharedOutputListenerActor{
		ownerPaneID:   ownerPaneID,
		targetPaneID:  targetPaneID,
		targetAlias:   targetAlias,
		pub:           pub,
		nc:            nc,
		maxContextBuf: 51200,
	}
}

// Receive implements actor.Actor.
func (l *PaneSharedOutputListenerActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		l.br = bridge.New(l.nc, ctx.Self(), ctx.ActorSystem(), l.pub.Codecs())
		l.br.SetPaneID(l.ownerPaneID)
		// Subscribe to per-mode output topics only (not merged).
		// SendConversation dual-publishes to both per-mode and merged topics,
		// so subscribing to both would cause duplicate forwarding.
		for _, suffix := range []string{"shell", "ai", "chat", "rysh", "external"} {
			sub := msg.T("pane", l.targetPaneID, "output", suffix)
			if err := l.br.AddSubject(sub); err != nil {
				slog.Error("pane-listener: subscribe per-mode failed",
					"owner", l.ownerPaneID, "target", l.targetPaneID, "suffix", suffix, "err", err)
			}
		}
		// Subscribe to per-mode history topics only (not merged).
		for _, suffix := range []string{"shell", "ai", "chat", "rysh", "external"} {
			sub := msg.T("pane", l.targetPaneID, "history", suffix)
			if err := l.br.AddSubject(sub); err != nil {
				slog.Error("pane-listener: subscribe history failed",
					"owner", l.ownerPaneID, "target", l.targetPaneID, "suffix", suffix, "err", err)
			}
		}
		// Subscribe to raw output and mode changes for interactive sharing.
		rawSubject := msg.T("pane", l.targetPaneID, "rawOutput")
		if err := l.br.AddSubject(rawSubject); err != nil {
			slog.Error("pane-listener: subscribe rawOutput failed",
				"owner", l.ownerPaneID, "target", l.targetPaneID, "err", err)
		}
		modeSubject := msg.T("pane", l.targetPaneID, "shareMode")
		if err := l.br.AddSubject(modeSubject); err != nil {
			slog.Error("pane-listener: subscribe shareMode failed",
				"owner", l.ownerPaneID, "target", l.targetPaneID, "err", err)
		}
		slog.Info("pane-listener: started",
			"owner", l.ownerPaneID, "target", l.targetPaneID)

	case *actor.Stopping:
		if l.br != nil {
			l.br.Stop()
			l.br = nil
		}
		slog.Info("pane-listener: stopped",
			"owner", l.ownerPaneID, "target", l.targetPaneID)

	case *msg.MsgPaneOutputAppend:
		l.handleOutput(m.Text)

	case *msg.MsgPaneShellOutputAppend:
		// Forward shell-specific output to owner's shell topic.
		_ = l.pub.SendPaneShellOutput(l.ownerPaneID, m.Text)

	case *msg.MsgPaneAIOutputAppend:
		// Forward AI-specific output to owner's AI topic.
		_ = l.pub.SendPaneAIOutput(l.ownerPaneID, m.Text)

	case *msg.MsgPaneChatOutputAppend:
		// Forward to owner's chat buffer (visible when owner is in chat mode).
		_ = l.pub.SendPaneChatOutput(l.ownerPaneID, m.Text)
		// Also forward to the merged output buffer so the message is visible
		// regardless of what input mode the owner pane is currently in.
		// Prefix with [alias] to identify the source, consistent with how
		// handleOutput prefixes all other forwarded messages.
		prefixed := fmt.Sprintf("[%s] %s", l.targetAlias, m.Text)
		_ = l.pub.SendPaneOutput(l.ownerPaneID, prefixed)

	case *msg.MsgPaneRyshOutputAppend:
		// Forward rysh output to owner's rysh topic.
		_ = l.pub.SendPaneRyshOutput(l.ownerPaneID, m.Text)

	// Unified conversation message forwarding (new).
	case *msg.MsgConversationAppend:
		// Check for pipeline events (##>) that need special handling
		// (softdev triggers, event:print, etc.) before forwarding.
		if m.Message != nil && strings.HasPrefix(m.Message.Content, "##>") {
			l.handleOutput(m.Message.Content)
		} else {
			_ = l.pub.SendConversation(l.ownerPaneID, m.Message)
		}

	case *msg.MsgConversationHistoryAppend:
		_ = l.pub.SendConversationHistory(l.ownerPaneID, m.Message)

	// Per-mode history forwarding.
	// The merged history entry (.history) is forwarded directly. Per-mode entries
	// (.history.shell, .history.ai) are forwarded to per-mode topics ONLY — not
	// dual-published — because the merged entry is already forwarded separately,
	// avoiding duplicate merged history entries in the owner pane.
	case *msg.MsgPaneHistoryAppend:
		_ = l.pub.SendPaneHistory(l.ownerPaneID, m.Entry)

	case *msg.MsgPaneShellHistoryAppend:
		_ = l.pub.Send(msg.T("pane", l.ownerPaneID, "history", "shell"), &msg.MsgPaneShellHistoryAppend{Entry: m.Entry})

	case *msg.MsgPaneAIHistoryAppend:
		_ = l.pub.Send(msg.T("pane", l.ownerPaneID, "history", "ai"), &msg.MsgPaneAIHistoryAppend{Entry: m.Entry})

	case *msg.MsgPaneChatHistoryAppend:
		_ = l.pub.SendPaneChatHistory(l.ownerPaneID, m.Entry)

	case *msg.MsgPaneRyshHistoryAppend:
		_ = l.pub.SendPaneRyshHistory(l.ownerPaneID, m.Entry)

	case *msg.MsgPaneRawOutputAppend:
		l.handleRawOutput(m)

	case *msg.MsgPaneShareModeChange:
		l.handleModeChange(m)
	}
}

// handleOutput processes text from the shared output topic line by line.
// This runs inside the actor's Receive, so sequential processing is guaranteed.
func (l *PaneSharedOutputListenerActor) handleOutput(text string) {
	if text == "" {
		return
	}

	lines := strings.Split(text, "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}

		const eventPrintPrefix = "##>event:print:"

		switch {
		case strings.HasPrefix(line, aiSoftdevPrefix):
			// AI softdev event — trigger LLMPromptExecutionActor with prompt.
			language, phase, userText, ok := ParseAISoftdevEvent(line)
			if !ok {
				_ = l.pub.SendPaneOutput(l.ownerPaneID,
					fmt.Sprintf("[%s] unrecognized ai:softdev event: %s\n", l.targetAlias, line))
				break
			}
			prompt, found := BuildSoftdevPrompt(language, phase, l.targetAlias, userText, l.contextBuf.String())
			if !found {
				slog.Error("pane-listener: softdev prompt not found",
					"owner", l.ownerPaneID, "target", l.targetPaneID,
					"language", language, "phase", phase,
					"key", language+":"+phase)
				_ = l.pub.SendPaneOutput(l.ownerPaneID,
					fmt.Sprintf("[%s] no prompt for ai:softdev event %s:%s\n", l.targetAlias, language, phase))
				break
			}
			_ = l.pub.SendPaneOutput(l.ownerPaneID,
				fmt.Sprintf("\n[%s] ⚡ ai:softdev event: %s:%s — triggering agentic prompt\n", l.targetAlias, language, phase))
			_ = l.pub.Send(
				msg.T("pane", l.ownerPaneID, "inbox"),
				&msg.MsgPaneExecPrompt{Prompt: prompt},
			)

		case strings.HasPrefix(line, shSoftdevPrefix):
			// SH softdev event — run shell command directly.
			language, phase, _, ok := ParseShSoftdevEvent(line)
			if !ok {
				_ = l.pub.SendPaneOutput(l.ownerPaneID,
					fmt.Sprintf("[%s] unrecognized sh:softdev event: %s\n", l.targetAlias, line))
				break
			}
			command, found := LookupSoftdevCommand(language, phase)
			if !found {
				_ = l.pub.SendPaneOutput(l.ownerPaneID,
					fmt.Sprintf("[%s] no command for sh:softdev event %s:%s\n", l.targetAlias, language, phase))
				break
			}
			_ = l.pub.SendPaneOutput(l.ownerPaneID,
				fmt.Sprintf("\n[%s] ⚙ sh:softdev event: %s:%s — running: %s\n", l.targetAlias, language, phase, command))
			_ = l.pub.Send(
				msg.T("pane", l.ownerPaneID, "inbox"),
				&msg.MsgPaneExecShell{Command: command},
			)

		case strings.HasPrefix(line, eventPrintPrefix):
			payload := strings.TrimPrefix(line, eventPrintPrefix)
			_ = l.pub.SendPaneOutput(l.ownerPaneID, payload+"\n")

		default:
			_ = l.pub.SendPaneOutput(l.ownerPaneID, line+"\n")
			// Accumulate non-event output as context for softdev events.
			l.appendContext(line)
		}
	}
}

// appendContext accumulates non-event output for softdev event context.
func (l *PaneSharedOutputListenerActor) appendContext(line string) {
	l.contextBuf.WriteString(line)
	l.contextBuf.WriteByte('\n')
	// Truncate from the front if over limit.
	if l.contextBuf.Len() > l.maxContextBuf {
		content := l.contextBuf.String()
		content = content[len(content)-l.maxContextBuf:]
		l.contextBuf.Reset()
		l.contextBuf.WriteString(content)
	}
}

// handleModeChange processes interactive mode transitions from the target pane.
func (l *PaneSharedOutputListenerActor) handleModeChange(m *msg.MsgPaneShareModeChange) {
	l.isInteractive = m.Interactive
	if m.Interactive && m.Rows > 0 && m.Cols > 0 {
		if l.remoteVTerm == nil {
			l.remoteVTerm = vterm.New(m.Rows, m.Cols)
			l.resetForwardedScrollback()
		} else {
			l.remoteVTerm.Resize(m.Rows, m.Cols)
		}
	}

	_ = l.pub.Send(msg.T("pane", l.ownerPaneID, "inbox"),
		&msg.MsgRemoteInteractiveModeChange{
			Interactive: m.Interactive,
			Rows:        m.Rows,
			Cols:        m.Cols,
		})
}

// handleRawOutput decodes base64 raw PTY bytes from the target pane, feeds
// them into VTerm, and pushes the rendered screen to the owning pane.
func (l *PaneSharedOutputListenerActor) handleRawOutput(m *msg.MsgPaneRawOutputAppend) {
	rawBytes, err := base64.StdEncoding.DecodeString(m.Data)
	if err != nil {
		return
	}

	if l.remoteVTerm == nil {
		l.remoteVTerm = vterm.New(24, 80)
		l.resetForwardedScrollback()
	}
	_, _ = l.remoteVTerm.Write(rawBytes)

	// Forward newly-evicted scrollback lines so the listening pane can scroll
	// the target program's history in copy mode.
	if total := l.remoteVTerm.ScrollbackEvictedTotal(); total > l.lastForwardedEvicted {
		if newRows := l.remoteVTerm.ScrollbackTailANSI(int(total - l.lastForwardedEvicted)); len(newRows) > 0 {
			_ = l.pub.Send(msg.T("pane", l.ownerPaneID, "inbox"),
				&msg.MsgRemoteScrollbackAppend{Rows: newRows})
		}
		l.lastForwardedEvicted = total
	}

	row, col := l.remoteVTerm.CursorPos()
	_ = l.pub.Send(msg.T("pane", l.ownerPaneID, "inbox"),
		&msg.MsgRemoteVTScreenUpdate{
			Screen:    l.remoteVTerm.Render(),
			CursorRow: row,
			CursorCol: col,
		})
}

// resetForwardedScrollback resets the forward cursor and tells the owner pane to
// discard any stale remote scrollback (called when a fresh remoteVTerm starts).
func (l *PaneSharedOutputListenerActor) resetForwardedScrollback() {
	l.lastForwardedEvicted = 0
	_ = l.pub.Send(msg.T("pane", l.ownerPaneID, "inbox"),
		&msg.MsgRemoteScrollbackAppend{Reset: true})
}
