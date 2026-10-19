// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// handsFree is the default join: no approvals, host-only unless allow is given.
func handsFree(t *testing.T, exec Execute, allow ...string) (*Service, *websocket.Conn) {
	t.Helper()
	s, conn, _ := fixture(t, exec)
	s.mu.Lock()
	s.cfg.Approve = false
	s.cfg.Allow = allow
	s.mu.Unlock()
	return s, conn
}

func said(text string, speaker int, name string, host bool, start float64) []byte {
	b, _ := json.Marshal(map[string]any{"event": "transcript.data", "data": map[string]any{"bot": map[string]any{"id": botID}, "data": map[string]any{"participant": map[string]any{"id": speaker, "name": name, "is_host": host}, "words": []any{map[string]any{"text": text, "start_timestamp": map[string]any{"relative": start}, "end_timestamp": map[string]any{"relative": start + 1}}}}}})
	return b
}

// settle runs the capture-quiet step the 250 ms ticker would run.
func settle(s *Service) {
	s.mu.Lock()
	s.settleLocked(time.Now().Add(2 * time.Second))
	s.mu.Unlock()
}

// readAudio reads the page-side frames up to the MP3 for one turn.
func readAudio(t *testing.T, conn *websocket.Conn) (string, string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal("no audio:", err)
		}
		if kind == websocket.BinaryMessage {
			t.Fatal("audio without control frame")
		}
		var m map[string]string
		_ = json.Unmarshal(data, &m)
		if m["type"] != "audio" {
			continue
		}
		kind, audio, err := conn.ReadMessage()
		if err != nil || kind != websocket.BinaryMessage {
			t.Fatal("missing audio body", err)
		}
		return m["turn"], string(audio)
	}
}

func TestMeetingHandsFreeTurnRunsAndSpeaksWithoutTerminalInput(t *testing.T) {
	var calls atomic.Int32
	s, conn := handsFree(t, func(_ context.Context, text string) (string, error) {
		calls.Add(1)
		if text != "what is the capital of France" {
			t.Errorf("executor got %q", text)
		}
		return "Paris.", nil
	})
	var spoken atomic.Value
	s.mu.Lock()
	s.speech = func(_ context.Context, text string) ([]byte, error) { spoken.Store(text); return []byte("MP3"), nil }
	s.mu.Unlock()
	if r := s.status(false); r.Mode != "hands_free" || r.Phase != "listening" || len(r.Allow) != 1 || r.Allow[0] != "host" {
		t.Fatal(r)
	}
	deliver(t, s, "wake", said("hey rysh what is the capital of France", 1, "Halil Ağın", true, 1))
	if r := s.status(false); r.State != "capturing" || r.Phase != "heard" || calls.Load() != 0 {
		t.Fatal("capture must wait for the speaker to finish:", r)
	}
	settle(s)
	turn, audio := readAudio(t, conn)
	if audio != "MP3" || spoken.Load() != "Paris." || calls.Load() != 1 {
		t.Fatalf("audio %q spoken %v calls %d", audio, spoken.Load(), calls.Load())
	}
	waitFor(t, func() bool { return s.status(false).Phase == "speaking" })
	if r := s.command(Command{Action: "approve", TurnID: turn}); r.Error == "" {
		t.Fatal("approve accepted in hands-free mode")
	}
	// Echo of its own reply, and a second wake while busy, are dropped: no queue.
	deliver(t, s, "busy", said("hey rysh what is the capital of Spain", 1, "Halil Ağın", true, 5))
	_ = conn.WriteJSON(map[string]string{"type": "played", "turn": turn})
	waitFor(t, func() bool { return s.status(false).State == "listening" })
	if calls.Load() != 1 || s.status(false).TurnID != "" {
		t.Fatal("busy wake was queued or turn retained")
	}
}

func TestMeetingHandsFreeAllowlist(t *testing.T) {
	ok := func(context.Context, string) (string, error) { return "fine", nil }
	s, _ := handsFree(t, ok)
	deliver(t, s, "guest", said("hey rysh delete everything", 2, "Guest", false, 1))
	r := s.status(false)
	if r.TurnID != "" || r.IgnoredWakes != 1 || !strings.Contains(r.Notice, `"Guest"`) {
		t.Fatal("default must be host only:", r)
	}
	deliver(t, s, "host", said("hey rysh summarise", 1, "Host Person", true, 3))
	if s.status(false).TurnID == "" {
		t.Fatal("host could not wake the bot")
	}

	s, _ = handsFree(t, ok, "HOST", "halil  ağın")
	if a := s.status(false).Allow; len(a) != 2 || a[0] != "host" || a[1] != "halil ağın" {
		t.Fatal("status must report the allowlist in effect:", a)
	}
	deliver(t, s, "named", said("hey rysh summarise", 3, "Halil Ağın", false, 1))
	if s.status(false).TurnID == "" {
		t.Fatal("allowed display name (case/space-insensitive) ignored")
	}
	s, _ = handsFree(t, ok, "Ann")
	deliver(t, s, "host-not-listed", said("hey rysh summarise", 1, "Host Person", true, 1))
	if s.status(false).TurnID != "" {
		t.Fatal("explicit list without host still admitted the host")
	}
	s, _ = handsFree(t, ok, "anyone")
	deliver(t, s, "anyone", said("hey rysh summarise", 4, "Visitor", false, 1))
	if s.status(false).TurnID == "" {
		t.Fatal("anyone did not admit a visitor")
	}
	s, _ = handsFree(t, ok, "anyone")
	deliver(t, s, "self", said("hey rysh summarise", 9, botName+" — say hey rysh", false, 1))
	if s.status(false).TurnID != "" {
		t.Fatal("the bot's own voice woke it")
	}
}

func TestMeetingSpokenStopFromAnyoneCancelsPlaybackAndExecution(t *testing.T) {
	started := make(chan struct{}, 1)
	s, conn := handsFree(t, func(ctx context.Context, text string) (string, error) {
		if text == "slow" {
			started <- struct{}{}
			<-ctx.Done()
			return "late result", nil
		}
		return "A long answer.", nil
	})
	// Stop while speaking, from a participant who may not wake the bot.
	deliver(t, s, "wake", said("hey rysh fast", 1, "Host", true, 1))
	settle(s)
	readAudio(t, conn)
	waitFor(t, func() bool { return s.status(false).State == "speaking" })
	deliver(t, s, "stop", said("Rysh, stop.", 2, "Guest", false, 4))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var m map[string]string
	if err := conn.ReadJSON(&m); err != nil || m["type"] != "clear" {
		t.Fatal("page not told to stop playback", m, err)
	}
	if r := s.status(false); r.State != "listening" || r.TurnID != "" || r.Notice != "Stopped by a participant" {
		t.Fatal(r)
	}

	// Stop while the executor runs: its context is cancelled, the late result dropped.
	s.mu.Lock()
	s.cooldown = time.Time{}
	s.mu.Unlock()
	deliver(t, s, "wake2", said("hey rysh slow", 1, "Host", true, 10))
	settle(s)
	<-started
	deliver(t, s, "stop2", said("hey rysh stop talking", 3, "Other", false, 12))
	waitFor(t, func() bool { return s.status(false).State == "listening" })
	time.Sleep(20 * time.Millisecond)
	if r := s.status(true); r.TurnID != "" || r.Result != "" {
		t.Fatal("stopped turn resurfaced:", r)
	}

	// "hey rysh stop" with nothing running is not a request.
	s.mu.Lock()
	s.cooldown = time.Time{}
	s.mu.Unlock()
	deliver(t, s, "idle-stop", said("hey rysh stop", 1, "Host", true, 20))
	if s.status(false).TurnID != "" {
		t.Fatal("stop phrase became a turn")
	}
}

func TestMeetingHandsFreeFailureIsSpokenBriefly(t *testing.T) {
	s, conn := handsFree(t, func(context.Context, string) (string, error) { return "unverified success", errors.New("boom") })
	var spoken atomic.Value
	s.mu.Lock()
	s.speech = func(_ context.Context, text string) ([]byte, error) { spoken.Store(text); return []byte("MP3"), nil }
	s.mu.Unlock()
	deliver(t, s, "wake", said("hey rysh do it", 1, "Host", true, 1))
	settle(s)
	turn, _ := readAudio(t, conn)
	if spoken.Load() != failurePhrase {
		t.Fatalf("spoke %q, want the failure phrase", spoken.Load())
	}
	waitFor(t, func() bool { return s.status(false).State == "speaking" })
	if r := s.status(true); r.Phase != "error" || strings.Contains(r.Result, "unverified") {
		t.Fatal("failure must read as error and never leak the executor output:", r)
	}
	_ = conn.WriteJSON(map[string]string{"type": "played", "turn": turn})
	waitFor(t, func() bool { return s.status(false).State == "listening" })
	if !strings.Contains(s.status(false).Notice, "Executor failed") {
		t.Fatal("failure reason not kept for the operator")
	}

	// If speech itself fails, even for the failure phrase, return to listening.
	s, _ = handsFree(t, func(context.Context, string) (string, error) { return "ok", nil })
	s.mu.Lock()
	s.speech = func(context.Context, string) ([]byte, error) { return nil, errors.New("tts down") }
	s.mu.Unlock()
	deliver(t, s, "wake", said("hey rysh do it", 1, "Host", true, 1))
	settle(s)
	waitFor(t, func() bool { return s.status(false).State == "listening" })
	if n := s.status(false).Notice; !strings.Contains(n, "Speech synthesis failed") {
		t.Fatal(n)
	}
}

func TestMeetingHandsFreeRecoversFromMediaReconnectButNotOperatorPause(t *testing.T) {
	s, conn, srv := fixture(t, nil)
	s.mu.Lock()
	s.cfg.Approve = false
	s.mu.Unlock()
	_ = conn.Close()
	waitFor(t, func() bool { return s.status(false).State == "paused" })
	c2, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/socket", http.Header{"Origin": []string{"https://meeting.example"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	_ = c2.WriteJSON(map[string]string{"token": s.token})
	waitFor(t, func() bool { return s.status(false).MediaConnected })
	s.mu.Lock()
	s.recoverLocked()
	s.mu.Unlock()
	if r := s.status(false); r.State != "listening" {
		t.Fatal("hands-free did not resume after page reconnect:", r)
	}
	s.command(Command{Action: "pause"})
	s.mu.Lock()
	s.recoverLocked()
	s.mu.Unlock()
	if s.status(false).State != "paused" {
		t.Fatal("operator pause must stay until resume")
	}
	s.command(Command{Action: "resume"})
	s.command(Command{Action: "cancel"})
	if s.status(false).State != "listening" {
		t.Fatal("cancel must keep listening")
	}
}

func TestMeetingStopPhraseAndPhaseVocabulary(t *testing.T) {
	for phrase, want := range map[string]bool{
		"rysh stop": true, "Hey Rish, stop.": true, "stop, rysh": true, "Rush cancel": true, "harish stop": true,
		"we should stop": false, "hey rysh what should we stop doing next week": false, "stop the rich people": true,
	} {
		if got := stopRequested(strings.Fields(phrase)); got != want {
			t.Errorf("%q: %v", phrase, got)
		}
	}
	for state, want := range map[string]string{"joining": "joining", "listening": "listening", "capturing": "heard", "executing": "thinking", "synthesizing": "preparing_speech", "speaking": "speaking", "failed": "error", "paused": "paused", "ended": "ended", "awaiting_approval": "awaiting_approval"} {
		if got := Phase(state, false); got != want {
			t.Errorf("%s -> %s, want %s", state, got, want)
		}
	}
	if Phase("speaking", true) != "error" {
		t.Error("failure phrase playback must read as error")
	}
}

// Design 048: with --lang the agent is told the answer language; the
// transcript it was woken by is otherwise unchanged.
func TestMeetingHandsFreeTurnAnswersInTheMeetingLanguage(t *testing.T) {
	got := make(chan string, 1)
	s, conn := handsFree(t, func(_ context.Context, text string) (string, error) { got <- text; return "Parîs.", nil })
	s.mu.Lock()
	s.cfg.Lang = "kmr"
	s.speech = func(context.Context, string) ([]byte, error) { return []byte("MP3"), nil }
	s.mu.Unlock()
	deliver(t, s, "wake", said("hey rysh what is the capital of France", 1, "Halil Ağın", true, 1))
	settle(s)
	readAudio(t, conn)
	if text := <-got; text != "(Answer in Kurmanji Kurdish, written in the Latin alphabet.) what is the capital of France" {
		t.Fatalf("executor got %q", text)
	}
}
