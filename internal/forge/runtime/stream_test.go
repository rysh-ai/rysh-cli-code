// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is an injectable clock (mirrors the replay player's seam: tests
// advance time explicitly, no sleeping).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// startBlocking starts a session whose run function pushes the given frames
// then blocks until cancelled. Returns the session id and a channel closed
// when the run function observes cancellation.
func startBlocking(t *testing.T, m *StreamManager, frames ...string) (string, <-chan struct{}) {
	t.Helper()
	cancelled := make(chan struct{})
	pushed := make(chan struct{})
	id, err := m.Start("grpc-stream", "test", func(ctx context.Context, push func(string)) error {
		for _, f := range frames {
			push(f)
		}
		close(pushed)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-pushed
	return id, cancelled
}

// TestStreamPollIncremental: poll returns only the frames received since the
// previous poll.
func TestStreamPollIncremental(t *testing.T) {
	clk := newFakeClock()
	m := NewStreamManager(StreamOptions{Now: clk.Now, NoSweeper: true})
	defer m.CloseAll()
	id, _ := startBlocking(t, m, "f1", "f2")

	res, err := m.Poll(id, 0)
	if err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	if len(res.Frames) != 2 || res.Frames[0] != "f1" || res.Frames[1] != "f2" {
		t.Fatalf("poll 1 frames = %v, want [f1 f2]", res.Frames)
	}
	if res.Done {
		t.Fatalf("session should still be live")
	}

	res, err = m.Poll(id, 0)
	if err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	if len(res.Frames) != 0 {
		t.Fatalf("poll 2 must be empty (incremental), got %v", res.Frames)
	}
}

// TestStreamRingBounding: the ring keeps only the newest MaxFrames frames and
// reports how many unpolled frames were dropped.
func TestStreamRingBounding(t *testing.T) {
	clk := newFakeClock()
	m := NewStreamManager(StreamOptions{Now: clk.Now, NoSweeper: true, MaxFrames: 3})
	defer m.CloseAll()
	id, _ := startBlocking(t, m, "a", "b", "c", "d", "e")

	res, err := m.Poll(id, 0)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(res.Frames) != 3 || res.Frames[0] != "c" || res.Frames[2] != "e" {
		t.Fatalf("frames = %v, want the newest 3 [c d e]", res.Frames)
	}
	if res.Dropped != 2 {
		t.Fatalf("dropped = %d, want 2 (a, b evicted unread)", res.Dropped)
	}
}

// TestStreamPollFrameCap: a poll returns at most maxFrames frames and reports
// the remainder as pending.
func TestStreamPollFrameCap(t *testing.T) {
	clk := newFakeClock()
	m := NewStreamManager(StreamOptions{Now: clk.Now, NoSweeper: true})
	defer m.CloseAll()
	id, _ := startBlocking(t, m, "a", "b", "c")

	res, _ := m.Poll(id, 2)
	if len(res.Frames) != 2 || res.Pending != 1 {
		t.Fatalf("frames=%v pending=%d, want 2 frames and 1 pending", res.Frames, res.Pending)
	}
	res, _ = m.Poll(id, 2)
	if len(res.Frames) != 1 || res.Frames[0] != "c" || res.Pending != 0 {
		t.Fatalf("second poll = %v pending=%d, want [c] and 0", res.Frames, res.Pending)
	}
}

// TestStreamFrameTruncation: an oversize frame is truncated with a marker.
func TestStreamFrameTruncation(t *testing.T) {
	clk := newFakeClock()
	m := NewStreamManager(StreamOptions{Now: clk.Now, NoSweeper: true, MaxFrameBytes: 8})
	defer m.CloseAll()
	id, _ := startBlocking(t, m, "0123456789abcdef")

	res, _ := m.Poll(id, 0)
	if len(res.Frames) != 1 || !strings.HasPrefix(res.Frames[0], "01234567") || !strings.Contains(res.Frames[0], "truncated") {
		t.Fatalf("frame = %q, want an 8-byte prefix with a truncation marker", res.Frames)
	}
}

// TestStreamStopCancels: Stop cancels the run context, returns the unread
// frames, and removes the session.
func TestStreamStopCancels(t *testing.T) {
	clk := newFakeClock()
	m := NewStreamManager(StreamOptions{Now: clk.Now, NoSweeper: true})
	defer m.CloseAll()
	id, cancelled := startBlocking(t, m, "tail-frame")

	res, err := m.Stop(id)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if len(res.Frames) != 1 || res.Frames[0] != "tail-frame" {
		t.Fatalf("stop should return unread frames, got %v", res.Frames)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("run context was not cancelled by Stop")
	}
	if _, err := m.Poll(id, 0); err == nil {
		t.Fatal("polling a stopped session must fail (session removed)")
	}
}

// TestStreamIdleExpiry: a session not polled for IdleTTL is cancelled by the
// sweeper (fake clock; Sweep driven explicitly).
func TestStreamIdleExpiry(t *testing.T) {
	clk := newFakeClock()
	m := NewStreamManager(StreamOptions{Now: clk.Now, NoSweeper: true, IdleTTL: time.Minute})
	defer m.CloseAll()
	id, cancelled := startBlocking(t, m, "x")

	clk.Advance(30 * time.Second)
	m.Sweep()
	if res, err := m.Poll(id, 0); err != nil || res.Done {
		t.Fatalf("session expired before the idle TTL: err=%v res=%+v", err, res)
	}

	clk.Advance(2 * time.Minute) // > IdleTTL since the last poll
	m.Sweep()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("idle session was not cancelled by Sweep")
	}
	res, err := m.Poll(id, 0)
	if err != nil {
		t.Fatalf("an expired session should still answer one poll with the reason: %v", err)
	}
	if !res.Done || !strings.Contains(res.Reason, "not polled") {
		t.Fatalf("poll after expiry = %+v, want Done with an idle-expiry reason", res)
	}

	// One idle period after that final poll, the session is removed.
	clk.Advance(2 * time.Minute)
	m.Sweep()
	if _, err := m.Poll(id, 0); err == nil {
		t.Fatal("expired session should be removed after the linger period")
	}
}

// TestStreamLifetimeExpiry: even a regularly-polled session is cancelled once
// it exceeds MaxLifetime.
func TestStreamLifetimeExpiry(t *testing.T) {
	clk := newFakeClock()
	m := NewStreamManager(StreamOptions{Now: clk.Now, NoSweeper: true, IdleTTL: time.Hour, MaxLifetime: 10 * time.Minute})
	defer m.CloseAll()
	id, cancelled := startBlocking(t, m, "x")

	for i := 0; i < 5; i++ { // keep polling so idle never triggers
		clk.Advance(3 * time.Minute)
		m.Sweep()
		_, _ = m.Poll(id, 0)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("session exceeded MaxLifetime but was not cancelled")
	}
}

// TestStreamCloseAllCancels: CloseAll cancels every live session (the
// workspace/integration teardown seam).
func TestStreamCloseAllCancels(t *testing.T) {
	clk := newFakeClock()
	m := NewStreamManager(StreamOptions{Now: clk.Now, NoSweeper: true})
	_, c1 := startBlocking(t, m, "x")
	_, c2 := startBlocking(t, m, "y")

	m.CloseAll()
	for i, c := range []<-chan struct{}{c1, c2} {
		select {
		case <-c:
		case <-time.After(2 * time.Second):
			t.Fatalf("session %d not cancelled by CloseAll", i)
		}
	}
	if _, err := m.Start("grpc-stream", "late", func(ctx context.Context, push func(string)) error { return nil }); err == nil {
		t.Fatal("Start after CloseAll must fail")
	}
}

// TestStreamMaxSessions: the manager refuses to start more than MaxSessions
// live sessions.
func TestStreamMaxSessions(t *testing.T) {
	clk := newFakeClock()
	m := NewStreamManager(StreamOptions{Now: clk.Now, NoSweeper: true, MaxSessions: 2})
	defer m.CloseAll()
	startBlocking(t, m, "a")
	startBlocking(t, m, "b")

	_, err := m.Start("grpc-stream", "third", func(ctx context.Context, push func(string)) error {
		<-ctx.Done()
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("third Start = %v, want a too-many-sessions error", err)
	}
}

// TestStreamList: List reports id/kind/counters for every session.
func TestStreamList(t *testing.T) {
	clk := newFakeClock()
	m := NewStreamManager(StreamOptions{Now: clk.Now, NoSweeper: true})
	defer m.CloseAll()
	id, _ := startBlocking(t, m, "a", "b")

	infos := m.List()
	if len(infos) != 1 || infos[0].ID != id || infos[0].Frames != 2 || infos[0].Pending != 2 {
		t.Fatalf("List = %+v, want one live session with 2 unread frames", infos)
	}
}
