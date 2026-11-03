// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// These all fail before a session is looked up, so they need no daemon and
// can never reach a live session.
func TestRunAnsaHungCmdLocalErrors(t *testing.T) {
	t.Setenv("RYSH_SESSION", "")
	t.Setenv("RYSH_PANE", "")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"ansa", "hung"}, "needs ONE of"},
		{[]string{"ansa", "hung", "@w", "--all"}, "needs ONE of"},
		{[]string{"ansa", "hung", "@w", "--claude-session", "x"}, "needs ONE of"},
		{[]string{"ansa", "hung", "@w", "@v"}, "takes one target"},
		{[]string{"ansa", "hung", "@w", "--minutes", "abc"}, "--minutes must be"},
		{[]string{"ansa", "hung", "@w", "--minutes", "0"}, "--minutes must be"},
		{[]string{"ansa", "hung", "@w", "--minutes"}, "--minutes needs a number"},
		{[]string{"ansa", "hung", "--claude-session"}, "needs a session id"},
		{[]string{"ansa", "hung", "@w", "--fleet", "x"}, "unknown flag"},
	} {
		err := runAnsaCmd(config.Config{}, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("runAnsaCmd(%q) = %v, want an error containing %q", tc.args, err, tc.want)
		}
	}
}

func TestAnsaUsageNamesHung(t *testing.T) {
	for _, want := range []string{"rysh ansa hung", "never transcript content"} {
		if !strings.Contains(ansaUsageText, want) {
			t.Errorf("rysh ansa usage lacks %q:\n%s", want, ansaUsageText)
		}
	}
}
