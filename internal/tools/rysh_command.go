package tools

// rysh_command (design 008, RA3) — run a rysh system command ("##" command)
// and read its output back.
//
// Why this tool exists at all: `pane_send` publishes MsgPaneExecShell /
// MsgPaneExecPrompt DIRECTLY to the pane inbox, deliberately bypassing the
// WorkspaceActor. The "##" prefix is handled in WorkspaceActor
// (internal/actors/workspace_rysh.go), reached ONLY via MsgSubmitInput on
// rysh.ws.{session}.inbox — the same door the TUI and the web dashboard use
// (internal/web/server.go, case "submit_input").
//
// Without this tool a humanoid asked to run `##tab list` sends that literal
// string to a shell, which errors, so every structural operation (tabs, panes,
// agents, share, worktrees, cost) is unreachable from a chat app. This is the
// tool that makes "operate my rysh session from Slack/WhatsApp/Telegram" true.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	// ryshCmdMaxWindow bounds the total time spent collecting output.
	ryshCmdMaxWindow = 5 * time.Second
	// ryshCmdQuietPeriod ends collection once output stops arriving. Rysh
	// commands answer fast; this keeps the common case near-instant instead of
	// always paying the full window.
	ryshCmdQuietPeriod = 600 * time.Millisecond
	// ryshCmdMaxLines caps what is handed back to the model. Chat channels are
	// the consumer here (design 008 §8) — a `##pane list` on a 40-pane session
	// is unreadable on a phone, so we truncate with an honest tail rather than
	// silently dropping.
	ryshCmdMaxLines = 200
)

// readOnlyRyshCommands are whole commands that never mutate state, regardless
// of subcommand (e.g. `##cost`, `##cost week`).
var readOnlyRyshCommands = map[string]bool{
	"help": true, "cost": true, "snap": true, "week": true, "budget": true,
}

// readOnlyRyshVerbs are subcommands that never mutate state. `##tab list` is
// read-only; `##tab new` is not.
var readOnlyRyshVerbs = map[string]bool{
	"list": true, "ls": true, "status": true, "info": true, "show": true,
	"get": true, "preview": true, "help": true,
	"shares": true, "channels": true,
}

// RyshCommandTool executes a rysh system command via the WorkspaceActor and
// returns the command's output.
type RyshCommandTool struct {
	nc          *nats.Conn
	session     string
	defaultPane string
}

// RyshCommandParams holds the parameters for a rysh system command.
type RyshCommandParams struct {
	// Command is the command body, with or without the leading "##"
	// (e.g. "tab list", "##pane new", "agent list").
	Command string `json:"command"`
	// PaneID optionally pins where the command runs and whose rysh output is
	// read back. Defaults to the caller's pane.
	PaneID string `json:"pane_id,omitempty"`
}

// NewRyshCommandTool creates a RyshCommandTool. defaultPaneID is the pane the
// command targets when the model does not name one — for a humanoid this is its
// chat output pane.
func NewRyshCommandTool(nc *nats.Conn, session, defaultPaneID string) *RyshCommandTool {
	return &RyshCommandTool{nc: nc, session: session, defaultPane: defaultPaneID}
}

// Spec returns the tool specification for the LLM.
func (t *RyshCommandTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "The rysh system command to run, with or without the leading '##'. Examples: 'tab list', 'pane new', 'agent list', 'session info', 'cost', 'share pane view'."},
    "pane_id": {"type": "string", "description": "Optional pane UUID to run the command against. Defaults to this agent's pane."}
  },
  "required": ["command"]
}`)
	return ToolSpec{
		Name: "rysh_command",
		Description: "Run a rysh system command (a '##' command) and return its output. " +
			"This is how you operate the rysh session itself — tabs, panes, agents, sharing, " +
			"worktrees, cost. Use 'help' to discover available commands. " +
			"Read-only commands (list/status/info/show/cost) run without approval; " +
			"anything that changes state requires confirmation.",
		Parameters:       schema,
		RequiresApproval: true,
	}
}

// RequiresApproval is an allowlist: read-only/idempotent rysh commands run
// unprompted, everything else (including anything unrecognised) requires
// confirmation. Safe-by-default, matching BashTool's discipline.
func (t *RyshCommandTool) RequiresApproval(params json.RawMessage) bool {
	var p RyshCommandParams
	if err := json.Unmarshal(params, &p); err != nil {
		return true
	}
	return !ryshCommandIsReadOnly(p.Command)
}

// ryshCommandIsReadOnly reports whether a rysh command only reads state.
func ryshCommandIsReadOnly(command string) bool {
	body := normalizeRyshCommand(command)
	// A "####"-escaped command is a remote-control payload for a subscriber
	// session, not a local read — never auto-approve it.
	if strings.HasPrefix(body, "#") {
		return false
	}
	fields := strings.Fields(strings.ToLower(body))
	switch len(fields) {
	case 0:
		return false
	case 1:
		return readOnlyRyshCommands[fields[0]]
	default:
		if readOnlyRyshCommands[fields[0]] {
			return true
		}
		return readOnlyRyshVerbs[fields[1]]
	}
}

// normalizeRyshCommand strips at most one leading "##" so the body can be
// re-prefixed exactly once. Stripping only one pair preserves "####cmd", which
// WorkspaceActor treats as a distinct remote-control escape — mangling it would
// silently change which session the command lands in.
func normalizeRyshCommand(command string) string {
	body := strings.TrimSpace(command)
	body = strings.TrimPrefix(body, "##")
	return strings.TrimSpace(body)
}

// Execute publishes the command through the WorkspaceActor and collects the
// resulting rysh output.
func (t *RyshCommandTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p RyshCommandParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid rysh_command params: %w", err)
	}

	body := normalizeRyshCommand(p.Command)
	if body == "" {
		return ErrOutput(ErrKindValidation, "command is required"), nil
	}
	// "##>" is a pipeline event, not a system command: it bypasses the PTY and
	// is published straight to the pane's output topic. Routing one through
	// here would produce no output and confuse the caller.
	if strings.HasPrefix(body, ">") {
		return ErrOutput(ErrKindValidation,
			"'##>' pipeline events are not system commands — use pane_send to emit one"), nil
	}

	paneID := p.PaneID
	if paneID == "" {
		paneID = t.defaultPane
	}
	if paneID == "" {
		return ErrOutput(ErrKindValidation,
			"no pane to run against: pass pane_id (this agent has no default pane)"), nil
	}
	if t.nc == nil {
		return ErrOutput(ErrKindMissing, "NATS connection not available"), nil
	}

	// Subscribe BEFORE publishing so fast commands cannot answer into the void.
	outCh := make(chan *nats.Msg, 256)
	sub, err := t.nc.ChanSubscribe(msg.T("pane", paneID, "output", "rysh"), outCh)
	if err != nil {
		return ErrOutputf(ErrKindTransient, "failed to subscribe to pane output: %v", err), nil
	}
	defer func() { _ = sub.Unsubscribe() }()

	text := "##" + body
	data, err := json.Marshal(msg.NATSEnvelope{
		TypeTag: msg.TagSubmitInput,
		Payload: mustMarshalSubmitInput(text, paneID),
	})
	if err != nil {
		return ErrOutputf(ErrKindInternal, "failed to marshal request: %v", err), nil
	}
	// The workspace inbox is the ONLY subject whose handler understands "##".
	if err := t.nc.Publish(msg.T("ws", t.session, "inbox"), data); err != nil {
		return ErrOutputf(ErrKindTransient, "failed to send command: %v", err), nil
	}
	_ = t.nc.Flush()

	lines := collectRyshOutput(ctx, outCh)

	truncated := false
	if len(lines) > ryshCmdMaxLines {
		dropped := len(lines) - ryshCmdMaxLines
		lines = lines[:ryshCmdMaxLines]
		lines = append(lines, fmt.Sprintf("… (%d more lines truncated)", dropped))
		truncated = true
	}

	content := strings.TrimRight(strings.Join(lines, "\n"), "\n")
	if content == "" {
		// Not an error: some commands are silent on success. Say so plainly
		// rather than returning an empty string the model has to guess about.
		content = fmt.Sprintf("(no output within %s — the command was accepted)", ryshCmdMaxWindow)
	}

	return &ToolOutput{
		Content: content,
		Metadata: map[string]string{
			"command":   text,
			"pane_id":   paneID,
			"truncated": fmt.Sprintf("%t", truncated),
		},
	}, nil
}

// mustMarshalSubmitInput builds the MsgSubmitInput payload. The struct is
// plain data, so marshalling cannot fail; errors are surfaced by the caller's
// envelope marshal.
func mustMarshalSubmitInput(text, paneID string) []byte {
	b, _ := json.Marshal(&msg.MsgSubmitInput{Text: text, Mode: "shell", PaneID: paneID})
	return b
}

// collectRyshOutput gathers rysh-conversation output until the stream goes
// quiet, the overall window expires, or the context is cancelled.
func collectRyshOutput(ctx context.Context, outCh <-chan *nats.Msg) []string {
	var lines []string
	deadline := time.NewTimer(ryshCmdMaxWindow)
	defer deadline.Stop()
	quiet := time.NewTimer(ryshCmdQuietPeriod)
	defer quiet.Stop()

	for {
		select {
		case m := <-outCh:
			if content, ok := decodeRyshOutput(m.Data); ok && content != "" {
				lines = append(lines, strings.Split(strings.TrimRight(content, "\n"), "\n")...)
			}
			if !quiet.Stop() {
				select {
				case <-quiet.C:
				default:
				}
			}
			quiet.Reset(ryshCmdQuietPeriod)
		case <-quiet.C:
			return lines
		case <-deadline.C:
			return lines
		case <-ctx.Done():
			return lines
		}
	}
}

// decodeRyshOutput extracts the content of a ConversationMessage carried on a
// pane's rysh output topic.
func decodeRyshOutput(data []byte) (string, bool) {
	var env msg.NATSEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return "", false
	}
	var appended msg.MsgConversationAppend
	if err := json.Unmarshal(env.Payload, &appended); err != nil {
		return "", false
	}
	if appended.Message == nil {
		return "", false
	}
	return appended.Message.Content, true
}
