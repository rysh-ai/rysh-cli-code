package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestDefaultUpstreams_MatchRegisteredDialects pins the registry wiring: the
// string-keyed routing map is now derived from the registered dialects, and a
// dialect silently dropping out of the registry must fail loudly here.
func TestDefaultUpstreams_MatchRegisteredDialects(t *testing.T) {
	want := map[string]string{
		"anthropic": "https://api.anthropic.com",
		"openai":    "https://api.openai.com",
		"gemini":    "https://generativelanguage.googleapis.com",
	}
	got := DefaultUpstreams()
	if len(got) != len(want) {
		t.Fatalf("DefaultUpstreams has %d entries, want %d: %v", len(got), len(want), got)
	}
	for name, base := range want {
		if got[name] != base {
			t.Errorf("upstream[%s] = %q, want %q", name, got[name], base)
		}
	}
}

// TestDialect_ApplyAuthHeaders exercises each dialect's ApplyAuth directly at
// the interface: the key must land in the provider's own header, and the
// competing credential header must be cleared.
func TestDialect_ApplyAuthHeaders(t *testing.T) {
	newReq := func(path string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer stale")
		req.Header.Set("x-api-key", "stale")
		return req
	}

	t.Run("anthropic", func(t *testing.T) {
		req := newReq("/v1/messages")
		lookupDialect("anthropic").ApplyAuth(req, "sk-ant-real")
		if req.Header.Get("x-api-key") != "sk-ant-real" {
			t.Errorf("x-api-key = %q, want the real key", req.Header.Get("x-api-key"))
		}
		if req.Header.Get("Authorization") != "" {
			t.Errorf("stale bearer must be cleared, got %q", req.Header.Get("Authorization"))
		}
		if req.Header.Get("anthropic-version") == "" {
			t.Error("anthropic-version must be defaulted")
		}
	})

	t.Run("openai", func(t *testing.T) {
		req := newReq("/v1/chat/completions")
		lookupDialect("openai").ApplyAuth(req, "sk-oai-real")
		if req.Header.Get("Authorization") != "Bearer sk-oai-real" {
			t.Errorf("Authorization = %q, want the real bearer", req.Header.Get("Authorization"))
		}
		if req.Header.Get("x-api-key") != "" {
			t.Errorf("stale x-api-key must be cleared, got %q", req.Header.Get("x-api-key"))
		}
	})

	t.Run("gemini native", func(t *testing.T) {
		req := newReq("/v1beta/models/gemini-2.0:generateContent")
		lookupDialect("gemini").ApplyAuth(req, "gem-real")
		if req.Header.Get("x-goog-api-key") != "gem-real" {
			t.Errorf("x-goog-api-key = %q, want the real key", req.Header.Get("x-goog-api-key"))
		}
		if req.Header.Get("Authorization") != "" {
			t.Errorf("stale bearer must be cleared, got %q", req.Header.Get("Authorization"))
		}
	})

	t.Run("gemini openai-compat", func(t *testing.T) {
		req := newReq("/v1beta/openai/chat/completions")
		lookupDialect("gemini").ApplyAuth(req, "gem-real")
		if req.Header.Get("Authorization") != "Bearer gem-real" {
			t.Errorf("compat surface wants a bearer, got %q", req.Header.Get("Authorization"))
		}
	})

	t.Run("no key passes caller credentials through", func(t *testing.T) {
		for _, name := range []string{"anthropic", "openai", "gemini"} {
			req := newReq("/v1/whatever")
			lookupDialect(name).ApplyAuth(req, "")
			if req.Header.Get("Authorization") != "Bearer stale" ||
				req.Header.Get("x-api-key") != "stale" {
				t.Errorf("%s: with no key the caller's own headers must survive", name)
			}
		}
	})
}

// TestServeHTTP_Budget429BodyIsDialectShaped is the end-to-end guard for
// design 001 §4.4: the synthetic budget error a wrapped CLI receives must be
// in ITS provider's native error shape — and the provider is never contacted.
func TestServeHTTP_Budget429BodyIsDialectShaped(t *testing.T) {
	tests := []struct {
		dialect string
		path    string
		marker  func(t *testing.T, m map[string]interface{})
	}{
		{"anthropic", "/anthropic/p1/v1/messages", func(t *testing.T, m map[string]interface{}) {
			if m["type"] != "error" ||
				m["error"].(map[string]interface{})["type"] != "rate_limit_error" {
				t.Fatalf("not anthropic-shaped: %v", m)
			}
		}},
		{"openai", "/openai/p1/v1/chat/completions", func(t *testing.T, m map[string]interface{}) {
			if m["error"].(map[string]interface{})["code"] != "rate_limit_exceeded" {
				t.Fatalf("not openai-shaped: %v", m)
			}
		}},
		{"gemini", "/gemini/p1/v1beta/models/g:generateContent", func(t *testing.T, m map[string]interface{}) {
			if m["error"].(map[string]interface{})["status"] != "RESOURCE_EXHAUSTED" {
				t.Fatalf("not gemini-shaped: %v", m)
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.dialect, func(t *testing.T) {
			upstreamHits := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamHits++
			}))
			defer upstream.Close()

			s := New(nil, nil, nil, map[string]string{tc.dialect: upstream.URL}, false)
			s.budget, _ = newTestChecker(&msg.MsgUsageCheckReply{
				PaneID: "p1", SpentTokens: 200_000, CeilingTokens: 100_000, Ok: false,
			}, nil)

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)

			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", rec.Code)
			}
			if upstreamHits != 0 {
				t.Fatalf("over-budget request reached the provider (%d hits)", upstreamHits)
			}
			body, _ := io.ReadAll(rec.Body)
			var m map[string]interface{}
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatalf("429 body is not valid JSON: %v", err)
			}
			tc.marker(t, m)
		})
	}
}

// TestServeHTTP_UnknownDialectIs404 pins the routing contract for names the
// upstreams map does not know: refuse locally, never guess an upstream.
func TestServeHTTP_UnknownDialectIs404(t *testing.T) {
	s := New(nil, nil, nil, nil, false) // default upstreams
	req := httptest.NewRequest(http.MethodPost, "/mystery/p1/v1/whatever",
		strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown dialect must 404, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown dialect") {
		t.Fatalf("body should name the failure, got %q", rec.Body.String())
	}
}
