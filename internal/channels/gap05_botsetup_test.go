// SPDX-License-Identifier: Apache-2.0

package channels

// gap05_botsetup_test.go — the bot-creation guides `rysh setup` prints for the
// three channels gap05 added (Mattermost, Matrix, LINE). Pure text, like the
// E33 guides in e33_botsetup_test.go.

import (
	"strings"
	"testing"
)

func TestBotSetupStepsGap05Channels(t *testing.T) {
	cases := []struct {
		channel string
		want    []string // facts the steps must carry
		id      BotIdentity
		last    []string // what the last step names once the bot is known
	}{
		{"mattermost", []string{"Bot Accounts", "access token", "https://", "user ID"},
			BotIdentity{ID: "abc", Username: "ryshbot"}, []string{"@ryshbot"}},
		{"matrix", []string{"access token", "https://", "unencrypted", "@you:example.org"},
			BotIdentity{ID: "@ryshbot:example.org", Username: "ryshbot"}, []string{"@ryshbot:example.org"}},
		{"line", []string{"Messaging API", "Channel secret", "Channel access token", "/line/webhook", "Your user ID"},
			BotIdentity{ID: "Uabc", Username: "123abcde"}, []string{"@123abcde"}},
	}
	for _, c := range cases {
		steps, ok := BotSetupSteps(c.channel, BotIdentity{})
		if !ok || len(steps) < 4 {
			t.Errorf("%s steps = %v (ok=%v)", c.channel, steps, ok)
			continue
		}
		all := strings.Join(steps, "\n")
		for _, w := range c.want {
			if !strings.Contains(all, w) {
				t.Errorf("%s steps miss %q", c.channel, w)
			}
		}
		if !strings.Contains(all, ".rysh/secrets") {
			t.Errorf("%s steps do not say where the token is stored", c.channel)
		}
		if !strings.HasPrefix(steps[0], "1. ") {
			t.Errorf("%s steps are not numbered: %q", c.channel, steps[0])
		}
		steps, _ = BotSetupSteps(c.channel, c.id)
		last := steps[len(steps)-1]
		for _, w := range c.last {
			if !strings.Contains(last, w) {
				t.Errorf("%s last step = %q, want it to name %q", c.channel, last, w)
			}
		}
	}
}
