// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePolicyFile writes doc to dir/FileName and returns the full path.
func writePolicyFile(t *testing.T, dir, doc string) string {
	t.Helper()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeOrgPolicyFile writes doc to a standalone org policy file.
func writeOrgPolicyFile(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "org-policy.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMerge_NotConfigured proves the no-org path is byte-identical to today's
// behavior: Merge(nil, p) must hand back the very same project policy.
func TestMerge_NotConfigured(t *testing.T) {
	p := Load(t.TempDir())
	if got := Merge(nil, p); got != p {
		t.Fatalf("Merge(nil, p) = %p, want the project policy unchanged (%p)", got, p)
	}
}

// TestMerge_StrictestWins is the core of design 013 §1's org merge: anything
// the org denies or gates stays denied/gated regardless of the project file,
// and permissive lists intersect (the project may narrow, never widen).
func TestMerge_StrictestWins(t *testing.T) {
	orgPath := writeOrgPolicyFile(t, `
bash:
  allow: ["go *", "git *"]
  deny: ["*rm -rf*"]
approval:
  auto_approve: [file_read]
  always_gate: ["deploy*"]
`)
	projDir := t.TempDir()
	// The project tries to re-allow everything the org restricts, and to
	// widen the allowlist with curl.
	writePolicyFile(t, projDir, `
bash:
  allow: ["go test*", "curl *", "*rm -rf*"]
approval:
  auto_approve: [file_read, web_fetch, "deploy*"]
`)
	m := Merge(LoadOrg(orgPath), Load(projDir))
	if m.Err != nil || !m.Loaded || !m.OrgActive {
		t.Fatalf("merge: Err=%v Loaded=%v OrgActive=%v", m.Err, m.Loaded, m.OrgActive)
	}

	cases := []struct {
		name   string
		tool   string
		cmd    string
		want   ApprovalAction
		wantID string
	}{
		{"org deny beats project allow", "bash", "cd /t && rm -rf x", ApprovalGate, "org.bash.deny[0]"},
		{"org gate beats project auto-approve", "deploy_prod", "", ApprovalGate, "org.approval.always_gate[0]"},
		{"intersection auto-approves (both allow)", "bash", "go test ./...", ApprovalAuto, "bash.allow[0]+org.bash.allow[0]"},
		{"project cannot widen (org missing)", "bash", "curl http://x", ApprovalDefault, ""},
		{"project narrows (project missing)", "bash", "git push", ApprovalDefault, ""},
		{"tool auto-approve needs both files", "web_fetch", "", ApprovalDefault, ""},
		{"tool auto-approve both files", "file_read", "", ApprovalAuto, "approval.auto_approve[0]+org.approval.auto_approve[0]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var input []byte
			if c.cmd != "" {
				input = bashInput(c.cmd)
			}
			act, id := m.EvaluateToolApproval(c.tool, input)
			if act != c.want || id != c.wantID {
				t.Fatalf("EvaluateToolApproval(%s, %q) = (%d, %q), want (%d, %q)",
					c.tool, c.cmd, act, id, c.want, c.wantID)
			}
		})
	}
}

// TestMerge_OrgOnly: with no project file the org policy IS the policy — its
// permissive rules stand alone (there is no project list to intersect with).
func TestMerge_OrgOnly(t *testing.T) {
	orgPath := writeOrgPolicyFile(t, `
bash:
  allow: ["go *"]
  deny: ["*rm -rf*"]
proxy:
  required: true
`)
	m := Merge(LoadOrg(orgPath), Load(t.TempDir()))
	if m.Err != nil || !m.Loaded || !m.OrgActive {
		t.Fatalf("merge: Err=%v Loaded=%v OrgActive=%v", m.Err, m.Loaded, m.OrgActive)
	}
	if act, id := m.EvaluateToolApproval("bash", bashInput("go build ./...")); act != ApprovalAuto || id != "org.bash.allow[0]" {
		t.Fatalf("org-only allow = (%d, %q), want (Auto, org.bash.allow[0])", act, id)
	}
	if act, id := m.EvaluateToolApproval("bash", bashInput("rm -rf /")); act != ApprovalGate || id != "org.bash.deny[0]" {
		t.Fatalf("org-only deny = (%d, %q), want (Gate, org.bash.deny[0])", act, id)
	}
	if !m.ProxyRequired {
		t.Fatal("org proxy.required must survive an org-only merge")
	}
}

// TestMerge_LimitsAndUnions covers the non-list rule classes: the lower budget
// ceiling wins per pane id, proxy.required is required by either file, and
// snat.required is the union of both files.
func TestMerge_LimitsAndUnions(t *testing.T) {
	org := &Policy{
		BudgetCeilings: map[string]int64{"pane-a": 100, "pane-c": 50},
		SNATRequired:   []string{"stripe", "github"},
		Loaded:         true,
	}
	proj := &Policy{
		BudgetCeilings: map[string]int64{"pane-a": 200, "pane-b": 10},
		SNATRequired:   []string{"github", "aws"},
		ProxyRequired:  true,
		Loaded:         true,
	}
	m := Merge(org, proj)
	want := map[string]int64{"pane-a": 100, "pane-b": 10, "pane-c": 50}
	if len(m.BudgetCeilings) != len(want) {
		t.Fatalf("ceilings = %v, want %v", m.BudgetCeilings, want)
	}
	for id, c := range want {
		if m.BudgetCeilings[id] != c {
			t.Fatalf("ceiling[%s] = %d, want %d (lower bound wins)", id, m.BudgetCeilings[id], c)
		}
	}
	if !m.ProxyRequired {
		t.Fatal("proxy.required by either file must be required")
	}
	if got := strings.Join(m.SNATRequired, ","); got != "stripe,github,aws" {
		t.Fatalf("snat required = %q, want union stripe,github,aws", got)
	}
	// The lower project ceiling must also beat a higher org one the other way
	// around: strictest wins, not "org wins".
	m2 := Merge(proj, org)
	if m2.BudgetCeilings["pane-a"] != 100 {
		t.Fatalf("reversed ceiling[pane-a] = %d, want 100", m2.BudgetCeilings["pane-a"])
	}
}

func TestLoadOrg_NotConfigured(t *testing.T) {
	if p := LoadOrg(""); p != nil {
		t.Fatalf("LoadOrg(\"\") = %+v, want nil (no org policy configured)", p)
	}
	if p := LoadOrg("   "); p != nil {
		t.Fatalf("LoadOrg(blank) = %+v, want nil", p)
	}
}

// TestLoadOrg_MissingFailsClosed: a configured-but-absent org file is a tamper
// signal, not "no rules" — unlike the project file, missing means blocked.
func TestLoadOrg_MissingFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "org-policy.yaml")
	p := LoadOrg(path)
	if p == nil || p.Err == nil {
		t.Fatalf("missing configured org file must set Err, got %+v", p)
	}
	if !strings.Contains(p.Err.Error(), "missing") {
		t.Fatalf("Err = %v, want a clear configured-but-missing message", p.Err)
	}
	if len(p.BashAllow) != 0 || len(p.BudgetCeilings) != 0 {
		t.Fatalf("missing org policy must apply no rules: %+v", p)
	}
}

func TestLoadOrg_UnparseableFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "org-policy.yaml")
	// Invalid YAML (a tab) → parse error → fail-closed.
	if err := os.WriteFile(path, []byte("bash:\n\tallow: [x]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := LoadOrg(path)
	if p.Err == nil {
		t.Fatal("expected parse error (fail-closed)")
	}
	if len(p.BashAllow) != 0 || len(p.BudgetCeilings) != 0 {
		t.Fatalf("broken org policy must apply no rules: %+v", p)
	}
}

// TestMerge_FailClosed: an error on EITHER side poisons the whole merge — a
// broken org file may not run ungoverned, and a broken project file may not
// silently drop the org constraints.
func TestMerge_FailClosed(t *testing.T) {
	brokenOrg := LoadOrg(filepath.Join(t.TempDir(), "org-policy.yaml")) // missing
	goodProjDir := t.TempDir()
	writePolicyFile(t, goodProjDir, "bash:\n  allow: [ls]\n")

	m := Merge(brokenOrg, Load(goodProjDir))
	if m.Err == nil {
		t.Fatal("broken org + good project must fail closed")
	}
	if m.Path != brokenOrg.Path {
		t.Fatalf("merged Path = %q, want the failing org file %q", m.Path, brokenOrg.Path)
	}
	if len(m.BashAllow) != 0 || len(m.OrgBashAllow) != 0 || len(m.BudgetCeilings) != 0 {
		t.Fatalf("failed merge must apply no rules: %+v", m)
	}

	goodOrgPath := writeOrgPolicyFile(t, "bash:\n  deny: [rm]\n")
	brokenProjDir := t.TempDir()
	writePolicyFile(t, brokenProjDir, "bash:\n\tallow: [x]\n")
	m2 := Merge(LoadOrg(goodOrgPath), Load(brokenProjDir))
	if m2.Err == nil {
		t.Fatal("good org + broken project must fail closed")
	}
	if m2.Path == goodOrgPath {
		t.Fatalf("merged Path should cite the failing project file, got the org path")
	}
}

// TestSummary_OrgActive: ##policy must say an org policy is in force and name
// the file, so an operator can see why a project rule is not taking effect.
func TestSummary_OrgActive(t *testing.T) {
	orgPath := writeOrgPolicyFile(t, "bash:\n  deny: [rm]\n")
	projDir := t.TempDir()
	writePolicyFile(t, projDir, "bash:\n  allow: [ls]\n")

	s := Merge(LoadOrg(orgPath), Load(projDir)).Summary()
	if !strings.Contains(s, "org policy") || !strings.Contains(s, orgPath) {
		t.Fatalf("Summary must name the active org policy file:\n%s", s)
	}

	// Org-only: still named, and not rendered as "loaded from <empty>".
	s2 := Merge(LoadOrg(orgPath), Load(t.TempDir())).Summary()
	if !strings.Contains(s2, orgPath) || strings.Contains(s2, "loaded from \n") {
		t.Fatalf("org-only Summary must name the org file:\n%s", s2)
	}
}
