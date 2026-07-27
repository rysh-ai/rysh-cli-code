// Package relay provides a thread-safe in-process registry that maps pane IDs
// to PTY relay handles. When the TUI activates a relay for a pane, the
// rawReadLoop in the PaneActor sends raw PTY output to the handle's OutputCh,
// and the relay writes it directly to stdout — bypassing the NATS/VTerm/snapshot
// polling loop for native-speed interactive programs (vim, htop, etc.).
package relay

import (
	"os"
	"sync"
	"sync/atomic"
)

// Handle holds the shared state between the PaneActor's rawReadLoop goroutine
// and the TUI's PTYRelay. All fields are safe for concurrent access:
// - OutputCh is a buffered channel (concurrent send/recv is safe by design).
// - active is an atomic.Bool.
// - altExitCh is only closed once (guarded by sync.Once in SignalAltExit).
type Handle struct {
	PtyFile  *os.File    // direct reference to PTY master fd
	OutputCh chan []byte // rawReadLoop sends chunks here when relay is active

	altExitCh chan struct{} // closed by rawReadLoop when alt screen exits during relay
	altOnce   sync.Once     // ensures altExitCh is closed at most once
	active    atomic.Bool   // set by TUI to activate/deactivate relay
}

// NewHandle creates a new relay Handle for the given PTY file.
func NewHandle(ptyFile *os.File) *Handle {
	return &Handle{
		PtyFile:   ptyFile,
		OutputCh:  make(chan []byte, 256),
		altExitCh: make(chan struct{}),
	}
}

// IsActive returns true if the relay is currently active (TUI has taken over).
func (h *Handle) IsActive() bool {
	return h.active.Load()
}

// Activate prepares the handle for a new relay session. It drains any stale
// data from OutputCh, resets the alt-screen exit channel, and sets active=true.
func (h *Handle) Activate() {
	// Drain stale data from OutputCh.
	for {
		select {
		case <-h.OutputCh:
		default:
			goto drained
		}
	}
drained:
	// Reset altExitCh and the Once guard for a fresh relay session.
	h.altExitCh = make(chan struct{})
	h.altOnce = sync.Once{}
	h.active.Store(true)
}

// Deactivate marks the relay as inactive. rawReadLoop will stop sending to OutputCh.
func (h *Handle) Deactivate() {
	h.active.Store(false)
}

// SignalAltExit closes the altExitCh to notify the relay that the interactive
// program has exited (alt screen deactivated). Safe to call multiple times.
func (h *Handle) SignalAltExit() {
	h.altOnce.Do(func() {
		close(h.altExitCh)
	})
}

// AltExitCh returns a channel that is closed when the alt screen exits during
// an active relay.
func (h *Handle) AltExitCh() <-chan struct{} {
	return h.altExitCh
}

// ---------------------------------------------------------------------------
// Global registry
// ---------------------------------------------------------------------------

var (
	mu       sync.RWMutex
	registry = make(map[string]*Handle)
)

// Register associates a pane ID with a relay Handle.
func Register(paneID string, h *Handle) {
	mu.Lock()
	registry[paneID] = h
	mu.Unlock()
}

// Unregister removes the relay Handle for a pane ID.
func Unregister(paneID string) {
	mu.Lock()
	delete(registry, paneID)
	mu.Unlock()
}

// Get returns the relay Handle for a pane ID, or nil if not registered.
func Get(paneID string) *Handle {
	mu.RLock()
	h := registry[paneID]
	mu.RUnlock()
	return h
}
