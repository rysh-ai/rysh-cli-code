// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

// The progress wall (MTG-13) is the bot's camera tile: media.html renders
// what the service is doing, pushed over the capability socket as
//
//	{"type":"status","phase":"thinking","state":"executing","speaker":"Ada",
//	 "request":"what changed today","activity":"running agent docs",
//	 "caption":"","elapsed_ms":1234}
//
// It only ever carries the captured request and the reply, never ambient
// speech, notices (operator text) or secrets. "phase" is the wall's stable
// vocabulary (Phase, shared with `meeting status` and the rysh-server company
// page, design 040); "state" is the raw service state; "agent" is the
// attached agent answering the turn (MTG-12), empty for rysh itself. Older pages ignore the
// message: "audio" and "clear" are unchanged.

// Wall phases: the names Phase returns. Keep in sync with LABELS in media.html.
const (
	WallJoining   = "joining"
	WallListening = "listening"
	WallHeard     = "heard"
	WallApproval  = "awaiting_approval" // operator gate (approve mode only)
	WallThinking  = "thinking"
	WallPreparing = "preparing_speech"
	WallSpeaking  = "speaking"
	WallError     = "error"
	WallPaused    = "paused"
	WallEnded     = "ended"
)

const (
	wallRequestRunes  = 160
	wallActivityRunes = 80
	wallCaptionRunes  = 6000
)

type wallStatus struct {
	Type      string `json:"type"`
	Phase     string `json:"phase"`
	State     string `json:"state"`
	Speaker   string `json:"speaker,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Request   string `json:"request,omitempty"`
	Activity  string `json:"activity,omitempty"`
	Caption   string `json:"caption,omitempty"`
	ElapsedMS *int64 `json:"elapsed_ms,omitempty"` // set during a turn, 0 included
}

// wallTracker remembers what the page last saw. Guarded by Service.mu.
type wallTracker struct {
	conn         *websocket.Conn
	last         wallStatus
	turnID       string
	turnStart    time.Time
	activity     string
	activityTurn string
	dataLast     wallStatus // what the room last got on the data channel
	dataSent     time.Time
}

// wallSink is a transport that carries the wall itself (a LiveKit room's
// data channel) rather than through the media page's socket.
type wallSink interface {
	PublishWallLocked(payload []byte) bool
}

// wallResend repeats an unchanged wall on the data channel, so an app that
// joined the room after the last change still shows the bot's state.
const wallResend = 3 * time.Second

// wallSnapshotLocked builds the message for the current service state.
// ElapsedMS is left nil; it is stamped at send time so it does not defeat
// change detection.
func (s *Service) wallSnapshotLocked(now time.Time) wallStatus {
	p := s.pending
	// Phase is shared with `meeting status`, so the tile and the CLI agree.
	w := wallStatus{Type: "status", Phase: Phase(s.state, p != nil && p.failed), State: s.state}
	if p == nil {
		s.wall.turnID = ""
		if len(s.agents) == 1 {
			// An agent bot (D9) is woken by its own name: the idle wall
			// says "hey <agent>", since with several bots "hey rysh" is
			// nobody's.
			w.Agent = s.agents[0].Name
		}
		return w
	}
	if s.wall.turnID != p.id {
		s.wall.turnID = p.id
		s.wall.turnStart = now
	}
	w.Speaker = clip(p.speaker, 80)
	w.Agent = p.agent
	w.Request = clip(p.text, wallRequestRunes)
	if s.wall.activityTurn == p.id && w.Phase == WallThinking {
		w.Activity = s.wall.activity
	}
	if w.Phase == WallSpeaking {
		w.Caption = clip(p.result, wallCaptionRunes)
	}
	return w
}

// pushWallLocked sends the status when it changed or the page reconnected.
func (s *Service) pushWallLocked(now time.Time) {
	if ws, ok := s.transport.(wallSink); ok {
		if s.report == nil {
			// The silent reporter (design 045) has no wall to show: it
			// is never addressed, so "say hey …" would mislead.
			s.pushWallDataLocked(ws, now)
		}
		return
	}
	if s.conn == nil {
		s.wall.conn = nil
		return
	}
	w := s.wallSnapshotLocked(now)
	if s.wall.conn == s.conn && w == s.wall.last {
		return
	}
	out := w
	if s.wall.turnID != "" {
		ms := now.Sub(s.wall.turnStart).Milliseconds()
		out.ElapsedMS = &ms
	}
	_ = s.conn.SetWriteDeadline(now.Add(2 * time.Second))
	if s.conn.WriteJSON(out) != nil {
		// The socket reader notices the close and pauses the service.
		_ = s.conn.Close()
		return
	}
	s.wall.conn = s.conn
	s.wall.last = w
}

// wallLoop keeps the page in step with the service until it closes.
func (s *Service) wallLoop() {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-tick.C:
			s.mu.Lock()
			s.pushWallLocked(now)
			s.mu.Unlock()
		}
	}
}

type activityKey struct{}

// ReportActivity lets the executor describe what it is doing for the
// current turn ("running agent docs", "reading README.md"). The label is
// shown on the meeting's camera tile, so it must never contain secrets.
// It is a no-op outside a meeting turn.
func ReportActivity(ctx context.Context, label string) {
	if f, ok := ctx.Value(activityKey{}).(func(string)); ok {
		f(label)
	}
}

// WithActivityReporter binds ReportActivity to f, for executors tested
// outside a Service.
func WithActivityReporter(ctx context.Context, f func(string)) context.Context {
	return context.WithValue(ctx, activityKey{}, f)
}

// withActivity binds ReportActivity for one turn; reports from a stale turn
// are dropped.
func (s *Service) withActivity(ctx context.Context, turnID string, gen uint64) context.Context {
	return context.WithValue(ctx, activityKey{}, func(label string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if gen != s.generation || s.pending == nil || s.pending.id != turnID {
			return
		}
		s.wall.activity = clip(label, wallActivityRunes)
		s.wall.activityTurn = turnID
	})
}

// clip removes control characters and bounds v to n runes.
func clip(v string, n int) string {
	v = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, v))
	if utf8.RuneCountInString(v) <= n {
		return v
	}
	r := []rune(v)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// pushWallDataLocked sends the status on the room's data channel when it
// changed, and again every wallResend for late joiners.
func (s *Service) pushWallDataLocked(ws wallSink, now time.Time) {
	w := s.wallSnapshotLocked(now)
	if w == s.wall.dataLast && now.Sub(s.wall.dataSent) < wallResend {
		return
	}
	out := w
	if s.wall.turnID != "" {
		ms := now.Sub(s.wall.turnStart).Milliseconds()
		out.ElapsedMS = &ms
	}
	payload, err := json.Marshal(out)
	if err != nil || !ws.PublishWallLocked(payload) {
		return
	}
	s.wall.dataLast, s.wall.dataSent = w, now
}
