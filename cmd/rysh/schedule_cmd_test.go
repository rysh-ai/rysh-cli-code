// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"testing"
)

// `rysh schedule` / `rysh trigger` are `rysh exec -- '##cron …'` /
// `'##trigger …'`. The one thing the alias adds is keeping a multi-word cron
// schedule one token: the shell strips the quotes from "0 9 * * *", and the
// ##cron parser needs them back to know where the schedule ends.
func TestAliasExecArgs(t *testing.T) {
	cases := []struct {
		word string
		in   []string
		want []string
	}{
		{"cron", []string{"schedule", "add", "standup", "0 9 * * 1-5", "@scribe", "write the standup"},
			[]string{"exec", "--", `##cron add standup "0 9 * * 1-5" @scribe write the standup`}},
		{"cron", []string{"schedule", "add", "hourly", "@every 1h", "summarise"},
			[]string{"exec", "--", `##cron add hourly "@every 1h" summarise`}},
		{"cron", []string{"schedule", "--session", "s1", "list"},
			[]string{"exec", "--session", "s1", "--", "##cron list"}},
		{"cron", []string{"schedule", "--json", "logs", "standup", "5"},
			[]string{"exec", "--json", "--", "##cron logs standup 5"}},
		{"cron", []string{"schedule"},
			[]string{"exec", "--", "##cron list"}},
		{"trigger", []string{"trigger", "add", "gh-push", "@reviewer", "review the push"},
			[]string{"exec", "--", "##trigger add gh-push @reviewer review the push"}},
		{"trigger", []string{"trigger", "listen", "127.0.0.1:23320"},
			[]string{"exec", "--", "##trigger listen 127.0.0.1:23320"}},
	}
	for _, c := range cases {
		if got := aliasExecArgs(c.word, c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("aliasExecArgs(%q, %q)\n got %q\nwant %q", c.word, c.in, got, c.want)
		}
	}
}

func TestAliasCommandsSkipPTYPreflight(t *testing.T) {
	for _, w := range []string{"schedule", "trigger"} {
		if !ptylessCommands[w] {
			t.Errorf("%q talks to a running daemon and must not require a PTY", w)
		}
	}
}
