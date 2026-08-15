// SPDX-License-Identifier: Apache-2.0

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
// recording (firstTSMs): a duration ("90s", "1m30s"), an RFC3339 timestamp,
// or a unix timestamp (seconds, or millis when > 1e12). A timestamp before
// the recording clamps to 0.
func parseFrom(spec string, firstTSMs int64) (time.Duration, error) {
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
	if tsMs <= firstTSMs {
		return 0, nil
	}
	return time.Duration(tsMs-firstTSMs) * time.Millisecond, nil
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

// TimedEvent is one recorded emission on the playback timeline, at offset At
// from the start of the recording. Unlike PlayStep (whose delays are already
// speed-scaled and relative), TimedEvents keep absolute recording time so a
// controlled player can seek and change speed mid-flight.
type TimedEvent struct {
	At   time.Duration
	Text string
}

// PlayerStatus is a snapshot of a controlled playback, for the REPLAY badge:
// position/total in recording time, current speed (+Inf = max), paused flag.
// Pos is the recording-time offset of the last emitted event (or the seek
// target) — it advances per emission, not per wall-clock tick.
type PlayerStatus struct {
	Pos    time.Duration
	Total  time.Duration
	Speed  float64
	Paused bool
}

// PlayerHooks are the callbacks a controlled playback drives. All run on the
// player goroutine and must not touch actor state directly (publish via
// thread-safe NATS, or hand off to the owning actor's mailbox).
type PlayerHooks struct {
	// Emit publishes one event's text.
	Emit func(string)
	// Clear wipes the destination before a backward-seek re-render. A backward
	// seek re-renders from the start of the buffer: clear, then re-emit every
	// event up to the target instantly (documented v2 behavior — there is no
	// terminal-state snapshotting to rewind to).
	Clear func()
	// OnDone runs exactly once when playback ends; stopped=true when Stop cut
	// it short.
	OnDone func(stopped bool)
	// OnStatus runs after every status change (emission, pause, seek, speed) —
	// the badge feed.
	OnStatus func(PlayerStatus)
}

// ctrlMsg is one control action sent to a controlled player's goroutine.
type ctrlMsg struct {
	kind    ctrlKind
	seekBy  time.Duration
	speedUp bool
}

type ctrlKind int

const (
	ctrlPauseToggle ctrlKind = iota
	ctrlSeek
	ctrlSpeed
)

// Speed bounds for interactive +/- changes. "max" (+Inf) stays +Inf on
// speed-up; speed-down from max drops to the top of the bounded range.
const (
	minSpeed = 0.25
	maxSpeed = 16.0
)

// Player runs one playback on its own goroutine until the schedule finishes
// or Stop is called. It never touches actor state: the emit/onDone callbacks
// carry everything it needs, and all publishing they do is thread-safe NATS.
//
// Two modes share the type: StartPlayer (v1, pre-planned steps, stop-only)
// and StartControlledPlayer (v2, a recording timeline plus pause/seek/speed
// controls and status reporting). Control methods are no-ops on a v1 player.
type Player struct {
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	// Controlled mode only (nil ctrl ⇒ v1 stop-only player).
	ctrl chan ctrlMsg

	mu sync.Mutex
	st PlayerStatus
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

// StartControlledPlayer schedules a recording timeline on a new goroutine with
// interactive controls (v2, the replay pane's engine). `tl` must be sorted by
// At (Capture.Timeline guarantees it). Events with At < from are skipped, the
// same seek-forward semantics as --from in v1. `after` is the clock seam.
//
// `lead` is an initial grace wait before the first emission (still
// stop/control-interruptible): the replay pane is created asynchronously
// (workspace → tab → lane → group → pane spawn + NATS subscribe), so emitting
// immediately could race the pane's output subscription and drop the first
// events. 0 = start immediately.
func StartControlledPlayer(tl []TimedEvent, from time.Duration, speed float64, lead time.Duration, after func(time.Duration) <-chan time.Time, h PlayerHooks) *Player {
	if speed <= 0 {
		speed = 1
	}
	total := time.Duration(0)
	if len(tl) > 0 {
		total = tl[len(tl)-1].At
	}
	if from < 0 {
		from = 0
	}
	if from > total {
		from = total
	}
	p := &Player{
		stop: make(chan struct{}),
		done: make(chan struct{}),
		ctrl: make(chan ctrlMsg),
	}
	go p.runControlled(tl, from, speed, lead, total, after, h)
	return p
}

func (p *Player) runControlled(tl []TimedEvent, from time.Duration, speed float64, lead, total time.Duration, after func(time.Duration) <-chan time.Time, h PlayerHooks) {
	defer close(p.done)
	st := PlayerStatus{Pos: from, Total: total, Speed: speed}
	// i is the index of the next event to play: --from skips events before the
	// seek point (v1 semantics), so the pane starts at the seeked moment.
	i := 0
	for i < len(tl) && tl[i].At < from {
		i++
	}
	push := func() {
		p.mu.Lock()
		p.st = st
		p.mu.Unlock()
		if h.OnStatus != nil {
			h.OnStatus(st)
		}
	}
	emitTo := func(target time.Duration) {
		for i < len(tl) && tl[i].At <= target {
			if h.Emit != nil {
				h.Emit(tl[i].Text)
			}
			i++
		}
	}
	apply := func(c ctrlMsg) {
		switch c.kind {
		case ctrlPauseToggle:
			st.Paused = !st.Paused
		case ctrlSeek:
			target := st.Pos + c.seekBy
			if target < 0 {
				target = 0
			}
			if target > total {
				target = total
			}
			if target < st.Pos {
				// Backward seek re-renders from the start of the buffer at max
				// speed: clear the destination, then instantly re-emit
				// everything up to the target (documented v2 behavior).
				if h.Clear != nil {
					h.Clear()
				}
				i = 0
				emitTo(target)
			} else if target > st.Pos {
				// Forward seek fast-forwards: skipped events are emitted
				// instantly so no recorded output is lost.
				emitTo(target)
			}
			st.Pos = target
		case ctrlSpeed:
			st.Speed = adjustSpeed(st.Speed, c.speedUp)
		}
		push()
	}
	push()
	// Lead-in grace: give the freshly created replay pane time to subscribe
	// before the first emission. Controls and Stop still work during the wait.
	if lead > 0 {
		t := after(lead)
		for waiting := true; waiting; {
			select {
			case <-t:
				waiting = false
			case c := <-p.ctrl:
				apply(c)
			case <-p.stop:
				if h.OnDone != nil {
					h.OnDone(true)
				}
				return
			}
		}
	}
	for i < len(tl) {
		if st.Paused {
			select {
			case c := <-p.ctrl:
				apply(c)
			case <-p.stop:
				if h.OnDone != nil {
					h.OnDone(true)
				}
				return
			}
			continue
		}
		// A control action during a gap restarts that gap's wait from the last
		// emitted position (the player tracks recording time per emission, not
		// per wall-clock tick) — an accepted v2 simplification.
		gap := tl[i].At - st.Pos
		delay := time.Duration(0)
		if gap > 0 && !math.IsInf(st.Speed, 1) {
			delay = time.Duration(float64(gap) / st.Speed)
		}
		if delay > 0 {
			select {
			case <-after(delay):
			case c := <-p.ctrl:
				apply(c)
				continue
			case <-p.stop:
				if h.OnDone != nil {
					h.OnDone(true)
				}
				return
			}
		} else {
			// Zero-delay bursts must still notice Stop and controls promptly.
			select {
			case c := <-p.ctrl:
				apply(c)
				continue
			case <-p.stop:
				if h.OnDone != nil {
					h.OnDone(true)
				}
				return
			default:
			}
		}
		if h.Emit != nil {
			h.Emit(tl[i].Text)
		}
		st.Pos = tl[i].At
		i++
		push()
	}
	if h.OnDone != nil {
		h.OnDone(false)
	}
}

// adjustSpeed doubles/halves a playback speed within [minSpeed, maxSpeed].
// From "max" (+Inf), speed-up stays max and speed-down drops to maxSpeed.
func adjustSpeed(cur float64, up bool) float64 {
	if math.IsInf(cur, 1) {
		if up {
			return cur
		}
		return maxSpeed
	}
	if up {
		cur *= 2
		if cur > maxSpeed {
			cur = maxSpeed
		}
	} else {
		cur /= 2
		if cur < minSpeed {
			cur = minSpeed
		}
	}
	return cur
}

// sendCtrl delivers a control action to the player goroutine, giving up if the
// playback finishes (or is stopped) first. No-op on a v1 (uncontrolled) player.
func (p *Player) sendCtrl(c ctrlMsg) {
	if p.ctrl == nil {
		return
	}
	select {
	case p.ctrl <- c:
	case <-p.done:
	}
}

// TogglePause pauses a running playback or resumes a paused one.
func (p *Player) TogglePause() { p.sendCtrl(ctrlMsg{kind: ctrlPauseToggle}) }

// SeekBy moves the playback position by d (negative = backward). A backward
// seek clears the destination and re-renders from the start of the buffer at
// max speed; a forward seek instantly emits the skipped events.
func (p *Player) SeekBy(d time.Duration) { p.sendCtrl(ctrlMsg{kind: ctrlSeek, seekBy: d}) }

// SpeedUp doubles the playback speed (capped); SpeedDown halves it (floored).
func (p *Player) SpeedUp()   { p.sendCtrl(ctrlMsg{kind: ctrlSpeed, speedUp: true}) }
func (p *Player) SpeedDown() { p.sendCtrl(ctrlMsg{kind: ctrlSpeed, speedUp: false}) }

// Status returns the current playback status. ok=false on a v1 player, which
// tracks no position.
func (p *Player) Status() (PlayerStatus, bool) {
	if p.ctrl == nil {
		return PlayerStatus{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.st, true
}

// FormatBadge renders the REPLAY badge text for a status, e.g.
// "REPLAY 01:23/04:56 ×2" ("×max" at max speed, "⏸" when paused).
func FormatBadge(st PlayerStatus) string {
	b := fmt.Sprintf("REPLAY %s/%s", fmtClock(st.Pos), fmtClock(st.Total))
	if math.IsInf(st.Speed, 1) {
		b += " ×max"
	} else if st.Speed != 1 {
		b += fmt.Sprintf(" ×%s", strconv.FormatFloat(st.Speed, 'f', -1, 64))
	}
	if st.Paused {
		b += " ⏸"
	}
	return b
}

// fmtClock renders a duration as MM:SS (or H:MM:SS past an hour).
func fmtClock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int(d / time.Second)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, (s/60)%60, s%60)
	}
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

// Timeline returns the recording for a pane ("" = all panes merged) as offsets
// from the first event, plus nothing else — the controlled player's input.
// Falls back to the durable stream when the RAM ring is empty (post-restart),
// exactly like export and PlaybackPlan.
func (c *Capture) Timeline(paneID string) ([]TimedEvent, error) {
	evs := c.events(paneID)
	if len(evs) == 0 {
		evs = c.streamEvents(paneID)
	}
	if len(evs) == 0 {
		return nil, fmt.Errorf("no captured output%s", paneSuffix(paneID))
	}
	first := evs[0].ts
	tl := make([]TimedEvent, len(evs))
	for i, e := range evs {
		tl[i] = TimedEvent{At: time.Duration(e.ts-first) * time.Millisecond, Text: e.text}
	}
	return tl, nil
}

// ParseFromOffset parses a --from spec against a timeline's first recorded
// timestamp — the controlled-player counterpart of PlaybackPlan's parsing.
func (c *Capture) ParseFromOffset(paneID, fromSpec string) (time.Duration, error) {
	evs := c.events(paneID)
	if len(evs) == 0 {
		evs = c.streamEvents(paneID)
	}
	if len(evs) == 0 {
		return 0, fmt.Errorf("no captured output%s", paneSuffix(paneID))
	}
	return parseFrom(fromSpec, evs[0].ts)
}
