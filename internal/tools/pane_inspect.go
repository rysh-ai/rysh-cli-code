// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// PaneInspectTool reads another pane's output buffer or status.
type PaneInspectTool struct {
	nc      *nats.Conn
	session string
}

// PaneInspectParams holds the parameters for inspecting a pane.
type PaneInspectParams struct {
	PaneID    string `json:"pane_id"`              // target pane ID
	TailLines int    `json:"tail_lines,omitempty"` // number of lines from the end (default 50)
}

// NewPaneInspectTool creates a PaneInspectTool.
func NewPaneInspectTool(nc *nats.Conn, session string) *PaneInspectTool {
	return &PaneInspectTool{nc: nc, session: session}
}

// Spec returns the tool specification for the LLM.
func (t *PaneInspectTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "pane_id": {"type": "string", "description": "The UUID of the pane to inspect"},
    "tail_lines": {"type": "integer", "description": "Number of lines to read from the end of the output buffer (default 50, max 500)"}
  },
  "required": ["pane_id"]
}`)
	return ToolSpec{
		Name:             "pane_inspect",
		Description:      "Read another pane's output buffer and status. Enables cross-pane awareness for multi-agent coordination.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval always returns false — read-only.
func (t *PaneInspectTool) RequiresApproval(_ json.RawMessage) bool {
	return false
}

// Execute requests a snapshot from the target pane via NATS.
func (t *PaneInspectTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p PaneInspectParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid pane_inspect params: %w", err)
	}

	if p.PaneID == "" {
		return ErrOutput(ErrKindValidation, "pane_id is required"), nil
	}

	tailLines := p.TailLines
	if tailLines <= 0 {
		tailLines = 50
	}
	if tailLines > 500 {
		tailLines = 500
	}

	if t.nc == nil {
		return ErrOutput(ErrKindMissing, "NATS connection not available"), nil
	}

	// Request pane snapshot via NATS request/reply. The pane's bridge decodes
	// by TypeTag and drops a tag the codec registry does not know, so the
	// tag must be the registered request type (an unknown one times out).
	subject := msg.T("pane", p.PaneID, "snapshot")
	envelope := msg.NATSEnvelope{
		TypeTag: msg.TagGetPaneSnapshot,
		Payload: []byte("{}"),
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return ErrOutputf(ErrKindInternal, "failed to marshal request: %v", err), nil
	}

	reply, err := t.nc.Request(subject, data, 5*time.Second)
	if err != nil {
		return ErrOutputf(ErrKindTransient, "pane %s not responding (may not exist): %v", p.PaneID, err), nil
	}

	// Parse the snapshot response.
	var respEnvelope msg.NATSEnvelope
	if err := json.Unmarshal(reply.Data, &respEnvelope); err != nil {
		return ErrOutputf(ErrKindValidation, "invalid response from pane: %v", err), nil
	}

	// Answer with a compact view: who the pane is, its state, and the last
	// tailLines lines of its current output. The rest of the snapshot (other
	// modes' output, histories, conversations) is not what the agent asked
	// for and does not reach the model.
	var snapReply msg.MsgPaneSnapshotReply
	if err := json.Unmarshal(respEnvelope.Payload, &snapReply); err != nil {
		return ErrOutputf(ErrKindValidation, "invalid snapshot from pane: %v", err), nil
	}
	snap := snapReply.Snapshot
	tail, truncated := lastLines(snap.Output, tailLines)
	b, err := json.MarshalIndent(paneInspectView{
		PaneID: p.PaneID, Title: snap.Title, Mode: snap.Mode, Status: snap.Status,
		Program: snap.Program, LastCommand: snap.LastCommand,
		OutputTail: tail, Truncated: truncated,
	}, "", "  ")
	if err != nil {
		return ErrOutputf(ErrKindInternal, "failed to encode the answer: %v", err), nil
	}
	content := string(b)

	return &ToolOutput{
		Content: content,
		Metadata: map[string]string{
			"pane_id":    p.PaneID,
			"tail_lines": fmt.Sprintf("%d", tailLines),
		},
	}, nil
}

// paneInspectView is what pane_inspect returns to the model.
type paneInspectView struct {
	PaneID      string `json:"pane_id"`
	Title       string `json:"title,omitempty"`
	Mode        string `json:"mode,omitempty"`
	Status      string `json:"status,omitempty"`
	Program     string `json:"program,omitempty"`
	LastCommand string `json:"last_command,omitempty"`
	OutputTail  string `json:"output_tail"`
	Truncated   bool   `json:"truncated"`
}

// paneInspectMaxTailBytes caps output_tail: a single huge line (a minified
// file, a progress bar without newlines) must not flood the model.
const paneInspectMaxTailBytes = 64 << 10

// lastLines returns the last n lines of s (a trailing newline does not count
// as an extra line), at most paneInspectMaxTailBytes of them (the end is
// kept), and whether anything before them was cut.
func lastLines(s string, n int) (string, bool) {
	if n <= 0 {
		return "", s != ""
	}
	body := strings.TrimSuffix(s, "\n")
	out, cut := s, false
	if body != "" {
		idx := len(body)
		for i := 0; i < n; i++ {
			j := strings.LastIndexByte(body[:idx], '\n')
			if j < 0 {
				idx = -1
				break
			}
			idx = j
		}
		if idx >= 0 {
			out, cut = s[idx+1:], true
		}
	}
	if len(out) > paneInspectMaxTailBytes {
		out, cut = out[len(out)-paneInspectMaxTailBytes:], true
	}
	return out, cut
}
