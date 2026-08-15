// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_Missing(t *testing.T) {
	p := Load(t.TempDir())
	if p.Loaded || p.Err != nil {
		t.Fatalf("missing file: Loaded=%v Err=%v", p.Loaded, p.Err)
	}
	if p.BudgetCeilings == nil {
		t.Fatal("BudgetCeilings should be non-nil")
	}
}

func TestLoad_Valid(t *testing.T) {
	dir := t.TempDir()
	doc := `
bash:
  allow: [terraform, kubectl]
  deny: [rm]
approval:
  auto_approve: ["go test *"]
  always_gate: ["git push --force*"]
budget:
  ceilings:
    pane-a: 500000
proxy:
  required: true
snat:
  required: [stripe]
`
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Load(dir)
	if p.Err != nil || !p.Loaded {
		t.Fatalf("valid load: Err=%v Loaded=%v", p.Err, p.Loaded)
	}
	if len(p.BashAllow) != 2 || p.BashAllow[0] != "terraform" {
		t.Fatalf("bash allow = %v", p.BashAllow)
	}
	if p.BudgetCeilings["pane-a"] != 500000 {
		t.Fatalf("ceiling = %v", p.BudgetCeilings)
	}
	if !p.ProxyRequired {
		t.Fatal("proxy.required not parsed")
	}
	if len(p.SNATRequired) != 1 || p.SNATRequired[0] != "stripe" {
		t.Fatalf("snat required = %v", p.SNATRequired)
	}
}

func TestLoad_FailClosed(t *testing.T) {
	dir := t.TempDir()
	// Invalid YAML (a tab) → parse error → fail-closed.
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("bash:\n\tallow: [x]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := Load(dir)
	if p.Err == nil {
		t.Fatal("expected parse error (fail-closed)")
	}
	// No rules applied on a broken policy.
	if len(p.BashAllow) != 0 || len(p.BudgetCeilings) != 0 {
		t.Fatalf("broken policy must apply no rules: %+v", p)
	}
}
