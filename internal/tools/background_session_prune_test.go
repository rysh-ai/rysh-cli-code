// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"fmt"
	"testing"
	"time"
)

// F-71: only kill_shell removed a background session, so every command that
// finished on its own kept its 256 KB ring buffer for the daemon's lifetime.
func TestFinishedBackgroundSessionsArePruned(t *testing.T) {
	m := NewBackgroundSessionManager()
	now := time.Now()
	add := func(id string, finished bool, at time.Time) {
		m.sessions[id] = &BackgroundSession{ID: id, finished: finished, finishedAt: at, buf: newRingBuffer(16)}
	}
	add("running-old", false, time.Time{})
	add("stale", true, now.Add(-2*finishedSessionTTL))
	for i := 0; i < maxFinishedSessions+5; i++ {
		add(fmt.Sprintf("fin-%02d", i), true, now.Add(-time.Duration(maxFinishedSessions+5-i)*time.Second))
	}

	m.mu.Lock()
	m.pruneFinishedLocked(now)
	m.mu.Unlock()

	if _, ok := m.Get("running-old"); !ok {
		t.Fatal("a running session was pruned")
	}
	if _, ok := m.Get("stale"); ok {
		t.Fatal("a session finished beyond the TTL was kept")
	}
	if len(m.sessions) != maxFinishedSessions+1 {
		t.Fatalf("%d sessions kept, want %d finished + 1 running", len(m.sessions), maxFinishedSessions)
	}
	for i := 0; i < 5; i++ {
		if _, ok := m.Get(fmt.Sprintf("fin-%02d", i)); ok {
			t.Fatalf("fin-%02d (among the oldest) survived the cap", i)
		}
	}
}
