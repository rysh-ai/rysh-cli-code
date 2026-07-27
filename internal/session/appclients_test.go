package session

// Tests for the app-client presence field (attached (app) display support).

import (
	"os"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

func appClientsCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Config{}
	cfg.SessionDir = t.TempDir()
	return cfg
}

func TestUpdateAppClientsReadModifyWrite(t *testing.T) {
	cfg := appClientsCfg(t)
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a record with attach bookkeeping that must survive the update.
	// Live pids (our own) — the store normalizes records with dead pids to
	// "stopped" on read, which would mask what this test asserts.
	self := os.Getpid()
	_, err = store.Upsert(Record{
		Name: "s1", Path: "/tmp/p", State: "running", PID: self, TUIPIDs: []int{self},
	})
	if err != nil {
		t.Fatal(err)
	}

	UpdateAppClients(cfg, "s1", 2)

	rec, err := store.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.AppClients != 2 {
		t.Errorf("AppClients = %d, want 2", rec.AppClients)
	}
	if rec.State != "running" || len(rec.TUIPIDs) != 1 {
		t.Errorf("attach bookkeeping clobbered: state=%s tuis=%v", rec.State, rec.TUIPIDs)
	}

	// Back to zero on disconnect.
	UpdateAppClients(cfg, "s1", 0)
	rec, _ = store.Get("s1")
	if rec.AppClients != 0 {
		t.Errorf("AppClients = %d, want 0", rec.AppClients)
	}

	// Unknown session: silent no-op.
	UpdateAppClients(cfg, "nope", 3)
}

func TestSelfHealPreservesAppClients(t *testing.T) {
	self := Record{Name: "s1", PID: 42, NATSPort: 4242}
	// Ours but state clobbered to "stopped": heal must keep the live count.
	existing := Record{Name: "s1", PID: 42, NATSPort: 4242, State: "stopped", AppClients: 1}
	healed, changed := SelfHeal(existing, true, self)
	if !changed {
		t.Fatal("expected a heal for a stopped-but-alive record")
	}
	if healed.AppClients != 1 {
		t.Errorf("heal dropped AppClients: %d, want 1", healed.AppClients)
	}
	if healed.State != "detached" {
		t.Errorf("healed state = %q, want detached", healed.State)
	}
}
