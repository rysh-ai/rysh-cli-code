// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 WO-7 (v), U-6/H-5: the session's email.safety / subject_tag / audit_log
// reach the humanoid's email contact, and the restrictive setting wins — a
// contact cannot switch off a guard the session turned on.
func TestSessionEmailSafetyReachesContact(t *testing.T) {
	session := config.EmailConfig{Provider: "imap-smtp", IMAPHost: "imap.test", Safety: "read_only", SubjectTag: "RYSH-TEST-M1", AuditLog: "/tmp/a.jsonl"}
	for name, contact := range map[string]msg.ChannelConfig{
		"bare":            {Enabled: true},
		"contact off":     {Enabled: true, EmailConfig: &msg.EmailChannelConfig{Safety: "off"}},
		"contact own tag": {Enabled: true, EmailConfig: &msg.EmailChannelConfig{SubjectTag: "OTHER-TAG-123"}},
	} {
		got := applySessionEmailDefaults(map[string]msg.ChannelConfig{"email": contact}, session)["email"].EmailConfig
		if got.Safety != "read_only" {
			t.Errorf("%s: Safety = %q, want read_only (session guard must win)", name, got.Safety)
		}
		if got.AuditLog != "/tmp/a.jsonl" {
			t.Errorf("%s: AuditLog = %q", name, got.AuditLog)
		}
		if got.SubjectTag == "" {
			t.Errorf("%s: SubjectTag empty", name)
		}
	}
	// A contact may turn the guard on by itself, with the session's off.
	own := applySessionEmailDefaults(map[string]msg.ChannelConfig{"email": {EmailConfig: &msg.EmailChannelConfig{Safety: "read_only", SubjectTag: "RYSH-TEST-M1"}}}, config.EmailConfig{})["email"].EmailConfig
	if own.Safety != "read_only" || own.SubjectTag != "RYSH-TEST-M1" {
		t.Errorf("contact-only guard lost: %#v", own)
	}
	// Default: off, nothing set.
	off := applySessionEmailDefaults(map[string]msg.ChannelConfig{"email": {}}, config.EmailConfig{IMAPHost: "imap.test"})["email"].EmailConfig
	if off.Safety != "" || off.SubjectTag != "" {
		t.Errorf("guard must default off: %#v", off)
	}
}

// E17 WO-11, root-0320: the allow-list reaches the contact with the same
// precedence as the rest of the guard — a session that turns the guard on can
// only narrow the contact's allow-list, never widen it; disjoint layers leave
// a list the guard refuses (fail closed).
func TestSessionEmailAllowFromReachesContact(t *testing.T) {
	session := config.EmailConfig{Provider: "imap-smtp", IMAPHost: "imap.test", Safety: "read_only", SubjectTag: "RYSH-TEST-M1", AllowFrom: []string{"m1-run@example.test", "b@example.test"}}
	apply := func(contact msg.ChannelConfig, s config.EmailConfig) []string {
		return applySessionEmailDefaults(map[string]msg.ChannelConfig{"email": contact}, s)["email"].EmailConfig.AllowFrom
	}
	if got := apply(msg.ChannelConfig{}, session); fmt.Sprint(got) != "[m1-run@example.test b@example.test]" {
		t.Errorf("session only: %v", got)
	}
	if got := apply(msg.ChannelConfig{EmailConfig: &msg.EmailChannelConfig{AllowFrom: []string{"B@example.test", "c@example.test"}}}, session); fmt.Sprint(got) != "[B@example.test]" {
		t.Errorf("both layers: %v, want the intersection [B@example.test]", got)
	}
	if got := apply(msg.ChannelConfig{EmailConfig: &msg.EmailChannelConfig{AllowFrom: []string{"c@example.test"}}}, session); len(got) != 1 || strings.Contains(got[0], "@") {
		t.Errorf("disjoint layers: %v, want a single non-address sentinel the guard refuses", got)
	}
	contactOnly := apply(msg.ChannelConfig{EmailConfig: &msg.EmailChannelConfig{Safety: "read_only", AllowFrom: []string{"c@example.test"}}}, config.EmailConfig{})
	if fmt.Sprint(contactOnly) != "[c@example.test]" {
		t.Errorf("contact only: %v", contactOnly)
	}
	// A session whose guard is off does not touch the contact's list.
	off := session
	off.Safety = ""
	if got := apply(msg.ChannelConfig{EmailConfig: &msg.EmailChannelConfig{AllowFrom: []string{"c@example.test"}}}, off); fmt.Sprint(got) != "[c@example.test]" {
		t.Errorf("session guard off: %v", got)
	}
}
