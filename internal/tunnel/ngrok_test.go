// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// agentStub is a stand-in for the ngrok agent's local API.
type agentStub struct {
	tunnels []map[string]any
	created []map[string]any
	deleted []string
	// createStatus, when non-zero, makes POST /api/tunnels fail with it.
	createStatus int
	createBody   string
}

func (a *agentStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tunnels", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"tunnels": a.tunnels})
		case http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			a.created = append(a.created, body)
			if a.createStatus != 0 {
				w.WriteHeader(a.createStatus)
				_, _ = w.Write([]byte(a.createBody))
				return
			}
			name, _ := body["name"].(string)
			addr, _ := body["addr"].(string)
			created := map[string]any{
				"name":       name,
				"public_url": "https://created.ngrok.app",
				"proto":      "https",
				"config":     map[string]any{"addr": "http://localhost:" + addr},
			}
			a.tunnels = append(a.tunnels, created)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(created)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/tunnels/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		a.deleted = append(a.deleted, r.URL.Path[len("/api/tunnels/"):])
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func tunnelEntry(name, url, proto, addr string) map[string]any {
	return map[string]any{
		"name":       name,
		"public_url": url,
		"proto":      proto,
		"config":     map[string]any{"addr": addr},
	}
}

// An agent already forwarding our port is adopted verbatim — no create, and
// Stop leaves it alone. This is the LaunchAgent case: the tunnel outlives the
// session, and a session restart must not take it down.
func TestStartAdoptsExistingTunnel(t *testing.T) {
	agent := &agentStub{tunnels: []map[string]any{
		tunnelEntry("rysh-web-23001", "https://coming-gush.ngrok-free.dev", "https", "http://localhost:23001"),
		tunnelEntry("other", "https://other.ngrok.app", "https", "http://localhost:9999"),
	}}
	srv := agent.server(t)

	tun, err := Start(context.Background(), Options{Port: 23001, APIBase: srv.URL})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if tun.URL != "https://coming-gush.ngrok-free.dev" {
		t.Fatalf("URL = %q, want the existing tunnel's", tun.URL)
	}
	if tun.Origin != OriginAdopted || !tun.Adopted() {
		t.Fatalf("Origin = %q, want adopted", tun.Origin)
	}
	if len(agent.created) != 0 {
		t.Fatalf("created %d tunnels, want 0 — an existing one was already up", len(agent.created))
	}
	if err := tun.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(agent.deleted) != 0 {
		t.Fatalf("Stop deleted %v — an adopted tunnel is not ours to close", agent.deleted)
	}
}

// The http twin of the same tunnel must not win over its https entry: the URL
// goes to a browser.
func TestStartPrefersHTTPS(t *testing.T) {
	agent := &agentStub{tunnels: []map[string]any{
		tunnelEntry("rysh (http)", "http://plain.ngrok.app", "http", "localhost:23232"),
		tunnelEntry("rysh", "https://secure.ngrok.app", "https", "localhost:23232"),
	}}
	srv := agent.server(t)

	tun, err := Start(context.Background(), Options{Port: 23232, APIBase: srv.URL})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if tun.URL != "https://secure.ngrok.app" {
		t.Fatalf("URL = %q, want the https entry", tun.URL)
	}
}

// A running agent with no tunnel for our port is asked to add one, rather than
// a second agent being spawned — the free plan allows exactly one session.
func TestStartCreatesViaRunningAgent(t *testing.T) {
	agent := &agentStub{tunnels: []map[string]any{
		tunnelEntry("someone-else", "https://other.ngrok.app", "https", "localhost:4000"),
	}}
	srv := agent.server(t)

	tun, err := Start(context.Background(), Options{Port: 23001, APIBase: srv.URL, Domain: "rysh.ngrok.app"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if tun.Origin != OriginCreated {
		t.Fatalf("Origin = %q, want created", tun.Origin)
	}
	if tun.URL != "https://created.ngrok.app" {
		t.Fatalf("URL = %q", tun.URL)
	}
	if len(agent.created) != 1 {
		t.Fatalf("create calls = %d, want 1", len(agent.created))
	}
	if got := agent.created[0]["addr"]; got != "23001" {
		t.Fatalf("addr = %v, want the local port", got)
	}
	if got := agent.created[0]["domain"]; got != "rysh.ngrok.app" {
		t.Fatalf("domain = %v, want it passed through", got)
	}
	// What we created, we close.
	if err := tun.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(agent.deleted) != 1 || agent.deleted[0] != tun.Name {
		t.Fatalf("deleted = %v, want [%s]", agent.deleted, tun.Name)
	}
}

// A running agent that refuses the tunnel is reported with its own reason, and
// no rival agent is spawned behind its back.
func TestStartReportsAgentRefusal(t *testing.T) {
	agent := &agentStub{
		createStatus: http.StatusBadRequest,
		createBody:   `{"error_code":"ERR_NGROK_324","msg":"domain is already bound"}`,
	}
	srv := agent.server(t)

	_, err := Start(context.Background(), Options{Port: 23001, APIBase: srv.URL})
	if err == nil {
		t.Fatal("Start succeeded, want the agent's refusal")
	}
	if want := "domain is already bound"; !contains(err.Error(), want) {
		t.Fatalf("error %q does not carry the agent's reason %q", err, want)
	}
}

func TestLookup(t *testing.T) {
	agent := &agentStub{tunnels: []map[string]any{
		tunnelEntry("rysh", "https://found.ngrok.app", "https", "http://localhost:23001"),
	}}
	srv := agent.server(t)

	got, err := Lookup(context.Background(), srv.URL, 23001)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got != "https://found.ngrok.app" {
		t.Fatalf("Lookup = %q", got)
	}
	if _, err := Lookup(context.Background(), srv.URL, 24000); err != ErrNoTunnel {
		t.Fatalf("Lookup(unpublished port) = %v, want ErrNoTunnel", err)
	}
}

// Lookup against a dead agent is an error, not a silent "no tunnel" — the two
// mean different things to the caller reporting status.
func TestLookupNoAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	if _, err := Lookup(context.Background(), srv.URL, 23001); err == nil || err == ErrNoTunnel {
		t.Fatalf("err = %v, want a transport/status error", err)
	}
}

func TestAddrPort(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"23001", 23001},
		{"localhost:23001", 23001},
		{"http://localhost:23001", 23001},
		{"https://127.0.0.1:23232/", 23232},
		{"", 0},
		{"not-a-port", 0},
		{"localhost:99999", 0},
	} {
		if got := addrPort(tc.in); got != tc.want {
			t.Errorf("addrPort(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
