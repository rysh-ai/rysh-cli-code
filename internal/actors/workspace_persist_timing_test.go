package actors

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// fakeKV records Put calls. Only Put is exercised by persistToKVNow; the
// embedded nil interface satisfies the rest of nats.KeyValue at compile time.
type fakeKV struct {
	nats.KeyValue
	puts int
}

func (f *fakeKV) Put(key string, value []byte) (uint64, error) {
	f.puts++
	return uint64(f.puts), nil
}

// Layout-weight commands (resize/equalize/swap) are forwarded to the tab actor
// ASYNCHRONOUSLY, so the mutation is not applied yet when the workspace handler
// returns. persistToKV would, on the leading edge (first change after an idle
// period), immediately serialise the PRE-mutation state via the direct
// cross-actor ToKV() read and then clear kvDirty — orphaning the new weights
// until an unrelated change or a graceful shutdown, so a crash silently
// reverted the resize. persistToKVDeferred must instead leave the write to the
// trailing flush, which runs after the state has been applied.
func TestPersistToKVDeferredLeavesWriteToTrailingFlush(t *testing.T) {
	// Idle workspace: the debounce window has elapsed, so persistToKV would take
	// its leading-edge immediate-write path.
	newIdle := func() (*WorkspaceActor, *fakeKV) {
		f := &fakeKV{}
		w := &WorkspaceActor{wKV: f}
		w.lastKVWrite = time.Now().Add(-time.Hour)
		return w, f
	}

	// Baseline: the immediate persist does write now and clears the dirty flag.
	// (Correct for synchronous mutations, wrong for async-forwarded ones.)
	w, f := newIdle()
	w.persistToKV()
	if f.puts != 1 {
		t.Fatalf("persistToKV: got %d writes, want 1 (leading-edge write)", f.puts)
	}
	if w.kvDirty {
		t.Fatalf("persistToKV: expected dirty flag cleared after a successful write")
	}

	// The fix: deferred persist must NOT write immediately...
	w, f = newIdle()
	w.persistToKVDeferred()
	if f.puts != 0 {
		t.Errorf("persistToKVDeferred wrote immediately (%d writes) — that serialises "+
			"the pre-mutation layout weights", f.puts)
	}
	// ...and must leave the state dirty so the change is not orphaned.
	if !w.kvDirty {
		t.Errorf("persistToKVDeferred cleared the dirty flag — the applied mutation " +
			"would never be persisted")
	}

	// The trailing flush then writes the (now applied) state exactly once.
	w.maybeFlushKV()
	if f.puts != 1 {
		t.Errorf("trailing flush: got %d writes, want 1", f.puts)
	}
	if w.kvDirty {
		t.Errorf("trailing flush: expected dirty flag cleared after the write")
	}
}

// maybeFlushKV must respect the debounce window: a deferred mark inside the
// window waits, so a burst of resizes coalesces into one write instead of
// hammering KV on every arrow keypress.
func TestDeferredPersistCoalescesWithinDebounceWindow(t *testing.T) {
	f := &fakeKV{}
	w := &WorkspaceActor{wKV: f}
	w.lastKVWrite = time.Now() // just wrote: inside the debounce window

	for i := 0; i < 5; i++ {
		w.persistToKVDeferred()
		w.maybeFlushKV()
	}
	if f.puts != 0 {
		t.Errorf("expected writes to be debounced inside the window, got %d", f.puts)
	}
	if !w.kvDirty {
		t.Errorf("expected state to remain dirty while debounced")
	}

	// Once the window elapses, the pending state is flushed once.
	w.lastKVWrite = time.Now().Add(-time.Hour)
	w.maybeFlushKV()
	if f.puts != 1 {
		t.Errorf("expected exactly 1 coalesced write after the window elapsed, got %d", f.puts)
	}
}
