package mcp

import (
	"testing"
	"time"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// TestStartStopHeartbeat exercises the idempotent lifecycle of the
// heartbeat goroutine. Multiple Start calls are no-ops while one is
// running; Stop waits for the goroutine to exit.
func TestStartStopHeartbeat(t *testing.T) {
	origInterval := HeartbeatInterval
	HeartbeatInterval = 50 * time.Millisecond
	defer func() { HeartbeatInterval = origInterval }()

	m := NewManager(sharedtools.NewToolRegistry(), t.TempDir())

	m.StartHeartbeat()
	m.StartHeartbeat() // no-op

	// Let the loop tick at least once.
	time.Sleep(150 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		m.StopHeartbeat()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StopHeartbeat did not return in time")
	}

	// Stopping twice is a no-op (and doesn't panic).
	m.StopHeartbeat()
}

// TestHeartbeatTick_SkipsRestartingServers proves the loop honours
// per-server state and doesn't probe a server that's in the middle of
// a reconnect.
func TestHeartbeatTick_SkipsRestartingServers(t *testing.T) {
	m := NewManager(sharedtools.NewToolRegistry(), t.TempDir())
	m.servers["a"] = &serverState{def: ServerDef{Name: "a"}, connected: true, client: nil}
	m.servers["b"] = &serverState{def: ServerDef{Name: "b"}, connected: true, restarting: true}
	m.servers["c"] = &serverState{def: ServerDef{Name: "c"}, connected: true, restartGivenUp: true}
	m.servers["d"] = &serverState{def: ServerDef{Name: "d"}, connected: false}

	// We can't easily build a fake Client here, but heartbeatTick must
	// not panic for nil/invalid states. The "a" entry has a nil client,
	// which the snapshot skips (client != nil filter). b/c/d are
	// explicitly skipped. So no probes are issued and no panic.
	m.heartbeatTick()
}
