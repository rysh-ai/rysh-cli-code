// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/secretnat"
)

// security-lead ledger 220 (0.2.17 S0 question): the proxy now starts before
// the session's panes, but the SecretNAT known tier (registered secret values
// -> ${NAME}) is seeded after the restore, because it reads tab scopes. A
// request in between would be redacted by the pattern tier only, so a
// registered secret with no recognisable shape could leave unredacted. Until
// the workspace says the secrets are seeded, the proxy refuses with a
// retryable 503 and calls nobody.
func TestProxyRefusesUntilRegisteredSecretsAreSeeded(t *testing.T) {
	up, hits := countingUpstream(t)
	mgr, err := secretnat.NewManager(secretnat.Options{Enabled: true, Mode: secretnat.ModeSemantic})
	if err != nil {
		t.Fatal(err)
	}
	srv := New(nil, mgr, nil, map[string]string{"anthropic": up.URL}, false)
	srv.HoldUntilSecretsSeeded()
	if _, err := srv.StartPrivate(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	r := postThrough(t, srv.BaseURL(), "/anthropic/pane-1/v1/messages")
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusServiceUnavailable || *hits != 0 {
		t.Fatalf("before seeding: status %d, upstream hits %d; want 503 and 0", r.StatusCode, *hits)
	}
	if !strings.Contains(string(b), "registered secrets") {
		t.Fatalf("the refusal does not say why: %s", b)
	}

	srv.SecretsSeeded()
	if r := postThrough(t, srv.BaseURL(), "/anthropic/pane-1/v1/messages"); r.StatusCode != http.StatusOK || *hits != 1 {
		t.Fatalf("after seeding: status %d, upstream hits %d; want 200 and 1", r.StatusCode, *hits)
	}

	// Without SecretNAT there is no known tier to wait for.
	srv2 := New(nil, nil, nil, map[string]string{"anthropic": up.URL}, false)
	srv2.HoldUntilSecretsSeeded()
	if _, err := srv2.StartPrivate(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv2.Stop()
	if r := postThrough(t, srv2.BaseURL(), "/anthropic/pane-1/v1/messages"); r.StatusCode != http.StatusOK {
		t.Fatalf("no SecretNAT: status %d, want 200", r.StatusCode)
	}
}
