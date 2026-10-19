// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go"
)

// F-69: a closed pane's KV outlived it. DeleteKV skipped .llm_session, the
// rysh-memory summaries were never deleted, closing a tab deleted nothing, and
// the pane's own Stopping handler flushed its state back AFTER the parent's
// synchronous delete.

func paneWithKV(t *testing.T) (*PaneActor, nats.KeyValue, nats.KeyValue) {
	t.Helper()
	nc, _ := startJetStreamNATS(t, t.TempDir())
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	panes, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: "rysh-panes-test"})
	if err != nil {
		t.Fatalf("pane bucket: %v", err)
	}
	mem, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: memoryKVBucket})
	if err != nil {
		t.Fatalf("memory bucket: %v", err)
	}
	p := &PaneActor{id: "pane-1", nc: nc, kvStore: panes}
	return p, panes, mem
}

func mustExist(t *testing.T, kv nats.KeyValue, key string, want bool) {
	t.Helper()
	_, err := kv.Get(key)
	switch {
	case want && err != nil:
		t.Fatalf("key %q missing: %v", key, err)
	case !want && err == nil:
		t.Fatalf("key %q still present after the pane was removed", key)
	case !want && !errors.Is(err, nats.ErrKeyNotFound):
		t.Fatalf("key %q: unexpected error %v", key, err)
	}
}

func TestDeleteKVRemovesEverythingThePaneOwns(t *testing.T) {
	p, panes, mem := paneWithKV(t)
	for _, suffix := range paneKVSuffixes {
		_, _ = panes.Put(p.id+suffix, []byte("x"))
	}
	for _, mode := range memoryModes {
		_, _ = mem.Put(memoryKVKey(p.id, mode), []byte("x"))
	}
	_, _ = panes.Put("pane-2.llm_session", []byte("another pane's"))

	p.DeleteKV()

	for _, suffix := range paneKVSuffixes {
		mustExist(t, panes, p.id+suffix, false)
	}
	for _, mode := range memoryModes {
		mustExist(t, mem, memoryKVKey(p.id, mode), false)
	}
	mustExist(t, panes, "pane-2.llm_session", true)
}

func TestRemovedPaneDoesNotFlushItselfBack(t *testing.T) {
	// The parent deletes, then stops; the pane's Stopping handler used to
	// flush its full snapshot right back.
	p, panes, _ := paneWithKV(t)
	p.DeleteKV()
	p.flushKV()
	p.persistDurableMeta()
	for _, suffix := range []string{"", ".private", paneMetaKVSuffix} {
		mustExist(t, panes, p.id+suffix, false)
	}
}

func TestShutdownStopStillPersists(t *testing.T) {
	// Not removed: a daemon shutdown must keep flushing, or every restart
	// would lose every pane.
	p, panes, _ := paneWithKV(t)
	p.flushKV()
	mustExist(t, panes, p.id, true)
}
