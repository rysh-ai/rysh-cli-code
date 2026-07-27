package replay

import (
	"math"
	"sync"
	"testing"
	"time"
)

// The controlled player (v2, the replay pane's engine) is tested with a
// manual clock: `after` registers a wait the test fires explicitly, so pause /
// seek / speed assertions never sleep through real delays.

// manualAfter is a clock seam whose waits only fire when the test says so.
type manualAfter struct {
	mu     sync.Mutex
	waits  []chan time.Time
	delays []time.Duration
	// registered signals each new wait so tests can synchronize on the player
	// reaching its select.
	registered chan struct{}
}

func newManualAfter() *manualAfter {
	return &manualAfter{registered: make(chan struct{}, 100)}
}

func (m *manualAfter) after(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	m.mu.Lock()
	m.waits = append(m.waits, ch)
	m.delays = append(m.delays, d)
	m.mu.Unlock()
	m.registered <- struct{}{}
	return ch
}

// fireLatest fires the most recently registered wait.
func (m *manualAfter) fireLatest() {
	m.mu.Lock()
	ch := m.waits[len(m.waits)-1]
	m.mu.Unlock()
	ch <- time.Time{}
}

func (m *manualAfter) lastDelay() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.delays[len(m.delays)-1]
}

func (m *manualAfter) waitRegistered(t *testing.T) {
	t.Helper()
	select {
	case <-m.registered:
	case <-time.After(5 * time.Second):
		t.Fatal("player never registered a wait")
	}
}

// ctrlHarness wires a controlled player to channels the test can assert on.
type ctrlHarness struct {
	emits  chan string
	clears chan struct{}
	status chan PlayerStatus
	done   chan bool // receives onDone's stopped flag
}

func newCtrlHarness() *ctrlHarness {
	return &ctrlHarness{
		emits:  make(chan string, 100),
		clears: make(chan struct{}, 100),
		status: make(chan PlayerStatus, 100),
		done:   make(chan bool, 1),
	}
}

func (h *ctrlHarness) hooks() PlayerHooks {
	return PlayerHooks{
		Emit:     func(s string) { h.emits <- s },
		Clear:    func() { h.clears <- struct{}{} },
		OnDone:   func(stopped bool) { h.done <- stopped },
		OnStatus: func(st PlayerStatus) { h.status <- st },
	}
}

func (h *ctrlHarness) expectEmit(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-h.emits:
		if got != want {
			t.Fatalf("emitted %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for emission %q", want)
	}
}

// expectStatus drains status updates until pred passes (or times out).
func (h *ctrlHarness) expectStatus(t *testing.T, desc string, pred func(PlayerStatus) bool) PlayerStatus {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case st := <-h.status:
			if pred(st) {
				return st
			}
		case <-deadline:
			t.Fatalf("timed out waiting for status: %s", desc)
		}
	}
}

func tlABC() []TimedEvent {
	return []TimedEvent{
		{At: 0, Text: "a"},
		{At: time.Second, Text: "b"},
		{At: 2 * time.Second, Text: "c"},
	}
}

// TestControlledPlayerPauseResume: pause holds playback (a pending gap is
// abandoned), status reports Paused, resume re-arms the gap and playback
// continues.
func TestControlledPlayerPauseResume(t *testing.T) {
	ma := newManualAfter()
	h := newCtrlHarness()
	p := StartControlledPlayer(tlABC(), 0, 1, 0, ma.after, h.hooks())

	h.expectEmit(t, "a")
	ma.waitRegistered(t) // waiting the 1s gap before "b"
	if d := ma.lastDelay(); d != time.Second {
		t.Fatalf("gap delay = %v, want 1s", d)
	}

	p.TogglePause()
	h.expectStatus(t, "paused", func(st PlayerStatus) bool { return st.Paused })
	if st, ok := p.Status(); !ok || !st.Paused {
		t.Fatalf("Status() = %+v ok=%v, want paused", st, ok)
	}
	// Firing the abandoned wait must not emit while paused.
	ma.fireLatest()
	select {
	case got := <-h.emits:
		t.Fatalf("emitted %q while paused", got)
	case <-time.After(100 * time.Millisecond):
	}

	p.TogglePause()
	h.expectStatus(t, "resumed", func(st PlayerStatus) bool { return !st.Paused })
	ma.waitRegistered(t) // gap re-armed after resume
	ma.fireLatest()
	h.expectEmit(t, "b")
	h.expectStatus(t, "pos=1s", func(st PlayerStatus) bool { return st.Pos == time.Second })

	p.Stop()
	select {
	case stopped := <-h.done:
		if !stopped {
			t.Fatal("onDone stopped = false, want true")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("player did not stop")
	}
}

// TestControlledPlayerSeekForward: a forward seek instantly emits the skipped
// events and jumps the position; the next gap is measured from the seek point.
func TestControlledPlayerSeekForward(t *testing.T) {
	tl := append(tlABC(), TimedEvent{At: 30 * time.Second, Text: "d"})
	ma := newManualAfter()
	h := newCtrlHarness()
	p := StartControlledPlayer(tl, 0, 1, 0, ma.after, h.hooks())

	h.expectEmit(t, "a")
	ma.waitRegistered(t)

	p.SeekBy(10 * time.Second)
	h.expectEmit(t, "b")
	h.expectEmit(t, "c")
	h.expectStatus(t, "pos=10s", func(st PlayerStatus) bool { return st.Pos == 10*time.Second })
	if len(h.clears) != 0 {
		t.Fatal("forward seek must not clear the pane")
	}

	ma.waitRegistered(t) // gap to "d": 30s - 10s = 20s
	if d := ma.lastDelay(); d != 20*time.Second {
		t.Fatalf("post-seek delay = %v, want 20s", d)
	}
	ma.fireLatest()
	h.expectEmit(t, "d")
	select {
	case stopped := <-h.done:
		if stopped {
			t.Fatal("onDone stopped = true, want natural finish")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("player never finished")
	}
}

// TestControlledPlayerSeekBackward: a backward seek clears the destination and
// re-renders from the start of the buffer up to the target, then playback
// continues from there.
func TestControlledPlayerSeekBackward(t *testing.T) {
	ma := newManualAfter()
	h := newCtrlHarness()
	p := StartControlledPlayer(tlABC(), 0, 1, 0, ma.after, h.hooks())
	defer p.Stop()

	h.expectEmit(t, "a")
	ma.waitRegistered(t)
	ma.fireLatest()
	h.expectEmit(t, "b") // pos now 1s
	ma.waitRegistered(t) // waiting the 1s gap to "c"

	p.SeekBy(-10 * time.Second) // clamps to 0
	select {
	case <-h.clears:
	case <-time.After(5 * time.Second):
		t.Fatal("backward seek never cleared the destination")
	}
	h.expectEmit(t, "a") // re-render from the start up to target 0
	h.expectStatus(t, "pos=0", func(st PlayerStatus) bool { return st.Pos == 0 && !st.Paused })

	// Playback continues: next gap is b at 1s from pos 0.
	ma.waitRegistered(t)
	if d := ma.lastDelay(); d != time.Second {
		t.Fatalf("post-backseek delay = %v, want 1s", d)
	}
	ma.fireLatest()
	h.expectEmit(t, "b")
}

// TestControlledPlayerSpeedChanges: SpeedUp/SpeedDown rescale pending gaps and
// are reflected in status; adjustSpeed clamps at the bounds and handles max.
func TestControlledPlayerSpeedChanges(t *testing.T) {
	ma := newManualAfter()
	h := newCtrlHarness()
	p := StartControlledPlayer(tlABC(), 0, 1, 0, ma.after, h.hooks())
	defer p.Stop()

	h.expectEmit(t, "a")
	ma.waitRegistered(t)
	if d := ma.lastDelay(); d != time.Second {
		t.Fatalf("1x delay = %v, want 1s", d)
	}

	p.SpeedUp() // 2x
	h.expectStatus(t, "speed=2", func(st PlayerStatus) bool { return st.Speed == 2 })
	ma.waitRegistered(t)
	if d := ma.lastDelay(); d != 500*time.Millisecond {
		t.Fatalf("2x delay = %v, want 500ms", d)
	}

	p.SpeedDown() // back to 1x
	p.SpeedDown() // 0.5x
	h.expectStatus(t, "speed=0.5", func(st PlayerStatus) bool { return st.Speed == 0.5 })
	ma.waitRegistered(t) // re-armed after first SpeedDown
	ma.waitRegistered(t) // re-armed after second
	if d := ma.lastDelay(); d != 2*time.Second {
		t.Fatalf("0.5x delay = %v, want 2s", d)
	}
}

// TestControlledPlayerLeadDelaysFirstEmission: a lead-in grace defers even a
// zero-offset first event until the lead wait fires (the replay pane needs
// time to subscribe), and Stop during the lead aborts cleanly.
func TestControlledPlayerLeadDelaysFirstEmission(t *testing.T) {
	ma := newManualAfter()
	h := newCtrlHarness()
	p := StartControlledPlayer(tlABC(), 0, 1, 500*time.Millisecond, ma.after, h.hooks())
	defer p.Stop()

	ma.waitRegistered(t) // the lead wait itself
	if d := ma.lastDelay(); d != 500*time.Millisecond {
		t.Fatalf("lead delay = %v, want 500ms", d)
	}
	select {
	case got := <-h.emits:
		t.Fatalf("emitted %q before the lead elapsed", got)
	case <-time.After(100 * time.Millisecond):
	}
	ma.fireLatest()
	h.expectEmit(t, "a")

	// Stop during a second player's lead reports stopped without emitting.
	ma2 := newManualAfter()
	h2 := newCtrlHarness()
	p2 := StartControlledPlayer(tlABC(), 0, 1, time.Minute, ma2.after, h2.hooks())
	ma2.waitRegistered(t)
	p2.Stop()
	select {
	case stopped := <-h2.done:
		if !stopped {
			t.Fatal("stop during lead: onDone stopped = false, want true")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("player did not stop during lead")
	}
	if len(h2.emits) != 0 {
		t.Fatal("stop during lead must emit nothing")
	}
}

// TestAdjustSpeedBounds: the interactive speed ladder clamps to
// [minSpeed, maxSpeed] and treats "max" (+Inf) as above the ladder.
func TestAdjustSpeedBounds(t *testing.T) {
	if got := adjustSpeed(16, true); got != 16 {
		t.Fatalf("adjustSpeed(16, up) = %v, want clamp at 16", got)
	}
	if got := adjustSpeed(0.25, false); got != 0.25 {
		t.Fatalf("adjustSpeed(0.25, down) = %v, want clamp at 0.25", got)
	}
	if got := adjustSpeed(math.Inf(1), true); !math.IsInf(got, 1) {
		t.Fatalf("adjustSpeed(max, up) = %v, want max", got)
	}
	if got := adjustSpeed(math.Inf(1), false); got != 16 {
		t.Fatalf("adjustSpeed(max, down) = %v, want 16", got)
	}
	if got := adjustSpeed(1, true); got != 2 {
		t.Fatalf("adjustSpeed(1, up) = %v, want 2", got)
	}
}

// TestControlledPlayerFromAndTotal: --from skips earlier events, position
// starts at the seek point, and Total spans the whole recording.
func TestControlledPlayerFromAndTotal(t *testing.T) {
	ma := newManualAfter()
	h := newCtrlHarness()
	p := StartControlledPlayer(tlABC(), 1500*time.Millisecond, 1, 0, ma.after, h.hooks())
	defer p.Stop()

	st := h.expectStatus(t, "initial", func(st PlayerStatus) bool { return true })
	if st.Pos != 1500*time.Millisecond || st.Total != 2*time.Second {
		t.Fatalf("initial status = %+v, want pos 1.5s total 2s", st)
	}
	ma.waitRegistered(t) // "c" at 2s is 500ms away from the 1.5s seek point
	if d := ma.lastDelay(); d != 500*time.Millisecond {
		t.Fatalf("from-seek delay = %v, want 500ms", d)
	}
	ma.fireLatest()
	h.expectEmit(t, "c")
	select {
	case got := <-h.emits:
		t.Fatalf("unexpected extra emission %q (a/b are before --from)", got)
	case stopped := <-h.done:
		if stopped {
			t.Fatal("want natural finish")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("player never finished")
	}
}

// TestFormatBadge: the REPLAY badge shows position/total, a speed suffix when
// not 1x, and a pause marker.
func TestFormatBadge(t *testing.T) {
	cases := []struct {
		st   PlayerStatus
		want string
	}{
		{PlayerStatus{Pos: 83 * time.Second, Total: 296 * time.Second, Speed: 1}, "REPLAY 01:23/04:56"},
		{PlayerStatus{Pos: 83 * time.Second, Total: 296 * time.Second, Speed: 2}, "REPLAY 01:23/04:56 ×2"},
		{PlayerStatus{Pos: 0, Total: 5 * time.Second, Speed: 0.5, Paused: true}, "REPLAY 00:00/00:05 ×0.5 ⏸"},
		{PlayerStatus{Pos: 3661 * time.Second, Total: 7200 * time.Second, Speed: math.Inf(1)}, "REPLAY 1:01:01/2:00:00 ×max"},
	}
	for _, c := range cases {
		if got := FormatBadge(c.st); got != c.want {
			t.Fatalf("FormatBadge(%+v) = %q, want %q", c.st, got, c.want)
		}
	}
}

// TestCaptureTimeline: Timeline converts the RAM ring into offsets from the
// first event; an empty recording (and no durable stream) is an error.
func TestCaptureTimeline(t *testing.T) {
	c := NewCapture(nil, nil, "tl-sess")
	c.byPane["p1"] = []recEvent{{ts: 5000, text: "one"}, {ts: 6500, text: "two"}}

	tl, err := c.Timeline("p1")
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(tl) != 2 || tl[0].At != 0 || tl[0].Text != "one" || tl[1].At != 1500*time.Millisecond {
		t.Fatalf("timeline = %+v, want offsets 0 and 1.5s", tl)
	}

	if _, err := c.Timeline("nope"); err == nil {
		t.Fatal("Timeline(nope): want error for empty recording")
	}

	// ParseFromOffset resolves specs against the first recorded timestamp.
	if d, err := c.ParseFromOffset("p1", "1s"); err != nil || d != time.Second {
		t.Fatalf("ParseFromOffset = (%v, %v), want (1s, nil)", d, err)
	}
	if _, err := c.ParseFromOffset("p1", "bogus"); err == nil {
		t.Fatal("ParseFromOffset(bogus): want error")
	}
}

// TestUncontrolledPlayerIgnoresControls: v1 players (StartPlayer) have no
// control channel — control methods are safe no-ops and Status reports !ok.
func TestUncontrolledPlayerIgnoresControls(t *testing.T) {
	fa := &fakeAfter{}
	p := StartPlayer([]PlayStep{{Text: "x"}}, fa.after, func(string) {}, nil)
	<-p.Done()
	p.TogglePause()
	p.SeekBy(time.Second)
	p.SpeedUp()
	p.SpeedDown()
	if _, ok := p.Status(); ok {
		t.Fatal("Status() ok = true on an uncontrolled player")
	}
}
