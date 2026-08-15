// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Design 023 §6's "`##cost` unchanged" regression, one layer up from the one
// 022 §4.3 guards.
//
// The org-wide reporter is an OBSERVER of the record stream, never a
// participant in it. If it could change a local figure — by mutating the record,
// by being counted twice, or by failing loudly enough to skip a rollup — then
// turning on `upstream.governance` would quietly change what `##cost` says, and
// the local ledger would stop being the offline-capable source of truth it is
// documented to be.

type recordingObserver struct {
	seen  []msg.MsgUsageRecord
	panic bool
}

func (o *recordingObserver) Record(rec msg.MsgUsageRecord) {
	if o.panic {
		panic("an observer must never be able to take the ledger down")
	}
	o.seen = append(o.seen, rec)
}

func TestUsageObserver_LocalFiguresAreIdentical(t *testing.T) {
	now := time.Now()
	records := []*msg.MsgUsageRecord{
		{PaneID: "pane-a", Tenant: "acme", Model: "claude-opus-4-8", InTokens: 1000, OutTokens: 100, TS: now},
		{PaneID: "pane-a", Tenant: "acme", Model: "claude-opus-4-8", InTokens: 50, OutTokens: 20, TS: now},
		{PaneID: "pane-b", Model: "gpt-4o", InTokens: 700, OutTokens: 7, TS: now},
	}

	// Without the observer.
	plain := NewUsageActor("s", nil, nil)
	for _, r := range records {
		cp := *r
		plain.ingest(&cp)
	}

	// With it.
	observed := NewUsageActor("s", nil, nil)
	obs := &recordingObserver{}
	observed.SetUsageObserver(obs)
	for _, r := range records {
		cp := *r
		observed.ingest(&cp)
	}

	for _, pane := range []string{"pane-a", "pane-b"} {
		want, got := plain.spentToday(pane), observed.spentToday(pane)
		if want != got {
			t.Fatalf("pane %s: spent %d with the observer, %d without — enabling "+
				"upstream.governance changed a local figure", pane, got, want)
		}
	}
	if plain.spentTodayTenant("acme") != observed.spentTodayTenant("acme") {
		t.Fatalf("tenant rollup diverged: %d vs %d",
			observed.spentTodayTenant("acme"), plain.spentTodayTenant("acme"))
	}
	if len(obs.seen) != len(records) {
		t.Fatalf("observer saw %d records, want %d", len(obs.seen), len(records))
	}

	// The observer receives a PRICED copy — the server is not asked to re-derive
	// cost from a pricing table it does not have — and never the caller's
	// pointer, so it cannot mutate what the ledger just aggregated.
	if obs.seen[0].CostMicroUSD == 0 {
		t.Error("the observer was handed an unpriced record; the server would " +
			"have to invent a price for it")
	}
	if obs.seen[0].PaneID != "pane-a" || obs.seen[0].Tenant != "acme" {
		t.Errorf("observer got the wrong record: %+v", obs.seen[0])
	}
}

// TestUsageObserver_NilIsTheDefault: with no observer installed the ledger is
// byte-for-byte the OSS path. This is the "off by default" half of 023 §1.
func TestUsageObserver_NilIsTheDefault(t *testing.T) {
	u := NewUsageActor("s", nil, nil)
	if u.observer != nil {
		t.Fatal("a fresh usage actor must have no org-wide observer")
	}
	// Ingest must not panic without one.
	u.ingest(&msg.MsgUsageRecord{PaneID: "p", InTokens: 1, OutTokens: 1, TS: time.Now()})
	if u.spentToday("p") != 2 {
		t.Fatalf("spent = %d, want 2", u.spentToday("p"))
	}
}
