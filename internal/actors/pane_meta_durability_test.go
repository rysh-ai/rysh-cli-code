// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// paneMetaMemKV is an in-memory nats.KeyValue for the durable-meta sidecar
// tests (F-54). Only Put/Get/Delete are exercised; the embedded nil interface
// satisfies the rest at compile time.
type paneMetaMemKV struct {
	nats.KeyValue
	data map[string][]byte
}

func newPaneMetaMemKV() *paneMetaMemKV { return &paneMetaMemKV{data: map[string][]byte{}} }

func (m *paneMetaMemKV) Put(key string, value []byte) (uint64, error) {
	cp := make([]byte, len(value))
	copy(cp, value)
	m.data[key] = cp
	return 1, nil
}

func (m *paneMetaMemKV) Get(key string) (nats.KeyValueEntry, error) {
	v, ok := m.data[key]
	if !ok {
		return nil, nats.ErrKeyNotFound
	}
	return &paneMetaMemEntry{value: v}, nil
}

func (m *paneMetaMemKV) Delete(key string, _ ...nats.DeleteOpt) error {
	delete(m.data, key)
	return nil
}

type paneMetaMemEntry struct {
	nats.KeyValueEntry
	value []byte
}

func (e *paneMetaMemEntry) Value() []byte { return e.value }

// TestDurableMetaSurvivesLostSnapshot is the F-54 shape end to end: the big
// pane snapshot is GONE after a restart (its debounced Put failed silently, or
// the entry is unreadable), the layout restored the pane anyway — and the
// addressing layer must still come back, because fleet selectors, boards and
// resume all match on it. Before the sidecar this restored a pane with nil
// meta and no given-name: an unkillable fleet and a board pane on the wrong
// board, all silent.
func TestDurableMetaSurvivesLostSnapshot(t *testing.T) {
	kv := newPaneMetaMemKV()

	p := &PaneActor{id: "pane-f54", kvStore: kv}
	p.givenName = "wkr-2-e2-revenue-1"
	p.meta = map[string]string{
		"board.id":          "b-e2",
		"fleet.name":        "e2",
		"fleet.role":        "worker",
		"epic":              "E2",
		"rysh.auto_approve": "true",
	}
	p.persistDurableMeta()

	// "Restart": a fresh actor for the same pane id, and NO RestoreState call —
	// the big snapshot is lost. The sidecar alone must bring the layer back.
	restored := &PaneActor{id: "pane-f54", kvStore: kv}
	restored.restoreDurableMeta()

	if restored.givenName != "wkr-2-e2-revenue-1" {
		t.Errorf("given-name = %q, want %q", restored.givenName, "wkr-2-e2-revenue-1")
	}
	for k, want := range p.meta {
		if got := restored.meta[k]; got != want {
			t.Errorf("meta[%q] = %q, want %q — a fleet whose meta died is unkillable as a fleet", k, got, want)
		}
	}
}

// TestDurableMetaOutranksStaleSnapshot: once the sidecar exists it is
// authoritative — it is written whole and synchronously on every change, while
// the big snapshot is debounced and can be arbitrarily stale. A key deleted
// via `##pane meta delete` must NOT resurrect from an old snapshot, and a
// rename must not be undone by one.
func TestDurableMetaOutranksStaleSnapshot(t *testing.T) {
	kv := newPaneMetaMemKV()

	p := &PaneActor{id: "pane-stale", kvStore: kv}
	p.givenName = "new-name"
	p.meta = map[string]string{"fleet.name": "e2"} // "obsolete.key" already deleted
	p.persistDurableMeta()

	restored := &PaneActor{id: "pane-stale", kvStore: kv}
	restored.RestoreState(domain.PaneSnapshot{
		ID:        "pane-stale",
		GivenName: "old-name",
		Meta:      map[string]string{"fleet.name": "e2", "obsolete.key": "zombie"},
	})
	restored.restoreDurableMeta()

	if restored.givenName != "new-name" {
		t.Errorf("given-name = %q, want the sidecar's %q, not the stale snapshot's", restored.givenName, "new-name")
	}
	if _, ok := restored.meta["obsolete.key"]; ok {
		t.Error("deleted meta key resurrected from the stale snapshot — the sidecar must be authoritative")
	}
	if restored.meta["fleet.name"] != "e2" {
		t.Errorf("meta[fleet.name] = %q, want %q", restored.meta["fleet.name"], "e2")
	}
}

// TestDurableMetaMissingSidecarKeepsSnapshot pins the upgrade path: a pane
// persisted by a pre-sidecar binary has no sidecar entry, and restoring it
// must keep whatever the big snapshot carried rather than blanking it.
func TestDurableMetaMissingSidecarKeepsSnapshot(t *testing.T) {
	kv := newPaneMetaMemKV()

	restored := &PaneActor{id: "pane-legacy", kvStore: kv}
	restored.RestoreState(domain.PaneSnapshot{
		ID:        "pane-legacy",
		GivenName: "legacy-name",
		Meta:      map[string]string{"fleet.role": "manager"},
	})
	restored.restoreDurableMeta() // no sidecar entry: must be a no-op

	if restored.givenName != "legacy-name" {
		t.Errorf("given-name = %q, want the snapshot's %q (missing sidecar must restore nothing)", restored.givenName, "legacy-name")
	}
	if restored.meta["fleet.role"] != "manager" {
		t.Errorf("meta[fleet.role] = %q, want %q", restored.meta["fleet.role"], "manager")
	}
}
