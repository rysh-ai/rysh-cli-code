// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/cdp"
)

// The page the live tests drive. Two targets sit at KNOWN CSS-pixel positions
// with a GAP between them, and the geometry is chosen so that a mapping bug is
// not a near miss but a hit on the WRONG element:
//
//	#left spans x 0…150, #right spans x 200…300, both y 0…100.
//
// The test renders at half scale and clicks display (110,25). Mapped into
// source space that is (220,50) — inside #right. Forwarded UNMAPPED it is
// (110,25) — inside #left. So "which element did the real browser dispatch to"
// is a direct read-out of whether webPaneSourcePoint ran.
const webPaneLivePage = `<!doctype html><meta charset="utf-8">
<style>
  html,body{margin:0;padding:0;background:#fff}
  #left {position:absolute;left:0;  top:0;width:150px;height:100px;background:#ccc}
  #right{position:absolute;left:200px;top:0;width:100px;height:100px;background:#999}
</style>
<div id="left"></div><div id="right"></div>
<script>
  window.__hits = []; window.__keys = []; window.__keyups = [];
  document.addEventListener('click', function (e) {
    window.__hits.push({id: e.target.id || e.target.tagName, x: e.clientX, y: e.clientY});
  }, true);
  window.addEventListener('keydown', function (e) { window.__keys.push(e.key); }, true);
  // Releases are recorded separately so "pressed once" and "released once" are
  // two assertions rather than one: a key that is dispatched but never released
  // leaves __keys looking perfectly correct.
  window.addEventListener('keyup', function (e) { window.__keyups.push(e.key); }, true);
</script>`

// evalJSON runs code in the page and unmarshals its JSON string result. It
// mirrors how pushWebPaneFrame reads the page (webpane.go:293): execute_js
// answers {"result":"<stringified>"}, so the payload is decoded twice.
func evalJSON(t *testing.T, b *cdp.Browser, code string, out any) {
	t.Helper()
	params, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		t.Fatalf("marshal eval params: %v", err)
	}
	raw, _, err := b.Do("execute_js", json.RawMessage(params))
	if err != nil {
		t.Fatalf("execute_js %q: %v", code, err)
	}
	var wrap struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		t.Fatalf("execute_js wrapper: %v (raw %s)", err, raw)
	}
	if wrap.Result == "" {
		t.Fatalf("execute_js %q returned an empty result", code)
	}
	if err := json.Unmarshal([]byte(wrap.Result), out); err != nil {
		t.Fatalf("execute_js payload %q: %v", wrap.Result, err)
	}
}

type liveHit struct {
	ID string  `json:"id"`
	X  float64 `json:"x"`
	Y  float64 `json:"y"`
}

type livePane struct {
	browser *cdp.Browser
	port    int
	paneID  string
	srcW    float64 // the page's real viewport = the frame's SOURCE size
	srcH    float64
}

// newLiveWebPane launches a REAL headless Chrome on the fixture page and binds
// it to a web-pane session as the production dispatcher, so that from here on
// nothing in the path is a test double.
func newLiveWebPane(t *testing.T) livePane {
	t.Helper()
	if cdp.FindChromium() == "" {
		t.Skip("no Chromium/Chrome found — set RYSH_CHROMIUM_PATH to run this proof")
	}

	pagePath := filepath.Join(t.TempDir(), "webpane-live.html")
	if err := os.WriteFile(pagePath, []byte(webPaneLivePage), 0o644); err != nil {
		t.Fatalf("write page: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	b, err := cdp.Launch(ctx, cdp.LaunchOptions{
		UserDataDir: t.TempDir(),
		Headless:    true,
		URL:         "file://" + pagePath,
		WaitTimeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("launch chrome: %v", err)
	}
	t.Cleanup(b.Close)

	// Wait for the page's listeners to exist, then take the viewport as the
	// frame's SOURCE size — exactly what pushWebPaneFrame publishes.
	var page struct {
		Ready bool    `json:"ready"`
		W     float64 `json:"w"`
		H     float64 `json:"h"`
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		evalJSON(t, b, `JSON.stringify({ready: !!window.__hits, w: window.innerWidth, h: window.innerHeight})`, &page)
		if page.Ready && page.W > 0 && page.H > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("page never became ready (ready=%v %gx%g)", page.Ready, page.W, page.H)
		}
		time.Sleep(200 * time.Millisecond)
	}

	const paneID = "pane-live"
	s, port, _ := newWebPaneInputServer(t, paneID, page.W, page.H)

	s.webPaneMu.Lock()
	sess := s.webPanes[paneID]
	s.webPaneMu.Unlock()
	if sess == nil {
		t.Fatal("no web-pane session was installed")
	}
	sess.mu.Lock()
	sess.dispatcher = b
	sess.mu.Unlock()

	return livePane{browser: b, port: port, paneID: paneID, srcW: page.W, srcH: page.H}
}

// TestWebPaneInputReachesARealBrowser (E-16) closes the ONE seam every other
// webpane_input test leaves to a stand-in.
//
// The rest of this package asserts against recordingDispatcher, whose own
// comment says it "stands in for a pane's headless browser" — it accepts any
// action name with any params and records them. In production the dispatcher
// is the real *cdp.Browser (webpane.go:176). So the existing suite proves the
// server BUILDS the right call, and the client suite (rysh-cli-app,
// WebPaneView.test.tsx) proves the browser SENDS the right command, while
// nothing at all proved a click crosses into a real page. Two green suites
// either side of a stub is not a working feature — and the typing half of this
// proof found `F-46` the moment the stub was removed.
//
// This runs the production path end to end: a real ws `webpane_input` command
// → Server.webPaneInput → coordinate mapping → *cdp.Browser → real headless
// Chrome — and then asks the PAGE what it received.
func TestWebPaneInputReachesARealBrowser(t *testing.T) {
	if testing.Short() {
		t.Skip("launches a real browser; skipped under -short")
	}
	lp := newLiveWebPane(t)

	// Render at half scale, so display→source is a 2× map on both axes.
	dispW, dispH := lp.srcW/2, lp.srcH/2
	const wantX, wantY = 220.0, 50.0

	conn := dialWS(t, lp.port, "")
	sendWSCommand(t, conn, "webpane_input", map[string]any{
		"pane_id":        lp.paneID,
		"kind":           "click",
		"x":              110.0, // → source 220 (#right); unmapped it is #left
		"y":              25.0,  // → source 50
		"display_width":  dispW,
		"display_height": dispH,
		"button":         "left",
	})

	var hits []liveHit
	deadline := time.Now().Add(20 * time.Second)
	for {
		evalJSON(t, lp.browser, `JSON.stringify(window.__hits)`, &hits)
		if len(hits) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the real browser never received the click — webpane_input did not cross the dispatcher boundary")
		}
		time.Sleep(200 * time.Millisecond)
	}

	if len(hits) != 1 {
		t.Fatalf("got %d clicks, want exactly 1: %+v", len(hits), hits)
	}
	got := hits[0]
	if got.ID != "right" {
		t.Fatalf("the click landed on %q, want \"right\" — display (110,25) at %gx%g must map to source (%g,%g); "+
			"landing on \"left\" means the display coordinate was forwarded unmapped",
			got.ID, dispW, dispH, wantX, wantY)
	}
	if got.X != wantX || got.Y != wantY {
		t.Fatalf("the page saw the click at (%g,%g), want (%g,%g)", got.X, got.Y, wantX, wantY)
	}
}

// assertReleasedOnce checks the page saw exactly one keyup for key, and clears
// the release log for the next press.
//
// This is a separate assertion from the keydown checks on purpose. A key that
// is dispatched and never released leaves the keydown log looking perfectly
// correct, and — in a correctly launched Chromium — produces no repeats either,
// so every other assertion here passes while the page's key state stays stuck
// down. Verified by mutation: deleting the keyUp from cdp.pressRawKey passes
// the whole rest of this test and fails only here.
func assertReleasedOnce(t *testing.T, lp livePane, key string) {
	t.Helper()
	var ups []string
	deadline := time.Now().Add(5 * time.Second)
	for {
		evalJSON(t, lp.browser, `JSON.stringify(window.__keyups)`, &ups)
		if len(ups) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the page never saw %q released — the key is still down", key)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(ups) != 1 || ups[0] != key {
		t.Fatalf("after one %q press the page saw keyups %v, want exactly [%q]", key, ups, key)
	}
	evalJSON(t, lp.browser, `JSON.stringify((()=>{window.__keyups=[];return{ok:true}})())`, &struct {
		OK bool `json:"ok"`
	}{})
}

// TestWebPaneInputKeyReachesARealBrowser is the typing half of the same proof.
// It found `F-46` the moment the stand-in dispatcher was removed, and it is the
// regression guard for the fix.
//
// F-46 was TWO independent defects that looked like one, and neither was where
// the shape of the symptom suggested:
//
//   - a CHARACTER key ("a", "Z", "5") produced ZERO keydown events in the page
//     while press_key returned {"pressed":"a","trusted":true} and a nil error.
//     Not a focus problem and not a missing virtual key code: cdp.pressMainKey
//     sent a BARE `char` event for an unmodified character, and a `char` event
//     yields a keypress and NOTHING else — no keydown, no keyup. Every page
//     that listens for keydown saw nothing. Printable characters never reached
//     pressRawKey at all, so its keyCodes/keyText maps were never implicated.
//   - EVERY key then stuck down, auto-repeating ~400 events/second forever with
//     no further dispatch. That one is not in the dispatch code at all: a
//     Chromium launched with a URL on its command line auto-repeats keys
//     injected over CDP. Same binary, same flags, same page, same dispatch —
//     launched AT a file:// URL, 1241 events two seconds after ONE press;
//     launched at about:blank and then navigated, 3. The repeats carry code
//     "NumpadDecimal" rather than the key that was sent, which is why they read
//     as a malformed event rather than as a launch-time property.
//
// Fixed in cdp.pressCharKey (real keyDown/keyUp with text, code and virtual key
// code) and cdp.Launch (navigate after attaching, never a URL argument).
//
// The two assertions below are the exit condition in miniature: a typed
// character is OBSERVED BY THE PAGE, and after one press the page goes QUIET.
func TestWebPaneInputKeyReachesARealBrowser(t *testing.T) {
	lp := newLiveWebPane(t)
	conn := dialWS(t, lp.port, "")
	sendWSCommand(t, conn, "webpane_input", map[string]any{
		"pane_id": lp.paneID,
		"kind":    "key",
		"key":     "a",
	})

	var keys []string
	deadline := time.Now().Add(20 * time.Second)
	for {
		evalJSON(t, lp.browser, `JSON.stringify(window.__keys)`, &keys)
		if len(keys) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the real browser never received the keypress")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if keys[0] != "a" {
		t.Fatalf("the page saw key %q, want \"a\"", keys[0])
	}
	assertReleasedOnce(t, lp, "a")

	// No key was pressed after the first, so the page must go quiet: a key that
	// sticks down repeats forever and makes the pane unusable.
	evalJSON(t, lp.browser, `JSON.stringify((()=>{window.__keys=[];return{ok:true}})())`, &struct {
		OK bool `json:"ok"`
	}{})
	time.Sleep(1500 * time.Millisecond)
	evalJSON(t, lp.browser, `JSON.stringify(window.__keys)`, &keys)
	if len(keys) != 0 {
		t.Fatalf("the page received %d unsolicited key events after a single press — the key is stuck down", len(keys))
	}

	// A NAMED key too. The stuck-down half of F-46 was first measured on Enter,
	// ArrowDown and Escape, and named keys take a different branch of
	// cdp.pressMainKey (pressRawKey) from the character above — so testing only
	// "a" would leave the originally reported symptom unguarded.
	for _, key := range []string{"Enter", "ArrowDown", "Escape"} {
		evalJSON(t, lp.browser, `JSON.stringify((()=>{window.__keys=[];return{ok:true}})())`, &struct {
			OK bool `json:"ok"`
		}{})

		sendWSCommand(t, conn, "webpane_input", map[string]any{
			"pane_id": lp.paneID,
			"kind":    "key",
			"key":     key,
		})

		deadline := time.Now().Add(20 * time.Second)
		for {
			evalJSON(t, lp.browser, `JSON.stringify(window.__keys)`, &keys)
			if len(keys) > 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the real browser never received %q", key)
			}
			time.Sleep(200 * time.Millisecond)
		}
		if keys[0] != key {
			t.Fatalf("the page saw key %q, want %q", keys[0], key)
		}
		if len(keys) != 1 {
			t.Fatalf("one %q press produced %d keydowns before the page was even re-read — "+
				"it is repeating", key, len(keys))
		}
		assertReleasedOnce(t, lp, key)

		evalJSON(t, lp.browser, `JSON.stringify((()=>{window.__keys=[];return{ok:true}})())`, &struct {
			OK bool `json:"ok"`
		}{})
		time.Sleep(1500 * time.Millisecond)
		evalJSON(t, lp.browser, `JSON.stringify(window.__keys)`, &keys)
		if len(keys) != 0 {
			t.Fatalf("the page received %d unsolicited key events after a single %q — the key is stuck down",
				len(keys), key)
		}
	}
}
