// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/secretnat"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
)

// snatStatusOutput renders ##snat status for a workspace whose Setup carries
// the given rejected custom detectors.
func snatStatusOutput(t *testing.T, rejected []agentic.SNATRejectedDetector) string {
	t.Helper()
	t.Chdir(t.TempDir())
	mgr, err := secretnat.NewManager(secretnat.Options{Enabled: true})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	w := &WorkspaceActor{
		workspaceName: "ws-main",
		sessionName:   "sess",
		tabs:          []*tabInfo{{id: "tab-1111", title: "Build"}},
		secrets:       newSecretStore(nil, nil),
		agSetup:       &agentic.Setup{SecretNAT: mgr, SNATRejected: rejected},
	}
	var out strings.Builder
	w.handleSnatSubcommand(&out, "pane-1", []string{"status"})
	return out.String()
}

// TestSnatStatusNamesRejectedDetector is T1(b): the user forms their mental
// model at ##snat status, so a custom detector that was refused must be named
// THERE — with the pattern and the reason — not only in the daemon log.
func TestSnatStatusNamesRejectedDetector(t *testing.T) {
	got := snatStatusOutput(t, []agentic.SNATRejectedDetector{{
		Name:    "badone",
		Pattern: `acme_[a-z0-9`,
		Reason:  "error parsing regexp: missing closing ]: `[a-z0-9`",
	}})

	for _, want := range []string{"badone", `acme_[a-z0-9`, "missing closing ]"} {
		if !strings.Contains(got, want) {
			t.Errorf("##snat status does not mention %q — a typo is not actionable.\n--- status ---\n%s", want, got)
		}
	}
	// It must read as lost coverage, not as a stray note.
	if !strings.Contains(strings.ToLower(got), "not active") && !strings.Contains(strings.ToLower(got), "rejected") {
		t.Errorf("status does not say the detector is inactive.\n--- status ---\n%s", got)
	}
}

// TestSnatStatusCleanWhenNothingRejected: the normal case prints no rejection
// block at all.
func TestSnatStatusCleanWhenNothingRejected(t *testing.T) {
	got := snatStatusOutput(t, nil)
	if strings.Contains(strings.ToLower(got), "rejected") {
		t.Errorf("clean config printed a rejection block.\n--- status ---\n%s", got)
	}
}

// TestSnatStatusNamesExclusions is T2: ##snat status must state what is NOT
// covered, at the surface where the mental model forms.
//
// Wording is constrained by the project's claims table, which narrows this one
// — response bodies are scanned and reported, NEVER rewritten. Under-claiming
// is correct here; any hint that responses are sanitised is a defect.
func TestSnatStatusNamesExclusions(t *testing.T) {
	got := snatStatusOutput(t, nil)
	low := strings.ToLower(got)

	// Every assertion here must match text that ONLY the exclusion block
	// produces. A bare word does not: this test used to assert
	// strings.Contains(low, "env"), which the builtin detector list satisfies
	// all by itself — it always prints `envkv` — so the assertion passed on
	// code with no env-tier exclusion at all. An assertion that cannot fail is
	// not a guard, and it is worse than no assertion because it reads as one.

	// 1. The env-tier exclusion, and the way out of it.
	if !strings.Contains(low, "env secrets") {
		t.Errorf("status never labels the env-tier exclusion.\n--- status ---\n%s", got)
	}
	if !strings.Contains(low, "not in the known") {
		t.Errorf("status never states that env values are outside the known tier.\n--- status ---\n%s", got)
	}
	if !strings.Contains(got, "##secret new") {
		t.Errorf("status does not tell the user how to register an env secret (##secret new).\n--- status ---\n%s", got)
	}

	// 2. The response-side non-goal. "response bodies", not a bare "response",
	// for the same reason as above.
	if !strings.Contains(low, "response bodies") {
		t.Errorf("status never mentions the response-side non-goal.\n--- status ---\n%s", got)
	}
	if !strings.Contains(low, "never rewritten") {
		t.Errorf("status does not state that response bodies are never rewritten.\n--- status ---\n%s", got)
	}
}

// TestSnatStatusDoesNotWidenTheNarrowRow guards the 024 boundary directly: the
// status text must not claim responses are cleaned, sanitised, redacted or
// stripped. This test failing means a claim was widened at the command surface
// without going through E3.
func TestSnatStatusDoesNotWidenTheNarrowRow(t *testing.T) {
	low := strings.ToLower(snatStatusOutput(t, nil))
	for _, banned := range []string{
		"responses are sanitized", "responses are sanitised",
		"response bodies are sanitized", "response bodies are sanitised",
		"sanitizes responses", "sanitises responses",
		"redacts responses", "strips secrets from responses",
		"rewrites responses", "response bodies are rewritten",
	} {
		if strings.Contains(low, banned) {
			t.Errorf("status widens the 024 NARROW row with %q — responses are scanned and reported, never rewritten", banned)
		}
	}
}
