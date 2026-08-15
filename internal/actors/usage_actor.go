// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bridge"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/usage"
)

// UsageActor is the cost-observability ledger (design 003). It is a child of
// WorkspaceActor. It subscribes to every pane's usage subject, aggregates
// token/cost spend by day and pane, persists day aggregates to a per-session
// KV bucket (time-gated), serves a hot-path budget-check request/reply, and
// answers snapshot requests for the ##cost command and status bar.
//
// One usage schema, multiple producers: the agentic orchestrator today; the
// governance proxy (design 001) and CI/eval runners (design 009) later — all
// publish MsgUsageRecord to rysh.pane.{paneID}.usage.
type UsageActor struct {
	sessionName   string
	pub           *msg.NATSPublisher
	nc            *nats.Conn
	br            *bridge.NATSBridge
	kv            nats.KeyValue // per-session ledger bucket; nil ⇒ in-memory only
	retentionDays int

	// byDayPane[dayKey][paneID] accumulates spend. dayKey = UTC yyyy-mm-dd.
	byDayPane map[string]map[string]*usageAgg
	// byDayAgent[dayKey][agentName] accumulates spend attributed to a named
	// autonomous agent or humanoid (design 003 "by agent" — `##cost week`).
	// A record only lands here when its producer set AgentName; regular panes
	// leave it empty, so they never appear. This is a SEPARATE rollup from
	// byDayPane (an agent's paneID equals its name, so it also appears there);
	// the session total is summed from byDayPane only, so there is no
	// double-counting across the two views.
	byDayAgent map[string]map[string]*usageAgg
	// observer, when set, receives every record for org-wide reporting
	// (design 023 §4.4). Never consulted for a local figure.
	observer UsageObserver
	// byDayTenant[dayKey][tenant] accumulates spend attributed to a customer
	// (design 022 §4.3). Same shape and same reasoning as byDayAgent: it is a
	// SECOND INDEX over the same records, not a second stream of them. The
	// session total is summed from byDayPane only, so tenant accounting cannot
	// double-count `##cost` — which is exactly what emitting a second record
	// under a synthetic tenant id would have done.
	byDayTenant map[string]map[string]*usageAgg
	// ceilings[paneID] is the pane's hard token ceiling (0 ⇒ none).
	ceilings map[string]int64
	// tenantCeilings[tenant] is the customer's hard token ceiling (0 ⇒ none).
	tenantCeilings map[string]int64
	// lastPersist gates KV writes to ≤1 / 2s per persisted key.
	lastPersist map[string]time.Time
}

// usageAgg is the in-memory rollup persisted per (day, pane).
type usageAgg struct {
	In         int64 `json:"in"`
	Out        int64 `json:"out"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Cost       int64 `json:"cost_micro_usd"`
	Calls      int64 `json:"calls"`
	Unknown    bool  `json:"unknown,omitempty"`
}

func (a *usageAgg) tokens() int64 { return a.In + a.Out + a.CacheRead + a.CacheWrite }

// NewUsageActor constructs the ledger actor. The KV bucket is opened lazily in
// the Started handler; if JetStream/KV is unavailable the ledger runs
// in-memory (no persistence) rather than failing.
func NewUsageActor(sessionName string, pub *msg.NATSPublisher, nc *nats.Conn) *UsageActor {
	if sessionName == "" {
		sessionName = "default"
	}
	return &UsageActor{
		sessionName:    sessionName,
		pub:            pub,
		nc:             nc,
		retentionDays:  90,
		byDayPane:      map[string]map[string]*usageAgg{},
		byDayAgent:     map[string]map[string]*usageAgg{},
		byDayTenant:    map[string]map[string]*usageAgg{},
		ceilings:       map[string]int64{},
		tenantCeilings: map[string]int64{},
		lastPersist:    map[string]time.Time{},
	}
}

// SetTenantCeilings pre-seeds per-tenant token ceilings (design 022 §4.3),
// from [proxy] tenants and from policy keys of the form "tenant:<name>".
// Called before the actor starts, so it races nothing.
func (u *UsageActor) SetTenantCeilings(c map[string]int64) {
	for k, v := range c {
		if v <= 0 {
			continue
		}
		// LOWER WINS, not last-writer-wins. Two sources seed this map —
		// [proxy] tenants and policy's "tenant:<name>" keys — and policy's own
		// Merge already resolves a conflict between org and project files by
		// taking the lower ceiling. Overwriting here would make the ORDER of two
		// calls in workspace.go decide a customer's budget, and a looser number
		// arriving second would silently raise a cap policy had lowered.
		if cur, ok := u.tenantCeilings[k]; ok && cur > 0 && cur < v {
			continue
		}
		u.tenantCeilings[k] = v
	}
}

// SetRetentionDays overrides the default 90-day retention window.
func (u *UsageActor) SetRetentionDays(d int) {
	if d > 0 {
		u.retentionDays = d
	}
}

// SetCeilings pre-seeds per-pane token ceilings (e.g. from policy-as-code,
// design 013). Called before the actor starts, so it races nothing.
func (u *UsageActor) SetCeilings(c map[string]int64) {
	for k, v := range c {
		if v > 0 {
			u.ceilings[k] = v
		}
	}
}

func (u *UsageActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		u.openKV()
		u.restoreFromKV()
		u.pruneOld()
		u.br = bridge.New(u.nc, ctx.Self(), ctx.ActorSystem(), u.pub.Codecs())
		for _, subj := range []string{
			msg.UsageWildcardSubject(),
			msg.UsageCheckSubject(),
			msg.UsageInboxSubject(),
		} {
			if err := u.br.AddSubject(subj); err != nil {
				slog.Error("usage: subscribe failed", "subject", subj, "err", err)
			}
		}

	case *actor.Stopping:
		u.flushAll()
		if u.br != nil {
			u.br.Stop()
			u.br = nil
		}

	case *msg.MsgUsageRecord:
		u.ingest(m)

	case *msg.MsgUsageBudgetSet:
		if m.CeilingTokens <= 0 {
			delete(u.ceilings, m.PaneID)
		} else {
			u.ceilings[m.PaneID] = m.CeilingTokens
		}
		u.persistCeilings()

	case *msg.RequestEnvelope:
		switch inner := m.Inner.(type) {
		case *msg.MsgUsageCheck:
			_ = m.Reply(u.checkBudget(inner.PaneID, inner.Tenant))
		case *msg.MsgUsageSnapshotRequest:
			_ = m.Reply(u.snapshot(inner.Window))
		}
	}
}

// ---------------------------------------------------------------------------
// Ingest & aggregation
// ---------------------------------------------------------------------------

func dayKeyOf(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Format("2006-01-02")
}

func (u *UsageActor) ingest(rec *msg.MsgUsageRecord) {
	if rec == nil || rec.PaneID == "" {
		return
	}
	day := dayKeyOf(rec.TS)

	// Price the record once. A producer that already priced it (e.g. the proxy)
	// wins; otherwise use the pricing table. Unknown model ⇒ flagged, cost 0.
	cost := rec.CostMicroUSD
	known := cost > 0
	if cost == 0 {
		if c, ok := usage.CostMicroUSD(rec.Model, rec.InTokens, rec.OutTokens, rec.CacheRead, rec.CacheWrite); ok {
			cost, known = c, true
		}
	}

	// By pane (every record).
	paneAgg := aggFor(u.byDayPane, day, rec.PaneID)
	addToAgg(paneAgg, rec, cost, known)
	u.persistAgg(aggKVKey(day, rec.PaneID), paneAgg)

	// By agent (only records a producer attributed to a named agent/humanoid).
	if rec.AgentName != "" {
		agentAgg := aggFor(u.byDayAgent, day, rec.AgentName)
		addToAgg(agentAgg, rec, cost, known)
		u.persistAgg(agentAggKVKey(day, rec.AgentName), agentAgg)
	}

	// By tenant (design 022 §4.3) — the same record, indexed a second way.
	if rec.Tenant != "" {
		tenantAgg := aggFor(u.byDayTenant, day, rec.Tenant)
		addToAgg(tenantAgg, rec, cost, known)
		u.persistAgg(tenantAggKVKey(day, rec.Tenant), tenantAgg)
	}

	// The org-wide observer (design 023 §4.4). It batches the SAME record to
	// the server; it is additive and never authoritative, so `##cost` stays a
	// local, offline-capable view whether or not governance is on. Cost is
	// passed as priced here, so the server is not asked to re-derive it from a
	// pricing table it does not have.
	if u.observer != nil {
		priced := *rec
		priced.CostMicroUSD = cost
		u.observer.Record(priced)
	}
}

// UsageObserver is a side-channel consumer of the same records the ledger
// aggregates (design 023 §4.4). It exists so the shared-ledger client can be
// fed without UsageActor knowing anything about the network — the observer is
// expected to batch and never to block.
type UsageObserver interface {
	Record(rec msg.MsgUsageRecord)
}

// SetUsageObserver installs the org-wide reporter. Called before the actor
// starts, so it races nothing. nil ⇒ local-only, which is the default.
func (u *UsageActor) SetUsageObserver(o UsageObserver) { u.observer = o }

// aggFor returns (creating as needed) the aggregate for one (day, key) in a
// byDay map — shared by the pane and agent rollups.
func aggFor(byDay map[string]map[string]*usageAgg, day, key string) *usageAgg {
	byKey := byDay[day]
	if byKey == nil {
		byKey = map[string]*usageAgg{}
		byDay[day] = byKey
	}
	agg := byKey[key]
	if agg == nil {
		agg = &usageAgg{}
		byKey[key] = agg
	}
	return agg
}

// addToAgg folds one record's (pre-priced) usage into an aggregate.
func addToAgg(agg *usageAgg, rec *msg.MsgUsageRecord, cost int64, known bool) {
	agg.In += int64(rec.InTokens)
	agg.Out += int64(rec.OutTokens)
	agg.CacheRead += int64(rec.CacheRead)
	agg.CacheWrite += int64(rec.CacheWrite)
	agg.Cost += cost
	agg.Calls++
	if !known && (rec.InTokens > 0 || rec.OutTokens > 0) {
		agg.Unknown = true
	}
}

// spentToday returns the total tokens a pane has consumed today (budget window).
func (u *UsageActor) spentToday(paneID string) int64 {
	if panes := u.byDayPane[dayKeyOf(time.Now())]; panes != nil {
		if agg := panes[paneID]; agg != nil {
			return agg.tokens()
		}
	}
	return 0
}

// spentTodayTenant is the tenant equivalent of spentToday, read from the second
// index rather than by re-summing panes.
func (u *UsageActor) spentTodayTenant(tenant string) int64 {
	if tenants := u.byDayTenant[dayKeyOf(time.Now())]; tenants != nil {
		if agg := tenants[tenant]; agg != nil {
			return agg.tokens()
		}
	}
	return 0
}

// checkBudget answers the hot-path budget query.
//
// When a tenant is named, BOTH ceilings are evaluated and the refusal wins: a
// pane with headroom under a customer that is out of budget must still be
// stopped, or per-tenant caps would be trivially escaped by opening a new pane.
// The reply says which one bound, so the message names the right budget.
func (u *UsageActor) checkBudget(paneID, tenant string) *msg.MsgUsageCheckReply {
	ceiling := u.ceilings[paneID]
	spent := u.spentToday(paneID)
	paneOK := ceiling == 0 || spent < ceiling

	if tenant != "" {
		tCeiling := u.tenantCeilings[tenant]
		tSpent := u.spentTodayTenant(tenant)
		if tCeiling > 0 && tSpent >= tCeiling {
			return &msg.MsgUsageCheckReply{
				PaneID:        paneID,
				SpentTokens:   tSpent,
				CeilingTokens: tCeiling,
				Ok:            false,
				Scope:         msg.UsageScopeTenant,
				Tenant:        tenant,
			}
		}
	}

	return &msg.MsgUsageCheckReply{
		PaneID:        paneID,
		SpentTokens:   spent,
		CeilingTokens: ceiling,
		Ok:            paneOK,
		Scope:         msg.UsageScopePane,
	}
}

// snapshot builds the aggregates for a window ("today" | "week").
func (u *UsageActor) snapshot(window string) *msg.MsgUsageSnapshotReply {
	if window == "" {
		window = "today"
	}
	days := u.windowDays(window)

	byPane := mergeWindow(u.byDayPane, days)
	byAgent := mergeWindow(u.byDayAgent, days)

	// Session totals come from the pane rollup ONLY. The agent rollup re-counts
	// the same records under a different key, so summing both would double-count.
	var totalCost, totalTokens int64
	for _, a := range byPane {
		totalCost += a.CostMicroUSD
		totalTokens += a.InTokens + a.OutTokens + a.CacheRead + a.CacheWrite
	}

	var ceilings []msg.UsageCeiling
	for paneID, c := range u.ceilings {
		ceilings = append(ceilings, msg.UsageCeiling{
			PaneID:        paneID,
			CeilingTokens: c,
			SpentTokens:   u.spentToday(paneID),
		})
	}
	sort.Slice(ceilings, func(i, j int) bool { return ceilings[i].PaneID < ceilings[j].PaneID })

	return &msg.MsgUsageSnapshotReply{
		Window:              window,
		SessionCostMicroUSD: totalCost,
		SessionTokens:       totalTokens,
		ByPane:              byPane,
		ByAgent:             byAgent,
		Ceilings:            ceilings,
	}
}

// mergeWindow rolls up a byDay map over the window's days into a sorted
// []UsageAgg (most-expensive first). Shared by the pane and agent views.
func mergeWindow(byDay map[string]map[string]*usageAgg, days []string) []msg.UsageAgg {
	merged := map[string]*usageAgg{}
	for _, day := range days {
		for key, agg := range byDay[day] {
			m := merged[key]
			if m == nil {
				m = &usageAgg{}
				merged[key] = m
			}
			m.In += agg.In
			m.Out += agg.Out
			m.CacheRead += agg.CacheRead
			m.CacheWrite += agg.CacheWrite
			m.Cost += agg.Cost
			m.Calls += agg.Calls
			m.Unknown = m.Unknown || agg.Unknown
		}
	}
	out := make([]msg.UsageAgg, 0, len(merged))
	for key, agg := range merged {
		out = append(out, msg.UsageAgg{
			Key:          key,
			InTokens:     agg.In,
			OutTokens:    agg.Out,
			CacheRead:    agg.CacheRead,
			CacheWrite:   agg.CacheWrite,
			CostMicroUSD: agg.Cost,
			Calls:        agg.Calls,
			UnknownCost:  agg.Unknown,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CostMicroUSD != out[j].CostMicroUSD {
			return out[i].CostMicroUSD > out[j].CostMicroUSD
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// windowDays returns the UTC day keys covered by the window, newest first.
func (u *UsageActor) windowDays(window string) []string {
	now := time.Now().UTC()
	n := 1
	if window == "week" {
		n = 7
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, now.AddDate(0, 0, -i).Format("2006-01-02"))
	}
	return out
}

// ---------------------------------------------------------------------------
// KV persistence (self-contained bucket; degrades to in-memory)
// ---------------------------------------------------------------------------

func (u *UsageActor) openKV() {
	if u.nc == nil {
		return
	}
	js, err := u.nc.JetStream()
	if err != nil {
		slog.Warn("usage: JetStream unavailable, ledger in-memory only", "err", err)
		return
	}
	bucket := fmt.Sprintf("rysh-usage-%s", u.sessionName)
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: bucket, Storage: nats.FileStorage})
	if err != nil {
		if kv, err = js.KeyValue(bucket); err != nil {
			slog.Warn("usage: KV unavailable, ledger in-memory only", "bucket", bucket, "err", err)
			return
		}
	}
	u.kv = kv
}

func aggKVKey(day, paneID string) string     { return "agg/" + day + "/" + paneID }
func agentAggKVKey(day, name string) string  { return "agentagg/" + day + "/" + name }
func tenantAggKVKey(day, name string) string { return "tenantagg/" + day + "/" + name }

// persistAgg writes one aggregate under kvKey, time-gated to ≤1 write / 2s per
// key. Used for both pane ("agg/…") and agent ("agentagg/…") rollups.
func (u *UsageActor) persistAgg(kvKey string, agg *usageAgg) {
	if u.kv == nil {
		return
	}
	if last, ok := u.lastPersist[kvKey]; ok && time.Since(last) < 2*time.Second {
		return
	}
	data, err := json.Marshal(agg)
	if err != nil {
		return
	}
	if _, err := u.kv.Put(kvKey, data); err != nil {
		return
	}
	u.lastPersist[kvKey] = time.Now()
}

func (u *UsageActor) persistCeilings() {
	if u.kv == nil {
		return
	}
	data, err := json.Marshal(u.ceilings)
	if err != nil {
		return
	}
	_, _ = u.kv.Put("ceilings", data)
}

// flushAll writes every in-memory aggregate on shutdown, ignoring the time gate.
func (u *UsageActor) flushAll() {
	if u.kv == nil {
		return
	}
	for day, panes := range u.byDayPane {
		for paneID, agg := range panes {
			if data, err := json.Marshal(agg); err == nil {
				_, _ = u.kv.Put(aggKVKey(day, paneID), data)
			}
		}
	}
	for day, agents := range u.byDayAgent {
		for name, agg := range agents {
			if data, err := json.Marshal(agg); err == nil {
				_, _ = u.kv.Put(agentAggKVKey(day, name), data)
			}
		}
	}
	for day, tenants := range u.byDayTenant {
		for name, agg := range tenants {
			if data, err := json.Marshal(agg); err == nil {
				_, _ = u.kv.Put(tenantAggKVKey(day, name), data)
			}
		}
	}
	u.persistCeilings()
}

func (u *UsageActor) restoreFromKV() {
	if u.kv == nil {
		return
	}
	keys, err := u.kv.Keys()
	if err != nil {
		return // ErrNoKeysFound on a fresh bucket — nothing to restore.
	}
	for _, key := range keys {
		entry, err := u.kv.Get(key)
		if err != nil {
			continue
		}
		if key == "ceilings" {
			_ = json.Unmarshal(entry.Value(), &u.ceilings)
			continue
		}
		// "agentagg/" must be checked before "agg/" (it is not a prefix of it,
		// but keeping them explicit avoids a future refactor mixing them up).
		if strings.HasPrefix(key, "agentagg/") {
			restoreAgg(u.byDayAgent, strings.TrimPrefix(key, "agentagg/"), entry.Value())
			continue
		}
		if strings.HasPrefix(key, "tenantagg/") {
			restoreAgg(u.byDayTenant, strings.TrimPrefix(key, "tenantagg/"), entry.Value())
			continue
		}
		if strings.HasPrefix(key, "agg/") {
			restoreAgg(u.byDayPane, strings.TrimPrefix(key, "agg/"), entry.Value())
			continue
		}
	}
}

// restoreAgg parses a "{day}/{key}" suffix and its JSON value into byDay.
func restoreAgg(byDay map[string]map[string]*usageAgg, suffix string, data []byte) {
	parts := strings.SplitN(suffix, "/", 2)
	if len(parts) != 2 {
		return
	}
	var agg usageAgg
	if err := json.Unmarshal(data, &agg); err != nil {
		return
	}
	*aggFor(byDay, parts[0], parts[1]) = agg
}

// pruneOld drops in-memory and KV aggregates older than the retention window.
func (u *UsageActor) pruneOld() {
	cutoff := time.Now().UTC().AddDate(0, 0, -u.retentionDays).Format("2006-01-02")
	for day := range u.byDayPane {
		if day < cutoff {
			if u.kv != nil {
				for paneID := range u.byDayPane[day] {
					_ = u.kv.Delete(aggKVKey(day, paneID))
				}
			}
			delete(u.byDayPane, day)
		}
	}
	for day := range u.byDayAgent {
		if day < cutoff {
			if u.kv != nil {
				for name := range u.byDayAgent[day] {
					_ = u.kv.Delete(agentAggKVKey(day, name))
				}
			}
			delete(u.byDayAgent, day)
		}
	}
	for day := range u.byDayTenant {
		if day < cutoff {
			if u.kv != nil {
				for name := range u.byDayTenant[day] {
					_ = u.kv.Delete(tenantAggKVKey(day, name))
				}
			}
			delete(u.byDayTenant, day)
		}
	}
}
