// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// E30 PM-3c: the registry lock is never held across provider I/O. The
// workspace mailbox calls Lookup/ByPane/list/register; a bot start blocked
// in a provider must not stall them.

// within fails the test if fn does not return in time.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s blocked behind a bot start", what)
	}
}

// blockLiveKitDial makes the LiveKit bot's connect wait until release.
func blockLiveKitDial(h *joinRig) (entered chan struct{}, release func()) {
	entered, gate := make(chan struct{}, 4), make(chan struct{})
	DialLiveKit = func(ctx context.Context, serverURL, token string, opts LiveKitDialOptions) (LiveKitRoom, error) {
		entered <- struct{}{}
		<-gate
		return h.room, nil
	}
	return entered, func() { close(gate) }
}

func TestRegistryReadsDoNotWaitForABotStart(t *testing.T) {
	h := newJoinRig(t)
	entered, release := blockLiveKitDial(h)
	id := register(t, h.m, Command{PaneID: "lk", Room: existingRoom, Server: h.base}).ID
	other := register(t, h.m, Command{PaneID: "web", URL: meetA}).ID

	started := make(chan Reply, 1)
	go func() { started <- addAgent(h.m, id, "scribe", Command{}, h.creds) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("bot start never reached the provider")
	}

	within(t, "Lookup", func() {
		if mt, ok := h.m.Lookup(other); !ok || mt.PaneID != "web" {
			t.Errorf("Lookup %+v %v", mt, ok)
		}
	})
	within(t, "ByPane", func() {
		if mt, ok := h.m.ByPane("lk"); !ok || mt.ID != id || mt.hasBot() {
			t.Errorf("ByPane %+v %v", mt, ok)
		}
	})
	within(t, "list", func() {
		r := h.m.Do(Command{Action: "list"}, Credentials{}, nil)
		if len(r.Meetings) != 2 || r.Meetings[0].State != "starting" || r.Meetings[1].State != "idle" {
			t.Errorf("list %+v", r.Meetings)
		}
	})
	within(t, "register", func() { register(t, h.m, Command{PaneID: "third", URL: meetB}) })
	within(t, "status of the starting meeting", func() {
		if r := h.m.Do(Command{Action: "status", ID: id}, Credentials{}, nil); r.State != "starting" {
			t.Errorf("status %+v", r)
		}
	})
	within(t, "unregister of another meeting", func() {
		if r := h.m.Do(Command{Action: "unregister", ID: other}, Credentials{}, nil); r.Error != "" {
			t.Errorf("unregister %+v", r)
		}
	})
	// A second start or leave on the same meeting is refused, not queued.
	within(t, "second add", func() {
		// E-82: the refusal names the meeting, what runs and where to watch it.
		if r := addAgent(h.m, id, "coder", Command{}, h.creds); r.Error != "meeting "+id+" is busy (starting scribe) — try again when it is ready (##meeting status "+id+")" {
			t.Errorf("second add %+v", r)
		}
	})
	within(t, "leave while starting", func() {
		if r := h.m.Do(Command{Action: "leave", ID: id}, Credentials{}, nil); !strings.Contains(r.Error, "is busy (starting scribe)") {
			t.Errorf("leave %+v", r)
		}
	})

	release()
	r := <-started
	if r.Error != "" || r.ID != id || strings.Join(r.Agents, ",") != "scribe" {
		t.Fatalf("start %+v", r)
	}
	if mt, _ := h.m.Lookup(id); !mt.hasBot() {
		t.Fatal("bot not committed")
	}
	if r := addAgent(h.m, id, "coder", Command{}, h.creds); r.Error != "" {
		t.Fatalf("attach after start %+v", r)
	}
}

func TestUnregisterDuringStartMakesTheNewBotLeave(t *testing.T) {
	h := newJoinRig(t)
	entered, release := blockLiveKitDial(h)
	id := register(t, h.m, Command{PaneID: "lk", Room: existingRoom, Server: h.base}).ID
	started := make(chan Reply, 1)
	go func() { started <- addAgent(h.m, id, "scribe", Command{}, h.creds) }()
	<-entered
	within(t, "unregister (pane closed)", func() {
		r := h.m.Do(Command{Action: "unregister", ID: id}, Credentials{}, nil)
		if r.Error != "" || r.State != "stopped" || !strings.Contains(r.Notice, "still starting") {
			t.Errorf("unregister %+v", r)
		}
	})
	if _, ok := h.m.Lookup(id); ok {
		t.Fatal("unregistered meeting still listed")
	}
	release()
	r := <-started
	if r.State != "stopped" || !strings.Contains(r.Notice, "closed while its bot was starting") {
		t.Fatalf("start after unregister %+v", r)
	}
	h.room.mu.Lock()
	closed := h.room.closed
	h.room.mu.Unlock()
	if !closed || h.m.active() != nil {
		t.Fatal("the orphaned bot is still connected")
	}
}

func TestCloseDuringStartMakesTheNewBotLeave(t *testing.T) {
	h := newJoinRig(t)
	entered, release := blockLiveKitDial(h)
	id := register(t, h.m, Command{PaneID: "lk", Room: existingRoom, Server: h.base}).ID
	started := make(chan Reply, 1)
	go func() { started <- addAgent(h.m, id, "scribe", Command{}, h.creds) }()
	<-entered
	within(t, "Close", h.m.Close)
	release()
	if r := <-started; r.State != "stopped" {
		t.Fatalf("%+v", r)
	}
	h.room.mu.Lock()
	defer h.room.mu.Unlock()
	if !h.room.closed {
		t.Fatal("bot left connected after Close")
	}
}

// D9: while an agent bot of meeting A is being created (blocked at Recall), a
// second add on A is refused at once (one start per meeting at a time), and
// meeting B starts its own Recall bot meanwhile — there is no session cap.
func TestAgentStartIsPerMeetingNotPerSession(t *testing.T) {
	var creates atomic.Int32
	entered, gate := make(chan struct{}, 4), make(chan struct{})
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Hostname(), "recall.ai") {
			return old.RoundTrip(r)
		}
		body := `{}`
		if r.Method == "POST" && r.URL.Path == "/api/v1/bot/" {
			creates.Add(1)
			entered <- struct{}{}
			<-gate
			body = `{"id":"` + botID + `"}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	m := NewManager()
	t.Cleanup(m.Close)
	a := register(t, m, Command{PaneID: "p1", URL: meetA}).ID
	b := register(t, m, Command{PaneID: "p2", URL: meetB}).ID

	startedA, startedB := make(chan Reply, 1), make(chan Reply, 1)
	go func() { startedA <- addAgent(m, a, "scribe", webAdd(), recallKeys) }()
	<-entered
	within(t, "second add on the starting meeting", func() {
		if r := addAgent(m, a, "coder", webAdd(), recallKeys); !strings.Contains(r.Error, "is busy (starting scribe)") {
			t.Errorf("add %+v", r)
		}
	})
	go func() { startedB <- addAgent(m, b, "coder", webAdd(), recallKeys) }()
	within(t, "the other meeting's start reaches Recall", func() { <-entered })
	close(gate)
	if r := <-startedA; r.Error != "" {
		t.Fatalf("meeting A %+v", r)
	}
	if r := <-startedB; r.Error != "" {
		t.Fatalf("meeting B %+v", r)
	}
	if creates.Load() != 2 {
		t.Fatalf("%d Recall bots created, want 2", creates.Load())
	}
}
