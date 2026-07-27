package actors

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/forge"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/runtime"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/tools"
)

// forgeDiscoverWindow is how long ##forge list-remote / subscribe waits to
// collect catalog responses from peer sessions (scatter-gather).
const forgeDiscoverWindow = 2500 * time.Millisecond

// forgeSubscribeRetryDelay is the wait between bounded subscribe retries (used by
// declarative startup config when the source hasn't shared the API yet).
const forgeSubscribeRetryDelay = 3 * time.Second

// ForgeShareActor implements first-class forged-API sharing, decoupled from
// pane/tab sharing (##share). A session can be both a SOURCE (it shared an API
// with `##forge share api <name>`) and a SUBSCRIBER (it consumed someone else's
// API with `##forge subscribe <name> [--scope ...]`).
//
// Transport is peer-to-peer over the upstream workspace namespace ws.{ws}.forge.*
// (the transparent proxy relays it between same-workspace sessions; no server
// changes needed):
//
//	ws.{ws}.forge.catalog.req       discovery (scatter-gather request/reply)
//	ws.{ws}.forge.api.{name}.invoke per-API invocation (request/reply)
//
// SOURCE responsibilities: answer discovery with its shared APIs' op specs, and
// answer invocations by running the op on the owner's forge runner under the
// same governance as before (forge-origin + default-deny-mutation allow/deny +
// redaction + timeout; NO per-call approval) via evaluateInvoke.
//
// SUBSCRIBER responsibilities: discover, then register a tools.ForgedAPIProxy per
// op into the SUBSCRIBER-CHOSEN scope registry (pane/panegroup/lane/tab) — the
// source has no say in the subscriber's scope. Each proxy's Execute round-trips
// the invoke subject to the source.
//
// It is spawned as a child of WorkspaceActor when upstream is enabled.
type ForgeShareActor struct {
	sessionName string
	ws          string // upstream NATS namespace (workspace UUID)
	uid         string // per-session id, attributed on catalog responses (skip-self)
	config      config.UpstreamConfig
	pub         *msg.NATSPublisher
	forge       *forge.Manager
	scopes      *agentic.ScopeRegistries

	nc         *nats.Conn         // upstream connection (lazy; mailbox-only)
	catalogSub *nats.Subscription // discovery responder, armed while sharing (mailbox-only)

	// mu guards shared/subs because handleCatalogReq runs on a NATS callback
	// goroutine (off the mailbox) and reads the shared set. (Documented mutex
	// exception, like the VTerm wrapper.)
	mu     sync.Mutex
	shared map[string]*nats.Subscription // SOURCE: apiName -> invoke responder
	subs   map[string]*forgeSubscription // SUBSCRIBER: apiName -> registered proxies

	selfPID *actor.PID
	system  *actor.ActorSystem

	// dial opens the upstream connection; defaults to dialUpstream. Overridable in
	// tests to point at an in-process broker (no WebSocket / auth).
	dial func(config.UpstreamConfig, string, ...nats.Option) (*nats.Conn, error)
}

type forgeSubscription struct {
	reg       *tools.ToolRegistry
	toolNames []string
	scopeStr  string
}

// NewForgeShareActor builds the actor. fm/scopes come from the agentic Setup.
func NewForgeShareActor(sessionName string, cfg config.UpstreamConfig, pub *msg.NATSPublisher, fm *forge.Manager, scopes *agentic.ScopeRegistries) *ForgeShareActor {
	return &ForgeShareActor{
		sessionName: sessionName,
		ws:          cfg.WorkspaceName(),
		config:      cfg,
		pub:         pub,
		forge:       fm,
		scopes:      scopes,
		shared:      map[string]*nats.Subscription{},
		subs:        map[string]*forgeSubscription{},
		dial:        dialUpstream,
	}
}

// --- in-process command messages (sent by WorkspaceActor; not over NATS) ---

type msgForgeShareAPI struct{ Name, PaneID string }
type msgForgeUnshareAPI struct{ Name, PaneID string }
type msgForgeListRemote struct{ PaneID string }
type msgForgeSubscribe struct {
	Name        string
	Kind        agentic.ScopeKind
	IDs         agentic.ScopeIDs
	PaneID      string
	RetriesLeft int                 // >0 ⇒ reschedule discovery if the API isn't found yet (startup)
	Auth        *runtime.AuthConfig // (Model B) subscriber identity; nil ⇒ Model A (owner identity)
}
type msgForgeUnsubscribe struct{ Name, PaneID string }
type msgForgeShares struct{ PaneID string }        // list APIs THIS session shares
type msgForgeSubscriptions struct{ PaneID string } // list APIs THIS session subscribes to

// msgForgePing / msgForgePong are an in-band reachability probe: the workspace
// RequestFutures a ping and prints the pong, so a delivery failure surfaces in
// the pane synchronously (no debug logs needed).
type msgForgePing struct{}
type msgForgePong struct {
	UID    string
	WS     string
	Shared int
	Subs   int
}

// --- wire structs (JSON over the upstream namespace) ---

type forgeCatalogReq struct {
	Api string `json:"api,omitempty"` // optional name filter
}
type forgeRemoteAPI struct {
	Name   string             `json:"name"`
	Source string             `json:"source"`
	Ops    []msg.ForgedOpSpec `json:"ops"`
}
type forgeCatalogResp struct {
	Source string           `json:"source"`
	APIs   []forgeRemoteAPI `json:"apis"`
}

func forgeCatalogSubject(ws string) string { return fmt.Sprintf("ws.%s.forge.catalog.req", ws) }

// isAuthFailure reports whether an owner-relayed invoke error looks like a
// backend authentication failure (HTTP 401 / 403), so a delegated subscriber can
// refresh its token and retry once (forged-API auth plan, Model B).
func isAuthFailure(errMsg string) bool {
	if errMsg == "" {
		return false
	}
	return strings.Contains(errMsg, "HTTP 401") || strings.Contains(errMsg, "HTTP 403")
}

func forgeInvokeSubject(ws, api string) string {
	return fmt.Sprintf("ws.%s.forge.api.%s.invoke", ws, api)
}

// Receive implements actor.Actor.
func (a *ForgeShareActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		a.selfPID = ctx.Self()
		a.system = ctx.ActorSystem()
		a.uid = uuid.NewString()
	case *actor.Stopping:
		a.teardown()
	case *msgForgeShareAPI:
		a.shareAPI(m.Name, m.PaneID)
	case *msgForgeUnshareAPI:
		a.unshareAPI(m.Name, m.PaneID)
	case *msgForgeListRemote:
		a.listRemote(m.PaneID)
	case *msgForgeSubscribe:
		a.subscribe(m.Name, m.Kind, m.IDs, m.PaneID, m.RetriesLeft, m.Auth)
	case *msgForgeUnsubscribe:
		a.unsubscribe(m.Name, m.PaneID)
	case *msgForgeShares:
		a.listShares(m.PaneID)
	case *msgForgeSubscriptions:
		a.listSubscriptions(m.PaneID)
	case *msgForgePing:
		a.mu.Lock()
		ns, nsub := len(a.shared), len(a.subs)
		a.mu.Unlock()
		if ctx.Sender() != nil {
			ctx.Respond(&msgForgePong{UID: a.uid, WS: a.ws, Shared: ns, Subs: nsub})
		}
	}
}

// listShares reports the forged APIs this session is currently SHARING (source
// role), so the user can confirm `##forge share api <name>` took effect.
func (a *ForgeShareActor) listShares(paneID string) {
	a.mu.Lock()
	names := make([]string, 0, len(a.shared))
	for n := range a.shared {
		names = append(names, n)
	}
	a.mu.Unlock()
	if len(names) == 0 {
		a.out(paneID, "[forge] not sharing any APIs — share one with: ##forge share api <name>\n")
		return
	}
	sort.Strings(names)
	connected := a.nc != nil && a.nc.IsConnected()
	var b strings.Builder
	fmt.Fprintf(&b, "[forge] sharing %d API(s) (upstream: %s, workspace %s):\n", len(names), connState(connected), a.ws)
	for _, n := range names {
		fmt.Fprintf(&b, "  %-20s %d op(s)   subscribers run: ##forge subscribe %s\n", n, len(a.forge.APIOps(n)), n)
	}
	// Surface the invocation policy so the owner can confirm what is loaded. Reads
	// (GET/HEAD) are allowed by default; mutating ops (POST/PUT/...) are DENIED
	// unless they match the allow list (upstream.forged_api_allow).
	fmt.Fprintf(&b, "  policy: allow=%v block=%v  (mutating ops need an allow match)\n", a.config.ForgedAPIAllow, a.config.ForgedAPIBlock)
	a.out(paneID, b.String())
}

// listSubscriptions reports the remote APIs this session is consuming
// (subscriber role) and the scope each is mounted at.
func (a *ForgeShareActor) listSubscriptions(paneID string) {
	a.mu.Lock()
	type row struct {
		name, scope string
		ops         int
	}
	rows := make([]row, 0, len(a.subs))
	for n, s := range a.subs {
		rows = append(rows, row{name: n, scope: s.scopeStr, ops: len(s.toolNames)})
	}
	a.mu.Unlock()
	if len(rows) == 0 {
		a.out(paneID, "[forge] no active subscriptions — subscribe with: ##forge subscribe <name> [--scope ...]\n")
		return
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	var b strings.Builder
	fmt.Fprintf(&b, "[forge] %d active subscription(s):\n", len(rows))
	for _, r := range rows {
		fmt.Fprintf(&b, "  %-20s %d op(s)   scope: %s\n", r.name, r.ops, r.scope)
	}
	a.out(paneID, b.String())
}

func connState(connected bool) string {
	if connected {
		return "connected"
	}
	return "not connected"
}

func (a *ForgeShareActor) out(paneID, s string) {
	if a.pub == nil || paneID == "" {
		return
	}
	// Publish to BOTH the merged output buffer (shown in shell/prompt mode — where
	// the user usually is right after a ## command) AND the rysh buffer (rysh
	// mode), mirroring how the ## command output itself is delivered. Publishing
	// only to rysh made this async feedback invisible unless the pane happened to
	// be in rysh mode, so it looked like nothing happened.
	_ = a.pub.SendPaneOutput(paneID, s)
	_ = a.pub.SendPaneRyshOutput(paneID, s)
}

// ensureConn lazily connects to the upstream; reports a friendly error to the
// pane and returns false when upstream is not configured / unreachable.
func (a *ForgeShareActor) ensureConn(paneID string) bool {
	if a.nc != nil && a.nc.IsConnected() {
		return true
	}
	if !a.config.Enabled || a.config.URL == "" {
		a.out(paneID, "[forge] upstream is not configured — set [upstream] url/api_key/workspace to share or subscribe to APIs\n")
		return false
	}
	dial := a.dial
	if dial == nil {
		dial = dialUpstream
	}
	nc, err := dial(a.config, "rysh-forge-"+shortID(a.uid))
	if err != nil {
		a.out(paneID, fmt.Sprintf("[forge] upstream connect failed: %v\n", err))
		return false
	}
	a.nc = nc
	return true
}

// ---- SOURCE side ----

func (a *ForgeShareActor) shareAPI(name, paneID string) {
	slog.Info("forge-share: share api requested", "name", name, "pane", shortID(paneID), "url", a.config.URL, "ws", a.ws)
	ops := a.forge.APIOps(name)
	if len(ops) == 0 {
		a.out(paneID, fmt.Sprintf("[forge] %q is not enabled — run '##integration enable %s' first\n", name, name))
		return
	}
	a.mu.Lock()
	_, already := a.shared[name]
	a.mu.Unlock()
	if already {
		a.out(paneID, fmt.Sprintf("[forge] api %q is already shared (%d op(s))\n", name, len(ops)))
		return
	}
	// Progress feedback BEFORE the (network-blocking) connect, so a slow or failed
	// upstream connection is never silent.
	a.out(paneID, fmt.Sprintf("[forge] %q: %d op(s) found; connecting to upstream %s …\n", name, len(ops), a.config.URL))
	if !a.ensureConn(paneID) {
		return
	}
	// Arm the discovery responder once (the first shared API).
	if a.catalogSub == nil {
		sub, err := a.nc.Subscribe(forgeCatalogSubject(a.ws), a.handleCatalogReq)
		if err != nil {
			a.out(paneID, fmt.Sprintf("[forge] discovery responder failed: %v\n", err))
			return
		}
		a.catalogSub = sub
	}
	apiName := name
	sub, err := a.nc.Subscribe(forgeInvokeSubject(a.ws, name), func(mm *nats.Msg) { a.handleInvoke(apiName, mm) })
	if err != nil {
		a.out(paneID, fmt.Sprintf("[forge] invoke responder failed: %v\n", err))
		return
	}
	_ = a.nc.Flush()
	a.mu.Lock()
	a.shared[name] = sub
	a.mu.Unlock()
	a.out(paneID, fmt.Sprintf("[forge] ✓ sharing api %q (%d op(s)) — a subscriber can run: ##forge subscribe %s\n", name, len(ops), name))
	slog.Info("forge-share: shared api", "api", name, "ops", len(ops), "ws", a.ws)
}

func (a *ForgeShareActor) unshareAPI(name, paneID string) {
	a.mu.Lock()
	sub := a.shared[name]
	delete(a.shared, name)
	empty := len(a.shared) == 0
	a.mu.Unlock()
	if sub == nil {
		a.out(paneID, fmt.Sprintf("[forge] api %q is not shared\n", name))
		return
	}
	_ = sub.Unsubscribe()
	if empty && a.catalogSub != nil {
		_ = a.catalogSub.Unsubscribe()
		a.catalogSub = nil
	}
	a.out(paneID, fmt.Sprintf("[forge] stopped sharing api %q\n", name))
}

// handleCatalogReq answers a discovery request with this source's shared APIs
// (optionally filtered by name). Runs on a NATS callback goroutine.
func (a *ForgeShareActor) handleCatalogReq(m *nats.Msg) {
	if m.Reply == "" {
		return
	}
	var req forgeCatalogReq
	_ = json.Unmarshal(m.Data, &req)
	a.mu.Lock()
	names := make([]string, 0, len(a.shared))
	for n := range a.shared {
		names = append(names, n)
	}
	a.mu.Unlock()
	var apis []forgeRemoteAPI
	for _, n := range names {
		if req.Api != "" && req.Api != n {
			continue
		}
		apis = append(apis, forgeRemoteAPI{Name: n, Source: a.uid, Ops: specsOf(a.forge.APIOps(n))})
	}
	if len(apis) == 0 {
		return
	}
	if data, err := json.Marshal(forgeCatalogResp{Source: a.uid, APIs: apis}); err == nil {
		_ = m.Respond(data)
	}
}

// handleInvoke runs an invocation against a shared API, reusing evaluateInvoke
// for the full governance (forge-origin + default-deny-mutation + redaction +
// timeout, no per-call approval). Runs on a NATS callback goroutine, so the
// blocking forge HTTP call does not stall the actor mailbox.
func (a *ForgeShareActor) handleInvoke(apiName string, m *nats.Msg) {
	if m.Reply == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), forgedInvokeTimeout)
	defer cancel()
	specs := specsOf(a.forge.APIOps(apiName))
	res := evaluateInvoke(ctx, string(m.Data), "control", true, specs, a.config.ForgedAPIAllow, a.config.ForgedAPIBlock, a.forge, true, a.config.ForgedAPIDelegatedAuth)
	if res.Error != "" {
		slog.Warn("forge-share: invoke rejected/failed", "api", apiName, "kind", res.ErrorKind, "err", res.Error)
	}
	if data, err := json.Marshal(res); err == nil {
		_ = m.Respond(data)
	}
}

// ---- SUBSCRIBER side ----

func (a *ForgeShareActor) listRemote(paneID string) {
	a.out(paneID, fmt.Sprintf("[forge] connecting to upstream %s (workspace %s) …\n", a.config.URL, a.ws))
	if !a.ensureConn(paneID) {
		return
	}
	seen := map[string]int{}
	for _, r := range a.scatter("") {
		if r.Source == a.uid {
			continue // skip our own shares
		}
		for _, api := range r.APIs {
			if _, ok := seen[api.Name]; !ok {
				seen[api.Name] = len(api.Ops)
			}
		}
	}
	if len(seen) == 0 {
		a.out(paneID, fmt.Sprintf("[forge] no remote shared APIs found in workspace %s.\n"+
			"        Check: (1) the source ran '##forge share api <name>' and shows '✓ sharing';\n"+
			"        (2) BOTH sessions use the SAME [upstream] workspace UUID (compare the id above).\n", a.ws))
		return
	}
	var b strings.Builder
	b.WriteString("[forge] remote shared APIs:\n")
	for _, n := range sortedKeys(seen) {
		fmt.Fprintf(&b, "  %-20s %d op(s)    subscribe: ##forge subscribe %s [--scope pane|panegroup|lane|tab]\n", n, seen[n], n)
	}
	a.out(paneID, b.String())
}

func (a *ForgeShareActor) subscribe(name string, kind agentic.ScopeKind, ids agentic.ScopeIDs, paneID string, retriesLeft int, auth *runtime.AuthConfig) {
	if !a.ensureConn(paneID) {
		return
	}
	// Model B (delegated identity): build the subscriber's own TokenManager so each
	// invocation carries the subscriber's current access token. nil ⇒ Model A.
	var tm runtime.TokenManager
	if auth != nil && auth.IsTokenFlow() {
		t, err := runtime.NewTokenManager(*auth, nil)
		if err != nil {
			a.out(paneID, fmt.Sprintf("[forge] subscribe %q: invalid auth config: %v\n", name, err))
			return
		}
		tm = t
	}
	a.mu.Lock()
	_, exists := a.subs[name]
	a.mu.Unlock()
	if exists {
		a.out(paneID, fmt.Sprintf("[forge] already subscribed to %q — '##forge unsubscribe %s' first to re-scope\n", name, name))
		return
	}

	var ops []msg.ForgedOpSpec
	for _, r := range a.scatter(name) {
		if r.Source == a.uid {
			continue
		}
		for _, api := range r.APIs {
			if api.Name == name {
				ops = api.Ops
				break
			}
		}
		if ops != nil {
			break
		}
	}
	if len(ops) == 0 {
		// Bounded retry (used by startup config): the source may not have shared
		// yet, especially across two machines. Reschedule via a goroutine that
		// sends a follow-up subscribe to self. Captures only immutable identity.
		if retriesLeft > 0 {
			a.out(paneID, fmt.Sprintf("[forge] api %q not available yet; retrying in %s (%d attempt(s) left)…\n", name, forgeSubscribeRetryDelay, retriesLeft))
			selfPID, system := a.selfPID, a.system
			next := &msgForgeSubscribe{Name: name, Kind: kind, IDs: ids, PaneID: paneID, RetriesLeft: retriesLeft - 1, Auth: auth}
			go func() {
				time.Sleep(forgeSubscribeRetryDelay)
				if system != nil && selfPID != nil {
					system.Root.Send(selfPID, next)
				}
			}()
			return
		}
		a.out(paneID, fmt.Sprintf("[forge] no remote shared API named %q (try ##forge list-remote)\n", name))
		return
	}

	reg := a.scopes.RegistryFor(kind, ids)
	nc := a.nc
	ws := a.ws
	apiName := name
	var names []string
	for _, op := range ops {
		invoke := func(ctx context.Context, opn string, args json.RawMessage) (*tools.ToolOutput, error) {
			to := forgedInvokeClientTimeout
			if dl, ok := ctx.Deadline(); ok {
				if d := time.Until(dl); d > 0 && d < to {
					to = d
				}
			}
			// Model B: attach the subscriber's current access token (acquired/refreshed
			// locally). It travels out-of-band in ForgedInvokeRequest.Auth — never in
			// Args — so the owner injects it as the bearer for this call only.
			bearer := ""
			if tm != nil {
				t, terr := tm.Token(ctx)
				if terr != nil {
					return tools.ErrOutputf(tools.ErrKindPermissionDenied, "forge invoke: acquire access token: %v", terr), nil
				}
				bearer = t
			}
			call := func(b string) (msg.ForgedInvokeResult, error) {
				reqBody, _ := json.Marshal(msg.ForgedInvokeRequest{Op: opn, Args: args, Auth: b})
				reply, err := nc.Request(forgeInvokeSubject(ws, apiName), reqBody, to)
				if err != nil {
					return msg.ForgedInvokeResult{}, err
				}
				var res msg.ForgedInvokeResult
				if json.Unmarshal(reply.Data, &res) != nil {
					return msg.ForgedInvokeResult{}, fmt.Errorf("invalid forge invoke response")
				}
				return res, nil
			}
			res, err := call(bearer)
			if err != nil {
				return tools.ErrOutputf(tools.ErrKindTransient, "forge invoke failed (remote unavailable?): %v", err), nil
			}
			// Reactive refresh: if the call raced an access-token expiry (owner relayed
			// the backend's 401), refresh once and retry a single time.
			if tm != nil && bearer != "" && isAuthFailure(res.Error) {
				if nt, rerr := tm.Refresh(ctx); rerr == nil && nt != "" {
					if r2, e2 := call(nt); e2 == nil {
						res = r2
					}
				}
			}
			return &tools.ToolOutput{Content: res.Content, Error: res.Error, ErrorKind: res.ErrorKind}, nil
		}
		reg.Register(op.Name, tools.NewForgedAPIProxy(name, op.Name, op.Description, op.Schema, op.Mutating, invoke))
		names = append(names, op.Name)
	}
	a.mu.Lock()
	a.subs[name] = &forgeSubscription{reg: reg, toolNames: names, scopeStr: kind.String()}
	a.mu.Unlock()
	a.out(paneID, fmt.Sprintf("[forge] subscribed to api %q: %d op(s) at scope %s — callable in AI mode\n", name, len(names), kind.String()))
	slog.Info("forge-share: subscribed", "api", name, "ops", len(names), "scope", kind.String())
}

func (a *ForgeShareActor) unsubscribe(name, paneID string) {
	a.mu.Lock()
	s := a.subs[name]
	delete(a.subs, name)
	a.mu.Unlock()
	if s == nil {
		a.out(paneID, fmt.Sprintf("[forge] not subscribed to %q\n", name))
		return
	}
	for _, n := range s.toolNames {
		s.reg.Unregister(n)
	}
	a.out(paneID, fmt.Sprintf("[forge] unsubscribed from api %q (was scope %s)\n", name, s.scopeStr))
}

// scatter performs the discovery scatter-gather: publish a catalog request with
// a reply inbox and collect responders for forgeDiscoverWindow. Blocks the
// mailbox briefly (invoke serving runs on NATS callbacks, so it is unaffected).
func (a *ForgeShareActor) scatter(filter string) []forgeCatalogResp {
	if a.nc == nil {
		return nil
	}
	inbox := nats.NewInbox()
	sub, err := a.nc.SubscribeSync(inbox)
	if err != nil {
		return nil
	}
	defer sub.Unsubscribe()
	req, _ := json.Marshal(forgeCatalogReq{Api: filter})
	if a.nc.PublishRequest(forgeCatalogSubject(a.ws), inbox, req) != nil {
		return nil
	}
	_ = a.nc.Flush()
	var out []forgeCatalogResp
	deadline := time.Now().Add(forgeDiscoverWindow)
	for {
		rem := time.Until(deadline)
		if rem <= 0 {
			break
		}
		mm, err := sub.NextMsg(rem)
		if err != nil {
			break
		}
		var r forgeCatalogResp
		if json.Unmarshal(mm.Data, &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

func (a *ForgeShareActor) teardown() {
	a.mu.Lock()
	for _, sub := range a.shared {
		_ = sub.Unsubscribe()
	}
	a.shared = map[string]*nats.Subscription{}
	for _, s := range a.subs {
		for _, n := range s.toolNames {
			s.reg.Unregister(n)
		}
	}
	a.subs = map[string]*forgeSubscription{}
	a.mu.Unlock()
	if a.catalogSub != nil {
		_ = a.catalogSub.Unsubscribe()
		a.catalogSub = nil
	}
	if a.nc != nil {
		_ = a.nc.Drain()
		a.nc = nil
	}
}

func specsOf(ops []forge.SharedOp) []msg.ForgedOpSpec {
	out := make([]msg.ForgedOpSpec, len(ops))
	for i, o := range ops {
		out[i] = msg.ForgedOpSpec{Name: o.Name, Description: o.Description, Schema: o.Schema, Mutating: o.Mutating}
	}
	return out
}

func sortedKeys(m map[string]int) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
