// SPDX-License-Identifier: Apache-2.0

package session

// Tests for the web-server endpoint (port) recorded on the session record.
// This is the field the desktop app adopts a daemon through:
// its renderer speaks only HTTP/WebSocket, so a daemon whose endpoint is not
// advertised is one the app cannot open, no matter which front-end created it.

import (
	"os"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

func webEndpointCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Config{}
	cfg.SessionDir = t.TempDir()
	return cfg
}

// TestUpdateWebEndpointReadModifyWrite proves the daemon can advertise and
// withdraw its web endpoint without clobbering attach bookkeeping written by
// other writers (the attach flow, the web hub, ##proxy).
func TestUpdateWebEndpointReadModifyWrite(t *testing.T) {
	cfg := webEndpointCfg(t)
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Live pids (our own) so the store does not normalize the record to stopped.
	self := os.Getpid()
	if _, err := store.Upsert(Record{
		Name: "s1", Path: "/tmp/p", State: "running", PID: self, TUIPIDs: []int{self},
		NATSPort: 4222, AppClients: 1, ProxyPort: 51413,
	}); err != nil {
		t.Fatal(err)
	}

	UpdateWebEndpoint(cfg, "s1", 23232)

	rec, err := store.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.WebPort != 23232 {
		t.Errorf("web endpoint = %d, want 23232", rec.WebPort)
	}
	if rec.State != "running" || len(rec.TUIPIDs) != 1 || rec.NATSPort != 4222 ||
		rec.AppClients != 1 || rec.ProxyPort != 51413 {
		t.Errorf("bookkeeping clobbered: state=%s tuis=%v nats=%d app=%d proxy=%d",
			rec.State, rec.TUIPIDs, rec.NATSPort, rec.AppClients, rec.ProxyPort)
	}

	// `##rysh web stop` withdraws it: a port nothing listens on must not be
	// left on the record for a front-end to dial.
	UpdateWebEndpoint(cfg, "s1", 0)
	rec, _ = store.Get("s1")
	if rec.WebPort != 0 {
		t.Errorf("after stop: web endpoint = %d, want 0", rec.WebPort)
	}

	// Unknown session: silent no-op (must not create a record or panic).
	UpdateWebEndpoint(cfg, "nope", 8080)
	if _, err := store.Get("nope"); err == nil {
		t.Error("UpdateWebEndpoint created a record for an unknown session")
	}
}

// TestUpdateWebEndpointFromStopped covers the posture the desktop app's own
// daemons run with: a loopback web server, advertised onto a record that was
// last written as stopped.
func TestUpdateWebEndpointFromStopped(t *testing.T) {
	cfg := webEndpointCfg(t)
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(Record{Name: "s1", State: "stopped"}); err != nil {
		t.Fatal(err)
	}

	UpdateWebEndpoint(cfg, "s1", 41234)

	rec, _ := store.Get("s1")
	if rec.WebPort != 41234 {
		t.Errorf("WebPort = %d, want 41234", rec.WebPort)
	}
}

// TestWebEndpointSurvivesJSONRoundTrip pins the wire contract with the Electron
// side, which parses these records itself (electron/sessionStore.ts). The app
// reads snake_case `web_port`; a rename here silently strands it.
func TestWebEndpointSurvivesJSONRoundTrip(t *testing.T) {
	cfg := webEndpointCfg(t)
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(Record{
		Name: "s1", State: "detached", WebPort: 23232, Source: SourceCLI,
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(store.pathFor("s1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"web_port"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("record JSON is missing %s — the desktop app reads this key:\n%s", key, raw)
		}
	}
}

// TestUpdateWebMeta covers the wider hand-off: not just the port the app dials,
// but the bind address, the login name and the public URL a phone would use —
// the three things a session that is reachable from off this machine has to be
// able to say about itself.
func TestUpdateWebMeta(t *testing.T) {
	cfg := webEndpointCfg(t)
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	if _, err := store.Upsert(Record{
		Name: "s1", Path: "/tmp/p", State: "running", PID: self, TUIPIDs: []int{self},
		NATSPort: 4222, AppClients: 2, ProxyPort: 51413,
	}); err != nil {
		t.Fatal(err)
	}

	UpdateWebMeta(cfg, "s1", WebMeta{
		Port: 23001, Host: "0.0.0.0", User: "alice",
		PublicURL: "https://rysh-web.ngrok.app",
	})

	rec, err := store.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.WebPort != 23001 || rec.WebHost != "0.0.0.0" ||
		rec.WebUser != "alice" || rec.WebPublicURL != "https://rysh-web.ngrok.app" {
		t.Fatalf("web meta not recorded: %+v", rec)
	}
	// Other writers' bookkeeping survives the read-modify-write.
	if rec.NATSPort != 4222 || rec.AppClients != 2 || rec.ProxyPort != 51413 || len(rec.TUIPIDs) != 1 {
		t.Fatalf("clobbered another writer's fields: %+v", rec)
	}

	// A stopped server has no address, no login and no public URL — clearing
	// the port must clear the rest, or the record advertises a door that is
	// closed.
	UpdateWebMeta(cfg, "s1", WebMeta{Port: 0, Host: "0.0.0.0", User: "alice", PublicURL: "https://stale"})
	rec, err = store.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.WebPort != 0 || rec.WebHost != "" || rec.WebUser != "" || rec.WebPublicURL != "" {
		t.Fatalf("stopped server still advertised: %+v", rec)
	}

	// An unknown session is never conjured into existence.
	UpdateWebMeta(cfg, "nope", WebMeta{Port: 8080})
	if _, err := store.Get("nope"); err == nil {
		t.Error("UpdateWebMeta created a record for an unknown session")
	}
}

// The password is the one thing that must never reach the registry: the record
// is an ordinary file other tools read.
func TestWebMetaCarriesNoPassword(t *testing.T) {
	cfg := webEndpointCfg(t)
	store, err := NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	if _, err := store.Upsert(Record{Name: "s1", Path: "/tmp/p", State: "running", PID: self}); err != nil {
		t.Fatal(err)
	}
	UpdateWebMeta(cfg, "s1", WebMeta{Port: 23001, Host: "0.0.0.0", User: "alice"})

	data, err := os.ReadFile(store.pathFor("s1"))
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if strings.Contains(string(data), "password") {
		t.Fatalf("the session record mentions a password:\n%s", data)
	}
}
