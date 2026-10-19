// SPDX-License-Identifier: Apache-2.0

package limits

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// E15: the server's session cap is not enforced against a rysh daemon (every
// daemon dial is connection_type=share/channel, exempt at the proxy), so an
// over-cap account is never refused — and was never told. SessionLimitWarning
// asks GET /api/resource/check-session-limit and turns a 402 into one line the
// user can act on. It only ever informs: any other answer, or no answer, is
// silence, never an error.

func sessionServer(t *testing.T, status int, body string, delay time.Duration, gotKey *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/resource/check-session-limit" {
			http.NotFound(w, r)
			return
		}
		if gotKey != nil {
			*gotKey = r.Header.Get("X-API-Key")
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const overCapBody = `{"error":"session limit reached (3/3). Upgrade your plan to create more sessions — upgrade at /pricing","upgrade_url":"/pricing"}`

func TestSessionLimitWarning_OverCapNamesLimitAndResolvedLink(t *testing.T) {
	var key string
	srv := sessionServer(t, http.StatusPaymentRequired, overCapBody, 0, &key)

	got := NewChecker(srv.URL, "rysh_test_key").SessionLimitWarning()

	if key != "rysh_test_key" {
		t.Errorf("api key not sent as X-API-Key: %q", key)
	}
	want := "session limit reached (3/3). Upgrade your plan to create more sessions — upgrade at " + srv.URL + "/pricing"
	if got != want {
		t.Errorf("warning =\n  %q\nwant\n  %q", got, want)
	}
}

// An absolute upgrade_url passes through; a server message without the prose
// suffix still gets the link appended exactly once.
func TestSessionLimitWarning_AbsoluteURLAndBareMessage(t *testing.T) {
	srv := sessionServer(t, http.StatusPaymentRequired,
		`{"error":"session limit reached (3/3)","upgrade_url":"https://rysh.example/pricing"}`, 0, nil)
	got := NewChecker(srv.URL, "k").SessionLimitWarning()
	if want := "session limit reached (3/3) — upgrade at https://rysh.example/pricing"; got != want {
		t.Errorf("warning = %q, want %q", got, want)
	}
}

// A self-hosted server with no upgrade URL: the limit is still named, and no
// link is invented.
func TestSessionLimitWarning_NoUpgradeURLNamesLimitOnly(t *testing.T) {
	srv := sessionServer(t, http.StatusPaymentRequired,
		`{"error":"session limit reached (3/3). Upgrade your plan to create more sessions"}`, 0, nil)
	got := NewChecker(srv.URL, "k").SessionLimitWarning()
	if want := "session limit reached (3/3). Upgrade your plan to create more sessions"; got != want {
		t.Errorf("warning = %q, want %q", got, want)
	}
}

func TestSessionLimitWarning_SilentUnlessOverCap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"within cap", http.StatusOK, `{"allowed":true}`},
		{"older server without the endpoint", http.StatusNotFound, `404 page not found`},
		{"server error", http.StatusInternalServerError, `{"error":"boom"}`},
		{"plan lookup failed", http.StatusForbidden, `{"error":"create free subscription: sql: database is closed"}`},
		{"402 with an empty body", http.StatusPaymentRequired, ``},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := sessionServer(t, tc.status, tc.body, 0, nil)
			if got := NewChecker(srv.URL, "k").SessionLimitWarning(); got != "" {
				t.Errorf("expected no warning, got %q", got)
			}
		})
	}
}

func TestSessionLimitWarning_SilentWhenUnreachableOrNotConfigured(t *testing.T) {
	if got := NewChecker("http://127.0.0.1:1", "k").SessionLimitWarning(); got != "" {
		t.Errorf("unreachable server: expected no warning, got %q", got)
	}
	if got := NewChecker("", "").SessionLimitWarning(); got != "" {
		t.Errorf("unconfigured checker: expected no warning, got %q", got)
	}
}

// ##upstream status runs this on the workspace actor's mailbox: a slow server
// must not hold it for the checker's normal 10 s.
func TestSessionLimitWarning_GivesUpQuickly(t *testing.T) {
	srv := sessionServer(t, http.StatusPaymentRequired, overCapBody, 5*time.Second, nil)
	start := time.Now()
	got := NewChecker(srv.URL, "k").SessionLimitWarning()
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("took %s; must give up within the ~3 s budget", elapsed)
	}
	if got != "" {
		t.Errorf("timed-out check must be silent, got %q", got)
	}
}
