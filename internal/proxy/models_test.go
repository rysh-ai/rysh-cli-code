// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// Model allowlist (design 001 §4.7).

func TestModelRules_Matching(t *testing.T) {
	cases := []struct {
		name        string
		rules       config.ProxyModelRules
		model       string
		wantAllowed bool
	}{
		{"no rules allows everything", config.ProxyModelRules{}, "anything-at-all", true},
		{"allow glob matches family",
			config.ProxyModelRules{Allow: []string{"claude-*"}}, "claude-opus-4-8", true},
		{"allow glob refuses another family",
			config.ProxyModelRules{Allow: []string{"claude-*"}}, "gpt-4o", false},
		{"exact allow entry",
			config.ProxyModelRules{Allow: []string{"gpt-4o"}}, "gpt-4o", true},
		{"deny wins over allow",
			config.ProxyModelRules{Allow: []string{"claude-*"}, Deny: []string{"*-opus-*"}},
			"claude-opus-4-8", false},
		{"deny alone leaves the rest allowed",
			config.ProxyModelRules{Deny: []string{"*preview*"}}, "gpt-4o", true},
		{"deny alone refuses its match",
			config.ProxyModelRules{Deny: []string{"*preview*"}}, "o9-preview-2027", false},
		{"matching is case-insensitive",
			config.ProxyModelRules{Allow: []string{"claude-*"}}, "CLAUDE-Opus-4-8", true},
		{"an unnamed model is not judged",
			config.ProxyModelRules{Allow: []string{"claude-*"}}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := newModelRules(c.rules).allows(c.model)
			if got != c.wantAllowed {
				t.Fatalf("allows(%q) = %v (%s), want %v", c.model, got, why, c.wantAllowed)
			}
			if !got && why == "" {
				t.Error("a refusal must say why — the message is what the operator reads")
			}
		})
	}
}

// TestModelRules_RefusedRequestNeverReachesTheProvider is the assertion that
// matters: the point of an allowlist is that the bytes do not leave.
func TestModelRules_RefusedRequestNeverReachesTheProvider(t *testing.T) {
	up, hits := countingUpstream(t)

	srv := New(nil, nil, nil, map[string]string{"anthropic": up.URL}, false)
	srv.SetModelRules(config.ProxyModelRules{
		Allow: []string{"claude-sonnet-*"},
		Deny:  []string{"*-preview-*"},
	})
	if _, err := srv.StartPrivate(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	post := func(model string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.BaseURL()+"/anthropic/pane-1/v1/messages",
			"application/json",
			strings.NewReader(`{"model":"`+model+`","messages":[]}`))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	if r := post("claude-sonnet-4"); r.StatusCode != http.StatusOK {
		t.Fatalf("allowed model status = %d, want 200", r.StatusCode)
	}
	if *hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", *hits)
	}

	r := post("claude-opus-4-8")
	if r.StatusCode != http.StatusForbidden {
		t.Fatalf("unlisted model status = %d, want 403", r.StatusCode)
	}
	if *hits != 1 {
		t.Fatalf("a refused model still reached the provider (hits = %d)", *hits)
	}
	// The refusal is provider-shaped and names the model, so the CLI can print
	// something the user can act on.
	var m map[string]interface{}
	b, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("refusal is not JSON: %s", b)
	}
	text := m["error"].(map[string]interface{})["message"].(string)
	if !strings.Contains(text, "claude-opus-4-8") || !strings.Contains(text, "[proxy] models") {
		t.Errorf("refusal message is not actionable: %q", text)
	}

	if r := post("claude-sonnet-4-preview-x"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("denied model status = %d, want 403 (deny must beat allow)", r.StatusCode)
	}
	if *hits != 1 {
		t.Fatalf("a denied model reached the provider (hits = %d)", *hits)
	}

	// Both refusals are on the audit trail, tagged as the model rule.
	var blocked int
	for _, a := range srv.RecentAudits(10) {
		if a.Endpoint == "(model)" {
			blocked++
		}
	}
	if blocked != 2 {
		t.Fatalf("model refusals audited = %d, want 2", blocked)
	}
}

// TestStrict_DefaultsToWarnOnly pins 022 §8.2's posture: detection is negative
// evidence, so blocking is opt-in and nothing about it happens by default.
func TestStrict_DefaultsToWarnOnly(t *testing.T) {
	srv := New(nil, nil, nil, nil, false)
	if srv.Strict() {
		t.Fatal("strict must default off — an idle CLI is indistinguishable from " +
			"an escaped one, and stopping it by default punishes the innocent case")
	}
	srv.SetStrict(true)
	if !srv.Strict() {
		t.Fatal("SetStrict(true) did not take")
	}
	var nilSrv *Server
	if nilSrv.Strict() {
		t.Fatal("a nil server must read as not-strict, not panic")
	}
}

// TestStrictBlockNotice_SaysWhatAndWhy: the notice is the only explanation a
// user gets for their CLI dying, so it has to carry the CLI, the reason and the
// way out.
func TestStrictBlockNotice_SaysWhatAndWhy(t *testing.T) {
	got := StrictBlockNotice("codex")
	for _, want := range []string{"codex", "strict", "##proxy check codex", "OPENAI_BASE_URL"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice missing %q: %s", want, got)
		}
	}
}
