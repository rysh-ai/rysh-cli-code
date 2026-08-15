// SPDX-License-Identifier: Apache-2.0

package web

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// clipboard_copy is the first command that carries pane CONTENT back out to a
// remote client, so these tests pin two different kinds of property: that the
// right buffer comes back, and that it comes back to exactly one client.

func newClipboardTestServer(t *testing.T, paneID string, snap *domain.PaneSnapshot) *Server {
	t.Helper()
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	s := NewServer(23233, "clipboard-test-session", pub, nc, pub.Codecs())
	s.hub = newHub()

	rec := &paneRequestRecorder{t: t, nc: nc, seen: make(chan string, 4), fullSnap: snap}
	rec.serve(msg.T("pane", paneID, "inbox"))
	return s
}

// awaitClipboard reads the one reply the handler pushes to the asking client.
func awaitClipboard(t *testing.T, c *wsClient) map[string]interface{} {
	t.Helper()
	select {
	case raw := <-c.send:
		var env struct {
			Type string                 `json:"type"`
			Data map[string]interface{} `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode clipboard reply: %v", err)
		}
		if env.Type != "clipboard_content" {
			t.Fatalf("reply type = %q, want clipboard_content", env.Type)
		}
		return env.Data
	case <-time.After(5 * time.Second):
		t.Fatal("clipboard_copy never answered — a copy that hangs is indistinguishable " +
			"from a pane with nothing in it")
		return nil
	}
}

func copyRequest(t *testing.T, s *Server, params string) (*wsClient, map[string]interface{}) {
	t.Helper()
	c := &wsClient{send: make(chan []byte, 4)}
	s.handleClipboardCopy(c, json.RawMessage(params))
	return c, awaitClipboard(t, c)
}

// TestClipboardCopyReturnsTheShellBufferToTheAskerOnly is the whole point of the
// command AND its security property in one test. The reply must reach the
// requesting client's own channel and must NOT go through the hub: a pane buffer
// holds whatever the pane printed — tokens, keys, private output — and every
// other broadcast on this socket reaches every connected viewer.
func TestClipboardCopyReturnsTheShellBufferToTheAskerOnly(t *testing.T) {
	s := newClipboardTestServer(t, "p1", &domain.PaneSnapshot{
		ID:     "p1",
		Output: "total 4\ndrwxr-xr-x  3 halil  staff   96 Aug 13 10:02 .\n",
	})

	c, data := copyRequest(t, s, `{"request_id":"r1","pane_id":"p1"}`)

	if got := data["text"].(string); !strings.Contains(got, "drwxr-xr-x") {
		t.Errorf("clipboard text = %q, want the pane's shell output", got)
	}
	if got := data["request_id"].(string); got != "r1" {
		t.Errorf("request_id = %q, want r1 — the client correlates replies by it", got)
	}
	if got := data["source"].(string); got != "output" {
		t.Errorf("source = %q, want the defaulted \"output\"", got)
	}
	if data["truncated"].(bool) {
		t.Error("truncated = true for a 2-line buffer")
	}
	if got := data["err"].(string); got != "" {
		t.Errorf("err = %q, want empty", got)
	}

	// The security property. Every other server-side push goes through
	// hub.broadcast (buffered, cap 64), so if the handler had used
	// sendAll/sendWhere the message would be sitting in it right now — no hub
	// goroutine needs to be running for this to be a real check.
	if n := len(s.hub.broadcast); n != 0 {
		t.Errorf("clipboard_copy put %d message(s) on the hub broadcast channel; the reply "+
			"must go to the asking client ONLY, or one viewer's copy reaches every "+
			"connected client", n)
	}
	if len(c.send) != 0 {
		t.Errorf("%d extra message(s) queued for the asker; expected exactly one reply", len(c.send))
	}
}

// TestClipboardCopySelectsTheRequestedPlane — a rysh pane has several output
// planes and copying the wrong one is a silent wrong answer, not an error.
func TestClipboardCopySelectsTheRequestedPlane(t *testing.T) {
	s := newClipboardTestServer(t, "p2", &domain.PaneSnapshot{
		ID:         "p2",
		Output:     "SHELL-PLANE",
		AIOutput:   "AI-PLANE",
		RyshOutput: "RYSH-PLANE",
		VTScreen:   []string{"line one", "line two"},
	})

	for _, tc := range []struct{ source, want string }{
		{"output", "SHELL-PLANE"},
		{"ai_output", "AI-PLANE"},
		{"rysh_output", "RYSH-PLANE"},
		{"vt_screen", "line one\nline two"},
	} {
		_, data := copyRequest(t, s, `{"request_id":"r","pane_id":"p2","source":"`+tc.source+`"}`)
		if got := data["text"].(string); got != tc.want {
			t.Errorf("source %q returned %q, want %q", tc.source, got, tc.want)
		}
		if got := data["source"].(string); got != tc.source {
			t.Errorf("reply echoed source %q, want %q", got, tc.source)
		}
	}
}

// TestClipboardCopyUnknownSourceAnswersWithAnError — the rest of this protocol
// drops a bad command in silence (spec §4). A clipboard button that does
// nothing, with no error, is a bug report nobody can act on, so this command
// answers instead.
func TestClipboardCopyUnknownSourceAnswersWithAnError(t *testing.T) {
	s := newClipboardTestServer(t, "p3", &domain.PaneSnapshot{ID: "p3", Output: "x"})

	_, data := copyRequest(t, s, `{"request_id":"r3","pane_id":"p3","source":"secrets"}`)

	if got := data["err"].(string); got == "" {
		t.Fatal("an unknown source was answered with no err — silently returning empty text " +
			"tells the user their pane is empty, which is a different and wrong story")
	}
	if got := data["text"].(string); got != "" {
		t.Errorf("text = %q on an error reply, want empty", got)
	}
	if got := data["err"].(string); !strings.Contains(got, "secrets") {
		t.Errorf("err = %q, want it to name the rejected source", got)
	}
}

// TestClipboardCopyTruncatesFromTheTail — the ceiling exists so one copy of a
// busy pane cannot blow gorilla's write deadline and drop the connection. Which
// END survives matters: the tail is the recent output, and that is what the user
// pointing at the pane is asking for.
func TestClipboardCopyTruncatesFromTheTail(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 5000; i++ {
		b.WriteString("filler line that is here only to exceed the requested cap\n")
	}
	b.WriteString("THE-LAST-LINE\n")

	s := newClipboardTestServer(t, "p4", &domain.PaneSnapshot{ID: "p4", Output: b.String()})

	_, data := copyRequest(t, s, `{"request_id":"r4","pane_id":"p4","max_bytes":2048}`)

	text := data["text"].(string)
	if !data["truncated"].(bool) {
		t.Error("truncated = false after capping a 290 KB buffer to 2 KB")
	}
	if len(text) > 2048 {
		t.Errorf("returned %d bytes, want <= the requested 2048", len(text))
	}
	if !strings.Contains(text, "THE-LAST-LINE") {
		t.Error("truncation dropped the TAIL; the most recent output is the part the user wants")
	}
}

// TestClipboardCopyCannotRaiseTheCeiling — max_bytes is a request for LESS. A
// client asking for more than clipboardMaxBytes must not be able to talk the
// server into a write that misses the deadline.
func TestClipboardCopyCannotRaiseTheCeiling(t *testing.T) {
	huge := strings.Repeat("x", clipboardMaxBytes*2)
	s := newClipboardTestServer(t, "p5", &domain.PaneSnapshot{ID: "p5", Output: huge})

	_, data := copyRequest(t, s, `{"request_id":"r5","pane_id":"p5","max_bytes":99999999}`)

	if got := len(data["text"].(string)); got > clipboardMaxBytes {
		t.Errorf("returned %d bytes for a max_bytes of 99999999; the ceiling is %d and a "+
			"client must not be able to raise it", got, clipboardMaxBytes)
	}
	if !data["truncated"].(bool) {
		t.Error("truncated = false although the buffer was capped at the ceiling")
	}
}

// TestClipboardCopyIgnoresUnanswerableRequests — with no request_id there is no
// way to correlate a reply and with no pane_id nothing to answer about. This is
// the one case where silence is right, and it must not leak a reply carrying an
// empty correlation id that a client would match against the wrong request.
func TestClipboardCopyIgnoresUnanswerableRequests(t *testing.T) {
	s := newClipboardTestServer(t, "p6", &domain.PaneSnapshot{ID: "p6", Output: "x"})

	for _, params := range []string{
		`{"pane_id":"p6"}`,
		`{"request_id":"r6"}`,
		`{`,
	} {
		c := &wsClient{send: make(chan []byte, 4)}
		s.handleClipboardCopy(c, json.RawMessage(params))
		// Must outlast the 1s bus request timeout. At 250ms this test passed
		// even with the guard clause deleted: the handler went to the bus for a
		// pane named "", timed out a second later, and answered with an err —
		// long after the assertion had already succeeded.
		select {
		case raw := <-c.send:
			t.Errorf("params %s were answered with %s, want silence", params, raw)
		case <-time.After(2 * time.Second):
		}
	}
}
