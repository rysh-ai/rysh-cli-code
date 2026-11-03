// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// NamedFile is one input file and the label it is shown under.
type NamedFile struct {
	Label string
	Data  []byte
}

// PolicyPlan is what a Claude Code permissions import adds to
// .rysh/policy.yaml. It can only make the policy stricter:
//
//   - Gate patterns (from deny and ask rules) are appended to
//     approval.always_gate, which org merge and the gate evaluate first.
//   - Suggest patterns (from allow rules) are written as COMMENTS marked
//     REVIEW. They grant nothing until a person moves them into
//     approval.auto_approve by hand.
//
// Nothing else in the file is changed: bash.allow, auto_approve, budgets,
// proxy, snat and any other section keep their values.
type PolicyPlan struct {
	Gate    []string `json:"gate,omitempty"`    // new approval.always_gate entries
	Suggest []string `json:"suggest,omitempty"` // allow-rules, written as comments only

	existing []byte
	rendered []byte // the new .rysh/policy.yaml
}

// ParseClaude reads the permissions.allow / ask / deny lists of Claude Code
// settings files. Bash rules become command globs; rules for other tools
// (Read, Edit, WebFetch, mcp__…) have no rysh approval-glob equivalent and are
// listed in the notes. existing is the current .rysh/policy.yaml (nil when
// absent); one that does not parse is refused, never rewritten.
func ParseClaude(files []NamedFile, existing []byte) (*Plan, error) {
	p := &Plan{Source: "claude", Inputs: []string{}}
	pp := &PolicyPlan{existing: existing}
	if len(bytes.TrimSpace(existing)) > 0 {
		var probe map[string]any
		if err := yaml.Unmarshal(existing, &probe); err != nil {
			return nil, fmt.Errorf("existing .rysh/policy.yaml does not parse (%v); fix it before importing — it is never overwritten", err)
		}
	}
	have := map[string]bool{}
	for _, g := range existingGates(existing) {
		have[g] = true
	}
	suggested := map[string]bool{}
	for _, f := range files {
		p.Inputs = append(p.Inputs, f.Label)
		var doc struct {
			Permissions struct {
				Allow []string `json:"allow"`
				Ask   []string `json:"ask"`
				Deny  []string `json:"deny"`
			} `json:"permissions"`
		}
		if err := json.Unmarshal(f.Data, &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", f.Label, err)
		}
		gate := func(kind string, rules []string) {
			for _, r := range rules {
				glob, ok := bashGlob(r)
				if !ok {
					p.note("%s rule %q (%s) has no rysh approval-glob equivalent; not imported%s", kind, r, f.Label, pathRuleHint(r))
					continue
				}
				if !have[glob] {
					have[glob] = true
					pp.Gate = append(pp.Gate, glob)
				}
			}
		}
		gate("deny", doc.Permissions.Deny)
		gate("ask", doc.Permissions.Ask)
		for _, r := range doc.Permissions.Allow {
			glob, ok := bashGlob(r)
			if !ok {
				p.note("allow rule %q (%s) has no rysh approval-glob equivalent; not suggested", r, f.Label)
				continue
			}
			if !suggested[glob] {
				suggested[glob] = true
				pp.Suggest = append(pp.Suggest, glob)
			}
		}
	}
	sort.Strings(pp.Suggest)
	if len(pp.Gate) > 0 {
		p.note("Claude Code deny rules become always_gate: rysh asks before running them instead of refusing outright")
	}
	if len(pp.Gate) > 0 || len(pp.Suggest) > 0 {
		rendered, err := pp.policyFile()
		if err != nil {
			return nil, err
		}
		pp.rendered = rendered
		p.Policy = pp
	}
	return p, nil
}

// bashGlob turns a Claude Code Bash rule into a rysh command glob:
// Bash(npm run test:*) -> "npm run test*", Bash(make lint) -> "make lint",
// Bash -> "*". ok is false for rules about other tools.
func bashGlob(rule string) (string, bool) {
	rule = strings.TrimSpace(rule)
	if rule == "Bash" || rule == "Bash()" || rule == "Bash(*)" {
		return "*", true
	}
	if !strings.HasPrefix(rule, "Bash(") || !strings.HasSuffix(rule, ")") {
		return "", false
	}
	spec := strings.TrimSpace(rule[len("Bash(") : len(rule)-1])
	if spec == "" {
		return "*", true
	}
	if strings.HasSuffix(spec, ":*") {
		return strings.TrimSpace(strings.TrimSuffix(spec, ":*")) + "*", true
	}
	return spec, true
}

// existingGates reads approval.always_gate from a policy document.
func existingGates(existing []byte) []string {
	var doc struct {
		Approval struct {
			AlwaysGate []string `yaml:"always_gate"`
		} `yaml:"approval"`
	}
	_ = yaml.Unmarshal(existing, &doc)
	return doc.Approval.AlwaysGate
}

// policyFile renders the new .rysh/policy.yaml: the existing document with
// the gate patterns appended to approval.always_gate, then the allow-rule
// suggestions as a trailing comment block.
func (pp *PolicyPlan) policyFile() ([]byte, error) {
	var doc yaml.Node
	if len(bytes.TrimSpace(pp.existing)) > 0 {
		if err := yaml.Unmarshal(pp.existing, &doc); err != nil {
			return nil, err
		}
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf(".rysh/policy.yaml is not a mapping; not changed")
	}
	var out bytes.Buffer
	if len(pp.Gate) > 0 {
		root := doc.Content[0]
		approval := mapChild(root, "approval", yaml.MappingNode)
		gate := mapChild(approval, "always_gate", yaml.SequenceNode)
		if gate.Kind != yaml.SequenceNode || approval.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("approval.always_gate in .rysh/policy.yaml is not a list; not changed")
		}
		// Block style, so each imported entry reads on its own line. A comment
		// that sat after a flow list belongs to its key now: left on the list,
		// the encoder prints it after the next key instead.
		if gate.Style&yaml.FlowStyle != 0 && gate.LineComment != "" {
			for i := 0; i+1 < len(approval.Content); i += 2 {
				if approval.Content[i+1] == gate && approval.Content[i].LineComment == "" {
					approval.Content[i].LineComment, gate.LineComment = gate.LineComment, ""
				}
			}
		}
		gate.Style = 0
		for _, g := range pp.Gate {
			gate.Content = append(gate.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: g, Style: yaml.DoubleQuotedStyle,
				LineComment: "imported from Claude Code (deny/ask)"})
		}
		enc := yaml.NewEncoder(&out)
		enc.SetIndent(2)
		if err := enc.Encode(&doc); err != nil {
			return nil, err
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
	} else {
		out.Write(pp.existing)
		if out.Len() > 0 && !bytes.HasSuffix(out.Bytes(), []byte("\n")) {
			out.WriteByte('\n')
		}
	}
	if len(pp.Suggest) > 0 {
		out.WriteString("\n# REVIEW — allow-rules imported from Claude Code by `rysh import claude`.\n")
		out.WriteString("# They are NOT active. To auto-approve one, move it into approval.auto_approve\n")
		out.WriteString("# yourself after checking it; rysh never loosens a policy on import.\n")
		out.WriteString("#   auto_approve:\n")
		for _, s := range pp.Suggest {
			fmt.Fprintf(&out, "#     - %s\n", strings.NewReplacer("\n", " ", "\r", " ").Replace(fmt.Sprintf("%q", s)))
		}
	}
	return out.Bytes(), nil
}

// mapChild returns the value node under key in a mapping, adding an empty
// one of the given kind when the key is absent.
func mapChild(m *yaml.Node, key string, kind yaml.Kind) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			if v.Kind == yaml.ScalarNode && v.Tag == "!!null" {
				v.Kind, v.Tag, v.Value = kind, map[yaml.Kind]string{yaml.MappingNode: "!!map", yaml.SequenceNode: "!!seq"}[kind], ""
			}
			return v
		}
	}
	v := &yaml.Node{Kind: kind, Tag: map[yaml.Kind]string{yaml.MappingNode: "!!map", yaml.SequenceNode: "!!seq"}[kind]}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

// pathRuleHint points file-path rules at policy fs: rules, which are not part
// of this import.
func pathRuleHint(rule string) string {
	for _, tool := range []string{"Read(", "Edit(", "Write(", "NotebookEdit("} {
		if strings.HasPrefix(rule, tool) {
			return " (file-path rules belong to policy fs: rules)"
		}
	}
	return ""
}
