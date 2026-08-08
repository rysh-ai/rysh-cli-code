package actors

// The pane's KV write is debounced behind kvDirty, and its PRIMARY flush runs
// on a snapshot request — which only happens while a client is polling. A
// detached daemon has no such client, so without a periodic backstop a pane's
// dirty state reaches KV only at shutdown, and a SIGKILL (crash, OOM, or
// `rysh stop` hitting its kill fallback) loses it outright. That is how
// `##pane model` and `##pane name` came back empty after a restart.
//
// These tests pin the backstop itself. The end-to-end proof — set state, hard
// kill, restart, state is still there — is a live check, not a unit test.

import (
	"testing"
	"time"
)

// TestPanePersistIntervalMatchesWorkspaceBound: the workspace bounds its own
// exposure to ~1 minute on the cron tick. A pane backstop that is slower would
// silently widen the window the workspace already closed.
func TestPanePersistIntervalMatchesWorkspaceBound(t *testing.T) {
	if panePersistInterval > time.Minute {
		t.Errorf("panePersistInterval = %v, want ≤ 1m to match the workspace cron bound", panePersistInterval)
	}
	if panePersistInterval <= 0 {
		t.Fatal("panePersistInterval must be positive or the ticker never fires")
	}
}

// TestStopPersistTickerIsIdempotent: Stopping runs it, and a restarted actor
// runs startPersistTicker again — a double close would panic and take the
// daemon down during shutdown, which is exactly when state is being flushed.
func TestStopPersistTickerIsIdempotent(t *testing.T) {
	p := &PaneActor{id: "pane-1"}
	p.stopPersistTicker() // no ticker yet
	p.persistTickStop = make(chan struct{})
	p.stopPersistTicker()
	p.stopPersistTicker() // second call must not panic on a closed channel
	if p.persistTickStop != nil {
		t.Error("stopPersistTicker left the stop channel set; a restart would leak the goroutine")
	}
}

// TestMaybePersistOnlyWritesWhenDirty: the backstop fires every interval on
// every pane, so a clean pane must cost nothing — no snapshot build, no write.
func TestMaybePersistOnlyWritesWhenDirty(t *testing.T) {
	p := &PaneActor{id: "pane-1"}
	// kvStore is nil: persistNow returns before touching anything, so this
	// asserts the gate rather than the write. A dirty pane with no store must
	// keep its dirty flag so the next flush with a store still writes.
	p.kvDirty = true
	p.lastKVWrite = time.Now().Add(-time.Hour)
	p.maybePersist()
	if !p.kvDirty {
		t.Error("dirty flag cleared without a successful write — the state would be lost")
	}

	// Within the 2s debounce window nothing is attempted at all.
	p.lastKVWrite = time.Now()
	p.maybePersist()
	if !p.kvDirty {
		t.Error("debounce window cleared the dirty flag")
	}
}
