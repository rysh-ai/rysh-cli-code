// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/rysh-ai/rysh-cli-shared/secretnat"
)

// Rule names a credential-shape check. A refusal names the rule, never the
// value, its length, a prefix or a hash of it.
const (
	RuleKnownSecret = "known-secret"
	RulePrivateKey  = "private-key-block"
	RuleBearer      = "bearer-header"
	RuleURLPassword = "url-password"
	RulePrefix      = "token-prefix"
	RuleAssignment  = "sensitive-assignment"
	RuleEntropy     = "high-entropy"
	// RuleSNAT prefixes the name of the SecretNAT built-in detector that
	// matched (e.g. "snat:stripe").
	RuleSNAT = "snat:"
)

// ErrCredentialShaped is returned (wrapped, with the rule name) when a memory
// would hold a credential-shaped value.
var ErrCredentialShaped = errors.New("credential-shaped")

func refusal(rule string) error {
	return fmt.Errorf("%w: refused by rule %s — memories never hold secrets; store it with ##secret instead", ErrCredentialShaped, rule)
}

// minKnownSecretLen keeps a very short registered value (a test value, a PIN)
// from refusing ordinary words; the shape rules still apply to it.
const minKnownSecretLen = 8

var (
	rePrivateKey  = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	reBearer      = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{12,}`)
	reURLPassword = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s/:@]+:[^\s/@]+@`)
	// Well-known issued-credential prefixes (VCS, chat, cloud, AI providers).
	rePrefix = regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{16,}|xox[abprs]-[A-Za-z0-9-]{10,}|(AKIA|ASIA)[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{30,}|ya29\.[0-9A-Za-z_-]{20,}|sk-[A-Za-z0-9_-]{20,})`)
	// A sensitive-looking key assigned a non-trivial value: KEY=value, KEY: value.
	reAssignment = regexp.MustCompile(`(?i)\b[A-Za-z0-9_.-]*(password|passwd|passphrase|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential|client[_-]?secret)[A-Za-z0-9_.-]*\s*[:=]\s*["']?[^\s"']{6,}`)
	// Candidate runs for the entropy check.
	reRun = regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`)

	// snatDetectors is SecretNAT's built-in detector set (rysh-shared
	// secretnat), reused so memory refuses every shape the governance proxy
	// translates, without a second copy of those patterns. It is the full
	// built-in set: a detector switched off for SNAT translation ([snat]
	// disabled) still guards memory.
	snatDetectors = mustSNATRegistry()
)

func mustSNATRegistry() *secretnat.Registry {
	r, err := secretnat.NewDefaultRegistry(nil, nil)
	if err != nil {
		panic(err) // built-ins only; cannot fail
	}
	return r
}

// CredentialShaped reports whether text looks like it carries a credential,
// and which rule matched. known holds the session's registered secret values;
// they are compared in-process and never logged.
//
// Best effort, fail-closed on what it recognises: it is a guard against
// storing secrets in memory by accident, not a guarantee that none can be.
func CredentialShaped(text string, known []string) (string, bool) {
	for _, v := range known {
		if len(v) >= minKnownSecretLen && strings.Contains(text, v) {
			return RuleKnownSecret, true
		}
	}
	switch {
	case rePrivateKey.MatchString(text):
		return RulePrivateKey, true
	case reBearer.MatchString(text):
		return RuleBearer, true
	case reURLPassword.MatchString(text):
		return RuleURLPassword, true
	case rePrefix.MatchString(text):
		return RulePrefix, true
	case reAssignment.MatchString(text):
		return RuleAssignment, true
	}
	for _, run := range reRun.FindAllString(text, -1) {
		if randomLooking(run) {
			return RuleEntropy, true
		}
	}
	if ms := snatDetectors.DetectAll(text); len(ms) > 0 {
		return RuleSNAT + ms[0].Type, true
	}
	return "", false
}

// randomLooking is true for a run that mixes letters and digits with more
// than hex-level entropy, so hex digests (git shas) stay allowed.
func randomLooking(s string) bool {
	var letter, digit bool
	counts := map[rune]int{}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			letter = true
		}
		counts[r]++
	}
	if !letter || !digit {
		return false
	}
	n := float64(len(s))
	var h float64
	for _, c := range counts {
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h > 4.3
}
