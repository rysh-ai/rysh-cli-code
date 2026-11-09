// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sharedagentic "github.com/rysh-ai/rysh-cli-shared/agentic"
)

// CD5: ##config load registers the read-only "company" server as before and a
// second "company-write" server whose every call goes through approval. The
// Claude Code config gets only the read-only server (K1).
func TestCompanyMCPServersSplitReadAndWrite(t *testing.T) {
	servers := companyMCPAddArgs("/bin/rysh", "/w/rysh.config.yaml")
	if len(servers) != 2 {
		t.Fatalf("want 2 servers, got %d: %v", len(servers), servers)
	}
	read, write := strings.Join(servers[0], " "), strings.Join(servers[1], " ")
	if !strings.HasPrefix(read, "add company stdio /bin/rysh company mcp ") || strings.Contains(read, "--approve") || strings.Contains(read, "--write") {
		t.Fatalf("read server: %s", read)
	}
	if !strings.HasPrefix(write, "add company-write stdio /bin/rysh company mcp --write ") || !strings.Contains(write, "--approve") ||
		!strings.Contains(write, "RYSH_COMPANY_CONFIG=/w/rysh.config.yaml") {
		t.Fatalf("write server: %s", write)
	}

	dir := t.TempDir()
	p, err := writeClaudeMCPConfig(dir, "/bin/rysh", "/w/rysh.config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	var doc struct {
		MCPServers map[string]struct {
			Args []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	if got := strings.Join(doc.MCPServers["company"].Args, " "); got != "company mcp" {
		t.Fatalf(".mcp.json company args %q", got)
	}
	// security-lead K1: Claude Code panes never run rysh's approval gate
	// (bypass mode or "always allow" would write unasked), so the write
	// server is never handed to them.
	if _, ok := doc.MCPServers["company-write"]; ok {
		t.Fatal(".mcp.json carries company-write; Claude Code would bypass rysh's gate")
	}
}

// C-C2 without a policy file: the approval hook is still installed and gates
// the company write tools; other tools keep the default.
func TestCompanyWriteToolsGateWithoutAPolicyFile(t *testing.T) {
	w := &WorkspaceActor{}
	f := w.approvalPolicyFunc()
	if f == nil {
		t.Fatal("no approval hook without a policy file: the company write gate would not apply")
	}
	for _, tool := range []string{"company_enquiry_update", "company_order_status"} {
		if d, id := f(tool, nil); d != sharedagentic.PolicyGate || id == "" {
			t.Errorf("%s: %v %q, want a gate", tool, d, id)
		}
	}
	if d, _ := f("company_ledger", nil); d != sharedagentic.PolicyDefault {
		t.Errorf("company_ledger: %v, want the default", d)
	}
}
