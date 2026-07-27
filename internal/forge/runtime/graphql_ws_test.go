package runtime

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
)

// fakeGraphQLWSServer implements just enough graphql-transport-ws for the
// tests: it upgrades, expects connection_init (→ ack), expects one subscribe,
// then runs `script` with the connection and the subscribe message.
func fakeGraphQLWSServer(t *testing.T, script func(conn *websocket.Conn, sub gqlWSMessage)) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{
		Subprotocols: []string{"graphql-transport-ws"},
		CheckOrigin:  func(*http.Request) bool { return true },
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		var m gqlWSMessage
		if err := conn.ReadJSON(&m); err != nil || m.Type != gqlWSConnectionInit {
			t.Errorf("want connection_init, got %+v (err %v)", m, err)
			return
		}
		if err := conn.WriteJSON(gqlWSMessage{Type: gqlWSConnectionAck}); err != nil {
			return
		}
		if err := conn.ReadJSON(&m); err != nil || m.Type != gqlWSSubscribe {
			t.Errorf("want subscribe, got %+v (err %v)", m, err)
			return
		}
		script(conn, m)
	}))
}

func next(conn *websocket.Conn, id string, payload string) error {
	return conn.WriteJSON(gqlWSMessage{ID: id, Type: gqlWSNext, Payload: json.RawMessage(payload)})
}

// TestGraphQLWSSubscribe: the full happy path — init/ack, subscribe, N next
// frames, complete — lands N frames in the session and a clean end.
func TestGraphQLWSSubscribe(t *testing.T) {
	var gotQuery string
	srv := fakeGraphQLWSServer(t, func(conn *websocket.Conn, sub gqlWSMessage) {
		var p struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(sub.Payload, &p)
		gotQuery = p.Query
		for i := 1; i <= 3; i++ {
			if err := next(conn, sub.ID, fmt.Sprintf(`{"data":{"onEvent":{"n":%d}}}`, i)); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		_ = conn.WriteJSON(gqlWSMessage{ID: sub.ID, Type: gqlWSComplete})
	})
	defer srv.Close()

	exec := NewGraphQLExecutor(srv.URL, nil, Credential{}, Options{})
	m := NewStreamManager(StreamOptions{NoSweeper: true})
	defer m.CloseAll()

	id, err := exec.Subscribe(m, "subscription { onEvent { n } }", nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	frames, res := waitFrames(t, m, id, 3, true)
	if len(frames) != 3 || !strings.Contains(frames[0], `"n": 1`) || !strings.Contains(frames[2], `"n": 3`) {
		t.Fatalf("frames = %v, want the 3 next payloads in order", frames)
	}
	if !res.Done || res.Reason != "stream ended" {
		t.Fatalf("end state = %+v, want a clean complete", res)
	}
	if gotQuery != "subscription { onEvent { n } }" {
		t.Errorf("server saw query %q", gotQuery)
	}
}

// TestGraphQLWSErrorPath: an "error" message ends the session with the
// GraphQL errors as the reason.
func TestGraphQLWSErrorPath(t *testing.T) {
	srv := fakeGraphQLWSServer(t, func(conn *websocket.Conn, sub gqlWSMessage) {
		_ = next(conn, sub.ID, `{"data":{"onEvent":{"n":1}}}`)
		_ = conn.WriteJSON(gqlWSMessage{ID: sub.ID, Type: gqlWSError,
			Payload: json.RawMessage(`[{"message":"subscription blew up"}]`)})
	})
	defer srv.Close()

	exec := NewGraphQLExecutor(srv.URL, nil, Credential{}, Options{})
	m := NewStreamManager(StreamOptions{NoSweeper: true})
	defer m.CloseAll()

	id, err := exec.Subscribe(m, "subscription { onEvent { n } }", nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	frames, res := waitFrames(t, m, id, 1, true)
	if !strings.Contains(frames[0], `"n": 1`) {
		t.Fatalf("pre-error frame missing: %v", frames)
	}
	if !res.Done || !strings.Contains(res.Reason, "subscription blew up") {
		t.Fatalf("end reason = %q, want the graphql error message", res.Reason)
	}
}

// TestGraphQLWSStopClosesSocket: stopping the session tears the websocket down
// (the server's read unblocks with an error).
func TestGraphQLWSStopClosesSocket(t *testing.T) {
	serverDone := make(chan struct{})
	srv := fakeGraphQLWSServer(t, func(conn *websocket.Conn, sub gqlWSMessage) {
		_ = next(conn, sub.ID, `{"data":{"tick":1}}`)
		// Block reading; when the client closes the socket this errors out.
		var m gqlWSMessage
		for conn.ReadJSON(&m) == nil {
		}
		close(serverDone)
	})
	defer srv.Close()

	exec := NewGraphQLExecutor(srv.URL, nil, Credential{}, Options{})
	m := NewStreamManager(StreamOptions{NoSweeper: true})
	defer m.CloseAll()

	id, err := exec.Subscribe(m, "subscription { tick }", nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	waitFrames(t, m, id, 1, false)
	if _, err := m.Stop(id); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case <-serverDone:
	case <-time.After(5 * time.Second):
		t.Fatal("server socket never closed after Stop")
	}
}

// TestGraphQLWSCloseAllTeardown: manager CloseAll (the integration/workspace
// teardown seam) closes live subscription sockets.
func TestGraphQLWSCloseAllTeardown(t *testing.T) {
	serverDone := make(chan struct{})
	srv := fakeGraphQLWSServer(t, func(conn *websocket.Conn, sub gqlWSMessage) {
		_ = next(conn, sub.ID, `{"data":{"tick":1}}`)
		var m gqlWSMessage
		for conn.ReadJSON(&m) == nil {
		}
		close(serverDone)
	})
	defer srv.Close()

	exec := NewGraphQLExecutor(srv.URL, nil, Credential{}, Options{})
	m := NewStreamManager(StreamOptions{NoSweeper: true})

	id, err := exec.Subscribe(m, "subscription { tick }", nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	waitFrames(t, m, id, 1, false)
	m.CloseAll()
	select {
	case <-serverDone:
	case <-time.After(5 * time.Second):
		t.Fatal("server socket never closed after CloseAll")
	}
}

// TestGraphQLWSAuthHeader: the websocket dial carries the same injected auth
// headers as the HTTP path.
func TestGraphQLWSAuthHeader(t *testing.T) {
	gotKey := make(chan string, 1)
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey <- r.Header.Get("X-Api-Key")
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var m gqlWSMessage
		_ = conn.ReadJSON(&m)
		_ = conn.WriteJSON(gqlWSMessage{Type: gqlWSConnectionAck})
		_ = conn.ReadJSON(&m)
		_ = conn.WriteJSON(gqlWSMessage{ID: m.ID, Type: gqlWSComplete})
	}))
	defer srv.Close()

	auth := []ir.AuthScheme{{Name: "key", Type: "apiKey", In: "header", KeyName: "X-Api-Key"}}
	exec := NewGraphQLExecutor(srv.URL, auth, Credential{APIKey: "k-123"}, Options{})
	m := NewStreamManager(StreamOptions{NoSweeper: true})
	defer m.CloseAll()

	if _, err := exec.Subscribe(m, "subscription { tick }", nil); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	select {
	case k := <-gotKey:
		if k != "k-123" {
			t.Fatalf("X-Api-Key = %q, want the injected credential", k)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never received the dial")
	}
}

// TestDeriveWSURL pins the http→ws scheme convention and the override.
func TestDeriveWSURL(t *testing.T) {
	cases := map[string]string{
		"http://api.example.com/graphql":  "ws://api.example.com/graphql",
		"https://api.example.com/graphql": "wss://api.example.com/graphql",
		"ws://already/ws":                 "ws://already/ws",
	}
	for in, want := range cases {
		if got := DeriveWSURL(in); got != want {
			t.Errorf("DeriveWSURL(%q) = %q, want %q", in, got, want)
		}
	}
	exec := NewGraphQLExecutor("https://api.example.com/graphql", nil, Credential{}, Options{})
	if got := exec.WSEndpoint(); got != "wss://api.example.com/graphql" {
		t.Errorf("WSEndpoint = %q, want the derived wss URL", got)
	}
	exec.SetWSEndpoint("wss://sub.example.com/ws")
	if got := exec.WSEndpoint(); got != "wss://sub.example.com/ws" {
		t.Errorf("WSEndpoint after override = %q", got)
	}
}
