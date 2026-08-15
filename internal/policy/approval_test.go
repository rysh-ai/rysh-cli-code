// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"encoding/json"
	"testing"
)

func bashInput(cmd string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"command": cmd})
	return b
}

// TestEvaluateToolApproval covers the four rule classes design 013 parses but
// nothing enforced before this: bash.allow/deny (command-matched) and
// approval.auto_approve/always_gate (tool-name-matched), plus restrictive-wins
// precedence and rule-ID citation.
func TestEvaluateToolApproval(t *testing.T) {
	p := &Policy{
		BashAllow:   []string{"git status", "ls*"},
		BashDeny:    []string{"*rm -rf*"},
		AutoApprove: []string{"file_read", "grep"},
		AlwaysGate:  []string{"git_commit", "file_write"},
		Loaded:      true,
	}

	cases := []struct {
		name   string
		tool   string
		input  json.RawMessage
		want   ApprovalAction
		wantID string
	}{
		{"bash.allow exact", "bash", bashInput("git status"), ApprovalAuto, "bash.allow[0]"},
		{"bash.allow glob", "bash", bashInput("ls -la /tmp"), ApprovalAuto, "bash.allow[1]"},
		{"bash.deny substring", "bash", bashInput("cd /t && rm -rf x"), ApprovalGate, "bash.deny[0]"},
		{"auto_approve tool", "file_read", nil, ApprovalAuto, "approval.auto_approve[0]"},
		{"always_gate tool", "git_commit", nil, ApprovalGate, "approval.always_gate[0]"},
		{"unmatched tool", "web_fetch", nil, ApprovalDefault, ""},
		{"unmatched command", "bash", bashInput("echo hi"), ApprovalDefault, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			act, id := p.EvaluateToolApproval(c.tool, c.input)
			if act != c.want || id != c.wantID {
				t.Fatalf("EvaluateToolApproval(%s) = (%d, %q), want (%d, %q)", c.tool, act, id, c.want, c.wantID)
			}
		})
	}
}

// TestApprovalRestrictiveWins proves a permissive rule can never un-gate a call
// the operator ALSO listed as restricted — the whole point of a governance
// policy. Same tool/command matched by both classes must resolve to Gate.
func TestApprovalRestrictiveWins(t *testing.T) {
	// Tool name in BOTH auto_approve and always_gate → gate.
	p := &Policy{AutoApprove: []string{"edit"}, AlwaysGate: []string{"edit"}, Loaded: true}
	if act, id := p.EvaluateToolApproval("edit", nil); act != ApprovalGate {
		t.Fatalf("auto+gate on same tool = %d (%q), want Gate", act, id)
	}
	// Command in BOTH bash.allow and bash.deny → gate.
	p2 := &Policy{BashAllow: []string{"*curl*"}, BashDeny: []string{"*curl*"}, Loaded: true}
	if act, _ := p2.EvaluateToolApproval("bash", bashInput("curl http://x")); act != ApprovalGate {
		t.Fatalf("allow+deny on same command = %d, want Gate", act)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"git status", "git status", true},
		{"git status", "git statuses", false},
		{"ls*", "ls -la", true},
		{"rm -rf*", "rm -rf /tmp/x", true}, // '*' must cross '/', unlike path.Match
		{"*rm -rf*", "cd /t && rm -rf x", true},
		{"a*b", "aXb", true},
		{"a*b", "aXbY", false}, // trailing chars after b must not match
		{"", "anything", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pat, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pat, c.s, got, c.want)
		}
	}
}
