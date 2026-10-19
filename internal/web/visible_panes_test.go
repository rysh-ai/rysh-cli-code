// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A client that declared its visible panes must not be sent frames for any
// other pane, must be caught up when a pane becomes visible (even though that
// pane's frame has not changed since it was last broadcast), and must then get
// changes like everyone else. A client that never declared anything keeps
// receiving every pane, so older bundles are unaffected.
func TestStreamPaneVTSendsOnlyVisiblePanes(t *testing.T) {
	s, nc, _ := newControlTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.hub = newHub()
	go s.hub.run(ctx)

	r := &vtResponder{t: t, nc: nc, paneID: "pane-1", screen: make(chan []string, 1)}
	r.cur = []string{"idle screen"}
	r.serve()

	all := &wsClient{hub: s.hub, send: make(chan []byte, 256), streamContent: true}
	picky := &wsClient{hub: s.hub, send: make(chan []byte, 256), streamContent: true}
	picky.vt.set([]string{"some-other-pane"})
	s.hub.register <- all
	s.hub.register <- picky

	go s.streamPaneVT(ctx)

	if got, _ := countFrames(all, time.Second); got != 1 {
		t.Fatalf("unfiltered client got %d frames, want 1", got)
	}
	if got, _ := countFrames(picky, 200*time.Millisecond); got != 0 {
		t.Fatalf("client not showing pane-1 got %d frames for it, want 0", got)
	}

	// pane-1 comes on screen. Its frame is unchanged, so dedup would skip it;
	// the catch-up must deliver it to this client alone.
	s.handleVisiblePanes(picky, json.RawMessage(`{"pane_ids":["pane-1"]}`))
	got, last := countFrames(picky, time.Second)
	if got != 1 || len(last) == 0 || last[0] != "idle screen" {
		t.Fatalf("newly visible pane: got %d frames (last %v), want 1 carrying the current screen", got, last)
	}
	if got, _ := countFrames(all, 200*time.Millisecond); got != 0 {
		t.Errorf("catch-up leaked to the unfiltered client: %d frames", got)
	}

	// A real change now reaches both.
	r.change([]string{"changed"})
	if got, _ := countFrames(picky, time.Second); got == 0 {
		t.Error("visible pane changed and the filtered client got nothing")
	}
	if got, _ := countFrames(all, 200*time.Millisecond); got == 0 {
		t.Error("visible pane changed and the unfiltered client got nothing")
	}

	// Off screen again: further changes are withheld. The change is SIGNALLED, so
	// a filter that let it through would deliver it on the next tick, well inside
	// the 500ms below — without the signal this check could not fail (E-103).
	s.handleVisiblePanes(picky, json.RawMessage(`{"pane_ids":[]}`))
	r.change([]string{"changed again"})
	if got, _ := countFrames(picky, 500*time.Millisecond); got != 0 {
		t.Errorf("hidden pane still streamed %d frames", got)
	}
}

// Every client filtered away from a pane means the pane is not fetched at all.
func TestCollectVTDemandSkipsPanesNobodyShows(t *testing.T) {
	h := newHub()
	a := &wsClient{streamContent: true}
	b := &wsClient{streamContent: true}
	a.vt.set([]string{"p1"})
	b.vt.set([]string{"p2"})
	h.countClient(a, 1)
	h.countClient(b, 1)
	s := &Server{hub: h}

	d := s.collectVTDemand()
	if d.all || !d.polls("p1") || !d.polls("p2") || d.polls("p3") {
		t.Fatalf("demand = %+v; want p1 and p2 only", d)
	}
	b.vt.set([]string{"p2", "p3"})
	d = s.collectVTDemand()
	if !d.pending["p3"][b] || d.pending["p2"] != nil {
		t.Fatalf("pending = %v; want only p3 owed to b", d.pending)
	}
	if d = s.collectVTDemand(); len(d.pending) != 0 {
		t.Fatalf("pending not consumed: %v", d.pending)
	}

	h.countClient(&wsClient{streamContent: true}, 1) // never declared
	if d = s.collectVTDemand(); !d.all || !d.polls("anything") {
		t.Fatal("an undeclared client must keep every pane polled")
	}
}

// The upgrader negotiates permessage-deflate, and a large frame travels
// compressed and arrives intact.
func TestWebSocketNegotiatesCompression(t *testing.T) {
	payload := `{"type":"pane_vt","data":{"vt_screen":["` + strings.Repeat(" ", 4000) + `"]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.EnableWriteCompression(len(payload) >= compressMinBytes)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(payload))
	}))
	defer srv.Close()

	d := websocket.Dialer{EnableCompression: true}
	conn, resp, err := d.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if ext := resp.Header.Get("Sec-WebSocket-Extensions"); !strings.Contains(ext, "permessage-deflate") {
		t.Fatalf("Sec-WebSocket-Extensions = %q, want permessage-deflate", ext)
	}
	_, got, err := conn.ReadMessage()
	if err != nil || string(got) != payload {
		t.Fatalf("read: err=%v, intact=%v", err, string(got) == payload)
	}
}
