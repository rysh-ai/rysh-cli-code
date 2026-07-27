package replay

import (
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The playback scheduler is tested with a fake clock (the `after` seam):
// tests assert the requested delays instead of sleeping through them.

func evsAt(offsets ...int64) []recEvent {
	const base = 1_700_000_000_000
	out := make([]recEvent, len(offsets))
	for i, o := range offsets {
		out[i] = recEvent{ts: base + o, text: string(rune('a' + i))}
	}
	return out
}

// TestPlanStepsTimingAndSpeed: inter-event gaps become delays, scaled by
// speed; the first event plays immediately; "max" speed drops all delays.
func TestPlanStepsTimingAndSpeed(t *testing.T) {
	evs := evsAt(0, 1000, 3000)

	steps := planSteps(evs, 0, 1)
	if len(steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(steps))
	}
	want := []time.Duration{0, time.Second, 2 * time.Second}
	for i, s := range steps {
		if s.Delay != want[i] {
			t.Fatalf("step %d delay = %v, want %v", i, s.Delay, want[i])
		}
	}
	if steps[0].Text != "a" || steps[2].Text != "c" {
		t.Fatalf("step texts = %q %q %q, want a b c", steps[0].Text, steps[1].Text, steps[2].Text)
	}

	// 4x speed quarters every delay.
	steps = planSteps(evs, 0, 4)
	if steps[1].Delay != 250*time.Millisecond || steps[2].Delay != 500*time.Millisecond {
		t.Fatalf("4x delays = %v %v, want 250ms 500ms", steps[1].Delay, steps[2].Delay)
	}

	// "max" speed = +Inf: no delays at all.
	sp, err := ParseSpeed("max")
	if err != nil {
		t.Fatalf("ParseSpeed(max): %v", err)
	}
	for i, s := range planSteps(evs, 0, sp) {
		if s.Delay != 0 {
			t.Fatalf("max-speed step %d delay = %v, want 0", i, s.Delay)
		}
	}
}

// TestPlanStepsFromFiltering: --from skips everything before the offset and
// the first kept event waits only its remaining distance from the seek point.
func TestPlanStepsFromFiltering(t *testing.T) {
	evs := evsAt(0, 1000, 3000)

	steps := planSteps(evs, 500*time.Millisecond, 1)
	if len(steps) != 2 {
		t.Fatalf("steps = %d, want 2 (event at 0ms skipped)", len(steps))
	}
	if steps[0].Text != "b" || steps[0].Delay != 500*time.Millisecond {
		t.Fatalf("first kept step = %q after %v, want b after 500ms", steps[0].Text, steps[0].Delay)
	}
	if steps[1].Delay != 2*time.Second {
		t.Fatalf("second kept delay = %v, want 2s", steps[1].Delay)
	}

	// Seeking exactly onto an event plays it immediately.
	steps = planSteps(evs, time.Second, 2)
	if len(steps) != 2 || steps[0].Delay != 0 || steps[1].Delay != time.Second {
		t.Fatalf("seek-on-event steps = %+v, want [b@0 c@1s]", steps)
	}

	// Past the end: nothing to play.
	if steps := planSteps(evs, time.Hour, 1); len(steps) != 0 {
		t.Fatalf("past-the-end steps = %+v, want none", steps)
	}
}

// TestParseFromSpecs: --from accepts a duration offset, an RFC3339 timestamp,
// or a unix timestamp (seconds or millis); junk is an error, and a timestamp
// before the recording clamps to the start.
func TestParseFromSpecs(t *testing.T) {
	const first = 1_700_000_000_000 // ms

	if d, err := parseFrom("", first); err != nil || d != 0 {
		t.Fatalf("empty spec = (%v, %v), want (0, nil)", d, err)
	}
	if d, err := parseFrom("1m30s", first); err != nil || d != 90*time.Second {
		t.Fatalf("duration spec = (%v, %v), want (90s, nil)", d, err)
	}

	ts := time.UnixMilli(first).Add(42 * time.Second).UTC().Format(time.RFC3339)
	if d, err := parseFrom(ts, first); err != nil || d != 42*time.Second {
		t.Fatalf("RFC3339 spec = (%v, %v), want (42s, nil)", d, err)
	}

	if d, err := parseFrom("1700000010", first); err != nil || d != 10*time.Second {
		t.Fatalf("unix-seconds spec = (%v, %v), want (10s, nil)", d, err)
	}
	if d, err := parseFrom("1700000005000", first); err != nil || d != 5*time.Second {
		t.Fatalf("unix-millis spec = (%v, %v), want (5s, nil)", d, err)
	}

	// Before the recording started: clamp to 0, don't error.
	if d, err := parseFrom("1699999999", first); err != nil || d != 0 {
		t.Fatalf("before-start spec = (%v, %v), want (0, nil)", d, err)
	}

	if _, err := parseFrom("bogus", first); err == nil {
		t.Fatal("bogus spec: want error")
	}
}

// fakeAfter records requested delays and fires immediately.
type fakeAfter struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (f *fakeAfter) after(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	f.delays = append(f.delays, d)
	f.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	return ch
}

// TestPlayerEmitsAllAndReportsDone: the player walks every step in order via
// the injected clock, then reports a non-stopped completion.
func TestPlayerEmitsAllAndReportsDone(t *testing.T) {
	steps := []PlayStep{
		{Delay: 0, Text: "one"},
		{Delay: 250 * time.Millisecond, Text: "two"},
		{Delay: time.Second, Text: "three"},
	}
	fa := &fakeAfter{}
	var mu sync.Mutex
	var got []string
	var stoppedFlag *bool
	p := StartPlayer(steps, fa.after,
		func(text string) { mu.Lock(); got = append(got, text); mu.Unlock() },
		func(stopped bool) { s := stopped; stoppedFlag = &s })

	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("player never finished")
	}
	if p.Active() {
		t.Fatal("Active() = true after Done")
	}
	if len(got) != 3 || got[0] != "one" || got[2] != "three" {
		t.Fatalf("emitted = %v, want all three in order", got)
	}
	// Only the non-zero delays hit the clock.
	if len(fa.delays) != 2 || fa.delays[0] != 250*time.Millisecond || fa.delays[1] != time.Second {
		t.Fatalf("clock delays = %v, want [250ms 1s]", fa.delays)
	}
	if stoppedFlag == nil || *stoppedFlag {
		t.Fatalf("onDone stopped = %v, want false", stoppedFlag)
	}
}

// TestPlayerStopCancelsMidPlayback: Stop() aborts a pending delay, no further
// events are emitted, and onDone reports stopped. Stop is idempotent.
func TestPlayerStopCancelsMidPlayback(t *testing.T) {
	firstEmitted := make(chan struct{})
	blockedAfter := func(d time.Duration) <-chan time.Time {
		return make(chan time.Time) // never fires — playback hangs on delay 2
	}
	steps := []PlayStep{
		{Delay: 0, Text: "one"},
		{Delay: time.Minute, Text: "never"},
	}
	var mu sync.Mutex
	var got []string
	var stoppedFlag *bool
	p := StartPlayer(steps, blockedAfter,
		func(text string) {
			mu.Lock()
			got = append(got, text)
			mu.Unlock()
			close(firstEmitted)
		},
		func(stopped bool) { s := stopped; stoppedFlag = &s })

	<-firstEmitted
	p.Stop()
	p.Stop() // idempotent
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("player did not stop")
	}
	if len(got) != 1 || got[0] != "one" {
		t.Fatalf("emitted = %v, want only the first event", got)
	}
	if stoppedFlag == nil || !*stoppedFlag {
		t.Fatalf("onDone stopped = %v, want true", stoppedFlag)
	}
}

// TestCaptureIgnoresReplayRoleOutput: replayed output is marked with the
// replay role and must not be re-recorded by the RAM ring — otherwise playing
// a recording would append a second copy of it to the recording itself.
func TestCaptureIgnoresReplayRoleOutput(t *testing.T) {
	nc, _ := startJetStreamNATS(t, t.TempDir())
	const session = "norerecord-sess"
	msg.SetSessionPrefix(session)
	codecs := msg.DefaultCodecRegistry()

	c := NewCapture(nc, codecs, session)
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer c.Stop()

	pub := msg.NewNATSPublisher(nc, codecs)
	publishAppend(t, pub, "p1", "real output\n", 1000)
	err := pub.Send(msg.T("pane", "p1", "output"), &msg.MsgConversationAppend{
		Message: &msg.ConversationMessage{Content: "replayed copy\n", TimestampMs: 2000, Role: RoleReplay},
	})
	if err != nil {
		t.Fatalf("publish replayed: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && c.Count("p1") < 1 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // give the replayed copy time to (wrongly) land
	if got := c.Count("p1"); got != 1 {
		t.Fatalf("captured %d event(s), want 1 (replay-role output must be skipped)", got)
	}
}

// TestPlaybackPlanFallsBackToStream: with a fresh (post-restart) RAM ring,
// PlaybackPlan builds its schedule from the durable stream, like export does.
func TestPlaybackPlanFallsBackToStream(t *testing.T) {
	nc, _ := startJetStreamNATS(t, t.TempDir())
	const session = "playplan-sess"
	msg.SetSessionPrefix(session)
	codecs := msg.DefaultCodecRegistry()

	c := NewCapture(nc, codecs, session)
	if err := c.EnableDurable(DurableOptions{}); err != nil {
		t.Fatalf("EnableDurable: %v", err)
	}
	pub := msg.NewNATSPublisher(nc, codecs)
	publishAppend(t, pub, "p1", "one\n", 1000)
	publishAppend(t, pub, "p1", "two\n", 1500)
	waitStreamMsgs(t, nc, StreamName(session), 2)

	steps, err := c.PlaybackPlan("p1", "", "2")
	if err != nil {
		t.Fatalf("PlaybackPlan: %v", err)
	}
	if len(steps) != 2 || steps[0].Text != "one\n" || steps[1].Delay != 250*time.Millisecond {
		t.Fatalf("steps = %+v, want two steps with a 250ms second delay", steps)
	}

	// Empty recording is an error the caller can print.
	if _, err := c.PlaybackPlan("nope", "", ""); err == nil {
		t.Fatal("PlaybackPlan(nope): want error for a pane with no recording")
	}
	// Bad flags are errors, not silent defaults.
	if _, err := c.PlaybackPlan("p1", "junk", ""); err == nil {
		t.Fatal("PlaybackPlan bad --from: want error")
	}
	if _, err := c.PlaybackPlan("p1", "", "-3"); err == nil {
		t.Fatal("PlaybackPlan bad --speed: want error")
	}
}
