package mcp

import (
	"errors"
	"sync"
	"testing"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// captureEmitter is a concurrency-safe StatusEvent collector for tests.
type captureEmitter struct {
	mu     sync.Mutex
	events []StatusEvent
}

func (c *captureEmitter) fn(ev StatusEvent) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
}

func (c *captureEmitter) snapshot() []StatusEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]StatusEvent, len(c.events))
	copy(out, c.events)
	return out
}

// TestEmit_NoEmitterIsNoop verifies emit() is safe when no emitter is wired.
func TestEmit_NoEmitterIsNoop(t *testing.T) {
	m := NewManager(sharedtools.NewToolRegistry(), t.TempDir())
	m.emit(StatusEvent{Server: "x", Phase: PhaseConnected}) // must not panic
}

// TestMarkUnhealthy_GivenUpEmits proves that a server which has already
// exhausted its session restart budget emits a given_up transition (and does
// NOT schedule another reconnect).
func TestMarkUnhealthy_GivenUpEmits(t *testing.T) {
	m := NewManager(sharedtools.NewToolRegistry(), t.TempDir())
	cap := &captureEmitter{}
	m.SetStatusEmitter(cap.fn)

	// Seed a server that has already hit the cap.
	m.servers["github"] = &serverState{
		def:             ServerDef{Name: "github"},
		connected:       false,
		restartAttempts: MaxRestartAttemptsPerSession,
	}

	if scheduled := m.MarkUnhealthy("github", errors.New("boom")); scheduled {
		t.Fatalf("MarkUnhealthy should not schedule a reconnect past the cap")
	}

	events := cap.snapshot()
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 emit, got %d: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Server != "github" || ev.Phase != PhaseGivenUp {
		t.Fatalf("expected given_up for github, got %+v", ev)
	}
	if ev.Max != MaxRestartAttemptsPerSession {
		t.Fatalf("expected Max=%d, got %d", MaxRestartAttemptsPerSession, ev.Max)
	}
}

// TestRemoveServer_EmitsRemoved proves removing a known server reports a
// removed transition so the TUI can drop it from the footer.
func TestRemoveServer_EmitsRemoved(t *testing.T) {
	m := NewManager(sharedtools.NewToolRegistry(), t.TempDir())
	cap := &captureEmitter{}
	m.SetStatusEmitter(cap.fn)

	m.servers["fs"] = &serverState{def: ServerDef{Name: "fs"}, connected: true}

	if err := m.RemoveServer("fs"); err != nil {
		t.Fatalf("RemoveServer: %v", err)
	}

	events := cap.snapshot()
	if len(events) != 1 || events[0].Phase != PhaseRemoved || events[0].Server != "fs" {
		t.Fatalf("expected one removed emit for fs, got %+v", events)
	}
}

// TestRemoveServer_UnknownDoesNotEmit verifies a no-op remove stays silent.
func TestRemoveServer_UnknownDoesNotEmit(t *testing.T) {
	m := NewManager(sharedtools.NewToolRegistry(), t.TempDir())
	cap := &captureEmitter{}
	m.SetStatusEmitter(cap.fn)

	if err := m.RemoveServer("nope"); err == nil {
		t.Fatalf("expected error removing unknown server")
	}
	if got := cap.snapshot(); len(got) != 0 {
		t.Fatalf("expected no emits for unknown remove, got %+v", got)
	}
}
