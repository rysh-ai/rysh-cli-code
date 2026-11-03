// SPDX-License-Identifier: Apache-2.0

package main

// E33 G3: `rysh doctor` validates Telegram and Discord credentials with the
// adapters' own non-binding Validate — no more "no non-binding validation"
// WARN for either. The token references here are deliberately unset, so the
// real adapters fail on the empty token before any request is built: this
// test makes no network call.

import (
	"strings"
	"testing"
)

func TestDoctorValidatesTelegramAndDiscord(t *testing.T) {
	for _, ch := range []string{"telegram", "discord"} {
		t.Run(ch, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			cfg := writeGoodState(t, dir)
			skill := "---\nname: remote\nmodel: claude-sonnet-5\ncontacts:\n  " + ch +
				":\n    bot_token: \"${E33_FAKE_UNSET_TOKEN}\"\n    governance: ai\n    allowlist: [\"123456789\"]\n---\nYou are remote.\n"
			if _, err := writeSkillFile("remote", skill, false); err != nil {
				t.Fatal(err)
			}
			t.Setenv("E33_FAKE_UNSET_TOKEN", "")

			checks := doctorChecks(cfg, fakeDeps()) // real channels.NewAdapter
			if c, ok := findCheck(checks, "channels", "no non-binding validation for "+ch); ok {
				t.Fatalf("doctor still WARNs for %s: %+v", ch, c)
			}
			c, ok := findCheck(checks, "channels", "remote/"+ch)
			if !ok {
				t.Fatalf("no channels check for remote/%s", ch)
			}
			if c.Status != doctorFail || !strings.Contains(c.Detail, "bot_token") {
				t.Errorf("check = %+v, want FAIL naming bot_token", c)
			}
			if !strings.Contains(c.Fix, "E33_FAKE_UNSET_TOKEN") {
				t.Errorf("fix = %q, want it to name the empty ${E33_FAKE_UNSET_TOKEN}", c.Fix)
			}
		})
	}
}
