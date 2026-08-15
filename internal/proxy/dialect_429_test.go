// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestRetryAfterSeconds pins the rounding contract (design 022 §3). Rounding
// DOWN would advertise a delay that is guaranteed to be refused again, and "0"
// invites the retry storm a limiter exists to prevent.
func TestRetryAfterSeconds(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "1"},
		{-5 * time.Second, "1"},
		{1 * time.Millisecond, "1"},
		{500 * time.Millisecond, "1"},
		{1 * time.Second, "1"},
		{1200 * time.Millisecond, "2"},
		{2500 * time.Millisecond, "3"},
		{time.Hour, "3600"},
	}
	for _, c := range cases {
		if got := retryAfterSeconds(c.in); got != c.want {
			t.Errorf("retryAfterSeconds(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDialect_WriteRateLimited_ShapeAndRetryAfter checks the generalized writer
// at the interface: the caller's delay reaches Retry-After, and each dialect's
// error body keeps the provider-native shape a wrapped CLI recognises.
func TestDialect_WriteRateLimited_ShapeAndRetryAfter(t *testing.T) {
	cases := []struct {
		dialect string
		check   func(t *testing.T, m map[string]interface{})
	}{
		{"anthropic", func(t *testing.T, m map[string]interface{}) {
			if m["type"] != "error" ||
				m["error"].(map[string]interface{})["type"] != "rate_limit_error" {
				t.Fatalf("not anthropic-shaped: %v", m)
			}
		}},
		{"openai", func(t *testing.T, m map[string]interface{}) {
			if m["error"].(map[string]interface{})["code"] != "rate_limit_exceeded" {
				t.Fatalf("not openai-shaped: %v", m)
			}
		}},
		{"gemini", func(t *testing.T, m map[string]interface{}) {
			if m["error"].(map[string]interface{})["status"] != "RESOURCE_EXHAUSTED" {
				t.Fatalf("not gemini-shaped: %v", m)
			}
		}},
		{"mystery", func(t *testing.T, m map[string]interface{}) {
			// Unregistered-but-routed names keep the anthropic default shape.
			if m["type"] != "error" {
				t.Fatalf("fallback lost its default shape: %v", m)
			}
		}},
	}

	for _, c := range cases {
		t.Run(c.dialect, func(t *testing.T) {
			rec := httptest.NewRecorder()
			lookupDialect(c.dialect).WriteRateLimited(rec, "slow down", 7*time.Second)

			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", rec.Code)
			}
			if got := rec.Header().Get("Retry-After"); got != "7" {
				t.Errorf("Retry-After = %q, want %q", got, "7")
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var m map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
				t.Fatalf("429 body is not valid JSON: %v", err)
			}
			c.check(t, m)
			if !strings.Contains(rec.Body.String(), "slow down") {
				t.Errorf("message did not reach the body: %s", rec.Body.String())
			}
		})
	}
}

// TestBudget429StillAdvertisesAnHour is the behaviour-preserving guard for the
// §3 refactor: generalizing the writer must not change what the ceiling path
// puts on the wire. A ceiling is a daily figure, so it still answers "3600".
func TestBudget429StillAdvertisesAnHour(t *testing.T) {
	s := New(nil, nil, nil, map[string]string{"anthropic": "http://127.0.0.1:1"}, false)
	s.budget, _ = newTestChecker(&msg.MsgUsageCheckReply{
		PaneID: "p1", SpentTokens: 200_000, CeilingTokens: 100_000, Ok: false,
	}, nil)

	req := httptest.NewRequest(http.MethodPost, "/anthropic/p1/v1/messages",
		strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "3600" {
		t.Errorf("Retry-After = %q, want 3600 (unchanged by the §3 refactor)", got)
	}
}
