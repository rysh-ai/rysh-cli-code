// SPDX-License-Identifier: Apache-2.0

package actors

// Tests for the per-pane mirror content plane (shared-tab interactive
// responsiveness): listener-side render throttling + watch gating, the
// workspace's per-pane VT endpoint and transition-aware dirty fan-out, the
// layout-only screen stripping, and the source-side focus-aware raw forwarding.

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// rawFrame builds the JSON payload handleRawFrame expects, carrying b as the
// base64 raw bytes.
func rawFrame(b []byte) []byte {
	data, _ := json.Marshal(map[string]string{
		"type": "raw",
		"data": base64.StdEncoding.EncodeToString(b),
	})
	return data
}

func newTestMirrorListener() *MirrorTabListenerActor {
	// nil pub / workspacePID: pushPaneVT and upstream publishes become no-ops,
	// which is fine — these tests assert throttle/watch STATE, not delivery.
	return NewMirrorTabListenerActor("share-x", "alias", "view", "me", config.UpstreamConfig{}, nil, nil)
}

func TestMirrorListenerRawFrameThrottle(t *testing.T) {
	r := newTestMirrorListener()

	// First frame: leading-edge emit (lastEmit set, no trailing flush armed).
	r.handleRawFrame("p1", rawFrame([]byte("hello")))
	pv := r.vterms["p1"]
	if pv == nil {
		t.Fatalf("VTerm not created on first raw frame")
	}
	if pv.lastEmit.IsZero() {
		t.Fatalf("first frame should emit on the leading edge")
	}
	if pv.flushPending {
		t.Fatalf("no trailing flush should be armed after a leading-edge emit")
	}
	firstEmit := pv.lastEmit

	// Second frame inside the throttle window: bytes applied, emit deferred.
	r.handleRawFrame("p1", rawFrame([]byte(" world")))
	if !pv.flushPending {
		t.Fatalf("second frame within the window should arm a trailing flush")
	}
	if pv.lastEmit != firstEmit {
		t.Fatalf("second frame within the window should not re-emit")
	}

	// The trailing flush emits the final frame and clears the pending flag.
	r.trailingFlushPane("p1")
	if pv.flushPending {
		t.Fatalf("trailing flush should clear flushPending")
	}
	if !pv.lastEmit.After(firstEmit) {
		t.Fatalf("trailing flush should emit (advance lastEmit)")
	}

	// A frame after the window has elapsed emits on the leading edge again.
	pv.lastEmit = time.Now().Add(-time.Second)
	r.handleRawFrame("p1", rawFrame([]byte("!")))
	if pv.flushPending {
		t.Fatalf("expected leading-edge emit after window elapsed")
	}
}

func TestMirrorListenerWatchGating(t *testing.T) {
	r := newTestMirrorListener()
	r.handleRawFrame("p1", rawFrame([]byte("x")))
	r.handleRawFrame("p2", rawFrame([]byte("y")))

	// No watch info yet: every pane renders at full rate.
	r.mu.Lock()
	if got := r.paneRenderIntervalLocked("p1"); got != rawRenderInterval {
		t.Fatalf("before watch info, interval = %v, want %v", got, rawRenderInterval)
	}
	r.mu.Unlock()

	// Watch only p1 (no focus subset => legacy: watched treated as focused). p2
	// drops to the slow cadence.
	r.applyWatchSet([]string{"p1"}, nil)
	r.mu.Lock()
	if got := r.paneRenderIntervalLocked("p1"); got != rawRenderInterval {
		t.Errorf("watched pane interval = %v, want %v", got, rawRenderInterval)
	}
	if got := r.paneRenderIntervalLocked("p2"); got != mirrorUnwatchedRenderInterval {
		t.Errorf("unwatched pane interval = %v, want %v", got, mirrorUnwatchedRenderInterval)
	}
	r.mu.Unlock()

	// Newly-watched pane is emitted immediately (focus switch snaps fresh).
	pv2 := r.vterms["p2"]
	pv2.lastEmit = time.Time{}
	r.applyWatchSet([]string{"p2"}, nil)
	if pv2.lastEmit.IsZero() {
		t.Fatalf("newly-watched pane should be emitted immediately")
	}

	// Empty (non-nil) watch set: everything unwatched.
	r.applyWatchSet(nil, nil)
	r.mu.Lock()
	if got := r.paneRenderIntervalLocked("p1"); got != mirrorUnwatchedRenderInterval {
		t.Errorf("after empty watch set, interval = %v, want slow", got)
	}
	r.mu.Unlock()
}

func TestMirrorListenerFocusTiering(t *testing.T) {
	r := newTestMirrorListener()
	r.handleRawFrame("p1", rawFrame([]byte("x")))
	r.handleRawFrame("p2", rawFrame([]byte("y")))

	// Both visible, only p1 focused: p1 renders at full rate, p2 at the medium
	// visible rate (the two-interactive-pane case).
	r.applyWatchSet([]string{"p1", "p2"}, []string{"p1"})
	r.mu.Lock()
	if got := r.paneRenderIntervalLocked("p1"); got != rawRenderInterval {
		t.Errorf("focused pane interval = %v, want %v", got, rawRenderInterval)
	}
	if got := r.paneRenderIntervalLocked("p2"); got != mirrorVisibleRenderInterval {
		t.Errorf("visible-unfocused pane interval = %v, want %v", got, mirrorVisibleRenderInterval)
	}
	r.mu.Unlock()

	// Switching focus to p2 snaps it to full rate and emits it immediately, even
	// though it was already visible.
	pv2 := r.vterms["p2"]
	pv2.lastEmit = time.Time{}
	r.applyWatchSet([]string{"p1", "p2"}, []string{"p2"})
	if pv2.lastEmit.IsZero() {
		t.Fatalf("newly-focused pane should be emitted immediately")
	}
	r.mu.Lock()
	if got := r.paneRenderIntervalLocked("p2"); got != rawRenderInterval {
		t.Errorf("newly-focused pane interval = %v, want %v", got, rawRenderInterval)
	}
	if got := r.paneRenderIntervalLocked("p1"); got != mirrorVisibleRenderInterval {
		t.Errorf("now-unfocused visible pane interval = %v, want %v", got, mirrorVisibleRenderInterval)
	}
	r.mu.Unlock()
}

func TestMirrorListenerVTFramePublish(t *testing.T) {
	r := newTestMirrorListener()

	// First frame: a leading-edge emit builds a KEYFRAME (no prior send state) and
	// advances the per-pane sequence. Send-state is updated even with a nil
	// publisher, so the keyframe/delta/seq logic is observable here.
	r.handleRawFrame("p1", rawFrame([]byte("hello")))
	pv := r.vterms["p1"]
	if pv == nil {
		t.Fatalf("VTerm not created")
	}
	if pv.vtSeq != 1 {
		t.Fatalf("first emit should advance seq to 1, got %d", pv.vtSeq)
	}
	if len(pv.vtLastSent) == 0 {
		t.Fatalf("first emit should record the sent screen as the delta base")
	}
	if pv.vtLastKeyframeAt.IsZero() {
		t.Fatalf("first emit should be a keyframe (lastKeyframeAt set)")
	}

	// A second emit (past the render throttle) advances the sequence again.
	pv.lastEmit = time.Now().Add(-time.Second)
	r.handleRawFrame("p1", rawFrame([]byte(" world")))
	if pv.vtSeq != 2 {
		t.Fatalf("second emit should advance seq to 2, got %d", pv.vtSeq)
	}

	// Becoming newly-watched force-emits a keyframe (vtLastSent reset) so a pane
	// that just became visible backfills the whole screen — this replaces the old
	// on-visibility resync pull to the WorkspaceActor.
	r.applyWatchSet([]string{"p1"}, []string{"p1"})
	if pv.vtSeq < 3 {
		t.Fatalf("force-keyframe on watch should emit (seq advanced), got %d", pv.vtSeq)
	}
}

func TestMirrorListenerModeLeaveStopsFlushTimer(t *testing.T) {
	r := newTestMirrorListener()
	r.handleRawFrame("p1", rawFrame([]byte("a")))
	r.handleRawFrame("p1", rawFrame([]byte("b"))) // arms the trailing flush

	leave, _ := json.Marshal(map[string]interface{}{"type": "mode", "interactive": false})
	r.handleModeFrame("p1", leave)
	if r.vterms["p1"] != nil {
		t.Fatalf("mode leave should drop the pane's VTerm")
	}
	// The armed timer firing later must be a no-op (pv gone).
	r.trailingFlushPane("p1")
}

func TestParseMirrorPaneID(t *testing.T) {
	share, src := parseMirrorPaneID(mirrorPaneID("s-1", "p-9"))
	if share != "s-1" || src != "p-9" {
		t.Errorf("round trip failed: %q %q", share, src)
	}
	for _, bad := range []string{"", "p-9", "mirror:onlyshare", "other:s:p"} {
		if share, src := parseMirrorPaneID(bad); share != "" || src != "" {
			t.Errorf("malformed %q parsed to %q/%q", bad, share, src)
		}
	}
}

func TestDisplayTabInteractiveRenderModes(t *testing.T) {
	src := sampleTab()
	// p2 is interactive per the layout doc only (degraded: no live VT stream).
	src.Lanes[1].PaneGroups[0].Panes[0].RawMode = true
	src.Lanes[1].PaneGroups[0].Panes[0].VTScreen = []string{"doc-screen"}
	mt := &mirrorTab{shareID: "s", mode: "control", hasData: true}
	mt.snap = src
	// p1 is live-interactive: its screen streams to the TUI via vtframe, never
	// through the snapshot.
	mt.liveInteractive = map[string]bool{"p1": true}

	full := mt.displayTab(false)
	p1 := full.Lanes[0].PaneGroups[0].Panes[0]
	p2 := full.Lanes[1].PaneGroups[0].Panes[0]
	// Live pane: RemoteInteractive, but the screen is NEVER embedded in the snapshot.
	if !p1.RemoteInteractive {
		t.Fatalf("live pane should be RemoteInteractive: %+v", p1)
	}
	if len(p1.RemoteVTScreen) != 0 {
		t.Errorf("live pane screen must come from vtframe, not the snapshot: %v", p1.RemoteVTScreen)
	}
	if p1.ControllingShareID != "s" {
		t.Errorf("live interactive pane must keep control metadata")
	}
	// Degraded pane: RemoteInteractive AND carries the doc VT seed (no vtframe).
	if !p2.RemoteInteractive || len(p2.RemoteVTScreen) == 0 {
		t.Fatalf("degraded pane should carry the doc VT seed: %+v", p2)
	}

	// layoutOnly does not change the interactive render: a live pane still omits its
	// screen, a degraded pane still embeds the doc seed (nothing else can fill it).
	lo := mt.displayTab(true)
	lp1 := lo.Lanes[0].PaneGroups[0].Panes[0]
	lp2 := lo.Lanes[1].PaneGroups[0].Panes[0]
	if !lp1.RemoteInteractive || len(lp1.RemoteVTScreen) != 0 {
		t.Errorf("layout-only live pane wrong: %+v", lp1)
	}
	if !lp2.RemoteInteractive || len(lp2.RemoteVTScreen) == 0 {
		t.Errorf("layout-only degraded pane must still carry the doc seed: %+v", lp2)
	}
}

func TestApplyMirrorPaneVTTransitions(t *testing.T) {
	w := &WorkspaceActor{} // pub nil: notify* are no-ops; assert state.
	mt := w.addMirrorTab("share-t", "r")
	mt.hasData = true
	mt.snap = sampleTab()

	// Enter: marks the pane live-interactive and sets the anti-prune guard. The
	// listener sends this on enter only (the screen itself streams via vtframe).
	w.applyMirrorPaneVT(&MsgMirrorPaneVTUpdate{
		ShareID: "share-t", SourcePaneID: "p1", Interactive: true,
	})
	if !mt.liveInteractive["p1"] {
		t.Fatalf("enter transition should mark the pane live-interactive")
	}
	if !mt.rawSeenSinceLayout["p1"] {
		t.Fatalf("raw-seen guard not set")
	}

	// Leave: the pane is no longer live-interactive.
	w.applyMirrorPaneVT(&MsgMirrorPaneVTUpdate{
		ShareID: "share-t", SourcePaneID: "p1", Interactive: false,
	})
	if mt.liveInteractive["p1"] {
		t.Fatalf("leave transition should clear the live-interactive mark")
	}
}

func TestUpstreamPaneWatchedSemantics(t *testing.T) {
	u := &UpstreamShareActor{}

	// No watch info at all: everything watched (legacy behaviour).
	if !u.paneWatched("p1") {
		t.Fatalf("no watchers: every pane should count as watched")
	}

	// A live watcher displaying p1: p1 watched, p2 not.
	u.applyWatcher(&subscriberWatchMsg{SubscriberID: "sub-a", PaneIDs: []string{"p1"}})
	if !u.paneWatched("p1") {
		t.Errorf("p1 should be watched")
	}
	if u.paneWatched("p2") {
		t.Errorf("p2 should be unwatched")
	}

	// Union across subscribers.
	u.applyWatcher(&subscriberWatchMsg{SubscriberID: "sub-b", PaneIDs: []string{"p2"}})
	if !u.paneWatched("p2") {
		t.Errorf("p2 should be watched via sub-b")
	}

	// Expired watchers are pruned; with none live, everything is watched again.
	for _, wt := range u.watchers {
		wt.lastSeen = time.Now().Add(-2 * shareWatcherExpiry)
	}
	if !u.paneWatched("p-any") {
		t.Errorf("all watchers expired: every pane should count as watched")
	}
	if len(u.watchers) != 0 {
		t.Errorf("expired watchers should be pruned, %d left", len(u.watchers))
	}
}

func TestUpstreamPaneBaseInterval(t *testing.T) {
	u := &UpstreamShareActor{}

	// No watchers: full rate (legacy / single-pane / mobile).
	if got := u.paneBaseInterval("p1"); got != rawFocusedFlushInterval {
		t.Fatalf("no watchers: %v, want %v", got, rawFocusedFlushInterval)
	}

	// p1 focused, p2 visible-only, p3 unwatched.
	u.applyWatcher(&subscriberWatchMsg{
		SubscriberID: "sub-a",
		PaneIDs:      []string{"p1", "p2"},
		FocusPaneIDs: []string{"p1"},
	})
	if got := u.paneBaseInterval("p1"); got != rawFocusedFlushInterval {
		t.Errorf("focused pane window = %v, want %v", got, rawFocusedFlushInterval)
	}
	if got := u.paneBaseInterval("p2"); got != rawVisibleFlushInterval {
		t.Errorf("visible pane window = %v, want %v", got, rawVisibleFlushInterval)
	}
	if got := u.paneBaseInterval("p3"); got != rawHoldFlushInterval {
		t.Errorf("unwatched pane window = %v, want %v", got, rawHoldFlushInterval)
	}

	// Legacy watcher (no focus subset): every visible pane is treated as focused.
	u.applyWatcher(&subscriberWatchMsg{SubscriberID: "sub-b", PaneIDs: []string{"p3"}})
	if got := u.paneBaseInterval("p3"); got != rawFocusedFlushInterval {
		t.Errorf("legacy-watched pane window = %v, want %v", got, rawFocusedFlushInterval)
	}
}

func TestUpstreamHoldRawAccumulatesAndDrops(t *testing.T) {
	u := &UpstreamShareActor{}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	u.holdRaw("p1", b64("abc"))
	u.holdRaw("p1", b64("def"))
	hb := u.rawHold["p1"]
	if hb == nil || string(hb.data) != "abcdef" {
		t.Fatalf("held bytes wrong: %+v", hb)
	}
	if !hb.flushPending {
		t.Fatalf("a deferred flush should be armed")
	}

	// Mode transitions invalidate held bytes.
	u.dropRawHold("p1")
	if u.rawHold["p1"] != nil {
		t.Fatalf("dropRawHold should discard the buffer")
	}

	// flushRawHold clears the buffer even while disconnected (same as the
	// unbatched path, which skips publishing when disconnected).
	u.holdRaw("p2", b64("zzz"))
	u.flushRawHold("p2")
	if hb := u.rawHold["p2"]; hb == nil || len(hb.data) != 0 || hb.flushPending {
		t.Fatalf("flushRawHold should clear data and pending flag: %+v", hb)
	}
}

func TestSyncPaneContentVisibilityHelpersCompile(t *testing.T) {
	// Guard the domain fields the TUI patch path relies on (compile-time check
	// that RemoteInteractive / RemoteVTScreen stay on PaneSnapshot).
	p := domain.PaneSnapshot{RemoteInteractive: true, RemoteVTScreen: []string{"r"}}
	if !p.RemoteInteractive || len(p.RemoteVTScreen) != 1 {
		t.Fatalf("unexpected: %+v", p)
	}
}
