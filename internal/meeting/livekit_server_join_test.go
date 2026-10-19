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
)

const existingRoom = "7c1d2e3f-aaaa-4bbb-8ccc-dddddddddddd"

// joinRyshServer is rysh-server with one room a human already opened: the
// join-room step may read it and mint an agent token, nothing else.
type joinRyshServer struct {
	mu     sync.Mutex
	calls  []string
	agents []string
	fail   map[string]int // "METHOD /path" → status
}

func (f *joinRyshServer) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		op := r.Method + " " + r.URL.Path
		f.calls = append(f.calls, op)
		if r.Header.Get("Authorization") != "Bearer rysh_testkey" {
			w.WriteHeader(401)
			return
		}
		if code := f.fail[op]; code != 0 {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"x","token":"leak-me"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch op {
		case "GET /api/meeting-rooms/" + existingRoom:
			_ = json.NewEncoder(w).Encode(map[string]any{"room": map[string]any{"id": existingRoom, "owner_id": ownerID, "room_name": "rysh-existing"}})
		case "POST /api/meeting-rooms/" + existingRoom + "/agent-token":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.agents = append(f.agents, body["agent"])
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "agent-jwt", "url": "wss://lk.example", "room_name": "rysh-existing", "identity": "agent-" + body["agent"], "name": body["agent"]})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type joinRig struct {
	m      *Manager
	room   *fakeRoom
	rs     *joinRyshServer
	base   string
	creds  Credentials
	ran    chan string
	dialed []string
}

func newJoinRig(t *testing.T) *joinRig {
	old := DialLiveKit
	t.Cleanup(func() { DialLiveKit = old })
	h := &joinRig{m: NewManager(), room: newFakeRoom(), rs: &joinRyshServer{fail: map[string]int{}}, ran: make(chan string, 4)}
	h.base = h.rs.server(t).URL
	h.creds = Credentials{Speech: "speech", RyshAPIKey: "rysh_testkey", LiveKit: LiveKitCredentials{STTKey: "dg"}}
	DialLiveKit = func(ctx context.Context, serverURL, token string, opts LiveKitDialOptions) (LiveKitRoom, error) {
		h.dialed = append(h.dialed, serverURL+" "+token+" "+opts.STTKey)
		return h.room, nil
	}
	t.Cleanup(h.m.Close)
	return h
}

func (h *joinRig) agent(name string) Agent {
	return Agent{Name: name, Execute: func(ctx context.Context, text string) (string, error) {
		h.ran <- name + ": " + text
		return "Yes.", nil
	}}
}

// join calls the join-room entry point the way a Manager path does:
// without m.mu, committing the bot afterwards.
func (h *joinRig) join(t *testing.T, roomID string, agents ...Agent) Reply {
	t.Helper()
	c := Command{Action: "host", Server: h.base}
	for _, a := range agents {
		c.Agents = append(c.Agents, a.Name)
	}
	c.Defaults()
	mt := &Meeting{ID: "m-00000a", Kind: KindLiveKit, Platform: PlatformLiveKit, Target: roomID}
	slot := &botSlot{}
	r := h.m.joinServerRoom(slot, c, h.creds, func(context.Context, string) (string, error) { return "session", nil }, agents, roomID)
	s := slot.bot
	if s != nil {
		h.m.mu.Lock()
		mt.Bots = []*Service{s}
		h.m.add(mt)
		h.m.mu.Unlock()
	}
	if s != nil {
		s.mu.Lock()
		s.speech = func(context.Context, string) ([]byte, error) { return []byte("OGG"), nil }
		s.speechStream = nil
		s.voice, s.voiceStream = nil, nil // agents speak in the stubbed default voice
		s.mu.Unlock()
		s.poll()
	}
	return r
}

func TestJoinServerRoomJoinsExistingRoomAsTheAgent(t *testing.T) {
	h := newJoinRig(t)
	r := h.join(t, existingRoom, h.agent("Scribe_Bot"))
	if r.Error != "" || r.State != "joining" || r.Room != "rysh-existing" || r.JoinURL != h.base+"/meet/"+existingRoom {
		t.Fatalf("%+v", r)
	}
	want := []string{"GET /api/meeting-rooms/" + existingRoom, "POST /api/meeting-rooms/" + existingRoom + "/agent-token"}
	if strings.Join(h.rs.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v (no create, no invite, no end)", h.rs.calls)
	}
	if len(h.rs.agents) != 1 || h.rs.agents[0] != "scribe-bot" {
		t.Fatalf("agent-token agent %v", h.rs.agents)
	}
	if len(h.dialed) != 1 || h.dialed[0] != "wss://lk.example agent-jwt dg" {
		t.Fatalf("dialed %v", h.dialed)
	}
	// The room owner is "host"; the bot's own voice is not a wake.
	h.room.events <- LiveKitEvent{Kind: LiveKitUtterance, Identity: "agent-scribe-bot", Name: "scribe-bot", Text: "hey rysh hello"}
	h.room.events <- LiveKitEvent{Kind: LiveKitUtterance, Identity: ownerID, Name: "Owner", Text: "hey rysh take notes"}
	if got := <-h.ran; got != "Scribe_Bot: take notes" {
		t.Fatal(got)
	}
	waitFor(t, func() bool { return h.room.plays() == 1 })

	// Later agents attach to the same bot; the last detach and leave keep
	// the room: it belongs to the human who opened it.
	if r := h.m.Do(Command{Action: "attach"}, h.creds, nil, h.agent("coder")); r.Error != "" {
		t.Fatalf("attach %+v", r)
	}
	if r := h.m.Do(Command{Action: "detach", Agent: "coder"}, h.creds, nil); r.Error != "" {
		t.Fatalf("detach %+v", r)
	}
	if r := h.m.Do(Command{Action: "leave"}, h.creds, nil); r.Error != "" || r.State != "stopped" {
		t.Fatalf("leave %+v", r)
	}
	for _, c := range h.rs.calls {
		if strings.HasPrefix(c, "DELETE") {
			t.Fatalf("the bot ended a room it did not create: %v", h.rs.calls)
		}
	}
}

func TestJoinServerRoomSeveralAgentsJoinAsRysh(t *testing.T) {
	h := newJoinRig(t)
	if r := h.join(t, existingRoom, h.agent("scribe"), h.agent("coder")); r.Error != "" {
		t.Fatalf("%+v", r)
	}
	if len(h.rs.agents) != 1 || h.rs.agents[0] != "rysh" {
		t.Fatalf("agent-token agent %v", h.rs.agents)
	}
}

func TestJoinServerRoomFailuresNeverEndTheRoom(t *testing.T) {
	get, tok := "GET /api/meeting-rooms/"+existingRoom, "POST /api/meeting-rooms/"+existingRoom+"/agent-token"
	cases := []struct {
		fail map[string]int
		want string
	}{
		{map[string]int{get: 404}, "room not found"},
		{map[string]int{tok: 403}, "write permission"},
		{map[string]int{tok: 410}, "ended or expired"},
		{map[string]int{tok: 503}, "not available on this rysh server"},
	}
	for _, tc := range cases {
		h := newJoinRig(t)
		h.rs.fail = tc.fail
		r := h.join(t, existingRoom, h.agent("scribe"))
		if r.State != "create_failed" || !strings.Contains(r.Error, tc.want) || strings.Contains(r.Error, "leak-me") || h.m.active() != nil || len(h.dialed) != 0 {
			t.Errorf("%v: %+v dialed %v", tc.fail, r, h.dialed)
		}
		for _, c := range h.rs.calls {
			if strings.HasPrefix(c, "DELETE") {
				t.Errorf("%v: ended the room", tc.fail)
			}
		}
	}

	h := newJoinRig(t)
	if r := h.join(t, "../../admin", h.agent("scribe")); !strings.Contains(r.Error, "invalid rysh room id") || len(h.rs.calls) != 0 {
		t.Fatalf("%+v calls %v", r, h.rs.calls)
	}
	h.creds.RyshAPIKey = ""
	if r := h.join(t, existingRoom, h.agent("scribe")); !strings.Contains(r.Error, RyshAPIKeySecret) || len(h.rs.calls) != 0 {
		t.Fatalf("%+v", r)
	}
	h.creds.RyshAPIKey = "rysh_testkey"
	h.base = "http://evil.example"
	if r := h.join(t, existingRoom, h.agent("scribe")); !strings.Contains(r.Error, "https") {
		t.Fatalf("%+v", r)
	}

	// The token is in hand but the bot cannot connect: the room is left as
	// it is, for the people already in it.
	h = newJoinRig(t)
	DialLiveKit = nil
	r := h.join(t, existingRoom, h.agent("scribe"))
	if r.State != "create_failed" || !strings.Contains(r.Notice, "room is unchanged") || h.m.active() != nil {
		t.Fatalf("%+v", r)
	}
}

func TestServerAgentName(t *testing.T) {
	for in, want := range map[string]string{
		"scribe": "scribe", "Code_Reviewer": "code-reviewer", " -bot- ": "bot", "ünï": "n", "üï": "rysh",
		"": "rysh", strings.Repeat("a", 33): "rysh", "a2": "a2",
	} {
		if got := serverAgentName([]string{in}); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	if got := serverAgentName(nil); got != "rysh" {
		t.Error(got)
	}
	if got := serverAgentName([]string{"a", "b"}); got != "rysh" {
		t.Error(got)
	}
}

// The bot's wall reaches the app on the room's "rysh.wall" data topic: idle
// with its agent's name, then the turn (design 042 LK-4; it was only ever
// sent to the Recall media page, so a rysh room's wall never left
// "<agent> is in the room").
func TestLiveKitBotPublishesItsWall(t *testing.T) {
	h := newJoinRig(t)
	release := make(chan struct{})
	scribe := Agent{Name: "scribe", Execute: func(ctx context.Context, text string) (string, error) {
		h.ran <- text
		<-release // hold the turn in "thinking" until its wall is seen
		return "Yes.", nil
	}}
	if r := h.join(t, existingRoom, scribe); r.Error != "" {
		t.Fatalf("%+v", r)
	}
	has := func(parts ...string) bool {
		for _, d := range h.room.published() {
			ok := strings.HasPrefix(d, "rysh.wall ")
			for _, p := range parts {
				ok = ok && strings.Contains(d, p)
			}
			if ok {
				return true
			}
		}
		return false
	}
	waitFor(t, func() bool { return has(`"phase":"listening"`, `"agent":"scribe"`) })
	h.room.events <- LiveKitEvent{Kind: LiveKitUtterance, Identity: ownerID, Name: "Owner", Text: "hey scribe take notes"}
	if got := <-h.ran; got != "take notes" {
		t.Fatal(got)
	}
	waitFor(t, func() bool {
		return has(`"phase":"thinking"`, `"agent":"scribe"`, `"request":"take notes"`, `"speaker":"Owner"`, `"elapsed_ms":`)
	})
	close(release)
	waitFor(t, func() bool { return h.room.plays() == 1 })
}
