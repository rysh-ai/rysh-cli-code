// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/peer"
	coord "github.com/rysh-ai/rysh-cli-code/internal/peercoord"
)

func (w *WorkspaceActor) peerKey(name string) string {
	if w.secrets != nil {
		scope, _ := w.secretWorkspaceScope()
		if value, _, ok := w.secrets.Get(scope, name); ok {
			return value
		}
	}
	return os.Getenv(name)
}

func (w *WorkspaceActor) handlePeerCommand(out *strings.Builder, pane string, args []string) error {
	if coord.IsCommand(args) {
		done := make(chan *msg.MsgCLIResponse, 1)
		pub := w.pub
		w.coordinationCommand(&msg.MsgCLIPeerCommand{AsPaneID: pane, Args: args}, func(value interface{}) error {
			response := value.(*msg.MsgCLIResponse)
			text := response.Output
			if !response.OK {
				text = "peer: " + response.Error + "\n"
			}
			_ = pub.SendPaneRyshOutput(pane, text)
			done <- response
			return nil
		})
		w.pendingUserCommand = func() (string, error) {
			response := <-done
			if !response.OK {
				return response.Output, fmt.Errorf("%s", response.Error)
			}
			return response.Output, nil
		}
		return nil
	}

	response := w.handleCLIPeerCommand(&msg.MsgCLIPeerCommand{AsPaneID: pane, Args: args})
	out.WriteString(response.Output)
	if !response.OK {
		w.failRysh("%s", response.Error)
		return fmt.Errorf("%s", response.Error)
	}
	return nil
}

// The agent-facing path resolves the sender without changing focus or echoing
// into a pane. Peer responses are never forwarded to ANSA or a shell.
func (w *WorkspaceActor) handleCLIPeerCommand(m *msg.MsgCLIPeerCommand) *msg.MsgCLIResponse {
	fail := func(err error) *msg.MsgCLIResponse { return &msg.MsgCLIResponse{Error: err.Error()} }
	if m == nil {
		return fail(fmt.Errorf("empty peer command"))
	}
	p, err := peer.Parse(m.Args)
	if err != nil {
		return fail(err)
	}
	if p.Verb == "watch" {
		return fail(fmt.Errorf("run rysh peer watch in a terminal; ##peer tail reads one page"))
	}
	connection, err := peer.Load(w.cfg.RyshDir, w.cfg.SessionName)
	if err != nil {
		return fail(fmt.Errorf("load peer connection: %w", err))
	}
	encode := func(value any) *msg.MsgCLIResponse {
		data, err := json.Marshal(value)
		if err != nil {
			return fail(err)
		}
		return &msg.MsgCLIResponse{OK: true, Output: string(data) + "\n"}
	}
	if p.Verb == "disconnect" {
		connection.Enabled = false
		if err := peer.Save(w.cfg.RyshDir, w.cfg.SessionName, connection); err != nil {
			return fail(err)
		}
		if w.peerRuntime != nil {
			w.peerRuntime.SetConnected(false)
		}
		return &msg.MsgCLIResponse{OK: true, Output: "peer board disconnected\n"}
	}
	if p.Verb == "status" && !connection.Enabled {
		if p.JSON {
			return encode(map[string]bool{"connected": false})
		}
		return &msg.MsgCLIResponse{OK: true, Output: "peer board disconnected; use ##peer connect\n"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if p.Verb == "connect" {
		board := p.Board
		if board == "" {
			board = "team"
		}
		identity := connection.SessionID
		if identity == "" {
			identity = uuid.NewString()
		}
		candidate := peer.Connection{Enabled: true, URL: strings.TrimRight(p.URL, "/"), Workspace: p.Workspace, Board: board, KeySecret: p.KeySecret, SessionID: identity}
		client := peer.Client{Connection: candidate, Key: w.peerKey(p.KeySecret)}
		if candidate.Workspace == "" {
			candidate.Workspace, err = client.ResolveWorkspace(ctx)
			if err != nil {
				return fail(err)
			}
			client.Connection = candidate
		}
		status, err := client.Status(ctx)
		if err != nil {
			return fail(err)
		}
		if err := peer.Save(w.cfg.RyshDir, w.cfg.SessionName, candidate); err != nil {
			return fail(err)
		}
		if w.peerRuntime != nil {
			w.peerRuntime.SetConnected(true)
		}
		return &msg.MsgCLIResponse{OK: true, Output: fmt.Sprintf("peer board connected: %s / %s\nworkspace=%s user=%s role=%s session=%s\n", candidate.URL, board, status.Workspace, status.User, status.Role, identity)}
	}
	if !connection.Enabled {
		return fail(fmt.Errorf("peer board disconnected; use ##peer connect"))
	}
	if p.Board != "" {
		connection.Board = p.Board
	}
	client := peer.Client{Connection: connection, Key: w.peerKey(connection.KeySecret)}
	switch p.Verb {
	case "status":
		status, err := client.Status(ctx)
		if err != nil {
			return fail(err)
		}
		if p.JSON {
			return encode(status)
		}
		return &msg.MsgCLIResponse{OK: true, Output: fmt.Sprintf("peer board connected: %s / %s\nworkspace=%s user=%s role=%s session=%s\n", connection.URL, status.Board, status.Workspace, status.User, status.Role, connection.SessionID)}
	case "tail":
		page, err := client.Tail(ctx, p.After, p.Limit)
		if err != nil {
			return fail(err)
		}
		if p.JSON {
			return encode(page)
		}
		var out strings.Builder
		for _, message := range page.Messages {
			out.WriteString(peer.FormatMessage(message))
			out.WriteByte('\n')
		}
		fmt.Fprintf(&out, "next=%d has_more=%t\n", page.Next, page.HasMore)
		return &msg.MsgCLIResponse{OK: true, Output: out.String()}
	case "post", "reply":
		if strings.TrimSpace(m.AsPaneID) == "" {
			return fail(fmt.Errorf("peer post needs --as <pane-id> or RYSH_PANE"))
		}
		paneID := w.resolvePaneID(m.AsPaneID)
		if paneID == "" {
			return fail(fmt.Errorf("peer sender pane not found"))
		}
		snapshot := w.paneSnapshotByID(paneID)
		if snapshot == nil {
			return fail(fmt.Errorf("peer sender pane is unavailable"))
		}
		id := p.ID
		if id == "" {
			id = uuid.NewString()
		}
		post := peer.Post{ID: id, ThreadID: p.Thread, SessionID: connection.SessionID, SessionName: w.cfg.SessionName, AgentID: paneID, AgentName: msg.BoardPersona(snapshot.GivenName, snapshot.Title, paneID), Text: p.Text}
		receipt, err := client.Send(ctx, post)
		if err != nil {
			return fail(fmt.Errorf("%w; retry the same message with --id %s", err, id))
		}
		if p.JSON {
			return encode(receipt)
		}
		return &msg.MsgCLIResponse{OK: true, ID: receipt.Message.ID, Output: fmt.Sprintf("peer message stored: board=%s sequence=%d id=%s thread=%s replayed=%t\n", connection.Board, receipt.Message.Sequence, receipt.Message.ID, receipt.Message.ThreadID, receipt.Replayed)}
	}
	return fail(fmt.Errorf("unsupported peer command"))
}
