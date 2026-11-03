// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"testing"
	"time"
)

var promptRows = screen(
	"│ Do you want to proceed?                              │",
	"│ ❯ 1. Yes                                             │",
	"│   3. No, and tell Claude what to do differently (esc)│",
)

var plainRows = screen(filler(5)...)

type trackerRig struct {
	t      *testing.T
	tr     attentionTracker
	base   time.Time
	rows   []string
	reads  int
	fg     string
	output time.Time
	input  time.Time
}

func newTrackerRig(t *testing.T) *trackerRig {
	base := time.Unix(1_700_000_000, 0)
	return &trackerRig{t: t, base: base, fg: "claude", rows: plainRows}
}

func (r *trackerRig) at(ms int) time.Time { return r.base.Add(time.Duration(ms) * time.Millisecond) }

func (r *trackerRig) step(nowMS int) (changed, active bool) {
	return r.tr.step(attentionInput{
		now:        r.at(nowMS),
		lastOutput: r.output,
		lastInput:  r.input,
		program:    r.fg,
		rows: func() []string {
			r.reads++
			return r.rows
		},
	})
}

func TestTrackerRaisesOnceTheScreenIsQuiet(t *testing.T) {
	r := newTrackerRig(t)
	r.rows = promptRows
	r.output = r.at(0)
	if changed, active := r.step(100); changed || active {
		t.Fatalf("raised while output is still flowing (changed=%v active=%v)", changed, active)
	}
	if changed, active := r.step(int(attentionQuiet/time.Millisecond) + 10); !changed || !active {
		t.Fatalf("not raised after a quiet window (changed=%v active=%v)", changed, active)
	}
	if changed, active := r.step(5000); changed || !active {
		t.Fatalf("a steady prompt flipped state (changed=%v active=%v)", changed, active)
	}
}

func TestTrackerIgnoresAPaneWithNoForegroundProgram(t *testing.T) {
	r := newTrackerRig(t)
	r.fg = "" // the shell itself: `cat` of matching text must not raise
	r.rows = promptRows
	r.output = r.at(0)
	if _, active := r.step(2000); active {
		t.Fatal("raised with no foreground program")
	}
}

func TestTrackerClearsOnUserInputAndStaysClearUntilNewOutput(t *testing.T) {
	r := newTrackerRig(t)
	r.rows = promptRows
	r.output = r.at(0)
	if _, active := r.step(1000); !active {
		t.Fatal("setup: not raised")
	}
	r.input = r.at(1500) // the user typed into the pane
	if changed, active := r.step(1600); !changed || active {
		t.Fatalf("user input did not clear (changed=%v active=%v)", changed, active)
	}
	// The screen still shows the old prompt (no new output yet): stay clear.
	if _, active := r.step(3000); active {
		t.Fatal("re-raised from a screen the user already answered")
	}
	// New output, and the agent asks again.
	r.output = r.at(4000)
	if _, active := r.step(5000); !active {
		t.Fatal("a new prompt after new output was not raised")
	}
}

func TestTrackerClearsWhenThePromptGoes(t *testing.T) {
	r := newTrackerRig(t)
	r.rows = promptRows
	r.output = r.at(0)
	if _, active := r.step(1000); !active {
		t.Fatal("setup: not raised")
	}
	r.rows = plainRows
	r.output = r.at(2000)
	if changed, active := r.step(3000); !changed || active {
		t.Fatalf("prompt gone but still raised (changed=%v active=%v)", changed, active)
	}
}

func TestTrackerClearsWhenTheProgramExits(t *testing.T) {
	r := newTrackerRig(t)
	r.rows = promptRows
	r.output = r.at(0)
	if _, active := r.step(1000); !active {
		t.Fatal("setup: not raised")
	}
	r.fg = ""
	if changed, active := r.step(1200); !changed || active {
		t.Fatalf("program gone but still raised (changed=%v active=%v)", changed, active)
	}
}

func TestTrackerReadsTheScreenOnlyAfterNewOutput(t *testing.T) {
	r := newTrackerRig(t)
	r.output = r.at(0)
	r.step(1000)
	reads := r.reads
	if reads == 0 {
		t.Fatal("precondition: the screen was never read after new output")
	}
	for ms := 1300; ms < 5000; ms += 300 {
		r.step(ms)
	}
	if r.reads != reads {
		t.Fatalf("screen read %d more times with no new output", r.reads-reads)
	}
}
