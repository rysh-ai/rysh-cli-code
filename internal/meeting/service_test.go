// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const botID = "12345678-1234-1234-1234-123456789abc"

func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition timed out")
}
func fixture(t *testing.T, exec Execute) (*Service, *websocket.Conn, *httptest.Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{ctx: ctx, cancel: cancel, cfg: Command{PublicURL: "https://meeting.example", Approve: true, Allow: []string{"anyone"}}, token: strings.Repeat("a", 64), verifyKey: testKey, botID: botID, state: "listening", admitted: true, execute: exec, seen: map[string]time.Time{}, prefixes: map[int]time.Time{}, speech: func(context.Context, string) ([]byte, error) { return []byte("MP3"), nil }}
	srv := httptest.NewServer(s)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/socket", http.Header{"Origin": []string{"https://meeting.example"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(map[string]string{"token": s.token}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.status(false).MediaConnected })
	t.Cleanup(func() { cancel(); _ = conn.Close(); srv.Close() })
	return s, conn, srv
}
func transcript(text string, speaker int, start float64) []byte {
	b, _ := json.Marshal(map[string]any{"event": "transcript.data", "data": map[string]any{"bot": map[string]any{"id": botID}, "data": map[string]any{"participant": map[string]any{"id": speaker, "name": "Tester"}, "words": []any{map[string]any{"text": text, "start_timestamp": map[string]any{"relative": start}, "end_timestamp": map[string]any{"relative": start + 1}}}}}})
	return b
}
func deliver(t *testing.T, s *Service, id string, body []byte) int {
	t.Helper()
	req := httptest.NewRequest("POST", "https://meeting.example/events", bytes.NewReader(body))
	req.Header = signed(id, body, time.Now())
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w.Code
}
func finalize(s *Service) {
	s.mu.Lock()
	if s.pending != nil {
		s.state = "awaiting_approval"
	}
	s.mu.Unlock()
}
func TestMeetingRoundTripRequiresBothApprovalsAndAcknowledgment(t *testing.T) {
	var calls atomic.Int32
	s, conn, _ := fixture(t, func(_ context.Context, text string) (string, error) {
		calls.Add(1)
		if text != "read report" {
			t.Errorf("wrong executor text: %q", text)
		}
		return "Verified result", nil
	})
	if code := deliver(t, s, "ambient", transcript("ordinary conversation", 1, 1)); code != 200 {
		t.Fatal(code)
	}
	if s.status(false).TurnID != "" || calls.Load() != 0 {
		t.Fatal("ambient execution")
	}
	body := transcript("hey rysh read report", 1, 2)
	deliver(t, s, "wake", body)
	deliver(t, s, "wake", body)
	deliver(t, s, "wake-retry", body)
	finalize(s)
	id := s.status(false).TurnID
	if id == "" || calls.Load() != 0 {
		t.Fatal("missing pending or premature execution")
	}
	if s.status(false).Request != "" {
		t.Fatal("status disclosed transcript")
	}
	if r := s.command(Command{Action: "approve", TurnID: "wrong"}); r.Error == "" {
		t.Fatal("accepted stale approval")
	}
	s.command(Command{Action: "approve", TurnID: id})
	waitFor(t, func() bool { return s.status(false).State == "awaiting_speech" })
	if calls.Load() != 1 {
		t.Fatal("duplicate execution")
	}
	if r := s.command(Command{Action: "approve", TurnID: id}); r.Error == "" {
		t.Fatal("executed twice")
	}
	if r := s.command(Command{Action: "pending"}); r.Result != "Verified result" {
		t.Fatal(r)
	}
	s.command(Command{Action: "speak", TurnID: id})
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var control map[string]string
	if err := conn.ReadJSON(&control); err != nil {
		t.Fatal(err)
	}
	kind, audio, err := conn.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || string(audio) != "MP3" || control["turn"] != id {
		t.Fatal("missing playback", err)
	}
	waitFor(t, func() bool { return s.status(false).State == "speaking" })
	deliver(t, s, "echo", transcript("hey rysh read report", 1, 5))
	if calls.Load() != 1 {
		t.Fatal("echo executed")
	}
	_ = conn.WriteJSON(map[string]string{"type": "played", "turn": id})
	waitFor(t, func() bool { return s.status(false).State == "listening" })
	if s.status(true).Request != "" || s.status(true).Result != "" {
		t.Fatal("retained completed turn")
	}
}
func TestMeetingPauseDropsLateExecutorAndRevokesTurn(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s, _, _ := fixture(t, func(ctx context.Context, _ string) (string, error) {
		close(started)
		<-release
		return "late private result", nil
	})
	deliver(t, s, "wake", transcript("hey rysh test", 1, 1))
	finalize(s)
	id := s.status(false).TurnID
	s.command(Command{Action: "approve", TurnID: id})
	<-started
	s.pause()
	close(release)
	waitFor(t, func() bool { return s.status(false).State == "paused" })
	if r := s.command(Command{Action: "speak", TurnID: id}); r.Error == "" {
		t.Fatal("accepted revoked turn")
	}
	deliver(t, s, "paused", transcript("hey rysh test", 1, 3))
	if s.status(true).Result != "" || s.status(false).TurnID != "" {
		t.Fatal("retained paused turn")
	}
}
func TestMeetingRejectsUnsignedWrongBotPartialsAndOverlappingSpeakers(t *testing.T) {
	s, _, _ := fixture(t, nil)
	body := transcript("hey rysh test", 1, 1)
	req := httptest.NewRequest("POST", "/events", bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	other := bytes.ReplaceAll(body, []byte(botID), []byte("87654321-1234-1234-1234-123456789abc"))
	if deliver(t, s, "other", other) != 403 {
		t.Fatal("accepted wrong bot")
	}
	partial := bytes.ReplaceAll(body, []byte("transcript.data"), []byte("transcript.partial_data"))
	deliver(t, s, "partial", partial)
	if s.status(false).TurnID != "" {
		t.Fatal("accepted partial")
	}
	deliver(t, s, "split1", transcript("hey", 1, 1))
	deliver(t, s, "split2", transcript("rysh test", 1, 2))
	if s.status(true).Request != "test" {
		t.Fatal("split wake failed")
	}
	deliver(t, s, "overlap", transcript("other speaker", 2, 3))
	if s.status(false).TurnID != "" {
		t.Fatal("accepted overlapping speakers")
	}
}
func TestMeetingPollRevocationAndDisconnectFailClosed(t *testing.T) {
	s, conn, _ := fixture(t, nil)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status_changes":[{"code":"in_call_not_recording"}]}`)
	}))
	defer provider.Close()
	s.recall = recallClient{provider.URL, "key", newHTTPClient()}
	deliver(t, s, "wake", transcript("hey rysh test", 1, 1))
	s.poll()
	if r := s.command(Command{Action: "resume"}); r.Error == "" {
		t.Fatal("resumed without permission")
	}
	if s.status(false).TurnID != "" {
		t.Fatal("permission revocation retained turn")
	}
	_ = conn.Close()
	waitFor(t, func() bool { return !s.status(false).MediaConnected })
}
func TestMeetingFailureNeverSpeaksSuccess(t *testing.T) {
	s, _, _ := fixture(t, func(context.Context, string) (string, error) { return "unverified success", errors.New("failure") })
	s.cfg.AutoSpeak = true
	deliver(t, s, "wake", transcript("hey rysh test", 1, 1))
	finalize(s)
	s.command(Command{Action: "approve", TurnID: s.status(false).TurnID})
	waitFor(t, func() bool { return s.status(false).State == "failed" })
	if s.status(true).Result != "" {
		t.Fatal("failure output leaked")
	}
}
func TestManagerJoinLeaveUsesConfiguredSecretsAndNeverRetriesCreate(t *testing.T) {
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	var creates atomic.Int32
	var leaves atomic.Int32
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "eu-central-1.recall.ai" || r.Header.Get("Authorization") != "key" {
			t.Errorf("wrong credentials/region")
		}
		body := `{}`
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/bot/":
			creates.Add(1)
			body = `{"id":"` + botID + `"}`
		case strings.HasSuffix(r.URL.Path, "/leave_call/"):
			leaves.Add(1)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	manager := NewManager()
	t.Cleanup(manager.Close)
	r := manager.Do(Command{Action: "join", URL: "https://meet.google.com/abc-defg-hij", PublicURL: "https://meeting.example", Listen: "127.0.0.1:0", Region: "eu-central-1", Consent: true}, Credentials{Recall: "key", Verification: "whsec_" + base64.StdEncoding.EncodeToString(testKey), Speech: "speech"}, func(context.Context, string) (string, error) { return "", nil })
	if r.Error != "" || r.BotID != botID {
		t.Fatal(r)
	}
	if r := manager.Do(Command{Action: "join"}, Credentials{}, nil); r.Error == "" {
		t.Fatal("duplicate join")
	}
	if r := manager.Do(Command{Action: "leave"}, Credentials{}, nil); r.State != "stopped" {
		t.Fatal(r)
	}
	if creates.Load() != 1 || leaves.Load() != 1 {
		t.Fatalf("create %d leave %d", creates.Load(), leaves.Load())
	}
}

func TestMeetingExpiryCannotResumeAfterLeaveFailure(t *testing.T) {
	s, conn, _ := fixture(t, nil)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer provider.Close()
	s.recall = recallClient{provider.URL, "key", newHTTPClient()}
	deliver(t, s, "wake", transcript("hey rysh test", 1, 1))
	s.expire()
	if r := s.command(Command{Action: "resume"}); r.Error == "" {
		t.Fatal("expired meeting resumed")
	}
	if r := s.status(true); r.State != "leave_failed" || r.TurnID != "" {
		t.Fatal(r)
	}
	_ = conn.Close()
	waitFor(t, func() bool { return !s.status(false).MediaConnected })
	if s.status(false).State != "leave_failed" {
		t.Fatal("disconnect overwrote terminal state")
	}
}

func TestMeetingSocketRequiresOriginAndCapability(t *testing.T) {
	s, _, srv := fixture(t, nil)
	endpoint := "ws" + strings.TrimPrefix(srv.URL, "http") + "/socket"
	if c, _, err := websocket.DefaultDialer.Dial(endpoint, http.Header{"Origin": []string{"https://attacker.example"}}); err == nil {
		c.Close()
		t.Fatal("cross origin accepted")
	}
	c, _, err := websocket.DefaultDialer.Dial(endpoint, http.Header{"Origin": []string{"https://meeting.example"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.WriteJSON(map[string]string{"token": "wrong"})
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := c.ReadMessage(); err == nil {
		t.Fatal("invalid capability accepted")
	}
	if !s.status(false).MediaConnected {
		t.Fatal("invalid client replaced authenticated client")
	}
}

func TestManagerCreateRejectionIsDefiniteAndAmbiguityIsNot(t *testing.T) {
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	join := Command{Action: "join", URL: "https://meet.google.com/abc-defg-hij", PublicURL: "https://meeting.example", Listen: "127.0.0.1:0", Region: "eu-central-1", Consent: true}
	keys := Credentials{Recall: "key", Verification: "whsec_" + base64.StdEncoding.EncodeToString(testKey), Speech: "speech"}
	exec := func(context.Context, string) (string, error) { return "", nil }
	var status atomic.Int32
	var creates atomic.Int32
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		creates.Add(1)
		body := `{"activate_after":["Ensure this value is greater than or equal to 1."],"output_media":["bad url https://meeting.example/media#capability"]}`
		return &http.Response{StatusCode: int(status.Load()), Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	manager := NewManager()
	t.Cleanup(manager.Close)

	status.Store(400)
	r := manager.Do(join, keys, exec)
	if r.State != "create_failed" || !strings.Contains(r.Error, "HTTP 400") || !strings.Contains(r.Error, "activate_after: Ensure this value is greater than or equal to 1.") {
		t.Fatal(r)
	}
	if strings.Contains(r.Error, "://") || strings.Contains(r.Error, "capability") {
		t.Fatal("rejection echoed a URL or capability:", r.Error)
	}
	if r := manager.Do(Command{Action: "status"}, Credentials{}, nil); r.State != "stopped" {
		t.Fatal("definite rejection must not leave an active meeting:", r)
	}

	// A 5xx may still have created the bot: stay uncertain, never retry.
	status.Store(503)
	r = manager.Do(join, keys, exec)
	if r.State != "create_uncertain" || r.Error == "" {
		t.Fatal(r)
	}
	if creates.Load() != 2 {
		t.Fatalf("create attempts %d", creates.Load())
	}
}

func TestMeetingWakeAcceptsSTTSpellingsAnywhere(t *testing.T) {
	for phrase, want := range map[string]string{
		"hey rysh read report":             "read report",
		"Hey, Rish. What is the time?":     "What is the time?",
		"Hey Rush, what is the time":       "what is the time",
		"okay so hey Reesh tell me a joke": "tell me a joke",
		"Harish, summarise the call":       "summarise the call",
		"Hi Rhys: who is here?":            "who is here?",
		"Rish, what time is it":            "what time is it",
		"hey Reese what is on today":       "what is on today",
		"Hey Ryshe, open the notes":        "open the notes",
		"Hae Rysh, what time is it":        "what time is it", // Deepgram, live run 2026-09-28
	} {
		s, _, _ := fixture(t, func(context.Context, string) (string, error) { return "", nil })
		deliver(t, s, "wake", transcript(phrase, 1, 1))
		if got := s.status(true).Request; got != want {
			t.Errorf("%q: request %q, want %q", phrase, got, want)
		}
		if s.status(false).SegmentsHeard != 1 {
			t.Errorf("%q: heard %d", phrase, s.status(false).SegmentsHeard)
		}
	}
	for _, phrase := range []string{"we rushed the release", "rich text is fine", "hey there everyone", "the rish thing", "Rest, then continue"} {
		s, _, _ := fixture(t, func(context.Context, string) (string, error) { return "", nil })
		deliver(t, s, "ambient", transcript(phrase, 1, 1))
		if s.status(false).TurnID != "" {
			t.Errorf("%q woke the bot", phrase)
		}
	}
}

func TestMeetingNewCaptureClearsStaleNotice(t *testing.T) {
	s, _, _ := fixture(t, func(context.Context, string) (string, error) { return "", nil })
	s.mu.Lock()
	s.notice = "Pending turn expired"
	s.mu.Unlock()
	deliver(t, s, "wake", transcript("hey rysh test", 1, 1))
	if r := s.status(false); r.TurnID == "" || r.Notice != "" {
		t.Fatal("stale notice survived a new capture:", r)
	}
}

func TestManagerLeaveTreatsAlreadyEndedBotAsLeft(t *testing.T) {
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		code, body := 200, `{}`
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/bot/":
			body = `{"id":"` + botID + `"}`
		case strings.HasSuffix(r.URL.Path, "/leave_call/"):
			code, body = 400, `{"code":"cannot_command_unstarted_bot","detail":"Cannot send a command to a bot which has not been started."}`
		case r.Method == "GET":
			body = `{"status_changes":[{"code":"in_call_recording"},{"code":"call_ended"},{"code":"done"}]}`
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	manager := NewManager()
	t.Cleanup(manager.Close)
	r := manager.Do(Command{Action: "join", URL: "https://meet.google.com/abc-defg-hij", PublicURL: "https://meeting.example", Listen: "127.0.0.1:0", Region: "eu-central-1", Consent: true}, Credentials{Recall: "key", Verification: "whsec_" + base64.StdEncoding.EncodeToString(testKey), Speech: "speech"}, func(context.Context, string) (string, error) { return "", nil })
	if r.Error != "" {
		t.Fatal(r)
	}
	if r := manager.Do(Command{Action: "leave"}, Credentials{}, nil); r.State != "stopped" || r.Error != "" {
		t.Fatal("leave of an ended bot must succeed:", r)
	}
}

func TestManagerLeaveStillFailsWhileBotIsLive(t *testing.T) {
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		code, body := 200, `{}`
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/bot/":
			body = `{"id":"` + botID + `"}`
		case strings.HasSuffix(r.URL.Path, "/leave_call/"):
			code, body = 400, `{"code":"some_other_refusal"}`
		case r.Method == "GET":
			body = `{"status_changes":[{"code":"in_call_recording"}]}`
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	manager := NewManager()
	t.Cleanup(manager.Close)
	manager.Do(Command{Action: "join", URL: "https://meet.google.com/abc-defg-hij", PublicURL: "https://meeting.example", Listen: "127.0.0.1:0", Region: "eu-central-1", Consent: true}, Credentials{Recall: "key", Verification: "whsec_" + base64.StdEncoding.EncodeToString(testKey), Speech: "speech"}, func(context.Context, string) (string, error) { return "", nil })
	if r := manager.Do(Command{Action: "leave"}, Credentials{}, nil); r.State != "leave_failed" {
		t.Fatal("a live bot that refused leave must stay leave_failed:", r)
	}
}
