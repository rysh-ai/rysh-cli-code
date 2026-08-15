// SPDX-License-Identifier: Apache-2.0

package web

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// recordingDispatcher stands in for a pane's headless browser and captures the
// actions webpane_input forwards.
type recordingDispatcher struct {
	calls chan dispatchedCall
	err   error
}

type dispatchedCall struct {
	action string
	params json.RawMessage
}

func newRecordingDispatcher() *recordingDispatcher {
	return &recordingDispatcher{calls: make(chan dispatchedCall, 8)}
}

func (r *recordingDispatcher) Do(action string, params json.RawMessage) (json.RawMessage, string, error) {
	r.calls <- dispatchedCall{action: action, params: params}
	if r.err != nil {
		return nil, "", r.err
	}
	return json.RawMessage(`{}`), "", nil
}

func (r *recordingDispatcher) next(t *testing.T) dispatchedCall {
	t.Helper()
	select {
	case c := <-r.calls:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no input was forwarded to the browser")
		return dispatchedCall{}
	}
}

// jsPoint pulls the coordinates out of the dispatched execute_js snippet.
var jsPointRe = regexp.MustCompile(`const x=(-?[0-9.e+]+),y=(-?[0-9.e+]+)`)

func (c dispatchedCall) jsPoint(t *testing.T) (float64, float64) {
	t.Helper()
	if c.action != "execute_js" {
		t.Fatalf("pointer input dispatched as %q, want execute_js", c.action)
	}
	var p struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(c.params, &p); err != nil {
		t.Fatalf("dispatched params: %v", err)
	}
	m := jsPointRe.FindStringSubmatch(p.Code)
	if m == nil {
		t.Fatalf("no coordinates in dispatched code: %s", p.Code)
	}
	x, _ := strconv.ParseFloat(m[1], 64)
	y, _ := strconv.ParseFloat(m[2], 64)
	return x, y
}

// newWebPaneInputServer starts a server and installs a fake web-pane session
// whose frame source size is srcW x srcH.
func newWebPaneInputServer(t *testing.T, paneID string, srcW, srcH float64) (*Server, int, *recordingDispatcher) {
	t.Helper()
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	port := freePort(t)
	s := NewServer(port, "wpi-session", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/health", port))

	rec := newRecordingDispatcher()
	sess := &webPaneSession{paneID: paneID, dispatcher: rec, srcW: srcW, srcH: srcH}
	s.webPaneMu.Lock()
	s.webPanes = map[string]*webPaneSession{paneID: sess}
	s.webPaneMu.Unlock()
	return s, port, rec
}

// TestWebPaneInputMapsDisplayCoordsToSourceSpace (E-16) is the whole point of
// webpane_input: the pane renders a 1280x900 frame at 640x450, so a click at
// display (320,225) is a click at SOURCE (640,450). Forwarding the display
// coordinate unmapped puts every click at half the intended point.
func TestWebPaneInputMapsDisplayCoordsToSourceSpace(t *testing.T) {
	const srcW, srcH = 1280.0, 900.0
	_, port, rec := newWebPaneInputServer(t, "pane-1", srcW, srcH)

	conn := dialWS(t, port, "")
	sendWSCommand(t, conn, "webpane_input", map[string]any{
		"pane_id":        "pane-1",
		"kind":           "click",
		"x":              320.0,
		"y":              225.0,
		"display_width":  640.0,
		"display_height": 450.0,
	})

	gotX, gotY := rec.next(t).jsPoint(t)
	if gotX != 640 || gotY != 450 {
		t.Fatalf("forwarded click at (%g,%g); want source-space (640,450) for a display (320,225) click on a %gx%g frame shown at 640x450",
			gotX, gotY, srcW, srcH)
	}
}

// TestWebPaneInputScrollMapsToSourceSpace covers the non-click pointer kinds
// through the same mapping.
func TestWebPaneInputScrollMapsToSourceSpace(t *testing.T) {
	_, port, rec := newWebPaneInputServer(t, "pane-1", 1000, 500)

	conn := dialWS(t, port, "")
	sendWSCommand(t, conn, "webpane_input", map[string]any{
		"pane_id":        "pane-1",
		"kind":           "scroll",
		"x":              50.0,
		"y":              25.0,
		"display_width":  250.0,
		"display_height": 125.0,
		"delta_y":        120.0,
	})

	gotX, gotY := rec.next(t).jsPoint(t)
	if gotX != 200 || gotY != 100 {
		t.Fatalf("forwarded scroll at (%g,%g), want (200,100)", gotX, gotY)
	}
}

// TestWebPaneInputKeyNeedsNoMapping: keys carry no coordinates and go through
// press_key untouched.
func TestWebPaneInputKeyNeedsNoMapping(t *testing.T) {
	_, port, rec := newWebPaneInputServer(t, "pane-1", 1280, 900)

	conn := dialWS(t, port, "")
	sendWSCommand(t, conn, "webpane_input", map[string]any{
		"pane_id": "pane-1", "kind": "key", "key": "Enter", "modifiers": []string{"shift"},
	})

	call := rec.next(t)
	if call.action != "press_key" {
		t.Fatalf("key input dispatched as %q, want press_key", call.action)
	}
	var p struct {
		Key       string   `json:"key"`
		Modifiers []string `json:"modifiers"`
	}
	if err := json.Unmarshal(call.params, &p); err != nil {
		t.Fatalf("press_key params: %v", err)
	}
	if p.Key != "Enter" || len(p.Modifiers) != 1 || p.Modifiers[0] != "shift" {
		t.Fatalf("press_key params = %+v", p)
	}
}

// TestWebPaneInputFailuresAreVisible (fail-visible rule, same as
// TestWebPaneUnavailableIsVisible): every rejected input answers with a
// webpane_error, never a silent no-op.
func TestWebPaneInputFailuresAreVisible(t *testing.T) {
	cases := []struct {
		name    string
		params  map[string]any
		wantErr string
	}{
		{
			name:    "unknown pane",
			params:  map[string]any{"pane_id": "pane-nope", "kind": "click", "x": 1.0, "y": 1.0, "display_width": 10.0, "display_height": 10.0},
			wantErr: "no server-side browser",
		},
		{
			name:    "unsupported kind",
			params:  map[string]any{"pane_id": "pane-1", "kind": "teleport", "x": 1.0, "y": 1.0, "display_width": 10.0, "display_height": 10.0},
			wantErr: "unsupported webpane_input kind",
		},
		{
			name:    "zero display size",
			params:  map[string]any{"pane_id": "pane-1", "kind": "click", "x": 1.0, "y": 1.0, "display_width": 0.0, "display_height": 0.0},
			wantErr: "display size",
		},
		{
			name:    "key with no key",
			params:  map[string]any{"pane_id": "pane-1", "kind": "key"},
			wantErr: "requires a key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, port, _ := newWebPaneInputServer(t, "pane-1", 1280, 900)
			conn := dialWS(t, port, "")
			sendWSCommand(t, conn, "webpane_input", tc.params)

			data := readWSType(t, conn, "webpane_error", 5*time.Second)
			if e, _ := data["error"].(string); !strings.Contains(e, tc.wantErr) {
				t.Errorf("webpane_error = %q, want it to mention %q", e, tc.wantErr)
			}
		})
	}
}

// TestWebPaneInputRejectsUnknownSourceSize: before the first frame the server
// has no source dimensions, so it cannot map — and must say so rather than
// forward a coordinate in the wrong space.
func TestWebPaneInputRejectsUnknownSourceSize(t *testing.T) {
	_, port, _ := newWebPaneInputServer(t, "pane-1", 0, 0)

	conn := dialWS(t, port, "")
	sendWSCommand(t, conn, "webpane_input", map[string]any{
		"pane_id": "pane-1", "kind": "click",
		"x": 10.0, "y": 10.0, "display_width": 100.0, "display_height": 100.0,
	})

	data := readWSType(t, conn, "webpane_error", 5*time.Second)
	if e, _ := data["error"].(string); !strings.Contains(e, "no frame") {
		t.Errorf("webpane_error = %q, want it to mention a missing frame", e)
	}
}

// TestWebPaneSourcePoint pins the mapping itself.
func TestWebPaneSourcePoint(t *testing.T) {
	cases := []struct {
		name            string
		x, y            float64
		dispW, dispH    float64
		srcW, srcH      float64
		wantX, wantY    float64
		wantErrContains string
	}{
		{name: "half scale", x: 320, y: 225, dispW: 640, dispH: 450, srcW: 1280, srcH: 900, wantX: 640, wantY: 450},
		{name: "unscaled", x: 17, y: 42, dispW: 800, dispH: 600, srcW: 800, srcH: 600, wantX: 17, wantY: 42},
		{name: "upscaled", x: 400, y: 300, dispW: 1600, dispH: 1200, srcW: 800, srcH: 600, wantX: 200, wantY: 150},
		{name: "anisotropic", x: 100, y: 100, dispW: 200, dispH: 400, srcW: 800, srcH: 800, wantX: 400, wantY: 200},
		{name: "clamped to source", x: 999, y: 999, dispW: 100, dispH: 100, srcW: 200, srcH: 200, wantX: 199, wantY: 199},
		{name: "no display size", x: 1, y: 1, dispW: 0, dispH: 100, srcW: 200, srcH: 200, wantErrContains: "display size"},
		{name: "no frame yet", x: 1, y: 1, dispW: 100, dispH: 100, srcW: 0, srcH: 0, wantErrContains: "no frame"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x, y, err := webPaneSourcePoint(tc.x, tc.y, tc.dispW, tc.dispH, tc.srcW, tc.srcH)
			if tc.wantErrContains != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrContains) {
					t.Fatalf("err = %v, want it to mention %q", err, tc.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if x != tc.wantX || y != tc.wantY {
				t.Errorf("got (%g,%g), want (%g,%g)", x, y, tc.wantX, tc.wantY)
			}
		})
	}
}
