// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/secretnat"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
)

// newSnatTestWorkspace builds a bare WorkspaceActor with a config-tier secret
// store and a live SecretNAT manager (as NewSetup would provide).
func newSnatTestWorkspace(t *testing.T, cfgSecrets map[string]map[string]string) *WorkspaceActor {
	t.Helper()
	t.Chdir(t.TempDir()) // isolate from any project-local .rysh/secrets
	mgr, err := secretnat.NewManager(secretnat.Options{Enabled: true})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return &WorkspaceActor{
		workspaceName: "ws-main",
		sessionName:   "sess",
		tabs:          []*tabInfo{{id: "tab-1111", title: "Build"}},
		secrets:       newSecretStore(nil, cfgSecrets),
		agSetup:       &agentic.Setup{SecretNAT: mgr},
	}
}

// TestPushKnownSecrets: secrets from the ##secret store become the known tier
// — sanitizing a prompt replaces the VALUE with ${NAME}, and restore brings
// it back.
func TestPushKnownSecrets(t *testing.T) {
	w := newSnatTestWorkspace(t, map[string]map[string]string{
		"ws-main":  {"STRIPE_KEY": "sk_live_4eC39HqLyjWDarjtT1zdp7dc"},
		"tab-1111": {"TAB_TOKEN": "tabvalue-123456"},
	})
	w.pushKnownSecrets()

	sess := w.snatManager().Session("pane-1")
	out := sess.Sanitize("use sk_live_4eC39HqLyjWDarjtT1zdp7dc and tabvalue-123456")
	if out != "use ${STRIPE_KEY} and ${TAB_TOKEN}" {
		t.Fatalf("known tier not applied: %q", out)
	}
	if got := sess.Restore(out); !strings.Contains(got, "sk_live_4eC39") || !strings.Contains(got, "tabvalue-123456") {
		t.Fatalf("restore failed: %q", got)
	}
}

// TestSnatCommandToggle: ##snat on|off sets a per-pane override; --session
// flips the default; reset clears the override.
func TestSnatCommandToggle(t *testing.T) {
	w := newSnatTestWorkspace(t, nil)
	mgr := w.snatManager()
	sess := mgr.Session("pane-1")

	var out strings.Builder
	// Pane off while session default stays on.
	w.handleSnatSubcommand(&out, "pane-1", []string{"off"})
	if sess.Enabled() {
		t.Fatal("##snat off did not disable the pane")
	}
	if !mgr.Enabled() {
		t.Fatal("##snat off must not touch the session default")
	}
	// Reset → back to default (on).
	w.handleSnatSubcommand(&out, "pane-1", []string{"reset"})
	if !sess.Enabled() {
		t.Fatal("##snat reset did not restore the default")
	}
	// Session-wide off.
	w.handleSnatSubcommand(&out, "pane-1", []string{"off", "--session"})
	if mgr.Enabled() || sess.Enabled() {
		t.Fatal("##snat off --session did not disable the session default")
	}
	// Pane override back on despite the session default being off.
	w.handleSnatSubcommand(&out, "pane-1", []string{"on"})
	if !sess.Enabled() {
		t.Fatal("##snat on (pane) must win over the disabled session default")
	}
}

// TestSnatCommandOutputNeverLeaksValues: status and list output tokens and
// counters only — the real value must never appear.
func TestSnatCommandOutputNeverLeaksValues(t *testing.T) {
	const secret = "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	w := newSnatTestWorkspace(t, nil)
	sess := w.snatManager().Session("pane-1")
	if got := sess.Sanitize("key " + secret); strings.Contains(got, secret) {
		t.Fatalf("sanitize failed: %q", got)
	}

	var out strings.Builder
	w.handleSnatSubcommand(&out, "pane-1", []string{"list"})
	w.handleSnatSubcommand(&out, "pane-1", []string{"status"})
	text := out.String()
	if strings.Contains(text, secret) {
		t.Fatal("##snat output leaked a real secret value")
	}
	if !strings.Contains(text, "ghp_SNAT000001") {
		t.Fatalf("##snat list should show the token:\n%s", text)
	}
	if !strings.Contains(text, "github") {
		t.Fatalf("##snat list should show the detector name:\n%s", text)
	}
}

// TestSnatCommandGetRevealsLocally: ##snat get <token> reveals the real value
// for a detected-tier token and a known-tier ${NAME}, and refuses unknowns.
func TestSnatCommandGetRevealsLocally(t *testing.T) {
	const detected = "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	w := newSnatTestWorkspace(t, map[string]map[string]string{
		"ws-main": {"STRIPE_KEY": "sk_live_51H8KNOWNsecretVALUE0000abcd"},
	})
	w.pushKnownSecrets()
	sess := w.snatManager().Session("pane-1")
	// Mint a detected-tier token.
	tok := sess.Sanitize("key " + detected)
	tok = strings.TrimPrefix(tok, "key ")
	if tok == detected {
		t.Fatal("no detected token minted")
	}

	// get the detected token -> real value revealed locally.
	var out strings.Builder
	w.handleSnatSubcommand(&out, "pane-1", []string{"get", tok})
	if !strings.Contains(out.String(), detected) {
		t.Fatalf("##snat get did not reveal the detected value:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "revealed locally") {
		t.Fatalf("missing local-only notice:\n%s", out.String())
	}

	// get a known-tier ${NAME} -> real value.
	out.Reset()
	w.handleSnatSubcommand(&out, "pane-1", []string{"get", "${STRIPE_KEY}"})
	if !strings.Contains(out.String(), "sk_live_51H8KNOWNsecretVALUE0000abcd") {
		t.Fatalf("##snat get ${NAME} did not reveal known value:\n%s", out.String())
	}

	// unknown token -> graceful refusal, no panic.
	out.Reset()
	w.handleSnatSubcommand(&out, "pane-1", []string{"get", "sk_live_SNAT999999"})
	if !strings.Contains(out.String(), "no mapping") {
		t.Fatalf("expected 'no mapping' for unknown token:\n%s", out.String())
	}
}

// TestSnatCommandMode: ##snat mode switches token style for new tokens.
func TestSnatCommandMode(t *testing.T) {
	w := newSnatTestWorkspace(t, nil)
	var out strings.Builder
	w.handleSnatSubcommand(&out, "pane-1", []string{"mode", "private"})
	sess := w.snatManager().Session("pane-1")
	got := sess.Sanitize("sk_live_4eC39HqLyjWDarjtT1zdp7dc")
	if !strings.Contains(got, "SECRET_TOKEN_001") {
		t.Fatalf("private mode not applied: %q", got)
	}
}

// TestSnatUnavailable: without an agentic setup the command degrades
// gracefully.
func TestSnatUnavailable(t *testing.T) {
	w := &WorkspaceActor{}
	var out strings.Builder
	w.handleSnatSubcommand(&out, "pane-1", []string{"status"})
	if !strings.Contains(out.String(), "unavailable") {
		t.Fatalf("expected unavailable notice, got: %s", out.String())
	}
	// pushKnownSecrets must be a no-op, not a panic.
	w.pushKnownSecrets()
}
