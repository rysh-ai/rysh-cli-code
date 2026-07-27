package mcp

import (
	"context"
	"net/http/httptest"
	"testing"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// TestManagerReplayScope verifies a scoped MCP server is persisted with its scope
// key, is NOT re-registered globally at bootstrap after a restart, and IS
// re-registered into its scope registry by ReplayScope.
func TestManagerReplayScope(t *testing.T) {
	fs := newFakeServer()
	srv := httptest.NewServer(fs.httpHandler())
	defer srv.Close()

	dir := t.TempDir()

	// --- session 1: add a server scoped to lane:L1 (persists def with Scope) ---
	{
		global := sharedtools.NewToolRegistry()
		mgr := NewManager(global, dir)
		lane := sharedtools.NewChildRegistry(global)
		n, err := mgr.AddServerScoped(context.Background(),
			ServerDef{Name: "weather", Transport: TransportHTTP, URL: srv.URL},
			ScopeTarget{Key: "lane:L1", Registry: lane})
		if err != nil || n != 2 {
			t.Fatalf("AddServerScoped: n=%d err=%v", n, err)
		}
		// Registered into the lane scope, not global.
		if _, ok := lane.Get("weather_get_weather"); !ok {
			t.Fatalf("tool not registered into lane scope")
		}
		if _, ok := global.Get("weather_get_weather"); ok {
			t.Fatalf("scoped tool leaked into the global registry")
		}
		mgr.Close()
	}

	// The persisted def carries the scope key.
	defs, err := LoadStore(dir)
	if err != nil || len(defs) != 1 || defs[0].Scope != "lane:L1" {
		t.Fatalf("scope not persisted: defs=%+v err=%v", defs, err)
	}

	// --- session 2 (restart): fresh manager over the same store dir ---
	global2 := sharedtools.NewToolRegistry()
	mgr2 := NewManager(global2, dir)
	defer mgr2.Close()

	// Bootstrap must NOT register the scoped server globally.
	mgr2.Bootstrap(context.Background())
	if _, ok := global2.Get("weather_get_weather"); ok {
		t.Fatalf("scoped server was incorrectly bootstrapped into global")
	}

	// ReplayScope("lane:L1") must register it into the restored lane scope.
	lane2 := sharedtools.NewChildRegistry(global2)
	mgr2.ReplayScope(context.Background(), "lane:L1", ScopeTarget{Key: "lane:L1", Registry: lane2})
	if _, ok := lane2.Get("weather_get_weather"); !ok {
		t.Fatalf("ReplayScope did not register the scoped server's tools; names=%v", lane2.Names())
	}

	// Replaying a non-matching scope registers nothing.
	other := sharedtools.NewChildRegistry(global2)
	mgr2.ReplayScope(context.Background(), "lane:OTHER", ScopeTarget{Key: "lane:OTHER", Registry: other})
	if _, ok := other.Get("weather_get_weather"); ok {
		t.Fatalf("ReplayScope registered tools into a non-matching scope")
	}
}
