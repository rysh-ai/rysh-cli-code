// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// readWall returns the next text message from the page socket.
func readWall(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if kind != websocket.TextMessage {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
}
func push(s *Service) {
	s.mu.Lock()
	s.pushWallLocked(time.Now())
	s.mu.Unlock()
}
func expectNoMessage(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, data, err := conn.ReadMessage(); err == nil {
		t.Fatalf("unexpected message %s", data)
	}
}

func TestWallPushesOnlyChangesAndFollowsTheTurn(t *testing.T) {
	release := make(chan struct{})
	s, conn, _ := fixture(t, func(ctx context.Context, _ string) (string, error) {
		ReportActivity(ctx, "running agent docs\n")
		<-release
		return "Two commits landed today.", nil
	})
	push(s)
	if m := readWall(t, conn); m["type"] != "status" || m["phase"] != WallListening || m["request"] != nil {
		t.Fatal(m)
	}
	deliver(t, s, "ambient", transcript("salary numbers are private", 2, 1))
	deliver(t, s, "wake", transcript("hey rysh what changed today", 1, 2))
	push(s)
	m := readWall(t, conn)
	if m["phase"] != WallHeard || m["request"] != "what changed today" || m["speaker"] != "Tester" {
		t.Fatal(m)
	}
	finalize(s)
	id := s.status(false).TurnID
	s.command(Command{Action: "approve", TurnID: id})
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.wall.activity != ""
	})
	push(s)
	m = readWall(t, conn)
	for m["phase"] != WallThinking {
		m = readWall(t, conn)
	}
	if m["activity"] != "running agent docs" || m["state"] != "executing" || m["elapsed_ms"] == nil {
		t.Fatal(m)
	}
	close(release)
	waitFor(t, func() bool { return s.status(false).State == "awaiting_speech" })
	s.command(Command{Action: "speak", TurnID: id})
	waitFor(t, func() bool { return s.status(false).State == "speaking" })
	push(s)
	for m["phase"] != WallSpeaking {
		m = readWall(t, conn)
	}
	if m["caption"] != "Two commits landed today." || m["activity"] != nil {
		t.Fatal(m)
	}
	_ = conn.WriteJSON(map[string]string{"type": "played", "turn": id})
	waitFor(t, func() bool { return s.status(false).State == "listening" })
	push(s)
	for m["phase"] != WallListening {
		m = readWall(t, conn)
	}
	if m["request"] != nil || m["caption"] != nil || m["speaker"] != nil {
		t.Fatal("completed turn left on the wall", m)
	}
}

func TestWallNeverShowsAmbientSpeechOrNotices(t *testing.T) {
	s, conn, _ := fixture(t, func(context.Context, string) (string, error) { return "ok", nil })
	deliver(t, s, "ambient", transcript("the api key is sk-live-123", 1, 1))
	s.mu.Lock()
	s.notice = "operator-only notice"
	s.mu.Unlock()
	push(s)
	raw, _ := json.Marshal(readWall(t, conn))
	if strings.Contains(string(raw), "sk-live") || strings.Contains(string(raw), "notice") {
		t.Fatalf("wall leaked %s", raw)
	}
	push(s)
	expectNoMessage(t, conn)
}

func TestWallRepublishesToAReconnectedPage(t *testing.T) {
	s, conn, _ := fixture(t, func(context.Context, string) (string, error) { return "ok", nil })
	push(s)
	readWall(t, conn)
	s.mu.Lock()
	s.wall.conn = nil // as if a new page connected
	s.mu.Unlock()
	push(s)
	if m := readWall(t, conn); m["phase"] != WallListening {
		t.Fatal(m)
	}
}

func TestWallLoopPushesWithoutCommands(t *testing.T) {
	s, conn, _ := fixture(t, func(context.Context, string) (string, error) { return "ok", nil })
	go s.wallLoop()
	if m := readWall(t, conn); m["phase"] != WallListening {
		t.Fatal(m)
	}
	s.pause()
	m := readWall(t, conn)
	for m["type"] != "status" {
		m = readWall(t, conn) // the "clear" control from pause
	}
	if m["phase"] != WallPaused {
		t.Fatal(m)
	}
}

func TestReportActivityDropsStaleAndForeignTurns(t *testing.T) {
	ReportActivity(context.Background(), "no meeting") // no-op, no panic
	s, _, _ := fixture(t, func(context.Context, string) (string, error) { return "ok", nil })
	deliver(t, s, "wake", transcript("hey rysh go", 1, 1))
	s.mu.Lock()
	ctx := s.withActivity(context.Background(), s.pending.id, s.generation)
	s.clearLocked()
	s.mu.Unlock()
	ReportActivity(ctx, "late")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wall.activity != "" {
		t.Fatal("stale activity recorded")
	}
}

func TestClip(t *testing.T) {
	if got := clip("a\x1bb "+strings.Repeat("x", 300), 10); got != "a b xxxxx…" {
		t.Errorf("clip = %q", got)
	}
}

func TestWallNamesTheAgentAndSharesStatusPhase(t *testing.T) {
	s, _, _ := fixture(t, func(context.Context, string) (string, error) { return "ok", nil })
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = &turn{id: "t1", agent: "scribe", speaker: "Ada", text: "notes", result: "Sorry, that failed.", failed: true}
	s.state = "speaking"
	w := s.wallSnapshotLocked(time.Now())
	if w.Phase != WallError || w.Agent != "scribe" || w.Phase != s.statusLocked(false).Phase {
		t.Fatalf("%+v", w)
	}
	if w.Caption != "" {
		t.Fatal("caption shown outside the speaking phase")
	}
}

// An agent bot's idle wall names its agent, so the tile says "hey <agent>"
// (D9: with two bots "hey rysh" is nobody's); rysh itself and a bot with
// several agents stay unnamed.
func TestWallIdleNamesTheBotsOnlyAgent(t *testing.T) {
	s, _, _ := fixture(t, func(context.Context, string) (string, error) { return "ok", nil })
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = "listening"
	if w := s.wallSnapshotLocked(time.Now()); w.Agent != "" {
		t.Fatalf("rysh bot %+v", w)
	}
	s.agents = []meetingAgent{{Agent: Agent{Name: "scribe"}}}
	if w := s.wallSnapshotLocked(time.Now()); w.Phase != WallListening || w.Agent != "scribe" {
		t.Fatalf("agent bot %+v", w)
	}
	s.agents = append(s.agents, meetingAgent{Agent: Agent{Name: "coder"}})
	if w := s.wallSnapshotLocked(time.Now()); w.Agent != "" {
		t.Fatalf("two agents %+v", w)
	}
}
