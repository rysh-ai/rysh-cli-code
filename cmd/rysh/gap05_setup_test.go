// SPDX-License-Identifier: Apache-2.0

package main

// gap05_setup_test.go — `rysh setup` for the channels gap05 added
// (Mattermost, Matrix, LINE): offered, guided, written headless with secrets
// as ${NAME} only, and validated through the adapter's identity check.
//
// Every credential is a synthetic name with a fake value; every validator is
// a fake; every run happens in a throwaway directory.

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
)

var gap05SetupChannels = []string{"mattermost", "matrix", "line"}

// TestSetupOffersGap05Channels: setup's picker, its usage line, the
// steps-only guides and the owner prompt all know the three channels.
func TestSetupOffersGap05Channels(t *testing.T) {
	offered := map[string]bool{}
	for _, p := range setupPresets() {
		offered[p.Channel] = true
	}
	guides := map[string]bool{}
	for _, ch := range setupGuideChannels {
		guides[ch] = true
	}
	generic := ownerContactHint("no-such-channel")
	for _, ch := range gap05SetupChannels {
		if !offered[ch] {
			t.Errorf("setup does not offer %s", ch)
		}
		if !guides[ch] {
			t.Errorf("setup --steps-only has no %s guide", ch)
		}
		if h := ownerContactHint(ch); h.What == generic.What || h.Where == "" {
			t.Errorf("%s owner hint = %+v, want a specific id format and where to find it", ch, h)
		}
	}
	if !strings.Contains(strings.Join(setupPresetOrder, " "), "mattermost matrix line") {
		t.Errorf("setupPresetOrder = %v", setupPresetOrder)
	}
}

// gap05HeadlessCase is one channel's headless setup: its flags (names only)
// and what its profile block must hold afterwards.
type gap05HeadlessCase struct {
	channel   string
	owner     string
	flags     []string
	resolve   []string
	serverURL string
	botToken  string
	appSecret string
	bot       string
	handle    string
}

func gap05HeadlessCases() []gap05HeadlessCase {
	return []gap05HeadlessCase{
		{channel: "mattermost", owner: "8xq3owneridmattermost00000",
			flags:   []string{"--server-url", "https://chat.example.com", "--bot-token-env", "MM_GAP05_FAKE"},
			resolve: []string{"E33_FAKE_KEY", "MM_GAP05_FAKE"}, serverURL: "https://chat.example.com",
			botToken: "${MM_GAP05_FAKE}", bot: "ryshbot", handle: "@ryshbot"},
		{channel: "matrix", owner: "@owner:example.org",
			flags:   []string{"--server-url", "https://matrix.example.org", "--bot-token-env", "MX_GAP05_FAKE"},
			resolve: []string{"E33_FAKE_KEY", "MX_GAP05_FAKE"}, serverURL: "https://matrix.example.org",
			botToken: "${MX_GAP05_FAKE}", bot: "ryshbot", handle: "@ryshbot"},
		{channel: "line", owner: "U0123456789abcdef0123456789abcdef",
			flags: []string{"--bot-token-env", "LN_GAP05_TOKEN", "--app-secret-env", "LN_GAP05_SECRET",
				"--webhook-port", "8790"},
			resolve:  []string{"E33_FAKE_KEY", "LN_GAP05_TOKEN", "LN_GAP05_SECRET"},
			botToken: "${LN_GAP05_TOKEN}", appSecret: "${LN_GAP05_SECRET}", bot: "123abcde", handle: "@123abcde"},
	}
}

// TestSetupHeadlessGap05Channels: the flag form writes each channel's block
// with secrets as ${NAME}, the owner as the only allowlisted sender, PASSes on
// the validator, prints the binding line, and never prints a value.
func TestSetupHeadlessGap05Channels(t *testing.T) {
	for _, c := range gap05HeadlessCases() {
		t.Run(c.channel, func(t *testing.T) {
			setupWorkspace(t)
			args := append([]string{"--provider", "anthropic", "--key-env", "E33_FAKE_KEY", "--no-validate",
				"--channel", c.channel, "--owner", c.owner}, c.flags...)
			var js strings.Builder
			out, err := runSetupCaptured(t, args, assistantEnv{
				in:      bufio.NewReader(strings.NewReader("")),
				deps:    e33Deps(c.resolve, botStub{stubAdapter: stubAdapter{typ: c.channel}, bot: c.bot}),
				jsonOut: &js,
			})
			if err != nil {
				t.Fatalf("runSetup: %v\n%s", err, masked(out))
			}
			fm, _ := parseAssistantSkill(t, assistantSkillPath())
			cc, ok := fm.Contacts[c.channel]
			if !ok {
				t.Fatalf("profile has no contacts.%s block", c.channel)
			}
			if cc.BotToken != c.botToken || cc.AppSecret != c.appSecret || cc.ServerURL != c.serverURL {
				t.Errorf("block: bot_token %q app_secret %q server_url %q", cc.BotToken, cc.AppSecret, cc.ServerURL)
			}
			if len(cc.Allowlist) != 1 || cc.Allowlist[0] != c.owner {
				t.Errorf("allowlist = %v, want only %s", cc.Allowlist, c.owner)
			}
			var res setupJSON
			if err := json.Unmarshal([]byte(js.String()), &res); err != nil {
				t.Fatalf("setup --json: %v", err)
			}
			if !res.OK || res.Fails != 0 || res.Binding == nil || res.Binding.Handle != c.handle {
				t.Errorf("json ok %v fails %d binding %+v", res.OK, res.Fails, res.Binding)
			}
			if !strings.Contains(out, c.channel+" @"+c.bot+" → assistant (owner "+c.owner+")") {
				t.Errorf("binding line missing:\n%s", masked(out))
			}
			if !strings.Contains(out, "Step 3 — create your "+c.channel+" bot") {
				t.Errorf("bot-creation steps not printed:\n%s", masked(out))
			}
			if strings.Contains(out+js.String(), e33FakeValue) {
				t.Error("setup printed the credential value")
			}
		})
	}
}

// TestSetupRefusesGap05SecretsInArgv: LINE's channel secret is new to the
// presets, so its flag must be refused as a value like every other secret.
func TestSetupRefusesGap05SecretsInArgv(t *testing.T) {
	setupWorkspace(t)
	_, err := runSetupCaptured(t, []string{"--channel", "line", "--owner", "U0", "--app-secret", e33FakeValue},
		assistantEnv{in: bufio.NewReader(strings.NewReader("")), deps: e33Deps(nil, stubAdapter{typ: "line"})})
	if err == nil || !strings.Contains(err.Error(), "--app-secret-env") {
		t.Fatalf("err = %v, want a refusal naming --app-secret-env", err)
	}
	if strings.Contains(err.Error(), e33FakeValue) {
		t.Error("the refusal echoed the value")
	}
}

// TestSetupStepsOnlyGap05: --steps-only --channel <new> prints that guide.
func TestSetupStepsOnlyGap05(t *testing.T) {
	setupWorkspace(t)
	for _, ch := range gap05SetupChannels {
		var js strings.Builder
		if _, err := runSetupCaptured(t, []string{"--steps-only", "--json", "--channel", ch}, assistantEnv{
			in: bufio.NewReader(strings.NewReader("")), deps: e33Deps(nil, stubAdapter{typ: ch}), jsonOut: &js,
		}); err != nil {
			t.Fatalf("%s: %v", ch, err)
		}
		var got setupStepsJSON
		_ = json.Unmarshal([]byte(js.String()), &got)
		want, _ := channels.BotSetupSteps(ch, channels.BotIdentity{})
		if len(got.Guides) != 1 || got.Guides[0].Channel != ch || len(want) == 0 ||
			strings.Join(got.Guides[0].Steps, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s guides = %+v", ch, got.Guides)
		}
	}
}
