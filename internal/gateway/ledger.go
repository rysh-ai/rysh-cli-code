// Package gateway is the daemon half of the LLM gateway's server-side control
// plane (design 023): it reports governed spend to rysh-server, leases a slice
// of an org-wide allowance and enforces locally against that slice, and pulls
// central policy.
//
// THE ONE RULE THIS PACKAGE EXISTS TO OBEY
// ----------------------------------------
// The hot path never leaves the process. A per-request WAN call would put
// provider-latency-scale delay in front of every LLM request and turn a network
// blip into what looks like a provider outage (023 §4.2/G4). So:
//
//   - Allow() reads local numbers only. No I/O, no locks held across a call, no
//     blocking. Ever.
//   - Everything that talks to the network — usage flushes, lease renewals,
//     policy polls — runs on this package's own goroutine, off the request path.
//
// INERT BY DEFAULT
// ----------------
// Nothing here does anything unless BOTH `upstream.enabled` and the explicit
// `upstream.governance` opt-in are set. Enabled() is checked at every entry
// point, and TestLedgerClient_DisabledMakesNoNetworkCalls holds the line.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Flush thresholds (design 023 §4.4): every 10 s or 256 metered calls,
// whichever comes first, and on shutdown.
const (
	FlushInterval  = 10 * time.Second
	FlushMaxCalls  = 256
	httpTimeout    = 5 * time.Second
	policyPollFreq = 60 * time.Second
)

// Partition modes (design 023 §4.3).
const (
	PartitionLocal  = "local"  // fall back to the machine-local ceiling (default)
	PartitionStrict = "strict" // refuse
	PartitionOpen   = "open"   // allow
)

// rollupKey is the (tenant, day) the server table is keyed on.
type rollupKey struct {
	tenant string
	date   string
}

// lease is one outstanding grant for a scope.
type lease struct {
	granted   int64 // tokens the server handed out
	spent     int64 // tokens consumed against this grant
	expires   time.Time
	remaining int64 // the scope's remaining allowance as of the grant, for sizing
	ceiling   int64 // 0 ⇒ unlimited
	// exhausted marks a scope the SERVER said is out of budget (granted 0).
	// That is a decision, not a partition, so it is not softened by the
	// partition mode.
	exhausted bool
	// spentSinceLease accumulates spend to reconcile on the next lease call
	// (023 §4.2: the renewal round trip carries it, so reconciliation costs no
	// extra request).
	spentSince int64
	renewing   bool
	// everGranted distinguishes "we have talked to the server about this scope"
	// from "we never have" — the first start / offline row of §4.3's matrix.
	everGranted bool
}

// LedgerClient owns all four flows in design 023 §4.1.
type LedgerClient struct {
	cfg      config.UpstreamConfig
	daemonID string
	pub      *msg.NATSPublisher

	// httpDo, publish and now are injectable so tests need neither a server, a
	// broker, nor a sleep.
	httpDo  func(*http.Request) (*http.Response, error)
	publish func(*sharedmsg.MsgGatewayUsageBatch) error
	now     func() time.Time

	mu      sync.Mutex
	pending map[rollupKey]*sharedmsg.GatewayUsageRollup
	calls   int64
	seq     int64
	leases  map[string]*lease

	// policy is the last-known-good central document (023 §4.7).
	policyETag string
	policyBody []byte
	policyAt   time.Time

	// policyHook, when set, is called with the body of a CHANGED central
	// policy document. It runs on this package's goroutine, so the callback
	// must not touch actor state directly — it is expected to hand the work to
	// the owning actor's mailbox.
	policyHook func([]byte)

	stop chan struct{}
	done chan struct{}
}

// SetPolicyHook installs the change callback. Call before Start.
func (c *LedgerClient) SetPolicyHook(fn func([]byte)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.policyHook = fn
	c.mu.Unlock()
}

// New builds a client. It is safe to construct one even when governance is
// off — every method is a no-op then, which keeps the call sites free of
// conditionals.
func New(cfg config.UpstreamConfig, daemonID string, pub *msg.NATSPublisher) *LedgerClient {
	c := &LedgerClient{
		cfg:      cfg,
		daemonID: daemonID,
		pub:      pub,
		httpDo:   (&http.Client{Timeout: httpTimeout}).Do,
		now:      time.Now,
		pending:  map[rollupKey]*sharedmsg.GatewayUsageRollup{},
		leases:   map[string]*lease{},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	c.publish = func(b *sharedmsg.MsgGatewayUsageBatch) error {
		if c.pub == nil {
			return errNoPublisher
		}
		return c.pub.SendGatewayUsage(b)
	}
	return c
}

// errNoPublisher keeps a batch pending rather than dropping it when the daemon
// has no broker connection yet — the same path a publish failure takes.
var errNoPublisher = errors.New("gateway: no NATS publisher")

// Enabled reports whether the control plane is active. Both switches must be
// on AND the connection details must be present: a half-configured upstream
// must not produce a client that fails a request per flush.
func (c *LedgerClient) Enabled() bool {
	if c == nil || !c.cfg.Enabled || !c.cfg.Governance {
		return false
	}
	return c.cfg.URL != "" && c.cfg.APIKey != "" && c.cfg.Workspace != ""
}

// partitionMode is the configured degradation, defaulting to local. An
// unrecognised value reads as local rather than as open: a typo in a config
// file must never be the thing that lifts a ceiling.
func (c *LedgerClient) partitionMode() string {
	switch strings.ToLower(strings.TrimSpace(c.cfg.GovernanceOnPartition)) {
	case PartitionStrict:
		return PartitionStrict
	case PartitionOpen:
		return PartitionOpen
	default:
		return PartitionLocal
	}
}

// Start runs the background loop: usage flushes, lease renewals, policy polls.
// It returns immediately, and is a no-op when governance is off.
func (c *LedgerClient) Start() {
	if !c.Enabled() {
		close(c.done)
		return
	}
	go func() {
		// Pull once immediately: waiting a full poll interval would leave a
		// daemon running under no central policy for the first minute of its
		// life, which is exactly when a fresh machine most needs one.
		c.PullPolicy(context.Background())
	}()
	go c.loop()
}

// Stop flushes what is pending and shuts the loop down. Safe to call twice.
func (c *LedgerClient) Stop() {
	if c == nil {
		return
	}
	select {
	case <-c.stop:
		return // already stopping
	default:
		close(c.stop)
	}
	<-c.done
}

func (c *LedgerClient) loop() {
	defer close(c.done)
	flush := time.NewTicker(FlushInterval)
	defer flush.Stop()
	poll := time.NewTicker(policyPollFreq)
	defer poll.Stop()
	for {
		select {
		case <-c.stop:
			// A daemon shutting down still owes the server its last numbers.
			c.Flush()
			return
		case <-flush.C:
			c.Flush()
		case <-poll.C:
			c.PullPolicy(context.Background())
		}
	}
}

// ---- S1: usage reporting -------------------------------------------------

// Record accumulates one metered call. It is called from the usage path, so it
// does nothing but arithmetic under a short lock.
//
// The daemon keeps its own rollups regardless (023 §4.4): `##cost` stays a
// local, offline-capable view and this is a second, additive observer — never
// a replacement, and never a second source of truth for the local ledger.
func (c *LedgerClient) Record(rec msg.MsgUsageRecord) {
	if !c.Enabled() {
		return
	}
	ts := rec.TS
	if ts.IsZero() {
		ts = c.now()
	}
	key := rollupKey{tenant: rec.Tenant, date: ts.UTC().Format("2006-01-02")}

	c.mu.Lock()
	r, ok := c.pending[key]
	if !ok {
		r = &sharedmsg.GatewayUsageRollup{Tenant: key.tenant, UsageDate: key.date}
		c.pending[key] = r
	}
	r.InTokens += int64(rec.InTokens)
	r.OutTokens += int64(rec.OutTokens)
	r.CacheRead += int64(rec.CacheRead)
	r.CacheWrite += int64(rec.CacheWrite)
	r.CostMicroUSD += rec.CostMicroUSD
	r.Calls++
	c.calls++
	due := c.calls >= FlushMaxCalls
	c.mu.Unlock()

	if due {
		// Off the caller's goroutine: Record sits on the usage path, and a
		// publish is not something the metering path should ever wait on.
		go c.Flush()
	}
}

// Flush publishes the pending rollups as one batch.
//
// On a publish failure the rollups are put BACK and the sequence number is not
// consumed, so nothing is lost and the retry is byte-identical — which is
// exactly what the server's (daemon_id, seq) seen-set is built to absorb.
func (c *LedgerClient) Flush() {
	if !c.Enabled() {
		return
	}
	c.mu.Lock()
	if len(c.pending) == 0 {
		c.mu.Unlock()
		return
	}
	rollups := make([]sharedmsg.GatewayUsageRollup, 0, len(c.pending))
	for _, r := range c.pending {
		rollups = append(rollups, *r)
	}
	c.seq++
	seq := c.seq
	pending := c.pending
	c.pending = map[rollupKey]*sharedmsg.GatewayUsageRollup{}
	c.calls = 0
	c.mu.Unlock()

	batch := &sharedmsg.MsgGatewayUsageBatch{
		DaemonID: c.daemonID, Seq: seq, WorkspaceID: c.cfg.Workspace,
		Rollups: rollups, SentAt: c.now(),
	}
	if err := c.publish(batch); err != nil {
		slog.Warn("gateway: usage batch publish failed, keeping it for the next flush",
			"seq", seq, "err", err)
		c.mu.Lock()
		for k, r := range pending {
			if cur, ok := c.pending[k]; ok {
				cur.InTokens += r.InTokens
				cur.OutTokens += r.OutTokens
				cur.CacheRead += r.CacheRead
				cur.CacheWrite += r.CacheWrite
				cur.CostMicroUSD += r.CostMicroUSD
				cur.Calls += r.Calls
				continue
			}
			c.pending[k] = r
		}
		c.seq-- // the batch never went out; do not burn the sequence number
		c.mu.Unlock()
	}
}

// ---- S2: leases ----------------------------------------------------------

// Verdicts explain why Allow answered as it did — for the log line, and for
// tests that must tell "out of budget" apart from "partitioned". Plain strings
// so LedgerClient satisfies proxy.LeaseGate without an adapter.
const (
	VerdictNoGovernance = "no_governance" // feature off
	VerdictLeased       = "leased"        // spending inside a valid lease
	VerdictExhausted    = "exhausted"     // the SERVER says the scope is out of budget
	VerdictLeaseSpent   = "lease_spent"   // local slice used up, renewal not back yet
	VerdictPartition    = "partition"     // no usable lease; partition mode decided
)

// Allow reports whether a tenant may spend right now.
//
// IN-PROCESS AND NON-BLOCKING, ALWAYS. It reads a local number and may kick a
// renewal off on another goroutine; it never waits for one. This is the
// property design 023 §7's first risk row is about.
func (c *LedgerClient) Allow(tenant string) (bool, string) {
	if !c.Enabled() {
		return true, VerdictNoGovernance
	}
	scope := sharedmsg.TenantScope(tenant)
	now := c.now()

	c.mu.Lock()
	l, ok := c.leases[scope]
	if !ok {
		l = &lease{}
		c.leases[scope] = l
	}
	valid := ok && l.everGranted && now.Before(l.expires)
	remaining := l.granted - l.spent
	exhausted := l.exhausted
	needRenew := c.needsRenewLocked(l, now)
	c.mu.Unlock()

	if needRenew {
		go c.renew(scope)
	}

	switch {
	case exhausted:
		// The server answered "0". Not a partition — a decision.
		return false, VerdictExhausted
	case valid && remaining > 0:
		return true, VerdictLeased
	case valid:
		// Lease still in date but spent out; the renewal above will extend it.
		return false, VerdictLeaseSpent
	default:
		// No lease, or an expired one: §4.3's partition row. `local` — the
		// default — means the machine-local ceiling (which the caller has
		// already applied) governs, so this returns allow.
		return c.partitionMode() != PartitionStrict, VerdictPartition
	}
}

// Spend records tokens against the current lease. Pure arithmetic; the renewal
// it may trigger happens on the next Allow.
func (c *LedgerClient) Spend(tenant string, tokens int64) {
	if !c.Enabled() || tokens <= 0 {
		return
	}
	scope := sharedmsg.TenantScope(tenant)
	c.mu.Lock()
	l, ok := c.leases[scope]
	if !ok {
		l = &lease{}
		c.leases[scope] = l
	}
	l.spent += tokens
	l.spentSince += tokens
	c.mu.Unlock()
}

// needsRenewLocked implements 023 §4.2's renewal trigger: at 20% of the lease
// remaining, or 60 s before expiry, whichever comes first. Also true when there
// is no lease at all, which is how the first one is acquired.
func (c *LedgerClient) needsRenewLocked(l *lease, now time.Time) bool {
	if l.renewing || l.exhausted {
		return false
	}
	if !l.everGranted || now.After(l.expires) {
		return true
	}
	if remaining := l.granted - l.spent; l.granted > 0 &&
		float64(remaining) <= float64(l.granted)*sharedmsg.GatewayLeaseRenewFraction {
		return true
	}
	return now.Add(sharedmsg.GatewayLeaseRenewBefore).After(l.expires)
}

// wantTokens is the ask: 10% of what the scope has left, floored and ceilinged
// (023 §4.2). The floor stops a nearly-exhausted ceiling from degrading into
// one lease call per request; the ceiling is what bounds the whole model's
// worst-case overspend.
func wantTokens(remaining int64) int64 {
	want := int64(float64(remaining) * sharedmsg.GatewayLeaseWantFraction)
	if want < sharedmsg.GatewayLeaseMinTokens {
		want = sharedmsg.GatewayLeaseMinTokens
	}
	if want > sharedmsg.GatewayLeaseMaxTokens {
		want = sharedmsg.GatewayLeaseMaxTokens
	}
	return want
}

// renew acquires or extends the lease for one scope. Runs off the request path.
func (c *LedgerClient) renew(scope string) {
	c.mu.Lock()
	l, ok := c.leases[scope]
	if !ok || l.renewing {
		c.mu.Unlock()
		return
	}
	l.renewing = true
	spentSince := l.spentSince
	remaining := l.remaining
	c.mu.Unlock()

	reply, err := c.acquireLease(context.Background(), scope, wantTokens(remaining), spentSince)

	c.mu.Lock()
	defer c.mu.Unlock()
	l.renewing = false
	if err != nil {
		// Leave the existing lease exactly as it is: §4.3's "unreachable, lease
		// valid" row is that spending CONTINUES against the slice already held.
		slog.Warn("gateway: lease renewal failed, spending against the existing lease",
			"scope", scope, "err", err)
		return
	}
	// Deduct exactly what was reported, never zero the counter: a request that
	// metered WHILE the renewal was in flight adds to spentSince, and blanking
	// it would silently forgive that spend — the ceiling would then leak by a
	// little on every renewal, which over a day is not a little.
	l.spentSince -= spentSince
	if l.spentSince < 0 {
		l.spentSince = 0
	}
	l.everGranted = true
	l.remaining = reply.RemainingToday
	l.ceiling = reply.CeilingTokens
	if reply.GrantedTokens <= 0 {
		l.exhausted = true
		// spentSince is deliberately left carrying: the next successful lease
		// call reconciles it.
		l.granted, l.spent = 0, 0
		slog.Warn("gateway: scope is out of budget upstream", "scope", scope,
			"ceiling", reply.CeilingTokens)
		return
	}
	l.exhausted = false
	l.granted = reply.GrantedTokens
	// The new slice starts already drawn down by whatever was spent but not yet
	// reconciled, so in-flight spend is charged once — here — rather than
	// disappearing between two grants.
	l.spent = l.spentSince
	l.expires = reply.ExpiresAt
}

// acquireLease is the HTTP round trip (023 §4.2). HTTP, not a NATS publish,
// because it needs a synchronous answer and a real error — a lost publish would
// silently become "no lease".
func (c *LedgerClient) acquireLease(
	ctx context.Context, scope string, want, spentSince int64,
) (sharedmsg.GatewayLeaseReply, error) {
	var out sharedmsg.GatewayLeaseReply
	body, err := json.Marshal(sharedmsg.GatewayLeaseRequest{
		Scope: scope, WantTokens: want, DaemonID: c.daemonID, SpentSince: spentSince,
	})
	if err != nil {
		return out, err
	}
	url := strings.TrimRight(c.cfg.URL, "/") +
		"/api/workspaces/" + c.cfg.Workspace + "/gateway/lease"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	c.authorize(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpDo(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("lease: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("lease: decode: %w", err)
	}
	return out, nil
}

// authorize applies the workspace API key. No new authentication story: the key
// is already workspace-scoped and the server's EnforceKeyScope already refuses
// a key operating outside its own workspace (023 §4.1).
func (c *LedgerClient) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("X-API-Key", c.cfg.APIKey)
}

// ---- S3: policy distribution ---------------------------------------------

// PullPolicy fetches the central policy document, using If-None-Match so an
// unchanged document costs a 304 (023 §4.7).
//
// It returns the body when it CHANGED, nil otherwise. An unreachable server or
// an error response leaves the last-known-good document in place: an expired
// cache must not become an escape hatch around a policy (013's fail-closed rule
// applies to a central document exactly as it does to a local one).
func (c *LedgerClient) PullPolicy(ctx context.Context) []byte {
	if !c.Enabled() {
		return nil
	}
	url := strings.TrimRight(c.cfg.URL, "/") +
		"/api/workspaces/" + c.cfg.Workspace + "/policy"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	c.authorize(req)
	c.mu.Lock()
	etag := c.policyETag
	c.mu.Unlock()
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := c.httpDo(req)
	if err != nil {
		slog.Warn("gateway: policy pull failed, keeping the last known good document", "err", err)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotModified {
		c.mu.Lock()
		c.policyAt = c.now()
		c.mu.Unlock()
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		slog.Warn("gateway: policy pull returned an error, keeping the last known good document",
			"status", resp.StatusCode)
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil
	}
	c.mu.Lock()
	c.policyETag = resp.Header.Get("ETag")
	c.policyBody = body
	c.policyAt = c.now()
	hook := c.policyHook
	c.mu.Unlock()
	if hook != nil {
		hook(body)
	}
	return body
}

// Policy returns the last-known-good central document and when it was
// refreshed. Empty when none has ever been fetched.
func (c *LedgerClient) Policy() ([]byte, time.Time) {
	if c == nil {
		return nil, time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.policyBody, c.policyAt
}

// ---- introspection -------------------------------------------------------

// LeaseState is a snapshot for `##proxy status` / tests. Values only, no
// pointers into the client's state.
type LeaseState struct {
	Scope     string
	Granted   int64
	Spent     int64
	Expires   time.Time
	Ceiling   int64
	Remaining int64
	Exhausted bool
}

// Leases snapshots every scope the daemon holds a lease for.
func (c *LedgerClient) Leases() []LeaseState {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]LeaseState, 0, len(c.leases))
	for scope, l := range c.leases {
		out = append(out, LeaseState{
			Scope: scope, Granted: l.granted, Spent: l.spent, Expires: l.expires,
			Ceiling: l.ceiling, Remaining: l.remaining, Exhausted: l.exhausted,
		})
	}
	return out
}
