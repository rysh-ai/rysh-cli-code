// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const ownerID = "0b6f3c5e-1111-4222-8333-944455556666"

// fakeRyshServer implements the LK-3 rysh-server meeting-room contract
// (/tmp/meet-work/lk-tokens.md) as far as host --server uses it.
type fakeRyshServer struct {
	mu        sync.Mutex
	calls     []string
	fail      map[string]int // "METHOD /path-suffix" → status
	ended     []string
	createReq map[string]any
	inviteReq map[string]any
}

func (f *fakeRyshServer) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		op := r.Method + " " + r.URL.Path
		f.calls = append(f.calls, op)
		if r.Header.Get("Authorization") != "Bearer rysh_testkey" {
			w.WriteHeader(401)
			return
		}
		for suffix, code := range f.fail {
			if strings.HasSuffix(op, suffix) || op == suffix {
				w.WriteHeader(code)
				_, _ = io.WriteString(w, `{"error":"x","upgrade_url":"https://rysh.ai/pricing","token":"leak-me"}`)
				return
			}
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch op {
		case "POST /api/meeting-rooms":
			f.createReq = body
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{"room": map[string]any{"id": "r-123", "owner_id": ownerID, "room_name": "rysh-r-123", "livekit_url": "wss://lk.example"}})
		case "POST /api/meeting-rooms/r-123/agent-token":
			if body["agent"] != "rysh" {
				w.WriteHeader(400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "agent-jwt", "url": "wss://lk.example", "room_name": "rysh-r-123", "identity": "agent-rysh"})
		case "POST /api/meeting-rooms/r-123/invites":
			f.inviteReq = body
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{"invite": map[string]any{"id": "i1"}, "token": "rysh_mi_x", "join_url": "https://rysh.example/meet/r-123#invite=rysh_mi_x"})
		case "DELETE /api/meeting-rooms/r-123":
			f.ended = append(f.ended, "r-123")
			_ = json.NewEncoder(w).Encode(map[string]any{"room": map[string]any{"id": "r-123"}})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type serverRig struct {
	m      *Manager
	room   *fakeRoom
	rs     *fakeRyshServer
	base   string
	creds  Credentials
	ran    chan string
	dialed []string
}

func newServerRig(t *testing.T) *serverRig {
	old := DialLiveKit
	t.Cleanup(func() { DialLiveKit = old })
	h := &serverRig{m: NewManager(), room: newFakeRoom(), rs: &fakeRyshServer{fail: map[string]int{}}, ran: make(chan string, 4)}
	h.base = h.rs.server(t).URL
	// No LiveKit API key/secret: server mode must not need them.
	h.creds = Credentials{Speech: "speech", RyshAPIKey: "rysh_testkey", LiveKit: LiveKitCredentials{STTKey: "dg"}}
	DialLiveKit = func(ctx context.Context, serverURL, token string, opts LiveKitDialOptions) (LiveKitRoom, error) {
		h.dialed = append(h.dialed, serverURL+" "+token+" "+opts.STTKey)
		return h.room, nil
	}
	t.Cleanup(h.m.Close)
	return h
}

func (h *serverRig) host(t *testing.T, c Command) Reply {
	t.Helper()
	c.Action, c.Server = "host", h.base
	r := h.m.Do(c, h.creds, func(ctx context.Context, text string) (string, error) {
		h.ran <- text
		return "Yes.", nil
	})
	if s := h.m.active(); s != nil {
		s.mu.Lock()
		s.speech = func(context.Context, string) ([]byte, error) { return []byte("OGG"), nil }
		s.speechStream = nil              // whole-file path; streaming has its own tests
		s.voice, s.voiceStream = nil, nil // agents speak in the stubbed default voice
		s.mu.Unlock()
		s.poll()
	}
	return r
}

func TestHostOnServerUsesServerTokensAndInvite(t *testing.T) {
	h := newServerRig(t)
	r := h.host(t, Command{Room: "standup", Minutes: 60})
	if r.Error != "" || r.State != "joining" || r.Room != "rysh-r-123" || r.JoinURL != "https://rysh.example/meet/r-123#invite=rysh_mi_x" {
		t.Fatalf("%+v", r)
	}
	if len(h.dialed) != 1 || h.dialed[0] != "wss://lk.example agent-jwt dg" {
		t.Fatalf("dialed %v", h.dialed)
	}
	if h.rs.createReq["title"] != "standup" || h.rs.createReq["ttl_hours"] != float64(2) {
		t.Fatalf("create body %v", h.rs.createReq)
	}
	if h.rs.inviteReq["ttl_minutes"] != float64(70) || h.rs.inviteReq["max_uses"] != float64(20) {
		t.Fatalf("invite body %v", h.rs.inviteReq)
	}
	want := []string{"POST /api/meeting-rooms", "POST /api/meeting-rooms/r-123/agent-token", "POST /api/meeting-rooms/r-123/invites"}
	if strings.Join(h.rs.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %v", h.rs.calls)
	}
	// "host" is the room owner's account id; the bot's own identity and a
	// guest account are not.
	h.room.events <- LiveKitEvent{Kind: LiveKitUtterance, Identity: "agent-rysh", Name: botName, Text: "hey rysh hello"}
	h.room.events <- LiveKitEvent{Kind: LiveKitUtterance, Identity: "guest-account", Name: "Owner Name", Text: "hey rysh hello"}
	waitFor(t, func() bool { return h.m.active().status(false).IgnoredWakes == 1 })
	h.room.events <- LiveKitEvent{Kind: LiveKitUtterance, Identity: ownerID, Name: "Owner", Text: "hey rysh are we live"}
	if got := <-h.ran; got != "are we live" {
		t.Fatal(got)
	}
	waitFor(t, func() bool { return h.room.plays() == 1 })
	if len(h.rs.ended) != 0 {
		t.Fatal("a working room was ended")
	}
}

func TestHostOnServerAllowsAccountIDs(t *testing.T) {
	h := newServerRig(t)
	h.host(t, Command{Allow: []string{"guest-account"}})
	h.room.events <- LiveKitEvent{Kind: LiveKitUtterance, Identity: "guest-account", Name: "G", Text: "hey rysh ping"}
	if got := <-h.ran; got != "ping" {
		t.Fatal(got)
	}
}

func TestHostOnServerFailures(t *testing.T) {
	cases := []struct {
		fail   map[string]int
		want   string
		ended  bool
		dialed bool
	}{
		{map[string]int{"POST /api/meeting-rooms": 402}, "upgrade: https://rysh.ai/pricing", false, false},
		{map[string]int{"POST /api/meeting-rooms": 409}, "maximum number of open rooms", false, false},
		{map[string]int{"POST /api/meeting-rooms": 503}, "not available on this rysh server", false, false},
		{map[string]int{"/agent-token": 403}, "write permission", true, false},
		{map[string]int{"/invites": 500}, "HTTP 500", true, false},
	}
	for _, tc := range cases {
		h := newServerRig(t)
		h.rs.fail = tc.fail
		r := h.host(t, Command{})
		if r.State != "create_failed" || !strings.Contains(r.Error, tc.want) || strings.Contains(r.Error, "leak-me") || h.m.active() != nil {
			t.Errorf("%v: %+v", tc.fail, r)
		}
		if (len(h.rs.ended) == 1) != tc.ended || (len(h.dialed) == 1) != tc.dialed {
			t.Errorf("%v: ended %v dialed %v", tc.fail, h.rs.ended, h.dialed)
		}
	}

	h := newServerRig(t)
	h.creds.RyshAPIKey = "rysh_wrong"
	if r := h.host(t, Command{}); !strings.Contains(r.Error, "rejected the API key") {
		t.Fatalf("%+v", r)
	}
	h.creds.RyshAPIKey = ""
	if r := h.host(t, Command{}); !strings.Contains(r.Error, RyshAPIKeySecret) || len(h.rs.calls) != 1 {
		t.Fatalf("%+v calls %v", r, h.rs.calls)
	}

	// The room and invite exist; only the bot failed to connect: people can
	// still use the invite, so the room is kept and the link returned.
	h = newServerRig(t)
	DialLiveKit = nil
	r := h.host(t, Command{})
	if r.State != "create_failed" || r.JoinURL == "" || len(h.rs.ended) != 0 || !strings.Contains(r.Notice, "invite works") {
		t.Fatalf("%+v", r)
	}
}

func TestValidServerURL(t *testing.T) {
	for _, ok := range []string{"https://rysh.ai", "https://dev.rysh.ai/", "http://127.0.0.1:8080", "http://localhost:8080"} {
		if _, err := validServerURL(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://rysh.ai", "rysh.ai", "https://u:p@rysh.ai", "https://rysh.ai?x=1", "ftp://rysh.ai", ""} {
		if _, err := validServerURL(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	c := Command{Action: "host", Server: "http://evil.example"}
	c.Defaults()
	if c.ValidateHost() == nil {
		t.Fatal("ValidateHost accepted a plain-http remote server")
	}
}

// E-86: a missing speech key is caught before any rysh-server request, so no
// room is created and none has to be ended again.
func TestHostOnServerPreflightsSpeechKeys(t *testing.T) {
	for _, tc := range []struct {
		name      string
		c         Command
		noTTS     bool
		noSTT     bool
		want, why string
	}{
		{"stt default", Command{}, false, true, "DEEPGRAM_API_KEY", "speech-to-text"},
		{"stt elevenlabs", Command{STT: ProviderElevenLabs}, false, true, "ELEVENLABS_API_KEY", "--stt elevenlabs"},
		{"tts default", Command{}, true, false, "DEEPGRAM_API_KEY", "text-to-speech"},
		{"tts openai", Command{TTS: ProviderOpenAI}, true, false, "OPENAI_API_KEY", "--tts openai"},
	} {
		h := newServerRig(t)
		if tc.noTTS {
			h.creds.Speech = ""
		}
		if tc.noSTT {
			h.creds.LiveKit.STTKey = ""
		}
		r := h.host(t, tc.c)
		if !strings.Contains(r.Error, "set "+tc.want+" ") || !strings.Contains(r.Error, tc.why) {
			t.Errorf("%s: error %q", tc.name, r.Error)
		}
		if len(h.rs.calls) != 0 || len(h.dialed) != 0 || h.m.active() != nil {
			t.Errorf("%s: calls %v dialed %v", tc.name, h.rs.calls, h.dialed)
		}
	}
}
