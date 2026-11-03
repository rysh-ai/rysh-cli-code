// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// These all fail before a session is looked up, so they need no daemon and
// can never reach a live session.
func TestRunAnsaKeysCmdLocalErrors(t *testing.T) {
	t.Setenv("RYSH_SESSION", "")
	t.Setenv("RYSH_PANE", "")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"ansa", "keys"}, "needs a target"},
		{[]string{"ansa", "keys", "--", "esc"}, "needs a target"},
		{[]string{"ansa", "keys", "@w"}, "no keys named"},
		{[]string{"ansa", "keys", "@w", "--", "hello"}, `unknown key "hello"`},
		{[]string{"ansa", "keys", "@w", "--", "1"}, `unknown key "1"`},
		{[]string{"ansa", "keys", "@w", "--", "esc*99"}, "repeat"},
		{[]string{"ansa", "keys", "@w", "--fleet", "x", "--", "esc"}, "unknown flag"},
		{[]string{"ansa", "keys", "--claude-session"}, "needs a session id"},
	} {
		err := runAnsaCmd(config.Config{}, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("runAnsaCmd(%q) = %v, want an error containing %q", tc.args, err, tc.want)
		}
	}
}

// TestAnsaUsageNamesKeysAudit — LEAD note N2: the CLI help names the audit file.
func TestAnsaUsageNamesKeysAudit(t *testing.T) {
	if !strings.Contains(ansaUsageText, "ansa-keys-audit.jsonl") {
		t.Errorf("rysh ansa usage does not name the keys audit file:\n%s", ansaUsageText)
	}
}
