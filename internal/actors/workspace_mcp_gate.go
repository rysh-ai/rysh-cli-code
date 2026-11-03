// SPDX-License-Identifier: Apache-2.0

package actors

// workspace_mcp_gate.go — the policy gate and audit log for `rysh mcp serve`
// (design 067).
//
// An outside MCP client writing into a session must pass the same gates a
// human-typed command does, and the gates live here, in the daemon: the
// workspace holds the policy it actually loaded (project file, org file or the
// central document, merged strictest-wins), and the fail-closed block state.
// A second copy loaded by the MCP process could disagree with it.
//
// The verdict is binary. A call a rule gates is REFUSED, never auto-approved
// and never turned into an approval prompt: there is no human on the far side
// of an MCP client to answer one, and a prompt nobody answers is a hang.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// MCPToolPrefix is how an MCP tool appears to policy rules: send_input is
// matched as "mcp_send_input", so `approval.always_gate: ["mcp_*"]` gates
// every MCP write without touching any agent's own tools.
const MCPToolPrefix = "mcp_"

// mcpAuditFile is the per-session log of MCP write attempts.
const mcpAuditFile = "mcp-audit.jsonl"

// MCPAuditEntry is one MCP write attempt. It never carries the input body:
// the target is a label (pane id, humanoid/channel), redacted and clipped.
type MCPAuditEntry struct {
	Time   time.Time `json:"time"`
	Caller string    `json:"caller"`
	Tool   string    `json:"tool"`
	Target string    `json:"target,omitempty"`
	Status string    `json:"status"` // allowed | refused
	Rule   string    `json:"rule,omitempty"`
	Reason string    `json:"reason,omitempty"`
}

// evaluateMCPGate decides one MCP write against a loaded policy. It returns
// "" when the call may run, or why it may not and the rule that said so.
func evaluateMCPGate(pol *policy.Policy, m *msg.MsgCLIMCPGate) (reason, rule string) {
	if pol != nil && pol.Err != nil {
		return "the policy file failed to load, so the governance posture is unknown (fail-closed, design 013)", "policy.load"
	}
	if why, blocked := policy.Blocked(); blocked {
		return "governed execution is blocked: " + why, "policy.blocked"
	}
	tool := strings.TrimSpace(m.Tool)
	if tool == "" {
		return "no tool named", ""
	}
	args := m.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if act, id := pol.EvaluateToolApproval(MCPToolPrefix+tool, args); act == policy.ApprovalGate {
		return fmt.Sprintf("%s requires approval by %s; MCP calls are never auto-approved", MCPToolPrefix+tool, id), id
	}
	if cmd := strings.TrimSpace(m.ShellCommand); cmd != "" {
		in, _ := json.Marshal(map[string]string{"command": cmd})
		if act, id := pol.EvaluateToolApproval("bash", in); act == policy.ApprovalGate {
			return fmt.Sprintf("the shell input matches %s; MCP calls are never auto-approved", id), id
		}
	}
	return "", ""
}

// handleCLIMCPGate answers one gate request and audits it.
func (w *WorkspaceActor) handleCLIMCPGate(m *msg.MsgCLIMCPGate) *msg.MsgCLIResponse {
	if m == nil {
		return &msg.MsgCLIResponse{OK: false, Error: "empty MCP gate request"}
	}
	reason, rule := evaluateMCPGate(w.policy, m)
	caller := strings.TrimSpace(m.Caller)
	if caller == "" {
		caller = "mcp"
	}
	entry := MCPAuditEntry{
		Time:   time.Now().UTC(),
		Caller: clipRunes(caller, 40),
		Tool:   clipRunes(m.Tool, 80),
		Target: clipRunes(meeting.Redact(m.Target), 160),
		Status: "allowed",
	}
	if reason != "" {
		entry.Status, entry.Rule, entry.Reason = "refused", rule, clipRunes(reason, 240)
	}
	if err := appendMCPAudit(w.mcpAuditPath(), entry); err != nil {
		// An MCP write that cannot be recorded does not run: the audit line
		// is the control, not a courtesy.
		return &msg.MsgCLIResponse{OK: false, Error: "mcp audit log unavailable: " + err.Error()}
	}
	if reason != "" {
		return &msg.MsgCLIResponse{OK: false, Error: "policy: " + reason}
	}
	return &msg.MsgCLIResponse{OK: true}
}

func (w *WorkspaceActor) mcpAuditPath() string {
	if w.cfg.SessionDir == "" {
		return ""
	}
	return filepath.Join(w.cfg.SessionDir, w.cfg.SessionName, mcpAuditFile)
}

var mcpAuditMu sync.Mutex

func appendMCPAudit(path string, e MCPAuditEntry) error {
	if path == "" {
		return fmt.Errorf("no session directory")
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	mcpAuditMu.Lock()
	defer mcpAuditMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// StripANSI is stripAnsiEscapes for `rysh mcp serve`, which hands pane text to
// an outside client as plain lines.
func StripANSI(s string) string { return stripAnsiEscapes(s) }

// ScreenShowsWork reports whether an agent CLI's screen shows a turn in
// progress — the same footer test the fleet watcher and ansa use.
func ScreenShowsWork(rows []string) bool { return ansaScreenIsWorking(rows) }
