package replay

import (
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"
)

// player.go — the playback half of design 006 §3.2, v1: `##replay play`
// re-emits the captured output events into the invoking pane with the
// original inter-event timing, scaled by --speed and seekable with --from.
//
// This is an IN-PANE timed replay, not the design's dedicated read-only
// replay pane: the recorded bytes are published back onto the pane's normal
// output path (as system messages tagged with RoleReplay), so replay is
// display-only by construction — nothing is ever written to the shell's
// stdin/PTY, and the live pane state is untouched beyond the printed output.
// The capture skips RoleReplay messages, so playing a recording does not
// append a second copy of it to the recording itself. (The durable stream,
// which captures at the broker, still stores the replayed bytes; exports and
// playback filter them out, so the only cost is storage inside the stream's
// retention caps.)

// RoleReplay marks a ConversationMessage as replayed output so the capture
// (RAM ring and stream walks) can exclude it from the recording.
const RoleReplay = "replay"

// PlayStep is one scheduled playback emission: wait Delay (already
// speed-scaled), then emit Text.
type PlayStep struct {
	Delay time.Duration
	Text  string
}

// ParseSpeed parses a --speed value: a positive number (2, 0.5, …) or "max"
// (no delays at all, design 006 §3.2). Empty means 1x.
func ParseSpeed(spec string) (float64, error) {
	if spec == "" {
		return 1, nil
	}
	if spec == "max" {
		return math.Inf(1), nil
	}
	v, err := strconv.ParseFloat(spec, 64)
	if err != nil || v <= 0 || math.IsNaN(v) {
		return 0, fmt.Errorf("invalid --speed %q (want a positive number or \"max\")", spec)
	}
	return v, nil
}

// parseFrom parses a --from value into an offset from the start of the
// recording (firstTsMs): a duration ("90s", "1m30s"), an RFC3339 timestamp,
// or a unix timestamp (seconds, or millis when > 1e12). A timestamp before
// the recording clamps to 0.
func parseFrom(spec string, firstTsMs int64) (time.Duration, error) {
	if spec == "" {
		return 0, nil
	}
	if d, err := time.ParseDuration(spec); err == nil {
		if d < 0 {
			d = 0
		}
		return d, nil
	}
	var tsMs int64
	if t, err := time.Parse(time.RFC3339, spec); err == nil {
		tsMs = t.UnixMilli()
	} else if n, err := strconv.ParseInt(spec, 10, 64); err == nil {
		if n > 1_000_000_000_000 { // past ~2001 in millis ⇒ millis
			tsMs = n
		} else {
			tsMs = n * 1000
		}
	} else {
		return 0, fmt.Errorf("invalid --from %q (want a duration, RFC3339, or unix timestamp)", spec)
	}
	if tsMs <= firstTsMs {
		return 0, nil
	}
	return time.Duration(tsMs-firstTsMs) * time.Millisecond, nil
}

// planSteps turns recorded events into a playback schedule: events before the
// `from` offset are skipped, the first kept event waits only its remaining
// distance from the seek point, and every delay is divided by speed
// (speed = +Inf ⇒ all delays are 0). Pure, so the timing logic is testable
// with no clock at all.
func planSteps(evs []recEvent, from time.Duration, speed float64) []PlayStep {
	if speed <= 0 {
		speed = 1
	}
	if len(evs) == 0 {
		return nil
	}
	first := evs[0].ts
	prev := first + from.Milliseconds() // the playback cursor, in recording time
	var steps []PlayStep
	for _, e := range evs {
		if e.ts < prev {
			continue
		}
		gap := time.Duration(e.ts-prev) * time.Millisecond
		steps = append(steps, PlayStep{Delay: time.Duration(float64(gap) / speed), Text: e.text})
		prev = e.ts
	}
	return steps
}

// PlaybackPlan builds the ##replay play schedule for a pane ("" = all panes
// merged), falling back to the durable stream when the RAM ring is empty
// (post-restart), exactly like export.
func (c *Capture) PlaybackPlan(paneID, fromSpec, speedSpec string) ([]PlayStep, error) {
	speed, err := ParseSpeed(speedSpec)
	if err != nil {
		return nil, err
	}
	evs := c.events(paneID)
	if len(evs) == 0 {
		evs = c.streamEvents(paneID)
	}
	if len(evs) == 0 {
		return nil, fmt.Errorf("no captured output%s", paneSuffix(paneID))
	}
	from, err := parseFrom(fromSpec, evs[0].ts)
	if err != nil {
		return nil, err
	}
	steps := planSteps(evs, from, speed)
	if len(steps) == 0 {
		return nil, fmt.Errorf("--from %s is past the end of the recording", fromSpec)
	}
	return steps, nil
}

// Player runs one playback on its own goroutine until the schedule finishes
// or Stop is called. It never touches actor state: the emit/onDone callbacks
// carry everything it needs, and all publishing they do is thread-safe NATS.
type Player struct {
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// StartPlayer schedules steps on a new goroutine. `after` is the clock seam
// (time.After in production; tests inject a fake). `emit` publishes one
// step's text. `onDone(stopped)` runs exactly once when playback ends —
// stopped=true when Stop cut it short. Both callbacks run on the player
// goroutine.
func StartPlayer(steps []PlayStep, after func(time.Duration) <-chan time.Time, emit func(string), onDone func(stopped bool)) *Player {
	p := &Player{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		for _, s := range steps {
			if s.Delay > 0 {
				select {
				case <-after(s.Delay):
				case <-p.stop:
					if onDone != nil {
						onDone(true)
					}
					return
				}
			} else {
				// Zero-delay bursts must still notice Stop promptly.
				select {
				case <-p.stop:
					if onDone != nil {
						onDone(true)
					}
					return
				default:
				}
			}
			emit(s.Text)
		}
		if onDone != nil {
			onDone(false)
		}
	}()
	return p
}

// Stop cancels the playback. Idempotent and safe from any goroutine.
func (p *Player) Stop() {
	p.stopOnce.Do(func() { close(p.stop) })
}

// Done is closed when the playback goroutine has fully exited.
func (p *Player) Done() <-chan struct{} { return p.done }

// Active reports whether the playback is still running.
func (p *Player) Active() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}
