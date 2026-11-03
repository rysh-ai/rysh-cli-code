// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

func mcpGateWorkspace(t *testing.T, policyYAML string) (*WorkspaceActor, string) {
	t.Helper()
	dir := t.TempDir()
	ryshDir := filepath.Join(dir, ".rysh")
	if err := os.MkdirAll(ryshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if policyYAML != "" {
		if err := os.WriteFile(filepath.Join(ryshDir, policy.FileName), []byte(policyYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w := &WorkspaceActor{cfg: config.Config{RyshDir: ryshDir, SessionDir: filepath.Join(ryshDir, "sessions"), SessionName: "s1"}}
	w.policy = policy.Load(ryshDir)
	return w, filepath.Join(ryshDir, "sessions", "s1", mcpAuditFile)
}

func readMCPAudit(t *testing.T, path string) []MCPAuditEntry {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	var out []MCPAuditEntry
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e MCPAuditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("audit line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

func sendGate(input string) *msg.MsgCLIMCPGate {
	args, _ := json.Marshal(map[string]string{"pane": "p1", "input": input})
	return &msg.MsgCLIMCPGate{Tool: "send_input", Args: args, Target: "p1", ShellCommand: input, Caller: "mcp"}
}

// The founder's rule for design 067: policy.yaml approval and always-gate
// rules apply to MCP-initiated actions, and a gated call fails — it is never
// auto-approved.
func TestMCPGateAlwaysGateRefusesAndAudits(t *testing.T) {
	w, audit := mcpGateWorkspace(t, "approval:\n  always_gate: [\"mcp_send_input\"]\n  auto_approve: [\"mcp_*\"]\n")
	resp := w.handleCLIMCPGate(sendGate("ls"))
	if resp.OK || !strings.Contains(resp.Error, "approval.always_gate[0]") || !strings.Contains(resp.Error, "never auto-approved") {
		t.Fatalf("always_gate: %+v", resp)
	}
	es := readMCPAudit(t, audit)
	if len(es) != 1 || es[0].Status != "refused" || es[0].Caller != "mcp" || es[0].Rule != "approval.always_gate[0]" || es[0].Tool != "send_input" {
		t.Fatalf("audit = %+v", es)
	}
}

func TestMCPGateBashDenyAppliesToShellInput(t *testing.T) {
	w, _ := mcpGateWorkspace(t, "bash:\n  deny: [\"*rm -rf*\"]\n")
	if resp := w.handleCLIMCPGate(sendGate("cd /tmp && rm -rf x")); resp.OK || !strings.Contains(resp.Error, "bash.deny[0]") {
		t.Fatalf("bash.deny: %+v", resp)
	}
	if resp := w.handleCLIMCPGate(sendGate("ls -la")); !resp.OK {
		t.Fatalf("an undenied command was refused: %+v", resp)
	}
	// A prompt is not a shell command: the bash rules do not apply to it.
	g := sendGate("rm -rf everything, please")
	g.ShellCommand = ""
	if resp := w.handleCLIMCPGate(g); !resp.OK {
		t.Fatalf("a prompt was matched against bash.deny: %+v", resp)
	}
}

func TestMCPGateBrokenPolicyFailsClosed(t *testing.T) {
	w, audit := mcpGateWorkspace(t, "approval: [this is: not valid\n")
	if w.policy.Err == nil {
		t.Fatal("fixture policy should fail to parse")
	}
	if resp := w.handleCLIMCPGate(sendGate("ls")); resp.OK || !strings.Contains(resp.Error, "fail-closed") {
		t.Fatalf("broken policy: %+v", resp)
	}
	if es := readMCPAudit(t, audit); es[0].Status != "refused" || es[0].Rule != "policy.load" {
		t.Fatalf("audit = %+v", es)
	}
}

func TestMCPGateAllowedIsAuditedWithoutTheBody(t *testing.T) {
	w, audit := mcpGateWorkspace(t, "")
	secretish := "export API_TOKEN=abc123def456 && echo the-body-text"
	if resp := w.handleCLIMCPGate(sendGate(secretish)); !resp.OK {
		t.Fatalf("no policy: %+v", resp)
	}
	b, _ := os.ReadFile(audit)
	if strings.Contains(string(b), "abc123def456") || strings.Contains(string(b), "the-body-text") {
		t.Fatalf("the audit log carries the input body: %s", b)
	}
	es := readMCPAudit(t, audit)
	if len(es) != 1 || es[0].Status != "allowed" || es[0].Target != "p1" {
		t.Fatalf("audit = %+v", es)
	}
	if info, err := os.Stat(audit); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("audit file mode: %v %v", info.Mode(), err)
	}
}

func TestMCPGateUnwritableAuditRefuses(t *testing.T) {
	w, _ := mcpGateWorkspace(t, "")
	w.cfg.SessionDir = ""
	if resp := w.handleCLIMCPGate(sendGate("ls")); resp.OK {
		t.Fatal("an MCP write that cannot be audited must not run")
	}
}
