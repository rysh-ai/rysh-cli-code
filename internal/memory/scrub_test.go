// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"strings"
	"testing"
)

// The credential-shaped inputs below are synthetic: built at run time from
// fragments so no secret-looking literal sits in the source, and no test
// prints, records or hashes a value. A failing case reports its name only.

func synthRun(n int) string {
	const alphabet = "Qw7eRt2YuI9oPa4SdF6gHj1KlZ8xCv3BnM5"
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(alphabet[(i*11+3)%len(alphabet)])
	}
	return b.String()
}

func TestCredentialShapedRefusesKnownShapes(t *testing.T) {
	run := synthRun(36)
	cases := []struct {
		name string
		text string
		rule string
	}{
		{"assignment-password", "db " + "pass" + "word=" + "hunter2hunter2", RuleAssignment},
		{"assignment-token-colon", "the api" + "_key: " + run, RuleAssignment},
		{"vcs-token-prefix", "use " + "gh" + "p_" + run, RulePrefix},
		{"chat-token-prefix", "bot " + "xo" + "xb-" + run, RulePrefix},
		{"cloud-key-id", "id " + "AK" + "IA" + "ABCDEFGHIJKLMNOP", RulePrefix},
		{"private-key-block", "-----BEGIN " + "RSA PRIVATE KEY-----\nabc", RulePrivateKey},
		{"bearer-header", "Authorization: " + "Bearer " + run, RuleBearer},
		{"url-password", "clone https://bob:" + "s3cretpass" + "@git.example.test/x", RuleURLPassword},
		{"high-entropy-run", "the value is " + run + run, RuleEntropy},
	}
	for _, c := range cases {
		rule, found := CredentialShaped(c.text, nil)
		if !found {
			t.Errorf("%s: not refused", c.name)
			continue
		}
		if rule != c.rule {
			t.Errorf("%s: rule = %q, want %q", c.name, rule, c.rule)
		}
	}
}

func TestCredentialShapedAllowsOrdinaryNotes(t *testing.T) {
	ok := []string{
		"prefer tabs over spaces in Go files",
		"build with make build; tests with go test ./...",
		"merged at 48153ba and 7a81d3fc1e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b", // hex shas stay allowed
		"the password policy lives in docs/security.md",                  // the word alone is fine
		"token budgets are set with ##cost budget",
		"see https://example.test/docs for the API",
		"user prefers British spelling",
	}
	for i, text := range ok {
		if rule, found := CredentialShaped(text, nil); found {
			t.Errorf("ordinary note %d refused by rule %q", i, rule)
		}
	}
}

func TestCredentialShapedMatchesKnownSecretValues(t *testing.T) {
	known := []string{"zz-" + synthRun(12), "short"}
	text := "remember that the deploy value is " + known[0] + " ok"
	rule, found := CredentialShaped(text, known)
	if !found || rule != RuleKnownSecret {
		t.Fatalf("known secret value: found=%v rule=%q, want %q", found, rule, RuleKnownSecret)
	}
	// Very short registered values are not matched (they would refuse
	// ordinary words); the other rules still apply.
	if _, found := CredentialShaped("a short note", known); found {
		t.Fatalf("a short registered value must not refuse ordinary text")
	}
}

func TestRefusalMessageNeverCarriesTheValue(t *testing.T) {
	secret := "gh" + "p_" + synthRun(36)
	err := refusal(RulePrefix)
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), secret[:6]) {
		t.Fatalf("refusal message carries part of the value")
	}
	if !strings.Contains(err.Error(), RulePrefix) {
		t.Fatalf("refusal message does not name the rule")
	}
}

// TestCredentialShapedReusesSNATDetectors: a shape only SecretNAT's built-in
// detectors know (no rule of this file matches it) is refused, and the rule
// names the SNAT detector. The fixture is low-entropy and built at run time.
func TestCredentialShapedReusesSNATDetectors(t *testing.T) {
	text := "billing uses " + "sk" + "_" + "test" + "_" + strings.Repeat("ab12", 5)
	rule, found := CredentialShaped(text, nil)
	if !found {
		t.Fatalf("snat-only shape: not refused")
	}
	if rule != RuleSNAT+"stripe" {
		t.Errorf("snat-only shape: rule = %q, want %q", rule, RuleSNAT+"stripe")
	}
}
