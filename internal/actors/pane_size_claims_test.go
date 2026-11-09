// SPDX-License-Identifier: Apache-2.0

package actors

// Pane PTY size arbitration: one pane has one PTY but can be on screen in
// several viewports at once (a terminal UI and a desktop-app window attached to
// the same daemon, or two of either). Each reports its own size, and the pane
// sizes the PTY to the SMALLEST of them so the grid fits inside every viewport
// showing it.
//
// Before this, each resize was a command that won last-writer-wins, so two
// attached front-ends fought over pty.Setsize and an interactive app reflowed
// on every frame.
//
// These tests exercise the arbitration directly (claims in, dimensions out).
// There is no PTY or VTerm behind them: handleResize tolerates a nil ptyFile
// and nil vtermEmu, writing only ptyRows/ptyCols, which is exactly the decision
// under test.

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// newSizeTestPane builds a PaneActor with just enough state for resize
// arbitration: no PTY and no VTerm (handleResize tolerates both being nil), but
// a real publisher, because every resize announces the new dimensions on the
// pane's .resized subject.
func newSizeTestPane(t *testing.T) *PaneActor {
	t.Helper()
	nc := startInProcessNATS(t)
	return &PaneActor{
		id:      "p1",
		pub:     msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry()),
		ptyRows: 24,
		ptyCols: 80,
	}
}

// liveClientID is a claim id whose pid is alive for the duration of the test.
func liveClientID() string { return fmt.Sprintf("tui:%d", os.Getpid()) }

func (p *PaneActor) size() (rows, cols int) { return int(p.ptyRows), int(p.ptyCols) }

// TestSmallestClaimWins is the core rule. A terminal and a desktop-app window
// showing the same pane must leave the PTY at dimensions BOTH can render: the
// minimum of each axis, taken independently, since one viewport can be wider
// while the other is taller.
func TestSmallestClaimWins(t *testing.T) {
	p := newSizeTestPane(t)

	p.claimPaneSize("web:1", 50, 200, false)
	if rows, cols := p.size(); rows != 50 || cols != 200 {
		t.Fatalf("single claim = %dx%d, want 50x200", rows, cols)
	}

	// A terminal attaches at a smaller size — the PTY must shrink to fit it,
	// not stay at the app's size (which the terminal would have to truncate).
	p.claimPaneSize(liveClientID(), 24, 80, false)
	if rows, cols := p.size(); rows != 24 || cols != 80 {
		t.Fatalf("after small claim = %dx%d, want 24x80", rows, cols)
	}

	// Axes are minimised independently: a short-but-wide viewport joins a
	// tall-but-narrow one and the result is short AND narrow.
	p2 := newSizeTestPane(t)
	p2.claimPaneSize("web:1", 60, 100, false) // tall, narrow
	p2.claimPaneSize("web:2", 20, 300, false) // short, wide
	if rows, cols := p2.size(); rows != 20 || cols != 100 {
		t.Errorf("mixed claims = %dx%d, want 20x100 (min of each axis)", rows, cols)
	}
}

// TestClaimReplacesOwnPrevious checks a viewport's later claim replaces its own
// earlier one rather than accumulating. Without this a window being dragged
// smaller then larger would ratchet the pane down and never let it back up.
func TestClaimReplacesOwnPrevious(t *testing.T) {
	p := newSizeTestPane(t)

	p.claimPaneSize("web:1", 20, 60, false)
	p.claimPaneSize("web:1", 60, 200, false) // same viewport, resized larger
	if rows, cols := p.size(); rows != 60 || cols != 200 {
		t.Errorf("after own re-claim = %dx%d, want 60x200 — a viewport must not "+
			"be permanently constrained by its own earlier size", rows, cols)
	}
}

// TestReleaseRestoresRoom covers a desktop-app window closing: the hub
// withdraws its claim and the pane expands to whatever viewports remain.
func TestReleaseRestoresRoom(t *testing.T) {
	p := newSizeTestPane(t)
	p.claimPaneSize("web:1", 60, 200, false)
	p.claimPaneSize("web:2", 24, 80, false)
	if rows, _ := p.size(); rows != 24 {
		t.Fatalf("expected the smaller claim to win, got %d rows", rows)
	}

	p.releasePaneSize("web:2")
	if rows, cols := p.size(); rows != 60 || cols != 200 {
		t.Errorf("after release = %dx%d, want 60x200", rows, cols)
	}

	// Releasing the last claim leaves the PTY exactly where it is. A detached
	// daemon keeps its shells running at their last size; resizing them for no
	// viewport would reflow long-running interactive apps for nobody.
	p.releasePaneSize("web:1")
	if rows, cols := p.size(); rows != 60 || cols != 200 {
		t.Errorf("after releasing the last claim = %dx%d, want it untouched at 60x200", rows, cols)
	}

	// Releasing an id that holds nothing is a no-op, not a resize.
	p.releasePaneSize("web:never-existed")
	if rows, cols := p.size(); rows != 60 || cols != 200 {
		t.Errorf("unknown release changed the size to %dx%d", rows, cols)
	}
}

// TestDeadTUIClaimIsPruned is the case a terminal UI can never report itself: it
// is killed outright, leaving its claim on record. The pane tests the pid
// instead, so a crash and a clean exit heal the same way.
func TestDeadTUIClaimIsPruned(t *testing.T) {
	p := newSizeTestPane(t)
	// A pid that is certainly not running. 0 and negatives are rejected by the
	// parser; this one is past any plausible live process in a test sandbox.
	p.claimPaneSize("tui:2147483646", 24, 80, false)
	p.claimPaneSize("web:1", 60, 200, false)

	// The dead terminal must not be holding the pane at 24x80.
	if rows, cols := p.size(); rows != 60 || cols != 200 {
		t.Errorf("dead TUI claim still constrains the pane: %dx%d, want 60x200", rows, cols)
	}
	if _, held := p.sizeClaims["tui:2147483646"]; held {
		t.Error("dead TUI claim was not pruned")
	}

	// A malformed tui: id is treated the same way — it can never be proven
	// alive, so it must not be able to clamp a pane forever.
	p2 := newSizeTestPane(t)
	p2.claimPaneSize("tui:not-a-pid", 24, 80, false)
	p2.claimPaneSize("web:1", 60, 200, false)
	if rows, _ := p2.size(); rows != 60 {
		t.Errorf("unparsable TUI claim still constrains the pane: %d rows", rows)
	}
}

// TestLiveTUIClaimSurvives is the other half: pruning must not throw away a
// terminal that is genuinely attached, or "smallest wins" would collapse into
// "the app always wins".
func TestLiveTUIClaimSurvives(t *testing.T) {
	p := newSizeTestPane(t)
	p.claimPaneSize(liveClientID(), 24, 80, false)
	p.claimPaneSize("web:1", 60, 200, false)
	p.pruneDeadSizeClaims()

	if _, held := p.sizeClaims[liveClientID()]; !held {
		t.Fatal("a live TUI's claim was pruned")
	}
	if rows, cols := p.size(); rows != 24 || cols != 80 {
		t.Errorf("size = %dx%d, want the live terminal's 24x80", rows, cols)
	}
}

// TestAnonymousClaimsShareOneKey pins backwards compatibility with a front-end
// build that predates client ids. Such a client sends no id, and every one of
// its resizes must replace the last rather than adding a fresh constraint —
// otherwise a stream of anonymous resizes would ratchet the pane to nothing.
func TestAnonymousClaimsShareOneKey(t *testing.T) {
	p := newSizeTestPane(t)
	p.claimPaneSize("", 20, 60, false)
	p.claimPaneSize("", 40, 120, false)
	p.claimPaneSize("", 60, 180, false)

	if len(p.sizeClaims) != 1 {
		t.Errorf("anonymous claims = %d entries, want 1 shared key", len(p.sizeClaims))
	}
	if rows, cols := p.size(); rows != 60 || cols != 180 {
		t.Errorf("size = %dx%d, want the latest anonymous claim 60x180", rows, cols)
	}
}

// TestInvalidClaimsIgnored: a viewport reporting nothing (a pane not yet laid
// out, a collapsed window) must not clamp the PTY to zero.
func TestInvalidClaimsIgnored(t *testing.T) {
	p := newSizeTestPane(t)
	p.claimPaneSize("web:1", 40, 120, false)
	p.claimPaneSize("web:2", 0, 0, false)
	p.claimPaneSize("web:3", -1, 50, false)

	if rows, cols := p.size(); rows != 40 || cols != 120 {
		t.Errorf("size = %dx%d, want the one valid claim 40x120", rows, cols)
	}
	if len(p.sizeClaims) != 1 {
		t.Errorf("claims = %v, want only the valid one recorded", p.sizeClaims)
	}
}

// TestReapStaleSizeClaimsIsRateLimited checks the sweep that runs off snapshot
// requests: it must heal a stale clamp, stay off the hot path in between, and
// never fire when there is only one claim (where dropping it would resize the
// pane for no viewport at all).
func TestReapStaleSizeClaimsIsRateLimited(t *testing.T) {
	p := newSizeTestPane(t)
	p.claimPaneSize("tui:2147483646", 24, 80, false) // pruned on arrival
	p.claimPaneSize("web:1", 60, 200, false)

	// Re-introduce a dead claim WITHOUT going through claimPaneSize (which
	// prunes eagerly), to model a viewport that died after claiming.
	p.sizeClaims["tui:2147483646"] = paneSizeClaim{rows: 24, cols: 80}
	p.handleResize(24, 80) // the pane is now clamped by a dead viewport
	p.lastSizeClaimReap = time.Now()

	// Inside the rate-limit window: no sweep, so the stale clamp stands.
	p.reapStaleSizeClaims()
	if rows, _ := p.size(); rows != 24 {
		t.Errorf("swept inside the rate-limit window (rows=%d)", rows)
	}

	// Past it: the dead claim goes and the pane gets its room back.
	p.lastSizeClaimReap = time.Now().Add(-2 * sizeClaimReapInterval)
	p.reapStaleSizeClaims()
	if rows, cols := p.size(); rows != 60 || cols != 200 {
		t.Errorf("after sweep = %dx%d, want 60x200", rows, cols)
	}

	// A single claim is left alone whatever its state: there is nothing to fall
	// back to, so dropping it would size the pane for no viewport.
	single := newSizeTestPane(t)
	single.sizeClaims = map[string]paneSizeClaim{"tui:2147483646": {rows: 24, cols: 80}}
	single.lastSizeClaimReap = time.Now().Add(-2 * sizeClaimReapInterval)
	single.reapStaleSizeClaims()
	if len(single.sizeClaims) != 1 {
		t.Error("a lone claim was reaped — nothing would be left to size the pane")
	}
}

// ---------------------------------------------------------------------------
// Shrink settling (backlog F-52 mitigation)
// ---------------------------------------------------------------------------
//
// Narrowing the PTY destroys the glyphs past the new width — vt10x copies each
// row into a narrower one and cannot reflow, so a shell's already-committed
// lines keep the hole permanently. Growing costs nothing. So a shrink waits one
// paneSizeSettleWindow for the claim set to stop moving, and a claim that was
// only ever transient never reaches the PTY at all.
//
// These tests drive the settle by hand rather than sleeping on the real timer's
// callback: sizeSettleSend is what production uses to hop back onto the mailbox
// goroutine, so a test that signals through it and then calls
// applySettledPaneSize itself is running exactly the production sequence,
// without reading pane state off the timer goroutine.

// settleHandshake wires the pane's settle wake-up to a channel and returns it.
func settleHandshake(p *PaneActor) chan struct{} {
	fired := make(chan struct{}, 4)
	p.sizeSettleSend = func() { fired <- struct{}{} }
	return fired
}

// TestTransientNarrowClaimNeverReachesPTY is the whole point of the settle
// window. A viewport that measures itself mid-layout, or opens and closes
// again, used to clip every row on screen on its way past.
func TestTransientNarrowClaimNeverReachesPTY(t *testing.T) {
	p := newSizeTestPane(t)
	fired := settleHandshake(p)

	p.claimPaneSize("web:1", 50, 200, false) // grows — immediate
	if rows, cols := p.size(); rows != 50 || cols != 200 {
		t.Fatalf("after growing claim = %dx%d, want 50x200", rows, cols)
	}

	// A transient viewport appears and claims something much smaller.
	p.claimPaneSize(liveClientID(), 24, 70, false)
	if rows, cols := p.size(); rows != 50 || cols != 200 {
		t.Fatalf("shrink applied immediately = %dx%d, want it held at 50x200", rows, cols)
	}
	if p.sizeSettleTimer == nil {
		t.Fatal("no settle armed for a shrink")
	}

	// ...and goes away again before the window elapses.
	p.releasePaneSize(liveClientID())
	if rows, cols := p.size(); rows != 50 || cols != 200 {
		t.Fatalf("after release = %dx%d, want 50x200 untouched", rows, cols)
	}
	if p.sizeSettleTimer != nil {
		t.Error("pending shrink not cancelled once the claim that armed it went away")
	}

	select {
	case <-fired:
		t.Error("settle fired for a claim that was withdrawn")
	case <-time.After(paneSizeSettleWindow + 100*time.Millisecond):
	}
}

// TestGenuineNarrowLandsAfterSettle — the window delays a real shrink, it does
// not veto it. A second front-end that really is smaller must still win, or the
// arbitration this file exists to test would be broken.
func TestGenuineNarrowLandsAfterSettle(t *testing.T) {
	p := newSizeTestPane(t)
	fired := settleHandshake(p)

	p.claimPaneSize("web:1", 50, 200, false)
	p.claimPaneSize(liveClientID(), 24, 70, false)

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("settle never fired for a shrink that stood")
	}
	p.applySettledPaneSize() // what the mailbox does on paneSizeSettleMsg

	if rows, cols := p.size(); rows != 24 || cols != 70 {
		t.Fatalf("settled size = %dx%d, want 24x70 (smallest claim)", rows, cols)
	}
}

// TestShrinkBurstCoalescesToOneResize — claims keep arriving while a shrink is
// pending. They must update the answer without each arming its own timer, and
// the PTY must move once, to the final value, rather than stepping down through
// every intermediate size (each step being a destructive clip).
func TestShrinkBurstCoalescesToOneResize(t *testing.T) {
	p := newSizeTestPane(t)
	fired := settleHandshake(p)

	p.claimPaneSize("web:1", 50, 200, false)
	p.claimPaneSize(liveClientID(), 24, 70, false)
	armed := p.sizeSettleTimer
	if armed == nil {
		t.Fatal("no settle armed")
	}

	// More claims land inside the window, each smaller than the last.
	p.claimPaneSize("web:1", 40, 90, false)
	p.claimPaneSize("web:1", 30, 80, false)
	if p.sizeSettleTimer != armed {
		t.Error("a claim inside the window re-armed the timer; shrinks must coalesce, " +
			"or a fast re-claiming front-end defers the resize indefinitely")
	}
	if rows, cols := p.size(); rows != 50 || cols != 200 {
		t.Fatalf("PTY moved mid-burst = %dx%d, want it held at 50x200", rows, cols)
	}

	<-fired
	p.applySettledPaneSize()

	// min(30,24) x min(80,70) — the whole burst, resolved once.
	if rows, cols := p.size(); rows != 24 || cols != 70 {
		t.Fatalf("settled size = %dx%d, want 24x70", rows, cols)
	}
}

// TestGrowIsNotDeferred — growing cannot lose anything, so it must not pay the
// settle latency. An interactive app given more room should get it now.
func TestGrowIsNotDeferred(t *testing.T) {
	p := newSizeTestPane(t)
	settleHandshake(p)

	p.claimPaneSize("web:1", 100, 300, false)
	if rows, cols := p.size(); rows != 100 || cols != 300 {
		t.Fatalf("grow = %dx%d, want 100x300 applied immediately", rows, cols)
	}
	if p.sizeSettleTimer != nil {
		t.Error("grow armed a settle timer")
	}
}

// TestArbitrationStaysInlineWithoutMailbox pins the escape hatch the other
// tests in this file rely on: with no actor system behind it there is nowhere
// to defer to, so a shrink applies inline and the arbitration result is
// observable synchronously.
func TestArbitrationStaysInlineWithoutMailbox(t *testing.T) {
	p := newSizeTestPane(t) // sizeSettleSend left nil

	p.claimPaneSize("web:1", 50, 200, false)
	p.claimPaneSize(liveClientID(), 24, 70, false)
	if rows, cols := p.size(); rows != 24 || cols != 70 {
		t.Fatalf("inline shrink = %dx%d, want 24x70", rows, cols)
	}
	if p.sizeSettleTimer != nil {
		t.Error("inline path armed a timer")
	}
}

// ---------------------------------------------------------------------------
// Idle terminal UIs (a window left attached where nobody types)
// ---------------------------------------------------------------------------
//
// Two live pids stand in for two attached terminal UIs: this test process and
// its parent. viewportLastInput replaces the tty read, so each test says when
// each terminal last had input.

// twoTUIClaimIDs returns claim ids for two distinct live processes.
func twoTUIClaimIDs() (a, b string, pidA, pidB int) {
	pidA, pidB = os.Getpid(), os.Getppid()
	return fmt.Sprintf("tui:%d", pidA), fmt.Sprintf("tui:%d", pidB), pidA, pidB
}

// stubLastInput wires the pane's input clock to a map the test edits.
func stubLastInput(p *PaneActor) map[int]time.Time {
	inputs := map[int]time.Time{}
	p.viewportLastInput = func(pid int) (time.Time, bool) {
		at, ok := inputs[pid]
		return at, ok
	}
	return inputs
}

// TestIdleTUIStopsClampingPane is the reported case: a 120x30 Terminal window
// left attached on the Mac mini held every pane it showed to 46x11 inside the
// user's 287x73 ssh window. A terminal UI idle well past the one in use no
// longer counts, so the pane fills the window being typed in.
func TestIdleTUIStopsClampingPane(t *testing.T) {
	p := newSizeTestPane(t)
	inputs := stubLastInput(p)
	small, big, pidSmall, pidBig := twoTUIClaimIDs()
	now := time.Now()
	inputs[pidSmall] = now.Add(-20 * time.Minute)
	inputs[pidBig] = now

	p.claimPaneSize(small, 11, 46, false)
	p.claimPaneSize(big, 54, 129, false)
	if rows, cols := p.size(); rows != 54 || cols != 129 {
		t.Fatalf("with the small window idle = %dx%d, want 54x129", rows, cols)
	}

	// Someone types in the small window: both are in use, so the pane goes back
	// to a grid both can render.
	inputs[pidSmall] = now.Add(time.Second)
	p.claimPaneSize(big, 54, 129, false)
	if rows, cols := p.size(); rows != 11 || cols != 46 {
		t.Fatalf("with both windows in use = %dx%d, want 11x46", rows, cols)
	}
}

// TestRecentInputOnBothKeepsSmallest: two terminal UIs typed into within
// viewportIdleWindow of each other are both in use, so smallest still wins.
func TestRecentInputOnBothKeepsSmallest(t *testing.T) {
	p := newSizeTestPane(t)
	inputs := stubLastInput(p)
	a, b, pidA, pidB := twoTUIClaimIDs()
	now := time.Now()
	inputs[pidA] = now.Add(-viewportIdleWindow / 2)
	inputs[pidB] = now

	p.claimPaneSize(a, 20, 60, false)
	p.claimPaneSize(b, 50, 200, false)
	if rows, cols := p.size(); rows != 20 || cols != 60 {
		t.Errorf("= %dx%d, want 20x60 (both in use)", rows, cols)
	}
}

// TestMostRecentTUICountsHoweverOld: the margin is relative to the newest
// input, not to the clock, so a pane watched for an hour without a keystroke
// keeps the size of the terminal last typed into.
func TestMostRecentTUICountsHoweverOld(t *testing.T) {
	p := newSizeTestPane(t)
	inputs := stubLastInput(p)
	small, big, pidSmall, pidBig := twoTUIClaimIDs()
	now := time.Now()
	inputs[pidSmall] = now.Add(-3 * time.Hour)
	inputs[pidBig] = now.Add(-time.Hour)

	p.claimPaneSize(small, 11, 46, false)
	p.claimPaneSize(big, 54, 129, false)
	if rows, cols := p.size(); rows != 54 || cols != 129 {
		t.Errorf("= %dx%d, want 54x129 (the terminal typed into last)", rows, cols)
	}
}

// TestUnknownInputCountsAsInUse: a terminal UI whose tty cannot be read keeps
// the smallest-wins rule, as do web claims, whose input the daemon never sees.
func TestUnknownInputCountsAsInUse(t *testing.T) {
	p := newSizeTestPane(t)
	inputs := stubLastInput(p)
	small, big, _, pidBig := twoTUIClaimIDs()
	inputs[pidBig] = time.Now()
	// small has no entry: its input time is unknown.
	p.claimPaneSize(small, 11, 46, false)
	p.claimPaneSize(big, 54, 129, false)
	if rows, cols := p.size(); rows != 11 || cols != 46 {
		t.Errorf("unknown tty = %dx%d, want 11x46", rows, cols)
	}

	w := newSizeTestPane(t)
	stubLastInput(w)[os.Getpid()] = time.Now()
	w.claimPaneSize("web:1", 30, 90, false)
	w.claimPaneSize(liveClientID(), 54, 129, false)
	if rows, cols := w.size(); rows != 30 || cols != 90 {
		t.Errorf("web claim = %dx%d, want 30x90 (web is never idle)", rows, cols)
	}
}

// TestReapLiftsClampWhenTUIGoesIdle: typing in one terminal sends no claim, so
// the snapshot-driven sweep is what notices the other one went idle.
func TestReapLiftsClampWhenTUIGoesIdle(t *testing.T) {
	p := newSizeTestPane(t)
	inputs := stubLastInput(p)
	small, big, pidSmall, pidBig := twoTUIClaimIDs()
	now := time.Now()
	inputs[pidSmall] = now
	inputs[pidBig] = now
	p.claimPaneSize(small, 11, 46, false)
	p.claimPaneSize(big, 54, 129, false)
	if rows, cols := p.size(); rows != 11 || cols != 46 {
		t.Fatalf("both in use = %dx%d, want 11x46", rows, cols)
	}

	inputs[pidBig] = now.Add(time.Minute) // the user types in the big window
	p.lastSizeClaimReap = time.Now().Add(-2 * sizeClaimReapInterval)
	p.reapStaleSizeClaims()
	if rows, cols := p.size(); rows != 54 || cols != 129 {
		t.Errorf("after sweep = %dx%d, want 54x129", rows, cols)
	}
}

// TestReapLeavesOverrideAlone: with no claim gone and no change in who is
// idle, the sweep must not re-arbitrate over a deliberate Override resize (a
// share subscriber's fullscreen at its own resolution).
func TestReapLeavesOverrideAlone(t *testing.T) {
	p := newSizeTestPane(t)
	inputs := stubLastInput(p)
	a, b, pidA, pidB := twoTUIClaimIDs()
	inputs[pidA] = time.Now()
	inputs[pidB] = time.Now()
	p.claimPaneSize(a, 20, 60, false)
	p.claimPaneSize(b, 50, 200, false)

	p.handleResize(70, 250) // Override
	p.lastSizeClaimReap = time.Now().Add(-2 * sizeClaimReapInterval)
	p.reapStaleSizeClaims()
	if rows, cols := p.size(); rows != 70 || cols != 250 {
		t.Errorf("sweep undid the override: %dx%d, want 70x250", rows, cols)
	}
}

// TestPassiveClaimDoesNotShrinkActiveViewport is the phone-over-tunnel bug: the
// mobile UI joining a pane a desktop window is showing must not clamp the PTY to
// the phone's ~45 columns. A passive claim only counts when no active claim is
// left, and the desktop's size returns the moment the phone is the only viewer
// it isn't.
func TestPassiveClaimDoesNotShrinkActiveViewport(t *testing.T) {
	p := newSizeTestPane(t)
	p.claimPaneSize("web:1", 50, 200, false)

	p.claimPaneSize("web:2", 30, 45, true) // phone connects
	if rows, cols := p.size(); rows != 50 || cols != 200 {
		t.Fatalf("after passive claim = %dx%d, want 50x200 — a phone must not "+
			"shrink a pane a desktop viewport is showing", rows, cols)
	}

	// Desktop window closes: the phone is all that is left, so it sizes the pane.
	p.releasePaneSize("web:1")
	if rows, cols := p.size(); rows != 30 || cols != 45 {
		t.Fatalf("passive claim alone = %dx%d, want 30x45", rows, cols)
	}

	// A desktop viewport returns and takes the pane back.
	p.claimPaneSize("web:3", 50, 200, false)
	if rows, cols := p.size(); rows != 50 || cols != 200 {
		t.Fatalf("active claim over passive = %dx%d, want 50x200", rows, cols)
	}

	// Two passive claims with nothing active still take the minimum.
	p2 := newSizeTestPane(t)
	p2.claimPaneSize("web:1", 30, 45, true)
	p2.claimPaneSize("web:2", 40, 40, true)
	if rows, cols := p2.size(); rows != 30 || cols != 40 {
		t.Errorf("passive-only claims = %dx%d, want 30x40", rows, cols)
	}
}
