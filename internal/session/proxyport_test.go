// SPDX-License-Identifier: Apache-2.0

package session

// Tests for the governance-proxy port field (design 001) on the session record.

import (
	"os"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

func proxyPortCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Config{}
	cfg.SessionDir = t.TempDir()
	return cfg
}

// TestUpdateProxyPortReadModifyWrite proves ##proxy on/off records the port
// without clobbering attach bookkeeping (design 001): registry-inspecting tools
// can read the governed endpoint, and a concurrent TUI attach is preserved.
func TestUpdateProxyPortReadModifyWrite(t *testing.T) {
	cfg := proxyPortCfg(t)
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Live pids (our own) so the store does not normalize the record to stopped.
	self := os.Getpid()
	if _, err := store.Upsert(Record{
		Name: "s1", Path: "/tmp/p", State: "running", PID: self, TUIPIDs: []int{self},
		NATSPort: 4222, AppClients: 1,
	}); err != nil {
		t.Fatal(err)
	}

	UpdateProxyPort(cfg, "s1", 51413)

	rec, err := store.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ProxyPort != 51413 {
		t.Errorf("ProxyPort = %d, want 51413", rec.ProxyPort)
	}
	// Everything the daemon/attach set must survive the read-modify-write.
	if rec.State != "running" || len(rec.TUIPIDs) != 1 || rec.NATSPort != 4222 || rec.AppClients != 1 {
		t.Errorf("bookkeeping clobbered: state=%s tuis=%v nats=%d app=%d",
			rec.State, rec.TUIPIDs, rec.NATSPort, rec.AppClients)
	}

	// ##proxy off clears it.
	UpdateProxyPort(cfg, "s1", 0)
	rec, _ = store.Get("s1")
	if rec.ProxyPort != 0 {
		t.Errorf("ProxyPort = %d, want 0 after off", rec.ProxyPort)
	}

	// Unknown session: silent no-op (must not create a record or panic).
	UpdateProxyPort(cfg, "nope", 8080)
	if _, err := store.Get("nope"); err == nil {
		t.Error("UpdateProxyPort created a record for an unknown session")
	}
}

// TestSelfHealPreservesProxyPort: a heal (daemon alive but record clobbered)
// must keep the live proxy port, like AppClients — otherwise the 30s record
// guard would drop the governed endpoint on the next stale-record repair.
func TestSelfHealPreservesProxyPort(t *testing.T) {
	self := Record{Name: "s1", PID: 42, NATSPort: 4242}
	existing := Record{Name: "s1", PID: 42, NATSPort: 4242, State: "stopped", ProxyPort: 51413}
	healed, changed := SelfHeal(existing, true, self)
	if !changed {
		t.Fatal("expected a heal for a stopped-but-alive record")
	}
	if healed.ProxyPort != 51413 {
		t.Errorf("heal dropped ProxyPort: %d, want 51413", healed.ProxyPort)
	}
}
