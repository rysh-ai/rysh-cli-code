// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// Non-printing session calls for `rysh mcp serve` (design 067). Each one is the
// same request an existing command makes — the layout snapshot `rysh pane list`
// reads, the VT frame the fan-out helper reads, the pane inbox `rysh send`
// writes — handed back as data instead of printed.

// mcpPaneTimeout bounds a per-pane read. The pane answers from memory, so the
// reasoning is boardQueryTimeout's.
const mcpPaneTimeout = 5 * time.Second

// Snapshot returns the session's structural snapshot: ids, titles, status,
// foreground program and pane meta, with no output buffers and no histories.
func Snapshot(store *session.Store, sessionName string) (*domain.WorkspaceSnapshot, error) {
	c, err := connect(store, sessionName)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return getSnapshot(c)
}

// PaneOutput returns one pane's snapshot with its output buffers and without
// its command histories.
func PaneOutput(store *session.Store, sessionName, paneID string) (*domain.PaneSnapshot, error) {
	c, err := connect(store, sessionName)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	reply, err := c.RequestFromSubject(msg.T("pane", paneID, "snapshot"), &msg.MsgGetPaneSnapshot{NoHistories: true}, mcpPaneTimeout)
	if err != nil {
		return nil, fmt.Errorf("pane %s did not answer: %w", paneID, err)
	}
	r, ok := reply.(*msg.MsgPaneSnapshotReply)
	if !ok {
		return nil, fmt.Errorf("unexpected pane snapshot reply type: %T", reply)
	}
	return &r.Snapshot, nil
}

// PaneScreen returns a pane's live VT frame. Interactive is false when no
// interactive program owns the screen.
func PaneScreen(store *session.Store, sessionName, paneID string) (*msg.MsgPaneVTReply, error) {
	c, err := connect(store, sessionName)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	reply, err := c.RequestFromSubject(msg.T("pane", paneID, "snapshot"), &msg.MsgGetPaneVT{}, mcpPaneTimeout)
	if err != nil {
		return nil, fmt.Errorf("pane %s did not answer: %w", paneID, err)
	}
	r, ok := reply.(*msg.MsgPaneVTReply)
	if !ok {
		return nil, fmt.Errorf("unexpected pane VT reply type: %T", reply)
	}
	return r, nil
}

// SendPaneInput is PaneSendInput without the printing: the same typed message
// to the same pane inbox. It is never ansa, which clears a human's composer.
func SendPaneInput(store *session.Store, sessionName, paneID, text, mode string) error {
	if paneID == "" {
		return fmt.Errorf("a pane id is required")
	}
	c, err := connect(store, sessionName)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.SendToSubject(msg.T("pane", paneID, "inbox"), paneExecMessage(text, mode)); err != nil {
		return fmt.Errorf("send input: %w", err)
	}
	return nil
}

// SubmitInputMessage is the workspace-inbox message for input a CLI command
// submits. Nobody typed it at a pane, so it is Programmatic: it moves nobody's
// focus and a ##memory add it carries is recorded as source: agent.
func SubmitInputMessage(text, mode string) *msg.MsgSubmitInput {
	return &msg.MsgSubmitInput{Text: text, Mode: mode, Programmatic: true}
}

// paneExecMessage is the message a pane inbox takes for one line of input.
// PaneActor only understands MsgPaneExecShell / MsgPaneExecPrompt.
func paneExecMessage(text, mode string) interface{} {
	if mode == "prompt" {
		return &msg.MsgPaneExecPrompt{Prompt: text}
	}
	return &msg.MsgPaneExecShell{Command: text}
}

// MCPGate asks the workspace whether an MCP write may run (and audits it).
func MCPGate(store *session.Store, sessionName string, req *msg.MsgCLIMCPGate) (*msg.MsgCLIResponse, error) {
	c, err := connect(store, sessionName)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return cliRequest(c, req)
}

// Humanoids asks the humanoid registry what humanoids and channels exist.
func Humanoids(store *session.Store, sessionName string) ([]msg.HumanoidInfo, error) {
	c, err := connect(store, sessionName)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	reply, err := c.RequestFromSubject(msg.T("humanoid", "registry", "inbox"), &msg.MsgHumanoidList{}, mcpPaneTimeout)
	if err != nil {
		return nil, fmt.Errorf("humanoid registry did not answer: %w", err)
	}
	r, ok := reply.(*msg.MsgHumanoidListReply)
	if !ok {
		return nil, fmt.Errorf("unexpected humanoid list reply type: %T", reply)
	}
	return r.Humanoids, nil
}

// HumanoidMCPSend asks one humanoid to send (or, under human governance,
// draft) a channel message.
func HumanoidMCPSend(store *session.Store, sessionName, humanoid string, req *msg.MsgHumanoidMCPSend) (*msg.MsgHumanoidMCPSendReply, error) {
	c, err := connect(store, sessionName)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	reply, err := c.RequestFromSubject(msg.T("humanoid", humanoid, "inbox"), req, defaultTimeout)
	if err != nil {
		return nil, fmt.Errorf("humanoid %s did not answer: %w", humanoid, err)
	}
	r, ok := reply.(*msg.MsgHumanoidMCPSendReply)
	if !ok {
		return nil, fmt.Errorf("unexpected humanoid reply type: %T", reply)
	}
	return r, nil
}

// WatchPaneProcess delivers a pane's foreground-program events (start/exit)
// until stop is called.
func WatchPaneProcess(store *session.Store, sessionName, paneID string) (<-chan msg.MsgPaneProcess, func(), error) {
	c, err := connect(store, sessionName)
	if err != nil {
		return nil, nil, err
	}
	ch := make(chan msg.MsgPaneProcess, 8)
	sub, err := c.Subscribe(msg.T("pane", paneID, "process"), func(v interface{}) {
		if p, ok := v.(*msg.MsgPaneProcess); ok {
			select {
			case ch <- *p:
			default:
			}
		}
	})
	if err != nil {
		c.Close()
		return nil, nil, err
	}
	return ch, func() { _ = sub.Unsubscribe(); c.Close() }, nil
}
