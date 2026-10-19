// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E-82: a `##meeting add agent` / `remove agent` / `leave` refused because the
// meeting's bot is starting or leaving must say so in the pane that typed it.
// Live run 1 (2026-09-30) showed "starting a bot …" and then nothing.

// gatedRoom is a LiveKit room whose Close waits for a gate, so a bot leave
// stays "leaving" for as long as the test needs.
type gatedRoom struct {
	events chan meeting.LiveKitEvent
	gate   <-chan struct{}
	once   sync.Once
}

func (r *gatedRoom) Events() <-chan meeting.LiveKitEvent { return r.events }
func (r *gatedRoom) Play(context.Context, []byte) error  { return nil }
func (r *gatedRoom) Close() error {
	<-r.gate
	r.once.Do(func() { close(r.events) })
	return nil
}

// busyLine is the refusal for a meeting whose bot is starting/leaving `who`.
func busyLine(id, what string) string {
	when := "when it is ready"
	if strings.HasPrefix(what, "leaving") {
		when = "when it has left"
	}
	return "meeting " + id + " is busy (" + what + ") — try again " + when + " (##meeting status " + id + ")"
}

func TestMeetingBusyAgentRefusalShowsInTheInvokingPane(t *testing.T) {
	h := newMeetingAgentsRig(t, nil)
	id := h.register(t, meeting.Command{PaneID: "mp", Room: agentsTestRoom, Server: h.server})

	// The first add's bot blocks in the LiveKit connect: the meeting is busy
	// "starting scribe" until release.
	entered, gate := make(chan struct{}, 1), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	meeting.DialLiveKit = func(context.Context, string, string, meeting.LiveKitDialOptions) (meeting.LiveKitRoom, error) {
		entered <- struct{}{}
		<-gate
		return &agentsRoom{events: make(chan meeting.LiveKitEvent)}, nil
	}
	var first strings.Builder
	h.w.meetingAgentCommand(&first, "p1", []string{"add", "agent", id, "scribe"})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first bot start never reached LiveKit")
	}

	want := busyLine(id, "starting scribe")
	for _, args := range [][]string{
		{"add", "agent", id, "planner"},
		{"remove", "agent", id, "scribe"},
	} {
		var out strings.Builder
		h.w.ryshFail = nil
		h.w.meetingAgentCommand(&out, "p1", args)
		if !strings.Contains(out.String(), want) {
			t.Errorf("%v: the invoking pane's output is %q, want the refusal %q", args, out.String(), want)
		}
		if strings.Contains(out.String(), "starting a bot") || strings.Contains(out.String(), "removing agent") {
			t.Errorf("%v: announced work it was refused: %q", args, out.String())
		}
		if h.w.ryshFail == nil {
			t.Errorf("%v: a refusal must fail the command (CLI exit status)", args)
		}
	}
	select {
	case n := <-h.notices:
		t.Fatalf("a refused command started work that reported later: %q", n)
	default:
	}

	release()
	select {
	case n := <-h.notices:
		if !strings.Contains(n, "agents scribe") {
			t.Fatalf("first add: %q", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first add never finished")
	}

	// Leaving: removing scribe blocks in the room's Close, so a second remove
	// meets "leaving scribe".
	leaveGate := make(chan struct{})
	releaseLeave := sync.OnceFunc(func() { close(leaveGate) })
	t.Cleanup(releaseLeave)
	meeting.DialLiveKit = func(context.Context, string, string, meeting.LiveKitDialOptions) (meeting.LiveKitRoom, error) {
		return &gatedRoom{events: make(chan meeting.LiveKitEvent), gate: leaveGate}, nil
	}
	if got := h.run(t, "add", "agent", id, "planner"); !strings.Contains(got, "agents scribe, planner") {
		t.Fatalf("add planner: %q", got)
	}
	var rm strings.Builder
	h.w.meetingAgentCommand(&rm, "p1", []string{"remove", "agent", id, "planner"})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if mt, _ := h.w.meeting.Lookup(id); mt.BusyError() != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the remove never started leaving")
		}
	}
	var out strings.Builder
	h.w.ryshFail = nil
	h.w.meetingAgentCommand(&out, "p1", []string{"remove", "agent", id, "planner"})
	if want := busyLine(id, "leaving planner"); !strings.Contains(out.String(), want) || h.w.ryshFail == nil {
		t.Errorf("remove while leaving: output %q fail %v, want %q", out.String(), h.w.ryshFail, want)
	}
	releaseLeave()
	select {
	case n := <-h.notices:
		if !strings.Contains(n, "agents scribe") {
			t.Fatalf("remove planner: %q", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("remove planner never finished")
	}
}

// A pane-less meeting (a CLI `host`) refuses a leave while its bot is leaving.
// A pane meeting is not refused: `leave` closes its pane.
func TestMeetingBusyLeaveRefusalShowsInTheInvokingPane(t *testing.T) {
	h := newMeetingAgentsRig(t, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /api/meeting-rooms":
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{"room": map[string]any{"id": "r-1", "owner_id": agentsTestOwner, "room_name": "rysh-r-1", "livekit_url": "wss://lk.example"}})
		case "POST /api/meeting-rooms/r-1/agent-token":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "agent-jwt", "url": "wss://lk.example", "room_name": "rysh-r-1", "identity": "agent-scribe"})
		case "POST /api/meeting-rooms/r-1/invites":
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]any{"invite": map[string]any{"id": "i1"}, "token": "rysh_mi_x", "join_url": "https://rysh.example/meet/r-1#invite=rysh_mi_x"})
		case "DELETE /api/meeting-rooms/r-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"room": map[string]any{"id": "r-1"}})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	meeting.DialLiveKit = func(context.Context, string, string, meeting.LiveKitDialOptions) (meeting.LiveKitRoom, error) {
		return &gatedRoom{events: make(chan meeting.LiveKitEvent), gate: gate}, nil
	}
	noop := func(context.Context, string) (string, error) { return "", nil }
	r := h.w.meeting.Do(meeting.Command{Action: "host", Server: srv.URL, Room: "standup", Minutes: 30, TTS: "deepgram", STT: "deepgram"},
		meeting.Credentials{Speech: "dg", RyshAPIKey: "rysh_testkey", LiveKit: meeting.LiveKitCredentials{STTKey: "dg"}}, noop,
		meeting.Agent{Name: "scribe", Execute: noop})
	if r.Error != "" || r.ID == "" {
		t.Fatalf("host: %+v", r)
	}
	id := r.ID
	left := make(chan meeting.Reply, 1)
	go func() { left <- h.w.meeting.Do(meeting.Command{Action: "leave", ID: id}, meeting.Credentials{}, nil) }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if mt, _ := h.w.meeting.Lookup(id); mt.BusyError() != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first leave never started")
		}
	}

	var out strings.Builder
	h.w.ryshFail = nil
	h.w.meetingLeave(&out, "p1", id)
	if want := busyLine(id, "leaving scribe"); !strings.Contains(out.String(), want) || h.w.ryshFail == nil {
		t.Errorf("leave while leaving: output %q fail %v, want %q", out.String(), h.w.ryshFail, want)
	}
	release()
	if r := <-left; r.Error != "" {
		t.Fatalf("first leave: %+v", r)
	}
}

// A meeting result that arrives after its command returned goes to the pane's
// merged output as well as its rysh buffer, as the `##` command's own output
// does: a pane in shell/prompt/chat mode shows only the former.
func TestMeetingNoticeReachesTheVisibleBuffer(t *testing.T) {
	nc := startInProcessNATS(t)
	w := &WorkspaceActor{pub: msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())}
	merged := recordSubject(t, nc, msg.T("pane", "p1", "output"))
	rysh := recordSubject(t, nc, msg.T("pane", "p1", "output", "rysh"))

	meetingPaneNotice(w, "p1", "\n[meeting] m-4c8abf: refused\n") // the seam's default
	settle(t, nc)

	for name, rec := range map[string]*subjectRecorder{"merged output (shell/prompt/chat modes)": merged, "rysh output": rysh} {
		_, payloads := rec.seen()
		if len(payloads) != 1 || !strings.Contains(string(payloads[0]), "m-4c8abf: refused") {
			t.Errorf("%s got %d message(s) %s", name, len(payloads), payloads)
		}
	}
}
