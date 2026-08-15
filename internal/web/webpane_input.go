// SPDX-License-Identifier: Apache-2.0

// Coordinate input forwarding for server-side web panes (E7 T1 / E-16).
//
// The client renders webpane_frame's JPEG scaled to whatever size the pane
// happens to be, so a pointer event arrives in DISPLAY space while the
// headless page only understands its own CSS-pixel SOURCE space. Forwarding a
// display coordinate unmapped is the entire bug class here: a 1280x900 page
// shown at 640x450 puts every click at half the intended point.
//
//	client → server: webpane_input {pane_id,kind,x,y,display_width,
//	                 display_height,button,key,modifiers,delta_x,delta_y}
//	server → client: webpane_error {pane_id,error}   (fail visible, never a
//	                 silent no-op — see TestWebPaneUnavailableIsVisible)
//
// x/y are relative to the top-left of the rendered FRAME IMAGE (not the pane),
// and display_width/display_height are that image's rendered size. The client
// therefore never scales coordinates itself; it reports what it drew and the
// server does the mapping against the source dimensions it published on the
// frame.
package web

import (
	"encoding/json"
	"fmt"
	"strings"
)

// webPaneDispatcher is the browser-side sink for forwarded input. Production
// is the session's *cdp.Browser; tests substitute a recorder.
type webPaneDispatcher interface {
	Do(action string, params json.RawMessage) (json.RawMessage, string, error)
}

// webPaneInputCmd is the webpane_input ws command payload.
type webPaneInputCmd struct {
	PaneID        string   `json:"pane_id"`
	Kind          string   `json:"kind"` // click | key | scroll | move
	X             float64  `json:"x"`
	Y             float64  `json:"y"`
	DisplayWidth  float64  `json:"display_width"`
	DisplayHeight float64  `json:"display_height"`
	Button        string   `json:"button"`
	Key           string   `json:"key"`
	Modifiers     []string `json:"modifiers"`
	DeltaX        float64  `json:"delta_x"`
	DeltaY        float64  `json:"delta_y"`
}

// webPaneInput forwards one input event to a pane's headless page. Every
// failure path pushes a webpane_error so a dropped event is always visible.
func (s *Server) webPaneInput(cmd webPaneInputCmd) {
	s.webPaneMu.Lock()
	sess := s.webPanes[cmd.PaneID]
	s.webPaneMu.Unlock()
	if sess == nil {
		s.pushWebPaneError(cmd.PaneID, "no server-side browser for this pane — reopen the web pane")
		return
	}

	kind := strings.ToLower(strings.TrimSpace(cmd.Kind))
	switch kind {
	case "click", "move", "scroll", "key":
	default:
		s.pushWebPaneError(cmd.PaneID, fmt.Sprintf("unsupported webpane_input kind %q (want click, key, scroll or move)", cmd.Kind))
		return
	}

	disp := sess.dispatch()
	if disp == nil {
		s.pushWebPaneError(cmd.PaneID, "server-side browser for this pane is not running — reopen the web pane")
		return
	}

	if kind == "key" {
		s.webPaneInputKey(cmd, disp)
		return
	}

	srcW, srcH := sess.sourceSize()
	x, y, err := webPaneSourcePoint(cmd.X, cmd.Y, cmd.DisplayWidth, cmd.DisplayHeight, srcW, srcH)
	if err != nil {
		s.pushWebPaneError(cmd.PaneID, err.Error())
		return
	}

	action, params, err := webPanePointerAction(kind, x, y, cmd)
	if err != nil {
		s.pushWebPaneError(cmd.PaneID, err.Error())
		return
	}
	if _, _, err := disp.Do(action, params); err != nil {
		s.pushWebPaneError(cmd.PaneID, fmt.Sprintf("%s input failed: %v", kind, err))
		return
	}
	s.pushWebPaneFrame(sess)
}

// webPaneInputKey forwards a key press. Keys carry no coordinates, so they
// need no space mapping.
func (s *Server) webPaneInputKey(cmd webPaneInputCmd, disp webPaneDispatcher) {
	if strings.TrimSpace(cmd.Key) == "" {
		s.pushWebPaneError(cmd.PaneID, "webpane_input kind=key requires a key")
		return
	}
	params, err := json.Marshal(map[string]any{"key": cmd.Key, "modifiers": cmd.Modifiers})
	if err != nil {
		s.pushWebPaneError(cmd.PaneID, fmt.Sprintf("key input failed: %v", err))
		return
	}
	if _, _, err := disp.Do("press_key", params); err != nil {
		s.pushWebPaneError(cmd.PaneID, fmt.Sprintf("key input failed: %v", err))
		return
	}
	s.webPaneMu.Lock()
	sess := s.webPanes[cmd.PaneID]
	s.webPaneMu.Unlock()
	if sess != nil {
		s.pushWebPaneFrame(sess)
	}
}

// webPaneSourcePoint maps a pointer coordinate from DISPLAY space (the
// rendered frame image) into SOURCE space (the page's CSS pixels).
//
// The axes scale independently: a pane is free to letterbox or stretch, and
// assuming a single uniform scale is the same bug in a subtler form. Both
// dimensions must be known — with no display size, or before the first frame
// has published a source size, there is no mapping and the caller must fail
// visibly rather than forward a coordinate in the wrong space.
func webPaneSourcePoint(x, y, dispW, dispH, srcW, srcH float64) (float64, float64, error) {
	if dispW <= 0 || dispH <= 0 {
		return 0, 0, fmt.Errorf("webpane_input needs the frame's rendered display size (got %gx%g)", dispW, dispH)
	}
	if srcW <= 0 || srcH <= 0 {
		return 0, 0, fmt.Errorf("no frame published for this pane yet — cannot map input into source space")
	}

	sx := x * srcW / dispW
	sy := y * srcH / dispH

	// Clamp inside the page: a click on a letterbox edge, or a coordinate
	// rounded past the last pixel, must not land outside the viewport.
	sx = clampFloat(sx, 0, srcW-1)
	sy = clampFloat(sy, 0, srcH-1)
	return sx, sy, nil
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// webPanePointerAction builds the browser action for a pointer event already
// expressed in SOURCE (CSS-pixel) coordinates.
func webPanePointerAction(kind string, x, y float64, cmd webPaneInputCmd) (string, json.RawMessage, error) {
	var code string
	switch kind {
	case "click":
		code = fmt.Sprintf(webPaneClickJS, x, y, webPaneButtonIndex(cmd.Button))
	case "move":
		code = fmt.Sprintf(webPaneMoveJS, x, y)
	case "scroll":
		code = fmt.Sprintf(webPaneScrollJS, x, y, cmd.DeltaX, cmd.DeltaY)
	default:
		return "", nil, fmt.Errorf("unsupported webpane_input kind %q", kind)
	}
	params, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		return "", nil, fmt.Errorf("%s input failed: %v", kind, err)
	}
	return "execute_js", params, nil
}

func webPaneButtonIndex(button string) int {
	switch strings.ToLower(strings.TrimSpace(button)) {
	case "middle":
		return 1
	case "right":
		return 2
	default:
		return 0
	}
}

// The JS below runs in the page and dispatches trusted-shaped events at a
// source-space point. elementFromPoint takes CSS pixels, which is exactly the
// space the server maps into.
const (
	webPaneClickJS = `(()=>{const x=%g,y=%g,b=%d;
  const el=document.elementFromPoint(x,y);
  if(!el) return {dispatched:false,reason:"no element at point"};
  const opt={bubbles:true,cancelable:true,composed:true,clientX:x,clientY:y,button:b,buttons:b===0?1:(b===1?4:2)};
  el.dispatchEvent(new MouseEvent("mousedown",opt));
  el.dispatchEvent(new MouseEvent("mouseup",{...opt,buttons:0}));
  if(b===0) el.dispatchEvent(new MouseEvent("click",{...opt,buttons:0}));
  if(typeof el.focus==="function") el.focus();
  return {dispatched:true,tag:el.tagName};})()`

	webPaneMoveJS = `(()=>{const x=%g,y=%g;
  const el=document.elementFromPoint(x,y);
  if(!el) return {dispatched:false,reason:"no element at point"};
  el.dispatchEvent(new MouseEvent("mousemove",{bubbles:true,cancelable:true,composed:true,clientX:x,clientY:y}));
  return {dispatched:true,tag:el.tagName};})()`

	webPaneScrollJS = `(()=>{const x=%g,y=%g,dx=%g,dy=%g;
  const el=document.elementFromPoint(x,y)||document.scrollingElement||document.body;
  el.dispatchEvent(new WheelEvent("wheel",{bubbles:true,cancelable:true,composed:true,clientX:x,clientY:y,deltaX:dx,deltaY:dy}));
  let n=el;
  while(n&&n!==document.body&&n!==document.documentElement){
    const s=getComputedStyle(n);
    if((n.scrollHeight>n.clientHeight&&/(auto|scroll)/.test(s.overflowY))||
       (n.scrollWidth>n.clientWidth&&/(auto|scroll)/.test(s.overflowX))){n.scrollBy(dx,dy);return {scrolled:"element"};}
    n=n.parentElement;
  }
  window.scrollBy(dx,dy);
  return {scrolled:"window"};})()`
)
