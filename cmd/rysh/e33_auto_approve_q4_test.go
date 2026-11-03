// SPDX-License-Identifier: Apache-2.0

package main

// E33 Q4 (root-order-052): setup writes auto_approve: false, so a tool call
// triggered from the chat is held and sent to the owner for a "yes". A profile
// that already exists keeps whatever it says.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssistantScaffoldWritesAutoApproveFalse(t *testing.T) {
	chdirTemp(t)
	for _, ch := range []string{"telegram", "discord", "slack"} {
		fields := channelCredFields(ch)
		content, err := buildAssistantSkill("anthropic", "claude-sonnet-5", ch,
			fields, make([]string, len(fields)), "424242")
		if err != nil {
			t.Fatalf("%s: %v", ch, err)
		}
		if !strings.Contains(content, "auto_approve: false") || strings.Contains(content, "auto_approve: true") {
			t.Errorf("%s: new profile must say auto_approve: false", ch)
		}
	}
}

func TestExistingProfileAutoApproveUnchanged(t *testing.T) {
	dir := chdirTemp(t)
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"auto_approve: true\n", true},
		{"auto_approve: false\n", false},
		{"", true}, // absent: the runtime default
	} {
		path := filepath.Join(dir, "assistant.md")
		body := "---\nname: assistant\nprofile: assistant\n" + tc.line + "---\nhi\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := assistantAutoApprove(path); got != tc.want {
			t.Errorf("profile %q: auto_approve read as %v, want %v", tc.line, got, tc.want)
		}
	}
}
