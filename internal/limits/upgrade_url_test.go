// SPDX-License-Identifier: Apache-2.0

package limits

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// G-8: the pane-limit message is printed into a terminal, where there is no
// dashboard to click. It used to end at "Upgrade your plan to create more panes"
// and name nowhere to go.
//
// The load-bearing property here is the negative one: when the server supplies
// no upgrade URL — an older server, or a self-hosted one that suppressed it —
// the message must degrade to *exactly* the text it carried before, and must
// never invent a link. A self-hoster's terminal advertising rysh.ai billing
// would be a defect, not a missing feature.

const legacyPaneLimitMsg = "pane limit reached (30/30 on Free plan). Upgrade your plan to create more panes"

func TestCheckCreateMessageUnchangedWithoutUpgradeURL(t *testing.T) {
	c := NewChecker("https://example.invalid", "key")
	c.cached = &ResourceLimits{PlanID: "free", PlanName: "Free", MaxPanes: 30} // no UpgradeURL
	c.lastFetch = time.Now() // fresh: do not trigger a background refresh

	err := c.CheckCreate(ResourceUsage{Panes: 30}, 1)
	if err == nil {
		t.Fatal("expected the pane limit to be refused")
	}
	if err.Error() != legacyPaneLimitMsg {
		t.Fatalf("without an upgrade URL the message must be byte-identical to the pre-G-8 text.\n got: %q\nwant: %q", err.Error(), legacyPaneLimitMsg)
	}
	if strings.Contains(err.Error(), "rysh.ai") {
		t.Fatalf("a message with no configured URL must never name a vendor: %q", err.Error())
	}
}

func TestCheckCreateMessageCarriesUpgradeURL(t *testing.T) {
	c := NewChecker("https://rysh.example.com", "key")
	c.cached = &ResourceLimits{PlanID: "free", PlanName: "Free", MaxPanes: 30, UpgradeURL: "/pricing"}
	c.lastFetch = time.Now() // fresh: do not trigger a background refresh

	err := c.CheckCreate(ResourceUsage{Panes: 30}, 1)
	if err == nil {
		t.Fatal("expected the pane limit to be refused")
	}
	// The server sends a deployment-relative path; the CLI must resolve it
	// against the server it is actually talking to, so a self-hoster sees their
	// own pricing page rather than somebody else's.
	if !strings.Contains(err.Error(), "https://rysh.example.com/pricing") {
		t.Fatalf("expected the resolved absolute URL in the message, got %q", err.Error())
	}
}

func TestResolveUpgradeURL(t *testing.T) {
	cases := []struct {
		name, server, raw, want string
	}{
		{"empty stays empty", "https://s.example.com", "", ""},
		{"blank stays empty", "https://s.example.com", "   ", ""},
		{"relative is joined onto the server", "https://s.example.com", "/pricing", "https://s.example.com/pricing"},
		{"trailing slash on server does not double up", "https://s.example.com/", "/pricing", "https://s.example.com/pricing"},
		{"absolute is passed through", "https://s.example.com", "https://other.example.com/x", "https://other.example.com/x"},
		{"no server and a relative path yields nothing usable", "", "/pricing", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveUpgradeURL(tc.server, tc.raw); got != tc.want {
				t.Fatalf("resolveUpgradeURL(%q, %q) = %q, want %q", tc.server, tc.raw, got, tc.want)
			}
		})
	}
}

// The field has to survive the wire, not just exist in the struct.
func TestUpgradeURLDecodesFromServerResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/resource/limits":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plan_id": "free", "plan_name": "Free", "max_panes": 30,
				"upgrade_url": "/pricing",
			})
		case "/api/resource/check-limits":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"allowed": false, "resource": "panes", "limit": 30, "current": 30,
				"error": "pane limit reached", "plan_id": "free", "plan_name": "Free",
				"upgrade_url": "/pricing",
			})
		}
	}))
	defer srv.Close()

	c := NewChecker(srv.URL, "key")
	if err := c.FetchLimits(); err != nil {
		t.Fatalf("fetch limits: %v", err)
	}
	if c.cached == nil || c.cached.UpgradeURL != "/pricing" {
		t.Fatalf("upgrade_url must decode off the limits response, got %+v", c.cached)
	}

	res, err := c.ServerCheckCreate(ResourceUsage{Panes: 30})
	if err != nil {
		t.Fatalf("server check: %v", err)
	}
	if res.UpgradeURL != "/pricing" {
		t.Fatalf("upgrade_url must decode off the check-limits response, got %+v", res)
	}
}
