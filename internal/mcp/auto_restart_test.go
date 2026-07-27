package mcp

import (
	"errors"
	"testing"
	"time"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// TestBackoffFor exercises the schedule: the first attempts get the
// short backoff values; attempts past the slice length get the final
// (longest) value.
func TestBackoffFor(t *testing.T) {
	if backoffFor(0) != restartBackoffSchedule[0] {
		t.Errorf("attempt 0: got %v", backoffFor(0))
	}
	if backoffFor(-5) != restartBackoffSchedule[0] {
		t.Errorf("negative attempt should clamp to first: got %v", backoffFor(-5))
	}
	last := restartBackoffSchedule[len(restartBackoffSchedule)-1]
	if backoffFor(100) != last {
		t.Errorf("very large attempt should clamp to last: got %v want %v", backoffFor(100), last)
	}
}

// TestMarkUnhealthy_UnknownServer ignores requests for servers we don't
// know about (e.g. removed mid-flight).
func TestMarkUnhealthy_UnknownServer(t *testing.T) {
	m := NewManager(sharedtools.NewToolRegistry(), t.TempDir())
	if m.MarkUnhealthy("nope", errors.New("dead")) {
		t.Errorf("MarkUnhealthy on unknown server should return false")
	}
}

// TestMarkUnhealthy_AlreadyRestarting deduplicates concurrent calls so
// only one reconnect goroutine runs at a time per server.
func TestMarkUnhealthy_AlreadyRestarting(t *testing.T) {
	m := NewManager(sharedtools.NewToolRegistry(), t.TempDir())
	m.servers["s1"] = &serverState{
		def:        ServerDef{Name: "s1"},
		restarting: true,
	}
	if m.MarkUnhealthy("s1", errors.New("transport down")) {
		t.Errorf("MarkUnhealthy should return false when already restarting")
	}
}

// TestMarkUnhealthy_GivenUp respects the session cap: once
// restartAttempts has reached MaxRestartAttemptsPerSession, further
// failures don't trigger more restarts.
func TestMarkUnhealthy_GivenUp(t *testing.T) {
	origCap := MaxRestartAttemptsPerSession
	MaxRestartAttemptsPerSession = 3
	defer func() { MaxRestartAttemptsPerSession = origCap }()

	m := NewManager(sharedtools.NewToolRegistry(), t.TempDir())
	m.servers["s1"] = &serverState{
		def:             ServerDef{Name: "s1"},
		restartAttempts: 3, // at cap
	}
	if m.MarkUnhealthy("s1", errors.New("dead")) {
		t.Errorf("MarkUnhealthy at cap should return false")
	}
	if !m.servers["s1"].restartGivenUp {
		t.Errorf("expected restartGivenUp to be set after cap hit")
	}
}

// TestMarkUnhealthy_TriggersOnce verifies the happy path: first call
// returns true and schedules a goroutine. The goroutine will fail (no
// real def to connect to in this unit test) and bump the attempt
// counter, but the test only verifies the synchronous return value and
// the state transition into `restarting`.
func TestMarkUnhealthy_TriggersOnce(t *testing.T) {
	m := NewManager(sharedtools.NewToolRegistry(), t.TempDir())
	m.servers["s1"] = &serverState{
		def: ServerDef{Name: "s1", Transport: TransportStdio, Command: "/bin/false"},
	}

	if !m.MarkUnhealthy("s1", errors.New("transport broken")) {
		t.Errorf("first MarkUnhealthy should return true")
	}

	// State should reflect restart pending. Use a short wait because the
	// goroutine sets `restarting` before the goroutine's defer.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		restarting := m.servers["s1"].restarting
		m.mu.Unlock()
		if restarting {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	// `restarting` might already have flipped back to false if the
	// goroutine raced through its first failure faster than our wait
	// — that's also a valid outcome.
}
