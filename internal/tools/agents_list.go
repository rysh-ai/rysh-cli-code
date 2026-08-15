// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// AgentsListTool lists all panes (agents) in the current session.
type AgentsListTool struct {
	nc      *nats.Conn
	session string
	codecs  *msg.CodecRegistry
}

// NewAgentsListTool creates an AgentsListTool.
func NewAgentsListTool(nc *nats.Conn, session string, codecs *msg.CodecRegistry) *AgentsListTool {
	return &AgentsListTool{nc: nc, session: session, codecs: codecs}
}

// Spec returns the tool specification for the LLM.
func (t *AgentsListTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {}
}`)
	return ToolSpec{
		Name:             "agents_list",
		Description:      "List all panes (agents) in the current workspace with their IDs, aliases, modes, and status. Use this to discover available panes before using pane_send or pane_inspect.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval always returns false — read-only.
func (t *AgentsListTool) RequiresApproval(_ json.RawMessage) bool {
	return false
}

// Execute requests a workspace snapshot and extracts pane information.
func (t *AgentsListTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	if t.nc == nil {
		return ErrOutput(ErrKindMissing, "NATS connection not available"), nil
	}

	// Request workspace snapshot.
	subject := msg.T("ws", "snapshot")
	reqPayload, _ := json.Marshal(&msg.MsgGetWorkspaceSnapshot{})
	envelope := msg.NATSEnvelope{
		TypeTag: msg.TagGetWorkspaceSnapshot,
		Payload: reqPayload,
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return ErrOutputf(ErrKindInternal, "failed to marshal request: %v", err), nil
	}

	reply, err := t.nc.Request(subject, data, 5*time.Second)
	if err != nil {
		return ErrOutputf(ErrKindTransient, "workspace not responding: %v", err), nil
	}

	// Parse the response.
	var respEnvelope msg.NATSEnvelope
	if err := json.Unmarshal(reply.Data, &respEnvelope); err != nil {
		return ErrOutputf(ErrKindValidation, "invalid response: %v", err), nil
	}

	var snapReply msg.MsgWorkspaceSnapshotReply
	if err := json.Unmarshal(respEnvelope.Payload, &snapReply); err != nil {
		return ErrOutputf(ErrKindInternal, "failed to parse snapshot: %v", err), nil
	}

	ws := snapReply.Snapshot

	// Extract pane information from all tabs.
	var sb strings.Builder
	totalPanes := 0

	sb.WriteString(fmt.Sprintf("%-36s  %-12s  %-8s  %-30s  %s\n",
		"Pane ID", "Alias", "Mode", "Status", "Tab"))
	sb.WriteString(strings.Repeat("-", 110) + "\n")

	for _, tab := range ws.Tabs {
		panes := collectPanes(tab)
		for _, p := range panes {
			alias := p.GivenName
			if alias == "" {
				alias = "(none)"
			}
			status := p.Status
			if status == "" {
				status = "idle"
			}
			active := ""
			if p.ID == ws.ActivePaneID {
				active = " *"
			}

			sb.WriteString(fmt.Sprintf("%-36s  %-12s  %-8s  %-30s  %s%s\n",
				p.ID, alias, p.Mode, truncateStr(status, 30), tab.Title, active))
			totalPanes++
		}
	}

	sb.WriteString(fmt.Sprintf("\nTotal: %d panes across %d tabs. Active pane marked with *.\n",
		totalPanes, len(ws.Tabs)))

	return &ToolOutput{
		Content: sb.String(),
		Metadata: map[string]string{
			"total_panes": fmt.Sprintf("%d", totalPanes),
			"total_tabs":  fmt.Sprintf("%d", len(ws.Tabs)),
		},
	}, nil
}

// collectPanes extracts all panes from a tab snapshot.
func collectPanes(tab domain.TabSnapshot) []domain.PaneSnapshot {
	var panes []domain.PaneSnapshot
	for p := range domain.PanesInTab(&tab) {
		panes = append(panes, *p)
	}
	return panes
}
