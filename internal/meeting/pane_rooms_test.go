// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// paneRyshServer is a fake rysh-server for meeting-pane rooms.
type paneRyshServer struct {
	mu    sync.Mutex
	calls []string
	auth  []string
	fail  map[string]int // "METHOD path" → status
}

func (f *paneRyshServer) start(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		f.mu.Lock()
		f.calls = append(f.calls, key)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		code := f.fail[key]
		f.mu.Unlock()
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case key == "POST /api/meeting-rooms":
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["title"] == "" || in["ttl_hours"] == nil {
				t.Errorf("create body %v", in)
			}
			_, _ = w.Write([]byte(`{"room":{"id":"room-new","owner_id":"u1","room_name":"rn-new"}}`))
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/meeting-rooms/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/meeting-rooms/")
			_, _ = w.Write([]byte(`{"room":{"id":"` + id + `","owner_id":"u1","room_name":"rn-` + id + `"}}`))
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/token"):
			_, _ = w.Write([]byte(`{"token":"lk-participant-jwt","url":"wss://lk.example","room_name":"rn","identity":"u1","name":"Ann","expires_at":"2026-09-28T21:00:00Z"}`))
		default:
			t.Errorf("unexpected %s", key)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestParseRoomRef(t *testing.T) {
	const server = "https://rysh.example"
	for arg, want := range map[string]string{
		"rysh:abc-123":                      "abc-123",
		"https://rysh.example/meet/abc-123": "abc-123",
		"https://RYSH.example/meet/abc_1":   "abc_1",
	} {
		ref, ok, err := ParseRoomRef(arg, server+"/")
		if !ok || err != nil || ref.Room != want || ref.Server != server || ref.MeetLink() != server+"/meet/"+want {
			t.Errorf("%s: %+v %v %v", arg, ref, ok, err)
		}
	}
	// Not a rysh room: left to the Meet/Zoom/Teams validator.
	for _, arg := range []string{"https://meet.google.com/abc-defg-hij", "new", "https://rysh.example/rooms/x",
		// Teams short links are /meet/<id> as well.
		"https://teams.microsoft.com/meet/2345678901234?p=AbCdEfGh", "https://teams.live.com/meet/9876543210987?p=XyZ123"} {
		if _, ok, _ := ParseRoomRef(arg, server); ok {
			t.Errorf("%s parsed as a rysh room", arg)
		}
	}
	// A rysh room, but refused: never send the key to another host.
	for _, arg := range []string{
		"https://evil.example/meet/abc", "http://rysh.example/meet/abc", "https://rysh.example/meet/a/b",
		"https://rysh.example/meet/abc?x=1", "https://u:p@rysh.example/meet/abc", "rysh:../x", "rysh:",
	} {
		if _, ok, err := ParseRoomRef(arg, server); !ok || err == nil {
			t.Errorf("%s: ok=%v err=%v", arg, ok, err)
		}
	}
	if _, ok, err := ParseRoomRef("rysh:abc", ""); !ok || err == nil || !strings.Contains(err.Error(), "RYSH_SERVER_URL") {
		t.Errorf("no server: %v", err)
	}
	// A /meet/ link on a session with no server names the link's host as
	// not this session's server (still refused).
	if _, ok, err := ParseRoomRef("https://evil.example/meet/x", ""); !ok || err == nil || !strings.Contains(err.Error(), "evil.example, which is not this session's rysh server") {
		t.Errorf("foreign link, no server: %v", err)
	}
}

func TestPaneRoomCreateCheckAndToken(t *testing.T) {
	f := &paneRyshServer{fail: map[string]int{}}
	srv := f.start(t)
	ctx := context.Background()
	ref, err := CreateRoom(ctx, srv.URL, "rysh_key", "", 90*time.Minute)
	if err != nil || ref.Room != "room-new" || ref.Server != srv.URL {
		t.Fatalf("%+v %v", ref, err)
	}
	if err := CheckRoom(ctx, RoomRef{Server: srv.URL, Room: "r1"}, "rysh_key"); err != nil {
		t.Fatal(err)
	}
	g, err := ParticipantToken(ctx, RoomRef{Server: srv.URL, Room: "r1"}, "rysh_key")
	if err != nil || g.Token != "lk-participant-jwt" || g.URL != "wss://lk.example" || g.Identity != "u1" || g.ExpiresAt == "" {
		t.Fatalf("%+v %v", g, err)
	}
	for _, a := range f.auth {
		if a != "Bearer rysh_key" {
			t.Fatalf("auth header %q", a)
		}
	}
	if got := strings.Join(f.calls, ","); got != "POST /api/meeting-rooms,GET /api/meeting-rooms/r1,POST /api/meeting-rooms/r1/token" {
		t.Fatal(got)
	}

	// Errors map to operator text and never echo the token or key.
	f.fail["POST /api/meeting-rooms/r2/token"] = 410
	if _, err := ParticipantToken(ctx, RoomRef{Server: srv.URL, Room: "r2"}, "rysh_key"); err == nil || !strings.Contains(err.Error(), "ended") || strings.Contains(err.Error(), "rysh_key") {
		t.Fatal(err)
	}
	f.fail["GET /api/meeting-rooms/r3"] = 404
	if err := CheckRoom(ctx, RoomRef{Server: srv.URL, Room: "r3"}, "rysh_key"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatal(err)
	}
	if _, err := ParticipantToken(ctx, RoomRef{Server: srv.URL, Room: "r1"}, ""); err == nil || !strings.Contains(err.Error(), RyshAPIKeySecret) {
		t.Fatal("token without a key:", err)
	}
	if _, err := ParticipantToken(ctx, RoomRef{Server: "https://evil.example:0", Room: "../x"}, "k"); err == nil {
		t.Fatal("invalid room accepted")
	}
}
