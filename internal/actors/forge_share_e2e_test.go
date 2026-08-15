// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/forge"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/tools"
)

// weatherSpec is a minimal OpenAPI 3 spec with one GET operation (getWeather),
// pointed at the test HTTP backend.
func weatherSpec(baseURL string) []byte {
	return []byte(`{
  "openapi": "3.0.0",
  "info": {"title": "weather", "version": "1.0.0"},
  "servers": [{"url": "` + baseURL + `"}],
  "paths": {
    "/weather": {
      "get": {"operationId": "getWeather", "responses": {"200": {"description": "ok"}}}
    }
  }
}`)
}

// TestForgeShareEndToEndTwoSessions is the live end-to-end test of forged-API
// sharing across TWO sessions over a real shared NATS broker and a real HTTP
// backend — the full path minus the WebSocket/auth transport:
//
//	SOURCE:  ##forge add weather + ##integration enable + ##forge share api weather
//	SUB:     ##forge subscribe weather --scope tab  →  call weather_getWeather in AI mode
//
// It asserts the op runs ON THE SOURCE (hits the backend) and the result returns
// to the subscriber, and that a built-in (`bash`) is rejected by forge-origin.
func TestForgeShareEndToEndTwoSessions(t *testing.T) {
	// --- shared "upstream" broker both sessions connect to ---
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	ns.Start()
	defer ns.Shutdown()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats not ready")
	}
	dial := func(_ config.UpstreamConfig, _ string, _ ...nats.Option) (*nats.Conn, error) {
		return nats.Connect(nats.DefaultURL, nats.InProcessServer(ns))
	}

	// --- real HTTP forge backend (the "weather service") ---
	var hits int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"city":"SF","tempC":20,"summary":"sunny"}`))
	}))
	defer backend.Close()

	cfg := config.UpstreamConfig{Enabled: true, URL: "http://upstream.test", Workspace: "ws-e2e"}

	// --- SOURCE session: enable "weather" in its forge manager, then share it ---
	dir := t.TempDir()
	rel, err := forge.StoreSpec(dir, "weather", weatherSpec(backend.URL), "json")
	if err != nil {
		t.Fatalf("StoreSpec: %v", err)
	}
	if err := forge.SaveStore(dir, []forge.Integration{{Name: "weather", Source: forge.SourceOpenAPI, SpecFile: rel, BaseURL: backend.URL}}); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}
	srcForge := forge.NewManager(tools.NewToolRegistry(), dir, nil)
	if _, _, err := srcForge.EnableByName(context.Background(), "weather", srcForge.GlobalTarget()); err != nil {
		t.Fatalf("enable weather: %v", err)
	}
	source := NewForgeShareActor("source", cfg, nil, srcForge, agentic.NewScopeRegistries(tools.NewToolRegistry()))
	source.dial = dial
	source.uid = "src"
	if !source.ensureConn("") {
		t.Fatal("source: ensureConn failed")
	}
	source.shareAPI("weather", "") // ##forge share api weather

	source.mu.Lock()
	_, isShared := source.shared["weather"]
	source.mu.Unlock()
	if !isShared {
		t.Fatal("source did not record the shared API")
	}

	// --- SUBSCRIBER session: discover + subscribe at TAB scope ---
	subScopes := agentic.NewScopeRegistries(tools.NewToolRegistry())
	subscriber := NewForgeShareActor("subscriber", cfg, nil, forge.NewManager(tools.NewToolRegistry(), t.TempDir(), nil), subScopes)
	subscriber.dial = dial
	subscriber.uid = "sub"
	if !subscriber.ensureConn("") {
		t.Fatal("subscriber: ensureConn failed")
	}
	ids := agentic.ScopeIDs{TabID: "T1"}
	subscriber.subscribe("weather", agentic.ScopeTab, ids, "", 0, nil) // ##forge subscribe weather --scope tab (Model A)

	// The proxy must now be live in the subscriber's TAB-scope registry, so every
	// pane in that tab can call it in AI mode.
	tabReg := subScopes.RegistryFor(agentic.ScopeTab, ids)
	proxy, ok := tabReg.Get("weather_getWeather")
	if !ok {
		t.Fatalf("weather_getWeather not registered in the subscriber's tab scope; names=%v", tabReg.Names())
	}

	// --- invoke: subscriber proxy → NATS → source → forge.RunOp → HTTP backend ---
	out, err := proxy.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("proxy execute: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("invoke returned error: %s (%s)", out.Error, out.ErrorKind)
	}
	if !strings.Contains(out.Content, "sunny") {
		t.Fatalf("unexpected invoke result (no backend payload): %q", out.Content)
	}
	if hits == 0 {
		t.Fatal("the op did not execute on the source (backend was never hit)")
	}
	t.Logf("e2e OK: weather_getWeather ran on source (%d backend hit) → %q", hits, out.Content)

	// --- forge-origin guarantee: a built-in name is rejected on the wire ---
	probe, _ := nats.Connect(nats.DefaultURL, nats.InProcessServer(ns))
	defer probe.Close()
	reqBody, _ := json.Marshal(msg.ForgedInvokeRequest{Op: "bash", Args: json.RawMessage(`{}`)})
	reply, err := probe.Request(forgeInvokeSubject("ws-e2e", "weather"), reqBody, 5*time.Second)
	if err != nil {
		t.Fatalf("probe request: %v", err)
	}
	var res msg.ForgedInvokeResult
	if err := json.Unmarshal(reply.Data, &res); err != nil {
		t.Fatalf("probe reply decode: %v", err)
	}
	if res.Error == "" || res.ErrorKind != tools.ErrKindPermissionDenied {
		t.Fatalf("built-in 'bash' should be permission_denied by forge-origin, got: %+v", res)
	}
	t.Logf("forge-origin OK: 'bash' rejected → %s", res.Error)
}
