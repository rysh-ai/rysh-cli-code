// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
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

const regWS = "ws-reg"

// startForgeBroker starts an embedded broker standing in for the upstream and
// returns a dial func for ForgeShareActor plus a plain client for the test.
func startForgeBroker(t *testing.T) (func(config.UpstreamConfig, string, ...nats.Option) (*nats.Conn, error), *nats.Conn) {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats not ready")
	}
	dial := func(_ config.UpstreamConfig, _ string, _ ...nats.Option) (*nats.Conn, error) {
		return nats.Connect(nats.DefaultURL, nats.InProcessServer(ns))
	}
	tc, err := dial(config.UpstreamConfig{}, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tc.Close)
	return dial, tc
}

// newWeatherSource builds a source ForgeShareActor with "weather" enabled.
func newWeatherSource(t *testing.T, dial func(config.UpstreamConfig, string, ...nats.Option) (*nats.Conn, error), pub *msg.NATSPublisher) *ForgeShareActor {
	t.Helper()
	dir := t.TempDir()
	rel, err := forge.StoreSpec(dir, "weather", weatherSpec("http://127.0.0.1:1"), "json")
	if err != nil {
		t.Fatal(err)
	}
	if err := forge.SaveStore(dir, []forge.Integration{{Name: "weather", Source: forge.SourceOpenAPI, SpecFile: rel, BaseURL: "http://127.0.0.1:1"}}); err != nil {
		t.Fatal(err)
	}
	fm := forge.NewManager(tools.NewToolRegistry(), dir, nil)
	if _, _, err := fm.EnableByName(context.Background(), "weather", fm.GlobalTarget()); err != nil {
		t.Fatal(err)
	}
	a := NewForgeShareActor("source", config.UpstreamConfig{Enabled: true, URL: "http://upstream.test", Workspace: regWS}, pub, fm, agentic.NewScopeRegistries(tools.NewToolRegistry()))
	a.dial = dial
	a.uid = "src-uid"
	t.Cleanup(a.teardown)
	return a
}

// registry is a fake server-side forge registry on the test broker.
type registry struct {
	mu        sync.Mutex
	registers []forgeRegisterReq
	unregs    []forgeRegisterReq
	sharedAt  []bool // whether the source already shared the api when its register arrived
}

func startRegistry(t *testing.T, tc *nats.Conn, src *ForgeShareActor, reply string) *registry {
	t.Helper()
	r := &registry{}
	sub1, err := tc.Subscribe(forgeRegisterSubject(regWS), func(m *nats.Msg) {
		var req forgeRegisterReq
		_ = json.Unmarshal(m.Data, &req)
		src.mu.Lock()
		_, already := src.shared[req.API]
		src.mu.Unlock()
		r.mu.Lock()
		r.registers = append(r.registers, req)
		r.sharedAt = append(r.sharedAt, already)
		r.mu.Unlock()
		_ = m.Respond([]byte(reply))
	})
	if err != nil {
		t.Fatal(err)
	}
	sub2, err := tc.Subscribe(forgeUnregisterSubject(regWS), func(m *nats.Msg) {
		var req forgeRegisterReq
		_ = json.Unmarshal(m.Data, &req)
		r.mu.Lock()
		r.unregs = append(r.unregs, req)
		r.mu.Unlock()
		if m.Reply != "" {
			_ = m.Respond([]byte(`{"ok":true}`))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub1.Unsubscribe(); _ = sub2.Unsubscribe() })
	_ = tc.Flush()
	return r
}

func (r *registry) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.registers), len(r.unregs)
}

func isShared(a *ForgeShareActor, name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.shared[name]
	return ok
}

// invokeServed reports whether anyone answers on the api's invoke subject.
func invokeServed(tc *nats.Conn, api string) bool {
	_, err := tc.Request(forgeInvokeSubject(regWS, api), []byte(`{}`), 300*time.Millisecond)
	return !errors.Is(err, nats.ErrNoResponders)
}

func TestForgeRegister_RegistersBeforeServing(t *testing.T) {
	dial, tc := startForgeBroker(t)
	src := newWeatherSource(t, dial, nil)
	reg := startRegistry(t, tc, src, `{"ok":true}`)

	src.shareAPI("weather", "")
	if !isShared(src, "weather") || !invokeServed(tc, "weather") {
		t.Fatal("an accepted registration did not share the api")
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if len(reg.registers) != 1 {
		t.Fatalf("registers = %d, want 1", len(reg.registers))
	}
	got := reg.registers[0]
	if got.V != 1 || got.API != "weather" || got.Source != "src-uid" || len(got.Ops) == 0 {
		t.Fatalf("register body = %+v, want v=1 api=weather source=src-uid and op names", got)
	}
	if reg.sharedAt[0] {
		t.Fatal("the register arrived after the api was already shared; it must come first")
	}
}

func TestForgeRegister_RefusedNameIsNotShared(t *testing.T) {
	dial, tc := startForgeBroker(t)
	local := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(local, msg.DefaultCodecRegistry())
	src := newWeatherSource(t, dial, pub)
	startRegistry(t, tc, src, `{"ok":false,"error":"the name is held by another member"}`)
	notices := paneNotices(t, local, "pane-forge", "the server refused the name")

	src.shareAPI("weather", "pane-forge")
	if isShared(src, "weather") {
		t.Fatal("a refused name was recorded as shared")
	}
	if invokeServed(tc, "weather") {
		t.Fatal("the source serves invoke for a name the server refused")
	}
	// out() writes each line to the merged and the rysh buffer, which fans out
	// to more than one bus message, so only presence is asserted.
	waitTrue(t, "the refusal notice", func() bool { return notices.Load() >= 1 })
}

func TestForgeRegister_OlderServerSharesAsBefore(t *testing.T) {
	dial, tc := startForgeBroker(t)
	src := newWeatherSource(t, dial, nil) // no registry on the broker
	start := time.Now()
	src.shareAPI("weather", "")
	if !isShared(src, "weather") || !invokeServed(tc, "weather") {
		t.Fatal("with no registry the api must be shared as before")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("share took %s with no registry; no-responders should answer at once", d)
	}
}

func TestForgeRegister_InvalidNameRefusedBeforeConnecting(t *testing.T) {
	dial, _ := startForgeBroker(t)
	local := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(local, msg.DefaultCodecRegistry())
	src := newWeatherSource(t, dial, pub)
	notices := paneNotices(t, local, "pane-forge", "cannot be empty or contain")
	for _, bad := range []string{"we.ather", "w*", "w>", "a b", ""} {
		src.shareAPI(bad, "pane-forge")
		src.subscribe(bad, agentic.ScopeTab, agentic.ScopeIDs{TabID: "T1"}, "pane-forge", 0, nil)
	}
	if src.nc != nil {
		t.Fatal("an invalid name opened the upstream connection")
	}
	// At least one message per refused call (see the refusal test on fan-out).
	waitTrue(t, "10 refusals", func() bool { return notices.Load() >= 10 })
}

func TestForgeRegister_HeartbeatAndUnregister(t *testing.T) {
	old := forgeReregisterInterval
	forgeReregisterInterval = 80 * time.Millisecond
	t.Cleanup(func() { forgeReregisterInterval = old })

	dial, tc := startForgeBroker(t)
	src := newWeatherSource(t, dial, nil)
	reg := startRegistry(t, tc, src, `{"ok":true}`)
	src.shareAPI("weather", "")
	waitTrue(t, "heartbeat re-registers", func() bool { n, _ := reg.counts(); return n >= 3 })

	src.unshareAPI("weather", "")
	if _, u := reg.counts(); u != 1 {
		t.Fatalf("unregisters after unshare = %d, want 1", u)
	}
	if src.reregStop != nil {
		t.Fatal("the heartbeat loop still runs with nothing shared")
	}
	n1, _ := reg.counts()
	time.Sleep(300 * time.Millisecond)
	if n2, _ := reg.counts(); n2 != n1 {
		t.Fatalf("registers went on after unshare: %d -> %d", n1, n2)
	}
}

func TestForgeRegister_TeardownUnregisters(t *testing.T) {
	dial, tc := startForgeBroker(t)
	src := newWeatherSource(t, dial, nil)
	reg := startRegistry(t, tc, src, `{"ok":true}`)
	src.shareAPI("weather", "")
	src.teardown()
	waitTrue(t, "the teardown unregister", func() bool { _, u := reg.counts(); return u == 1 })
}
