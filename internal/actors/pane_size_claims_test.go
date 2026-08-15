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

	p.claimPaneSize("web:1", 50, 200)
	if rows, cols := p.size(); rows != 50 || cols != 200 {
		t.Fatalf("single claim = %dx%d, want 50x200", rows, cols)
	}

	// A terminal attaches at a smaller size — the PTY must shrink to fit it,
	// not stay at the app's size (which the terminal would have to truncate).
	p.claimPaneSize(liveClientID(), 24, 80)
	if rows, cols := p.size(); rows != 24 || cols != 80 {
		t.Fatalf("after small claim = %dx%d, want 24x80", rows, cols)
	}

	// Axes are minimised independently: a short-but-wide viewport joins a
	// tall-but-narrow one and the result is short AND narrow.
	p2 := newSizeTestPane(t)
	p2.claimPaneSize("web:1", 60, 100) // tall, narrow
	p2.claimPaneSize("web:2", 20, 300) // short, wide
	if rows, cols := p2.size(); rows != 20 || cols != 100 {
		t.Errorf("mixed claims = %dx%d, want 20x100 (min of each axis)", rows, cols)
	}
}

// TestClaimReplacesOwnPrevious checks a viewport's later claim replaces its own
// earlier one rather than accumulating. Without this a window being dragged
// smaller then larger would ratchet the pane down and never let it back up.
func TestClaimReplacesOwnPrevious(t *testing.T) {
	p := newSizeTestPane(t)

	p.claimPaneSize("web:1", 20, 60)
	p.claimPaneSize("web:1", 60, 200) // same viewport, resized larger
	if rows, cols := p.size(); rows != 60 || cols != 200 {
		t.Errorf("after own re-claim = %dx%d, want 60x200 — a viewport must not "+
			"be permanently constrained by its own earlier size", rows, cols)
	}
}

// TestReleaseRestoresRoom covers a desktop-app window closing: the hub
// withdraws its claim and the pane expands to whatever viewports remain.
func TestReleaseRestoresRoom(t *testing.T) {
	p := newSizeTestPane(t)
	p.claimPaneSize("web:1", 60, 200)
	p.claimPaneSize("web:2", 24, 80)
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
	p.claimPaneSize("tui:2147483646", 24, 80)
	p.claimPaneSize("web:1", 60, 200)

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
	p2.claimPaneSize("tui:not-a-pid", 24, 80)
	p2.claimPaneSize("web:1", 60, 200)
	if rows, _ := p2.size(); rows != 60 {
		t.Errorf("unparsable TUI claim still constrains the pane: %d rows", rows)
	}
}

// TestLiveTUIClaimSurvives is the other half: pruning must not throw away a
// terminal that is genuinely attached, or "smallest wins" would collapse into
// "the app always wins".
func TestLiveTUIClaimSurvives(t *testing.T) {
	p := newSizeTestPane(t)
	p.claimPaneSize(liveClientID(), 24, 80)
	p.claimPaneSize("web:1", 60, 200)
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
	p.claimPaneSize("", 20, 60)
	p.claimPaneSize("", 40, 120)
	p.claimPaneSize("", 60, 180)

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
	p.claimPaneSize("web:1", 40, 120)
	p.claimPaneSize("web:2", 0, 0)
	p.claimPaneSize("web:3", -1, 50)

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
	p.claimPaneSize("tui:2147483646", 24, 80) // pruned on arrival
	p.claimPaneSize("web:1", 60, 200)

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

	p.claimPaneSize("web:1", 50, 200) // grows — immediate
	if rows, cols := p.size(); rows != 50 || cols != 200 {
		t.Fatalf("after growing claim = %dx%d, want 50x200", rows, cols)
	}

	// A transient viewport appears and claims something much smaller.
	p.claimPaneSize(liveClientID(), 24, 70)
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

	p.claimPaneSize("web:1", 50, 200)
	p.claimPaneSize(liveClientID(), 24, 70)

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

	p.claimPaneSize("web:1", 50, 200)
	p.claimPaneSize(liveClientID(), 24, 70)
	armed := p.sizeSettleTimer
	if armed == nil {
		t.Fatal("no settle armed")
	}

	// More claims land inside the window, each smaller than the last.
	p.claimPaneSize("web:1", 40, 90)
	p.claimPaneSize("web:1", 30, 80)
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

	p.claimPaneSize("web:1", 100, 300)
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

	p.claimPaneSize("web:1", 50, 200)
	p.claimPaneSize(liveClientID(), 24, 70)
	if rows, cols := p.size(); rows != 24 || cols != 70 {
		t.Fatalf("inline shrink = %dx%d, want 24x70", rows, cols)
	}
	if p.sizeSettleTimer != nil {
		t.Error("inline path armed a timer")
	}
}
