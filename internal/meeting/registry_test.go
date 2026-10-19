// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	meetA = "https://meet.google.com/abc-defg-hij"
	meetB = "https://meet.google.com/xyz-wxyz-xyz"
)

func register(t *testing.T, m *Manager, c Command) Reply {
	t.Helper()
	c.Action = "register"
	r := m.Do(c, Credentials{}, nil)
	if r.Error != "" {
		t.Fatalf("register %+v: %+v", c, r)
	}
	return r
}

// fakeRecall answers Recall create/leave through http.DefaultTransport and
// passes everything else (the LiveKit rig's httptest server) through.
func fakeRecall(t *testing.T) (creates, leaves *atomic.Int32) {
	creates, leaves = &atomic.Int32{}, &atomic.Int32{}
	old := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = old })
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Hostname(), "recall.ai") {
			return old.RoundTrip(r)
		}
		body := `{}`
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/bot/":
			creates.Add(1)
			body = `{"id":"` + botID + `"}`
		case strings.HasSuffix(r.URL.Path, "/leave_call/"):
			leaves.Add(1)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	return creates, leaves
}

func recallJoin(url, id string) Command {
	return Command{Action: "join", ID: id, URL: url, PublicURL: "https://meeting.example", Listen: "127.0.0.1:0", Region: "eu-central-1", Consent: true}
}

var recallKeys = Credentials{Recall: "key", Verification: "whsec_" + base64.StdEncoding.EncodeToString(testKey), Speech: "speech"}

func noopExec(context.Context, string) (string, error) { return "", nil }

func TestRegistryManyPaneMeetingsWithUniqueIDs(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Close)
	seen := map[string]bool{}
	for i, pane := range []string{"p1", "p2", "p3", "p4", "p5"} {
		c := Command{PaneID: pane, URL: meetA}
		if i%2 == 1 {
			c = Command{PaneID: pane, Room: "room" + pane, Server: "https://rysh.example/"}
		}
		r := register(t, m, c)
		if !ValidMeetingID(r.ID) || seen[r.ID] || r.PaneID != pane || r.State != "idle" || r.Notice != "" {
			t.Fatalf("%+v", r)
		}
		seen[r.ID] = true
		if i%2 == 1 && (r.Kind != KindLiveKit || r.Platform != PlatformLiveKit || r.Target != "room"+pane || r.Room != "room"+pane) {
			t.Fatalf("livekit %+v", r)
		}
		if i%2 == 0 && (r.Kind != KindWeb || r.Platform != PlatformMeet || r.Target != meetA) {
			t.Fatalf("web %+v", r)
		}
	}
	list := m.Do(Command{Action: "list"}, Credentials{}, nil)
	if len(list.Meetings) != 5 || list.Meetings[0].PaneID != "p1" || list.Meetings[4].PaneID != "p5" {
		t.Fatalf("list %+v", list)
	}
	if mt, ok := m.ByPane("p2"); !ok || mt.Server != "https://rysh.example" || mt.hasBot() {
		t.Fatalf("by pane %+v", mt)
	}

	id := list.Meetings[2].ID
	if r := m.Do(Command{Action: "status", ID: id}, Credentials{}, nil); r.ID != id || r.PaneID != "p3" || r.State != "idle" {
		t.Fatalf("status by id %+v", r)
	}
	// leave on a bot-less pane meeting keeps it: the pane still shows it.
	if r := m.Do(Command{Action: "leave", ID: id}, Credentials{}, nil); r.Error != "" || r.ID != id {
		t.Fatalf("leave %+v", r)
	}
	if r := m.Do(Command{Action: "unregister", ID: id}, Credentials{}, nil); r.Error != "" || r.State != "stopped" {
		t.Fatalf("unregister %+v", r)
	}
	if _, ok := m.Lookup(id); ok {
		t.Fatal("unregistered meeting still listed")
	}
	if r := m.Do(Command{Action: "status", ID: id}, Credentials{}, nil); !strings.Contains(r.Error, "no meeting "+id) {
		t.Fatalf("status of a removed id %+v", r)
	}
	if n := len(m.Do(Command{Action: "list"}, Credentials{}, nil).Meetings); n != 4 {
		t.Fatalf("list after unregister: %d", n)
	}
}

func TestRegistryRefusesBadRegistrations(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Close)
	register(t, m, Command{PaneID: "p1", URL: meetA})
	for name, c := range map[string]Command{
		"no pane":           {URL: meetA},
		"pane taken":        {PaneID: "p1", URL: meetB},
		"nothing":           {PaneID: "p2"},
		"both":              {PaneID: "p2", URL: meetA, Room: "r"},
		"zoom start link":   {PaneID: "p2", URL: "https://zoom.us/s/123456789?zak=secret"},
		"credentials":       {PaneID: "p2", URL: "https://user:pw@meet.google.com/abc-defg-hij"},
		"redirect":          {PaneID: "p2", URL: "https://example.com/abc-defg-hij"},
		"room without srv":  {PaneID: "p2", Room: "r"},
		"http server":       {PaneID: "p2", Room: "r", Server: "http://rysh.example"},
		"bad room":          {PaneID: "p2", Room: "../r", Server: "https://rysh.example"},
		"bad restore id":    {PaneID: "p2", URL: meetA, ID: "m-XYZ"},
		"restore id in use": {PaneID: "p2", URL: meetA, ID: m.Do(Command{Action: "list"}, Credentials{}, nil).Meetings[0].ID},
	} {
		c.Action = "register"
		if r := m.Do(c, Credentials{}, nil); r.Error == "" {
			t.Errorf("%s: registered %+v", name, r)
		}
	}
	if n := len(m.Do(Command{Action: "list"}, Credentials{}, nil).Meetings); n != 1 {
		t.Fatalf("refusals left %d meetings", n)
	}
}

// A meeting pane restored after a daemon restart keeps its id; status says
// its bot did not survive.
func TestRegistryRestoreKeepsID(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Close)
	r := register(t, m, Command{PaneID: "p1", URL: meetA, ID: "m-3f9a2c"})
	if r.ID != "m-3f9a2c" || !strings.Contains(r.Notice, "restart") {
		t.Fatalf("%+v", r)
	}
	if r := m.Do(Command{Action: "status"}, Credentials{}, nil); r.ID != "m-3f9a2c" || !strings.Contains(r.Notice, "bots do not survive") {
		t.Fatalf("%+v", r)
	}
	if fresh := register(t, m, Command{PaneID: "p2", URL: meetB}); fresh.ID == "m-3f9a2c" {
		t.Fatal("generated id collides with a restored one")
	}
}

func TestRegistryIDLessResolution(t *testing.T) {
	m := NewManager()
	t.Cleanup(m.Close)
	// 0 meetings: the pre-registry answers.
	if r := m.Do(Command{Action: "status"}, Credentials{}, nil); r.State != "stopped" || r.Error != "" {
		t.Fatalf("status with none %+v", r)
	}
	if r := m.Do(Command{Action: "pause"}, Credentials{}, nil); r.Error != "no active meeting" {
		t.Fatalf("pause with none %+v", r)
	}
	// 1 meeting, no bot: it is the one.
	one := register(t, m, Command{PaneID: "p1", URL: meetA})
	if r := m.Do(Command{Action: "status"}, Credentials{}, nil); r.ID != one.ID {
		t.Fatalf("only meeting %+v", r)
	}
	if r := m.Do(Command{Action: "pause"}, Credentials{}, nil); !strings.Contains(r.Error, "has no bot") || r.ID != one.ID {
		t.Fatalf("bot verb on a bot-less meeting %+v", r)
	}
	// 2 meetings, no bot: ambiguous, the error lists both ids.
	two := register(t, m, Command{PaneID: "p2", URL: meetB})
	r := m.Do(Command{Action: "status"}, Credentials{}, nil)
	if !strings.Contains(r.Error, one.ID) || !strings.Contains(r.Error, two.ID) {
		t.Fatalf("ambiguous %+v", r)
	}
	// ... until one of them has a bot: then that one.
	fakeRecall(t)
	if r := m.Do(recallJoin("", two.ID), recallKeys, noopExec); r.Error != "" || r.ID != two.ID || r.BotID != botID {
		t.Fatalf("join into pane meeting %+v", r)
	}
	if r := m.Do(Command{Action: "status"}, Credentials{}, nil); r.ID != two.ID || r.BotID != botID {
		t.Fatalf("the only meeting with a bot %+v", r)
	}
	// Leaving the pane meeting's bot keeps the meeting (its pane is open).
	if r := m.Do(Command{Action: "leave"}, Credentials{}, nil); r.State != "stopped" || r.ID != two.ID {
		t.Fatalf("leave %+v", r)
	}
	if mt, ok := m.Lookup(two.ID); !ok || mt.hasBot() {
		t.Fatalf("pane meeting after leave %+v %v", mt, ok)
	}
}

// Old CLI flow: id-less join/host create a pane-less meeting holding the bot;
// leave removes it entirely.
func TestRegistryPaneLessCLIMeeting(t *testing.T) {
	h := newHostRig(t)
	r := h.host(t, Command{})
	if r.Error != "" || !ValidMeetingID(r.ID) || r.PaneID != "" || r.Kind != KindLiveKit || r.Target == "" {
		t.Fatalf("%+v", r)
	}
	if r := h.m.Do(Command{Action: "status", ID: r.ID}, Credentials{}, nil); r.State == "" || r.Room == "" {
		t.Fatalf("status by id %+v", r)
	}
	if r := h.m.Do(Command{Action: "leave"}, Credentials{}, nil); r.State != "stopped" || r.Error != "" {
		t.Fatalf("leave %+v", r)
	}
	if n := len(h.m.Do(Command{Action: "list"}, Credentials{}, nil).Meetings); n != 0 {
		t.Fatalf("pane-less meeting survived leave: %d", n)
	}
	// A failed start leaves nothing behind.
	h.lk.status = 401
	if r := h.host(t, Command{}); r.State != "create_failed" || r.ID != "" {
		t.Fatalf("%+v", r)
	}
	if n := len(h.m.Do(Command{Action: "list"}, Credentials{}, nil).Meetings); n != 0 {
		t.Fatalf("failed host left %d meetings", n)
	}
}

// Decision D4: one Recall bot per session; the refusal names the meeting that
// holds it. LiveKit bots are not capped.
// D4 is gone (design 043 D9): a session holds several Recall bots, each
// on its own listener path; id-less commands then need an id.
func TestRegistrySeveralRecallBotsPerSession(t *testing.T) {
	h := newHostRig(t)
	real := http.DefaultTransport
	creates, leaves := fakeRecall(t)
	a := register(t, h.m, Command{PaneID: "p1", URL: meetA})
	b := register(t, h.m, Command{PaneID: "p2", URL: meetB})
	for _, c := range []Command{recallJoin(meetA, a.ID), recallJoin(meetB, b.ID)} {
		if r := h.m.Do(c, recallKeys, noopExec); r.Error != "" || r.ID != c.ID {
			t.Fatalf("Recall bot for %s: %+v", c.ID, r)
		}
	}
	if creates.Load() != 2 {
		t.Fatalf("Recall creates %d", creates.Load())
	}
	// A meeting's own bot is still one per CLI join.
	if r := h.m.Do(recallJoin(meetA, a.ID), recallKeys, noopExec); !strings.Contains(r.Error, "already has a bot") {
		t.Fatalf("second join into %s %+v", a.ID, r)
	}
	// A LiveKit bot beside them. (Its speech stream wants the real
	// *http.Transport.)
	fake := http.DefaultTransport
	http.DefaultTransport = real
	r := h.m.Do(Command{Action: "host"}, h.creds, noopExec)
	http.DefaultTransport = fake
	if r.Error != "" || r.Kind != KindLiveKit {
		t.Fatalf("host beside Recall bots %+v", r)
	}
	// Several bots: id-less bot commands are ambiguous.
	if r := h.m.Do(Command{Action: "pause"}, Credentials{}, nil); !strings.Contains(r.Error, "several meetings") {
		t.Fatalf("%+v", r)
	}
	// Unregister (pane closed) makes that meeting's bot leave, only it.
	if r := h.m.Do(Command{Action: "unregister", ID: a.ID}, Credentials{}, nil); r.Error != "" {
		t.Fatalf("%+v", r)
	}
	if leaves.Load() != 1 {
		t.Fatalf("Recall leaves %d", leaves.Load())
	}
	if mt, ok := h.m.Lookup(b.ID); !ok || !mt.hasBot() {
		t.Fatalf("meeting %s lost its bot: %+v", b.ID, mt)
	}
}

func TestRegistryJoinByIDChecks(t *testing.T) {
	h := newHostRig(t)
	fakeRecall(t)
	lk := register(t, h.m, Command{PaneID: "p1", Room: "room1", Server: "https://rysh.example"})
	web := register(t, h.m, Command{PaneID: "p2", URL: meetA})
	for name, c := range map[string]Command{
		"unknown id":     recallJoin(meetA, "m-000000"),
		"livekit room":   recallJoin("", lk.ID),
		"other url":      recallJoin(meetB, web.ID),
		"host by id":     {Action: "host", ID: web.ID},
		"host into room": {Action: "host", ID: lk.ID},
	} {
		if r := h.m.Do(c, recallKeys, noopExec); r.Error == "" {
			t.Errorf("%s: accepted %+v", name, r)
		}
	}
	if r := h.m.Do(recallJoin("", web.ID), recallKeys, noopExec); r.Error != "" {
		t.Fatal(r)
	}
	if r := h.m.Do(recallJoin("", web.ID), recallKeys, noopExec); !strings.Contains(r.Error, "already has a bot") {
		t.Fatalf("second bot in one meeting %+v", r)
	}
}
