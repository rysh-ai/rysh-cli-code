// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Approval evaluation for policy-as-code (design 013 §1). Consumes the four
// rule classes that Load parses but nothing enforced before this: bash.allow,
// bash.deny, approval.auto_approve, approval.always_gate. The verdict is fed to
// the orchestrator's approval decision via the process-global hook installed by
// WorkspaceActor.

// ApprovalAction is a policy verdict for a tool call.
type ApprovalAction int

const (
	// ApprovalDefault — no rule matched; use the tool's own classifier.
	ApprovalDefault ApprovalAction = iota
	// ApprovalAuto — a permissive rule matched; skip approval.
	ApprovalAuto
	// ApprovalGate — a restrictive rule matched; force approval.
	ApprovalGate
)

// EvaluateToolApproval matches a tool call against the four rule classes and
// returns the verdict plus the matched rule ID (e.g. "bash.deny[1]"; org rules
// cite an "org." prefix, e.g. "org.bash.deny[0]", so the audit line names the
// file the rule came from).
//
// Restrictive-wins: always_gate / bash.deny are evaluated before
// auto_approve / bash.allow, so a permissive rule can never silently un-gate a
// call the operator also listed as gated. With an org policy merged in
// (OrgActive), org restrictive rules are evaluated before everything else —
// nothing in the project file can override them — and permissive rules
// intersect: both files must allow, so the project can narrow the org
// allowlist but never widen it (the cited ID then names both concurring
// rules). Tool-name rule classes (approval.*) match the tool name; command
// rule classes (bash.*) match the bash command string (bash /
// bash_background). Globs are whole-string with '*' as the only wildcard — use
// "*x*" for a substring match (so a deny survives compound commands like
// `cd /t && rm -rf x`).
func (p *Policy) EvaluateToolApproval(toolName string, input json.RawMessage) (ApprovalAction, string) {
	if p == nil {
		return ApprovalDefault, ""
	}
	cmd := ""
	if isBashTool(toolName) {
		cmd = extractCommand(input)
	}

	// Org restrictive rules first: the org file is the higher authority and
	// its gates/denies cannot be overridden by anything in the project file.
	if p.OrgActive {
		if id, ok := matchRule("org.approval.always_gate", p.OrgAlwaysGate, toolName); ok {
			return ApprovalGate, id
		}
		if cmd != "" {
			if id, ok := matchRule("org.bash.deny", p.OrgBashDeny, cmd); ok {
				return ApprovalGate, id
			}
		}
	}
	// Project restrictive rules.
	if id, ok := matchRule("approval.always_gate", p.AlwaysGate, toolName); ok {
		return ApprovalGate, id
	}
	if cmd != "" {
		if id, ok := matchRule("bash.deny", p.BashDeny, cmd); ok {
			return ApprovalGate, id
		}
	}
	// Permissive rules (intersected with the org lists when OrgActive).
	if id, ok := p.matchPermissive("approval.auto_approve", p.OrgAutoApprove, p.AutoApprove, toolName); ok {
		return ApprovalAuto, id
	}
	if cmd != "" {
		if id, ok := p.matchPermissive("bash.allow", p.OrgBashAllow, p.BashAllow, cmd); ok {
			return ApprovalAuto, id
		}
	}
	return ApprovalDefault, ""
}

// matchPermissive resolves a permissive rule class under merge semantics: no
// org policy → the project list alone (pre-org behavior); org policy with no
// project file → the org list alone; both files present → intersection (both
// must match — the project can only narrow). The cited ID names every rule
// that had to concur.
func (p *Policy) matchPermissive(class string, orgList, projList []string, s string) (string, bool) {
	if !p.OrgActive {
		return matchRule(class, projList, s)
	}
	orgID, orgOK := matchRule("org."+class, orgList, s)
	if !p.projectLoaded {
		return orgID, orgOK
	}
	projID, projOK := matchRule(class, projList, s)
	if orgOK && projOK {
		return projID + "+" + orgID, true
	}
	return "", false
}

func isBashTool(name string) bool { return name == "bash" || name == "bash_background" }

// extractCommand pulls the shell command out of a bash tool's JSON input.
func extractCommand(input json.RawMessage) string {
	var p struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &p)
	return strings.TrimSpace(p.Command)
}

// matchRule returns the ID of the first pattern that globs s, or ("", false).
func matchRule(class string, patterns []string, s string) (string, bool) {
	for i, pat := range patterns {
		if globMatch(pat, s) {
			return fmt.Sprintf("%s[%d]", class, i), true
		}
	}
	return "", false
}

// globMatch matches s against a glob whose only wildcard is '*' (any run,
// including '/', unlike path.Match). The whole string must match.
func globMatch(pattern, s string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return pattern == s
	}
	parts := strings.Split(pattern, "*")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	re := "^" + strings.Join(parts, ".*") + "$"
	matched, err := regexp.MatchString(re, s)
	return err == nil && matched
}
