// SPDX-License-Identifier: Apache-2.0

package limits

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckerDisabledWhenNoConfig(t *testing.T) {
	c := NewChecker("", "")
	if c.Enabled() {
		t.Fatal("checker should be disabled when no URL/key")
	}
	err := c.CheckCreate(ResourceUsage{Panes: 100}, 1)
	if err != nil {
		t.Fatalf("expected nil error when disabled, got: %v", err)
	}
}

func TestCheckerEnabled(t *testing.T) {
	c := NewChecker("http://localhost:8080", "test-key")
	if !c.Enabled() {
		t.Fatal("checker should be enabled when URL and key are set")
	}
}

func TestCheckCreateNoLimits(t *testing.T) {
	c := NewChecker("http://localhost:8080", "test-key")
	err := c.CheckCreate(ResourceUsage{Panes: 100}, 1)
	if err != nil {
		t.Fatalf("expected nil error when no cached limits, got: %v", err)
	}
}

func TestCheckCreateWithLimits(t *testing.T) {
	c := NewChecker("http://localhost:8080", "test-key")
	c.cached = &ResourceLimits{
		PlanID:   "solo",
		PlanName: "Solo",
		MaxPanes: 30,
	}

	tests := []struct {
		name     string
		usage    ResourceUsage
		addPanes int
		wantErr  bool
	}{
		{
			name:     "within limits",
			usage:    ResourceUsage{Panes: 15},
			addPanes: 1,
			wantErr:  false,
		},
		{
			name:     "pane limit exceeded",
			usage:    ResourceUsage{Panes: 30},
			addPanes: 1,
			wantErr:  true,
		},
		{
			name:     "exact at limit is OK",
			usage:    ResourceUsage{Panes: 29},
			addPanes: 1,
			wantErr:  false,
		},
		{
			name:     "zero limit means unlimited",
			usage:    ResourceUsage{Panes: 400},
			addPanes: 1,
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "zero limit means unlimited" {
				c.cached = &ResourceLimits{
					PlanID:   "enterprise",
					PlanName: "Enterprise",
				}
				defer func() {
					c.cached = &ResourceLimits{
						PlanID:   "solo",
						PlanName: "Solo",
						MaxPanes: 30,
					}
				}()
			}
			err := c.CheckCreate(tt.usage, tt.addPanes)
			if (err != nil) != tt.wantErr {
				t.Errorf("CheckCreate() err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestFetchLimitsFromServer(t *testing.T) {
	expectedLimits := ResourceLimits{
		PlanID:        "solo",
		PlanName:      "Solo",
		MaxWorkspaces: 3,
		MaxSessions:   5,
		MaxPanes:      30,
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/resource/limits" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Errorf("unexpected API key: %s", r.Header.Get("X-API-Key"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(expectedLimits)
	}))
	defer server.Close()

	c := NewChecker(server.URL, "test-key")
	err := c.FetchLimits()
	if err != nil {
		t.Fatalf("FetchLimits() failed: %v", err)
	}

	cached := c.CachedLimits()
	if cached == nil {
		t.Fatal("cached limits should not be nil after fetch")
	}
	if cached.PlanID != "solo" {
		t.Errorf("expected plan_id 'solo', got %q", cached.PlanID)
	}
	if cached.MaxSessions != 5 {
		t.Errorf("expected max_sessions 5, got %d", cached.MaxSessions)
	}
	if cached.MaxPanes != 30 {
		t.Errorf("expected max_panes 30, got %d", cached.MaxPanes)
	}
}

func TestServerCheckCreate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/resource/check-limits" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		var usage ResourceUsage
		json.NewDecoder(r.Body).Decode(&usage)

		result := LimitCheckResult{
			Allowed:  true,
			PlanID:   "solo",
			PlanName: "Solo",
		}

		if usage.Panes >= 30 {
			result.Allowed = false
			result.Resource = "panes"
			result.Limit = 30
			result.Current = usage.Panes
			result.Error = "pane limit reached"
			w.WriteHeader(http.StatusForbidden)
		}

		json.NewEncoder(w).Encode(result)
	}))
	defer server.Close()

	c := NewChecker(server.URL, "test-key")

	result, err := c.ServerCheckCreate(ResourceUsage{Panes: 2})
	if err != nil {
		t.Fatalf("ServerCheckCreate() failed: %v", err)
	}
	if !result.Allowed {
		t.Error("expected allowed")
	}

	result, err = c.ServerCheckCreate(ResourceUsage{Panes: 30})
	if err != nil {
		t.Fatalf("ServerCheckCreate() failed: %v", err)
	}
	if result.Allowed {
		t.Error("expected denied")
	}
	if result.Resource != "panes" {
		t.Errorf("expected resource 'panes', got %q", result.Resource)
	}
}

func TestFetchLimitsDisabled(t *testing.T) {
	c := NewChecker("", "")
	err := c.FetchLimits()
	if err != nil {
		t.Fatalf("FetchLimits() should return nil when disabled, got: %v", err)
	}
	if c.CachedLimits() != nil {
		t.Fatal("cached limits should be nil when disabled")
	}
}
