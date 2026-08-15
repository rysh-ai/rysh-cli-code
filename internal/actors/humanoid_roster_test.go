// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestBuildHumanoidRosterMergesSources verifies the merge behind the default
// "##humanoid list": running instances, deactivated ones, and skill files that
// were never spawned all land in one roster, each with the right status, and a
// name present in both sources yields exactly one row.
func TestBuildHumanoidRosterMergesSources(t *testing.T) {
	loaded := []msg.HumanoidInfo{
		{Name: "slack-bot", Active: true},
		{Name: "email-bot", Active: false},
		{Name: "scratch", Active: true}, // spawned inline, no skill file
	}
	defs := []*humanoidDefinition{
		{Name: "slack-bot", Description: "Slack concierge"},
		{Name: "email-bot", Description: "Email desk"},
		{Name: "sms-notifier", Description: "SMS notifier"},
	}

	rows := buildHumanoidRoster(loaded, defs)
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows (3 loaded + 1 unspawned artefact), got %d: %+v", len(rows), rows)
	}

	byName := map[string]humanoidRosterRow{}
	for _, r := range rows {
		if _, dup := byName[r.Name]; dup {
			t.Fatalf("humanoid %q appears twice — sources not merged", r.Name)
		}
		byName[r.Name] = r
	}

	if got := byName["slack-bot"].Status; got != humanoidStatusRunning {
		t.Errorf("slack-bot status = %q, want %q", got, humanoidStatusRunning)
	}
	if got := byName["email-bot"].Status; got != humanoidStatusPaused {
		t.Errorf("deactivated email-bot status = %q, want %q", got, humanoidStatusPaused)
	}
	// The point of the merge: a skill file that was never spawned still shows up.
	if got := byName["sms-notifier"].Status; got != humanoidStatusStopped {
		t.Errorf("unspawned sms-notifier status = %q, want %q", got, humanoidStatusStopped)
	}
	if !byName["sms-notifier"].OnDisk {
		t.Errorf("sms-notifier should be marked as on disk")
	}
	// An inline humanoid has no file to re-spawn from; the roster must say so.
	if byName["scratch"].OnDisk {
		t.Errorf("inline humanoid %q should not be marked as on disk", "scratch")
	}
	if !byName["slack-bot"].OnDisk {
		t.Errorf("slack-bot has a skill file and should be marked as on disk")
	}
}

// TestBuildHumanoidRosterOrdering verifies running sorts before paused before
// stopped, and names sort alphabetically within a status.
func TestBuildHumanoidRosterOrdering(t *testing.T) {
	loaded := []msg.HumanoidInfo{
		{Name: "zulu", Active: true},
		{Name: "alpha", Active: true},
		{Name: "bravo", Active: false},
	}
	defs := []*humanoidDefinition{{Name: "charlie"}}

	rows := buildHumanoidRoster(loaded, defs)
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.Name)
	}
	want := []string{"alpha", "zulu", "bravo", "charlie"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("roster order = %v, want %v", got, want)
	}
}

// TestBuildHumanoidRosterEmptySources verifies the no-humanoids case produces
// no rows (the caller prints the "write one at ..." hint instead of a table).
func TestBuildHumanoidRosterEmptySources(t *testing.T) {
	if rows := buildHumanoidRoster(nil, nil); len(rows) != 0 {
		t.Errorf("expected no rows from empty sources, got %+v", rows)
	}
}

// TestChannelSummaries verifies the channel column: types are sorted, a
// disconnected live channel is flagged with "!", and no channels renders "-".
func TestChannelSummaries(t *testing.T) {
	live := liveChannelSummary([]msg.ChannelStatus{
		{Type: "slack", Connected: true},
		{Type: "email", Connected: false},
	})
	if live != "email!,slack" {
		t.Errorf("liveChannelSummary = %q, want %q", live, "email!,slack")
	}
	if got := liveChannelSummary(nil); got != "-" {
		t.Errorf("liveChannelSummary(nil) = %q, want %q", got, "-")
	}

	defined := definedChannelSummary(map[string]msg.ChannelConfig{
		"whatsapp": {Enabled: true},
		"slack":    {Enabled: true},
	})
	if defined != "slack,whatsapp" {
		t.Errorf("definedChannelSummary = %q, want %q", defined, "slack,whatsapp")
	}
	if got := definedChannelSummary(nil); got != "-" {
		t.Errorf("definedChannelSummary(nil) = %q, want %q", got, "-")
	}
}

// TestRenderHumanoidRoster verifies the table shows every state, counts them in
// the header, and names both spawn and stop so the roster is self-documenting.
func TestRenderHumanoidRoster(t *testing.T) {
	rows := []humanoidRosterRow{
		{Name: "slack-bot", Status: humanoidStatusRunning, Channels: "slack", Detail: "panes: chat", OnDisk: true},
		{Name: "scratch", Status: humanoidStatusRunning, Channels: "-", Detail: "no output pane registered"},
		{Name: "email-bot", Status: humanoidStatusPaused, Channels: "email!", Detail: "panes: mail", OnDisk: true},
		{Name: "sms-notifier", Status: humanoidStatusStopped, Channels: "sms", Detail: "SMS notifier", OnDisk: true},
	}

	var out strings.Builder
	renderHumanoidRoster(&out, ".rysh/humanoids", rows)
	s := out.String()

	if !strings.Contains(s, "4 humanoid(s): 2 running, 1 paused, 1 stopped") {
		t.Errorf("expected status counts in header, got:\n%s", s)
	}
	for _, name := range []string{"slack-bot", "scratch", "email-bot", "sms-notifier"} {
		if !strings.Contains(s, name) {
			t.Errorf("humanoid %q missing from roster:\n%s", name, s)
		}
	}
	for _, status := range []string{humanoidStatusRunning, humanoidStatusPaused, humanoidStatusStopped} {
		if !strings.Contains(s, status) {
			t.Errorf("status %q missing from roster:\n%s", status, s)
		}
	}
	if !strings.Contains(s, "inline (no skill file") {
		t.Errorf("inline humanoid should be flagged as unrecoverable:\n%s", s)
	}
	if !strings.Contains(s, "##humanoid spawn") || !strings.Contains(s, "##humanoid stop") {
		t.Errorf("footer should name both spawn and stop:\n%s", s)
	}
}
