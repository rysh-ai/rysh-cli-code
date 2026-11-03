// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

func claudeFiles(t *testing.T) []NamedFile {
	return []NamedFile{
		{Label: ".claude/settings.json", Data: readFixture(t, "claude-settings.json")},
		{Label: ".claude/settings.local.json", Data: readFixture(t, "claude-settings-local.json")},
	}
}

func TestParseClaude_Mapping(t *testing.T) {
	p, err := ParseClaude(claudeFiles(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Policy == nil {
		t.Fatal("no policy plan")
	}
	if want := []string{"curl*", "rm -rf*", "git push*", "*"}; !reflect.DeepEqual(p.Policy.Gate, want) {
		t.Errorf("gate = %q, want %q (deny then ask, deduplicated)", p.Policy.Gate, want)
	}
	if want := []string{"go test ./...", "make lint", "npm run test*"}; !reflect.DeepEqual(p.Policy.Suggest, want) {
		t.Errorf("suggest = %q, want %q", p.Policy.Suggest, want)
	}
	for _, r := range []string{"Read(./.env)", "mcp__tracker__delete_issue", "WebFetch(domain:docs.example.test)"} {
		if !hasNote(p, r) {
			t.Errorf("rule %q not reported as not imported; notes = %q", r, p.Notes)
		}
	}
	// The settings file's env block is never read into the plan.
	js, _ := p.JSON()
	for _, out := range [][]byte{[]byte(p.Text()), js, p.Changes("")[0].Data} {
		if strings.Contains(string(out), "placeholder-value-7") {
			t.Error("a settings env value reached the plan")
		}
	}
}

// loadPolicy writes data as .rysh/policy.yaml in a temp dir and loads it with
// the real (fail-closed) loader.
func loadPolicy(t *testing.T, data []byte) *policy.Policy {
	t.Helper()
	dir := t.TempDir()
	if data != nil {
		if err := os.WriteFile(filepath.Join(dir, policy.FileName), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return policy.Load(dir)
}

func superset(have, of []string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, o := range of {
		if !set[o] {
			return false
		}
	}
	return true
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// The import may only tighten a policy. For each starting file: the result
// loads with no error; every permissive list (bash.allow,
// approval.auto_approve) is exactly as it was; every restrictive list
// (bash.deny, approval.always_gate) is a superset of what it was; budgets,
// proxy flags and snat are unchanged; and a section the importer does not
// own (fs:, feature 06) is byte-for-byte the same subtree.
func TestParseClaude_OnlyTightens(t *testing.T) {
	starts := map[string][]byte{
		"no file":    nil,
		"empty file": []byte(""),
		"full": []byte(`# project policy
bash:
  allow: [terraform, kubectl]
  deny: [rm]
approval:
  auto_approve: ["go test *"]
  always_gate:  ["git push --force*", "curl*"]
budget:
  ceilings:
    reviewer: 500000
proxy:
  required: true
  strict: true
snat:
  required: [stripe]
fs:
  deny: ["./.env"]
`),
		"approval without gate": []byte("approval:\n  auto_approve: [\"make *\"]\n"),
		"null approval":         []byte("approval:\n"),
	}
	for name, start := range starts {
		t.Run(name, func(t *testing.T) {
			p, err := ParseClaude(claudeFiles(t), start)
			if err != nil {
				t.Fatal(err)
			}
			ch := p.Changes("")
			if len(ch) != 1 || ch[0].Path != ".rysh/policy.yaml" {
				t.Fatalf("changes = %+v", ch)
			}
			before, after := loadPolicy(t, start), loadPolicy(t, ch[0].Data)
			if after.Err != nil {
				t.Fatalf("result fails the fail-closed loader: %v\n%s", after.Err, shown(ch[0].Data))
			}
			if !reflect.DeepEqual(sorted(after.BashAllow), sorted(before.BashAllow)) {
				t.Errorf("bash.allow changed: %q -> %q", before.BashAllow, after.BashAllow)
			}
			if !reflect.DeepEqual(sorted(after.AutoApprove), sorted(before.AutoApprove)) {
				t.Errorf("auto_approve changed: %q -> %q", before.AutoApprove, after.AutoApprove)
			}
			if !superset(after.BashDeny, before.BashDeny) || !superset(after.AlwaysGate, before.AlwaysGate) {
				t.Errorf("a restrictive list lost entries: deny %q -> %q, gate %q -> %q", before.BashDeny, after.BashDeny, before.AlwaysGate, after.AlwaysGate)
			}
			if !superset(after.AlwaysGate, p.Policy.Gate) {
				t.Errorf("gate entries missing: %q not all in %q", p.Policy.Gate, after.AlwaysGate)
			}
			if !reflect.DeepEqual(after.BudgetCeilings, before.BudgetCeilings) || after.ProxyRequired != before.ProxyRequired ||
				after.ProxyStrict != before.ProxyStrict || !reflect.DeepEqual(after.SNATRequired, before.SNATRequired) {
				t.Errorf("budget/proxy/snat changed:\n%+v\n%+v", before, after)
			}
			for _, s := range p.Policy.Suggest {
				for _, a := range after.AutoApprove {
					if a == s {
						t.Errorf("suggestion %q became an active auto_approve rule", s)
					}
				}
			}
			if !strings.Contains(string(ch[0].Data), "# REVIEW") {
				t.Errorf("suggestions are not marked for review:\n%s", shown(ch[0].Data))
			}
			var b, a map[string]any
			_ = yaml.Unmarshal(start, &b)
			_ = yaml.Unmarshal(ch[0].Data, &a)
			if !reflect.DeepEqual(b["fs"], a["fs"]) {
				t.Errorf("the fs: section changed: %v -> %v", b["fs"], a["fs"])
			}
		})
	}
}

func TestParseClaude_Idempotent(t *testing.T) {
	p1, err := ParseClaude(claudeFiles(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	once := p1.Changes("")[0].Data
	p2, err := ParseClaude(claudeFiles(t), once)
	if err != nil {
		t.Fatal(err)
	}
	if p2.Policy != nil && len(p2.Policy.Gate) != 0 {
		t.Errorf("a second import re-adds gates: %q", p2.Policy.Gate)
	}
}

func TestParseClaude_RefusesBrokenPolicy(t *testing.T) {
	if _, err := ParseClaude(claudeFiles(t), []byte("approval: [unclosed\n")); err == nil {
		t.Error("an unparseable policy.yaml must be refused, never rewritten")
	}
	if _, err := ParseClaude(claudeFiles(t), []byte("approval:\n  always_gate: notalist\n")); err == nil {
		// Accepted at parse time is fine only if the render refuses.
		p, _ := ParseClaude(claudeFiles(t), []byte("approval:\n  always_gate: notalist\n"))
		if p != nil && len(p.Changes("")) != 0 {
			t.Error("an always_gate that is not a list must not be rewritten")
		}
	}
}

func TestParseClaude_NoRules(t *testing.T) {
	p, err := ParseClaude([]NamedFile{{Label: "s.json", Data: []byte(`{"model": "x"}`)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Changes("")) != 0 {
		t.Error("a settings file with no permissions must write nothing")
	}
}

func TestClaudePreviewGolden(t *testing.T) {
	p, err := ParseClaude(claudeFiles(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "claude.preview.txt", []byte(p.Text()))
	golden(t, "claude.policy.yaml", p.Changes("")[0].Data)
}

// Changing always_gate from a flow list to a block list must not move the
// user's comment on that line onto another key.
func TestParseClaude_KeepsCommentsInPlace(t *testing.T) {
	start := []byte("approval:\n  always_gate: [\"curl*\"] # keep\nfs:\n  deny: [\"./.env\"] # fs note\n")
	p, err := ParseClaude(claudeFiles(t), start)
	if err != nil {
		t.Fatal(err)
	}
	out := string(p.Changes("")[0].Data)
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "fs:") && strings.Contains(line, "#") {
			t.Errorf("a comment moved onto the fs: key:\n%s", shown(out))
		}
		if strings.HasPrefix(strings.TrimSpace(line), "always_gate:") && !strings.Contains(line, "# keep") {
			t.Errorf("the always_gate comment was lost or moved:\n%s", shown(out))
		}
	}
	if !strings.Contains(out, "# fs note") {
		t.Errorf("the fs: comment was lost:\n%s", shown(out))
	}
}

// shown is test output fit for a failure message; output carrying a fixture's
// synthetic secret value is withheld, because failure messages end up in
// committed RED logs.
func shown[T string | []byte](text T) string {
	if strings.Contains(string(text), "placeholder-value") {
		return "(output withheld: it contains a fixture secret value)"
	}
	return string(text)
}
