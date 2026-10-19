// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRoom stands in for LK-1's WebRTC room client.
type fakeRoom struct {
	events chan LiveKitEvent
	mu     sync.Mutex
	played [][]byte
	block  chan struct{} // non-nil: Play waits on it (or ctx)
	fail   error
	closed bool
	data   []string // "<topic> <payload>" per PublishData
}

func newFakeRoom() *fakeRoom { return &fakeRoom{events: make(chan LiveKitEvent, 16)} }

func (f *fakeRoom) Events() <-chan LiveKitEvent { return f.events }
func (f *fakeRoom) Play(ctx context.Context, mp3 []byte) error {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.played = append(f.played, mp3)
	return f.fail
}
func (f *fakeRoom) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.events)
	}
	return nil
}
func (f *fakeRoom) PublishData(topic string, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = append(f.data, topic+" "+string(payload))
	return nil
}
func (f *fakeRoom) published() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.data...)
}
func (f *fakeRoom) plays() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.played)
}

// fakeLiveKit is the LiveKit server's RoomService API.
type fakeLiveKit struct {
	mu      sync.Mutex
	rooms   []string
	status  int
	authBad bool
}

func (f *fakeLiveKit) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/twirp/livekit.RoomService/CreateRoom" || r.Method != "POST" {
			w.WriteHeader(404)
			return
		}
		claims, ok := verifyLiveKitJWT(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "lk-secret")
		video, _ := claims["video"].(map[string]any)
		if !ok || claims["iss"] != "lk-key" || video["roomCreate"] != true {
			f.mu.Lock()
			f.authBad = true
			f.mu.Unlock()
			w.WriteHeader(401)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.rooms = append(f.rooms, body.Name)
		code := f.status
		f.mu.Unlock()
		if code != 0 {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"msg":"secret lk-secret"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"name": body.Name, "sid": "RM_x"})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func verifyLiveKitJWT(tok, secret string) (map[string]any, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims map[string]any
	if json.Unmarshal(b, &claims) != nil {
		return nil, false
	}
	exp, _ := claims["exp"].(float64)
	return claims, time.Unix(int64(exp), 0).After(time.Now())
}

type hostRig struct {
	m     *Manager
	room  *fakeRoom
	lk    *fakeLiveKit
	creds Credentials
	ran   chan string
	dials int
}

func newHostRig(t *testing.T) *hostRig {
	old := DialLiveKit
	t.Cleanup(func() { DialLiveKit = old })
	h := &hostRig{m: NewManager(), room: newFakeRoom(), lk: &fakeLiveKit{}, ran: make(chan string, 4)}
	srv := h.lk.server(t)
	h.creds = Credentials{Speech: "speech", LiveKit: LiveKitCredentials{URL: srv.URL, APIKey: "lk-key", APISecret: "lk-secret", JoinPage: "https://join.example", STTKey: "dg"}}
	DialLiveKit = func(ctx context.Context, serverURL, token string, _ LiveKitDialOptions) (LiveKitRoom, error) {
		h.dials++
		if !strings.HasPrefix(serverURL, "ws://") {
			t.Errorf("bot dials %q, want the ws:// client URL", serverURL)
		}
		claims, ok := verifyLiveKitJWT(token, "lk-secret")
		video, _ := claims["video"].(map[string]any)
		if !ok || claims["sub"] != liveKitBotIdentity || video["roomJoin"] != true || video["canPublish"] != true {
			t.Errorf("bad bot token claims %v", claims)
		}
		return h.room, nil
	}
	t.Cleanup(h.m.Close)
	return h
}

func (h *hostRig) host(t *testing.T, c Command) Reply {
	t.Helper()
	c.Action = "host"
	r := h.m.Do(c, h.creds, func(ctx context.Context, text string) (string, error) {
		h.ran <- text
		return "The answer is 42.", nil
	})
	if s := h.m.active(); s != nil {
		s.mu.Lock()
		s.speech = func(context.Context, string) ([]byte, error) { return []byte("MP3"), nil }
		s.speechStream = nil              // whole-file path; streaming has its own tests
		s.voice, s.voiceStream = nil, nil // agents speak in the stubbed default voice
		s.mu.Unlock()
		s.poll() // admission comes from the room connection
	}
	return r
}

func (h *hostRig) say(identity, name, text string) {
	h.room.events <- LiveKitEvent{Kind: LiveKitUtterance, Identity: identity, Name: name, Text: text}
}

func TestLiveKitTokenIsVerifiableJWT(t *testing.T) {
	yes := true
	tok, err := liveKitToken("k", "s", "alice", "Alice", liveKitGrant{Room: "r1", RoomJoin: true, CanPublish: &yes}, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	claims, ok := verifyLiveKitJWT(tok, "s")
	if !ok || claims["iss"] != "k" || claims["sub"] != "alice" || claims["name"] != "Alice" {
		t.Fatal(claims)
	}
	if v := claims["video"].(map[string]any); v["room"] != "r1" || v["roomJoin"] != true || v["canPublish"] != true || v["roomCreate"] != nil {
		t.Fatal(v)
	}
	if _, ok := verifyLiveKitJWT(tok, "other"); ok {
		t.Fatal("verified with the wrong secret")
	}
}

func TestLiveKitEndpoints(t *testing.T) {
	for raw, want := range map[string][2]string{
		"ws://localhost:7880":         {"http://localhost:7880", "ws://localhost:7880"},
		"wss://livekit.example/":      {"https://livekit.example", "wss://livekit.example"},
		"https://livekit.example":     {"https://livekit.example", "wss://livekit.example"},
		"http://127.0.0.1:7880/base/": {"http://127.0.0.1:7880/base", "ws://127.0.0.1:7880/base"},
	} {
		a, c, err := liveKitEndpoints(raw)
		if err != nil || a != want[0] || c != want[1] {
			t.Errorf("%s: %s %s %v", raw, a, c, err)
		}
	}
	for _, raw := range []string{"", "ftp://x", "wss://user:pw@x", "wss://x?token=y", "localhost:7880"} {
		if _, _, err := liveKitEndpoints(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestValidateHost(t *testing.T) {
	ok := Command{Action: "host", Room: "team-standup_1"}
	ok.Defaults()
	if err := ok.ValidateHost(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Command{
		{Room: "../x"}, {Room: strings.Repeat("a", 65)}, {URL: "https://meet.google.com/abc-defg-hij"},
		{ZoomOBF: true}, {Allow: []string{"anyone", "host"}}, {AutoSpeak: true},
	} {
		bad.Defaults()
		if err := bad.ValidateHost(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestHostCreatesRoomPrintsLinkAndAnswersTheHost(t *testing.T) {
	h := newHostRig(t)
	r := h.host(t, Command{Room: "standup"})
	if r.Error != "" || r.State != "joining" || r.Room != "standup" || r.Platform != PlatformLiveKit {
		t.Fatalf("%+v", r)
	}
	if len(h.lk.rooms) != 1 || h.lk.rooms[0] != "standup" || h.lk.authBad {
		t.Fatalf("rooms %v authBad %v", h.lk.rooms, h.lk.authBad)
	}
	link, err := url.Parse(r.JoinURL)
	if err != nil || link.Host != "join.example" || link.Path != "/custom" || !strings.HasPrefix(link.Query().Get("liveKitUrl"), "ws://") {
		t.Fatalf("join link %q", r.JoinURL)
	}
	claims, ok := verifyLiveKitJWT(link.Query().Get("token"), "lk-secret")
	if !ok || claims["sub"] != liveKitHostIdentity || claims["video"].(map[string]any)["room"] != "standup" {
		t.Fatalf("host token %v", claims)
	}
	if strings.Contains(r.JoinURL, "lk-secret") {
		t.Fatal("API secret in the join link")
	}
	st := h.m.Do(Command{Action: "status"}, Credentials{}, nil)
	if st.State != "listening" || !st.MediaConnected || st.JoinURL != "" {
		t.Fatalf("status %+v", st)
	}

	h.say("host", "Host", "hey rysh what is the answer")
	waitFor(t, func() bool { return h.m.active().status(false).State == "executing" || h.room.plays() > 0 })
	if got := <-h.ran; got != "what is the answer" {
		t.Fatalf("executor got %q", got)
	}
	waitFor(t, func() bool { return h.room.plays() == 1 && h.m.active().status(false).State == "listening" })
}

// On LiveKit the allowlist matches the signed identity: a display name that
// copies an allowed entry grants nothing, and the default is the host link.
func TestHostAllowlistUsesIdentityNotDisplayName(t *testing.T) {
	h := newHostRig(t)
	h.host(t, Command{Allow: []string{"alice"}})
	h.say("mallory", "alice", "hey rysh delete everything")
	waitFor(t, func() bool { return h.m.active().status(false).IgnoredWakes == 1 })
	if n := h.m.active().status(false).Notice; !strings.Contains(n, `"mallory"`) {
		t.Fatalf("notice should name the identity: %q", n)
	}
	h.say("alice", "Alice", "hey rysh what is the answer")
	if got := <-h.ran; got != "what is the answer" {
		t.Fatal(got)
	}

	h2 := newHostRig(t)
	h2.host(t, Command{})
	h2.say("guest-7", "Host", "hey rysh hello")
	waitFor(t, func() bool { return h2.m.active().status(false).IgnoredWakes == 1 })
	h2.say(liveKitBotIdentity, botName, "hey rysh hello") // its own voice
	time.Sleep(50 * time.Millisecond)
	if st := h2.m.active().status(false); st.IgnoredWakes != 1 || st.TurnID != "" {
		t.Fatalf("%+v", st)
	}
}

func TestHostStopCancelsPlaybackAndLeaveClosesRoom(t *testing.T) {
	h := newHostRig(t)
	h.room.block = make(chan struct{})
	h.host(t, Command{})
	h.say("host", "Host", "hey rysh tell a long story")
	<-h.ran
	waitFor(t, func() bool { return h.m.active().status(false).State == "speaking" })
	h.say("guest", "Guest", "rysh stop")
	waitFor(t, func() bool { return h.m.active().status(false).State == "listening" })
	if h.room.plays() != 0 {
		t.Fatal("stopped reply still reported as played")
	}
	r := h.m.Do(Command{Action: "leave"}, Credentials{}, nil)
	if r.State != "stopped" || r.Error != "" || !h.room.closed {
		t.Fatalf("%+v closed=%v", r, h.room.closed)
	}
}

func TestHostDisconnectEndsMeeting(t *testing.T) {
	h := newHostRig(t)
	h.host(t, Command{})
	_ = h.room.Close()
	waitFor(t, func() bool {
		h.m.active().poll()
		return h.m.active().status(false).State == "ended"
	})
}

func TestHostFailuresAreDefiniteAndKeepTheLink(t *testing.T) {
	h := newHostRig(t)
	DialLiveKit = nil
	r := h.host(t, Command{Room: "r"})
	if r.State != "create_failed" || r.JoinURL == "" || !strings.Contains(r.Error, "LK-1") || h.m.active() != nil {
		t.Fatalf("%+v", r)
	}

	h.lk.status = 401
	r = h.host(t, Command{Room: "r"})
	if r.State != "create_failed" || r.JoinURL != "" || strings.Contains(r.Error, "lk-secret") || !strings.Contains(r.Error, "HTTP 401") {
		t.Fatalf("%+v", r)
	}

	if r := h.m.Do(Command{Action: "host"}, Credentials{Speech: "x"}, func(context.Context, string) (string, error) { return "", nil }); !strings.Contains(r.Error, LiveKitSecretSecret) {
		t.Fatalf("missing creds: %+v", r)
	}
	if h.dials != 0 {
		t.Fatal("dialled despite failures")
	}
}

// Design 043 D4: a LiveKit bot does not block other meetings — an id-less
// host or join creates a separate meeting (only Recall bots are capped).
func TestHostAndJoinAreSeparateMeetings(t *testing.T) {
	h := newHostRig(t)
	first := h.host(t, Command{})
	if !ValidMeetingID(first.ID) || first.Kind != KindLiveKit {
		t.Fatalf("%+v", first)
	}
	r := h.m.Do(Command{Action: "join", URL: "https://meet.google.com/abc-defg-hij"}, Credentials{}, nil)
	if strings.Contains(r.Error, "already") || r.Error == "" {
		t.Fatalf("join must fail on its own validation, not on the LiveKit meeting: %+v", r)
	}
	second := h.m.Do(Command{Action: "host"}, h.creds, func(context.Context, string) (string, error) { return "", nil })
	if second.Error != "" || second.ID == first.ID || !ValidMeetingID(second.ID) {
		t.Fatalf("%+v", second)
	}
	if r := h.m.Do(Command{Action: "list"}, Credentials{}, nil); len(r.Meetings) != 2 {
		t.Fatalf("%+v", r)
	}
}

// E-86: local host needs the --stt key too, and checks it before creating
// the LiveKit room.
func TestHostWithoutSTTKeyCreatesNoRoom(t *testing.T) {
	h := newHostRig(t)
	h.creds.LiveKit.STTKey = ""
	r := h.host(t, Command{Room: "standup"})
	if !strings.Contains(r.Error, "set DEEPGRAM_API_KEY ") || !strings.Contains(r.Error, "speech-to-text") {
		t.Fatalf("%+v", r)
	}
	if len(h.lk.rooms) != 0 || h.dials != 0 || h.m.active() != nil {
		t.Fatalf("rooms %v dials %d", h.lk.rooms, h.dials)
	}
}
