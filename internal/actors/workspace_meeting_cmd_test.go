// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// meetingCmdRig is a bare workspace over in-process NATS with a tab snapshot
// the test controls, a fake rysh-server, and the meeting seams captured.
type meetingCmdRig struct {
	w       *WorkspaceActor
	nc      *nats.Conn
	codecs  *msg.CodecRegistry
	self    chan any
	notices chan string
	server  *httptest.Server
	calls   chan string

	mu     sync.Mutex
	extra  []domain.PaneSnapshot     // panes in the snapshot besides the origin "p1"
	bodies map[string]map[string]any // last JSON body per "METHOD path"
}

func newMeetingCmdRig(t *testing.T) *meetingCmdRig {
	t.Helper()
	h := &meetingCmdRig{self: make(chan any, 8), notices: make(chan string, 16), calls: make(chan string, 16)}
	h.nc = startInProcessNATS(t)
	h.codecs = msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(h.nc, h.codecs)

	sub, err := h.nc.Subscribe(msg.T("tab", "tab-1", "snapshot"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		_ = json.Unmarshal(m.Data, &env)
		h.mu.Lock()
		groups := []domain.PaneGroupSnapshot{{ID: "grp-1", Panes: []domain.PaneSnapshot{{ID: "p1", Title: "p1"}}}}
		for _, p := range h.extra {
			groups = append(groups, domain.PaneGroupSnapshot{ID: "grp-" + p.ID, Panes: []domain.PaneSnapshot{p}})
		}
		h.mu.Unlock()
		snap := domain.TabSnapshot{ID: "tab-1", Lanes: []domain.LaneSnapshot{{ID: "lane-tab-1", PaneGroups: groups}}}
		re := &msg.RequestEnvelope{ReplyTo: env.ReplyTo, NC: h.nc, Codecs: h.codecs}
		_ = re.Reply(&msg.MsgTabSnapshotReply{Snapshot: snap})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	_ = h.nc.Flush()

	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		if h.bodies == nil {
			h.bodies = map[string]map[string]any{}
		}
		h.bodies[r.Method+" "+r.URL.Path] = body
		h.mu.Unlock()
		h.calls <- r.Method + " " + r.URL.Path + " " + r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/meeting-rooms":
			_, _ = w.Write([]byte(`{"room":{"id":"room-new","owner_id":"u1","room_name":"rn-new","expires_at":"2026-09-30T01:00:00Z"}}`))
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/invites"):
			room := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/meeting-rooms/"), "/invites")
			if body["email"] != nil || body["guest"] == true {
				_, _ = w.Write([]byte(`{"invite":{"id":"3f9a2c1e-7b4d-4e0a-9c11-000000000abc","max_uses":1,"allow_guest":true,"expires_at":"2026-09-30T01:00:00Z"},` +
					`"token":"rysh_mi_s","join_url":"https://rysh.test/join/` + room + `#invite=rysh_mi_s","guest_identity":"guest-3f9a2c1e7b4d"}`))
				return
			}
			_, _ = w.Write([]byte(`{"invite":{"id":"4f9a2c1e-7b4d-4e0a-9c11-000000000abc","max_uses":20,"expires_at":"2026-09-30T01:00:00Z"},` +
				`"token":"rysh_mi_s","join_url":"https://rysh.test/meet/` + room + `#invite=rysh_mi_s"}`))
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/invites"):
			_, _ = w.Write([]byte(`{"invites":[{"id":"3f9a2c1e-7b4d-4e0a-9c11-000000000abc","label":"jane@example.test","allow_guest":true,"max_uses":1,"uses":0,"expires_at":"2026-09-30T01:00:00Z"},` +
				`{"id":"4f9a2c1e-7b4d-4e0a-9c11-000000000abc","max_uses":20,"uses":3,"expires_at":"2026-09-30T01:00:00Z","revoked_at":"2026-09-29T20:00:00Z"}]}`))
		case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/invites/"):
			_, _ = w.Write([]byte(`{"message":"invite revoked"}`))
		case r.Method == "GET" && r.URL.Path == "/api/meeting-rooms/gone":
			w.WriteHeader(410)
		case r.Method == "GET":
			id := strings.TrimPrefix(r.URL.Path, "/api/meeting-rooms/")
			_, _ = w.Write([]byte(`{"room":{"id":"` + id + `","owner_id":"u1","room_name":"rn"}}`))
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/token"):
			_, _ = w.Write([]byte(`{"token":"lk-secret-token","url":"wss://lk.example","identity":"u1","expires_at":"2026-09-28T22:00:00Z"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(h.server.Close)
	t.Setenv(MeetingServerVar, h.server.URL)

	oldNotice, oldDelay := meetingPaneNotice, meetingPaneGoneDelay
	t.Cleanup(func() { meetingPaneNotice, meetingPaneGoneDelay = oldNotice, oldDelay })
	meetingPaneNotice = func(_ *WorkspaceActor, paneID, text string) { h.notices <- paneID + ":" + text }
	meetingPaneGoneDelay = 10 * time.Millisecond

	h.w = &WorkspaceActor{
		pub:          pub,
		activePaneID: "p1",
		tabs:         []*tabInfo{{id: "tab-1", title: "tab"}},
		usedAliases:  map[string]struct{}{},
		lastKVWrite:  time.Now(),
		cfg:          config.Config{Upstream: config.UpstreamConfig{APIKey: "rysh_upstream_key"}},
		meetingSend:  func(m any) { h.self <- m },
	}
	t.Cleanup(func() {
		if h.w.meeting != nil {
			h.w.meeting.Close()
		}
	})
	return h
}

func (h *meetingCmdRig) run(args ...string) string {
	var out strings.Builder
	h.w.handleMeetingCommand(&out, "p1", args)
	return out.String()
}

func (h *meetingCmdRig) next(t *testing.T) any {
	t.Helper()
	select {
	case m := <-h.self:
		return m
	case n := <-h.notices:
		t.Fatalf("notice instead of a self message: %s", n)
	case <-time.After(5 * time.Second):
		t.Fatal("no self message")
	}
	return nil
}

func (h *meetingCmdRig) notice(t *testing.T) string {
	t.Helper()
	select {
	case n := <-h.notices:
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("no notice")
	}
	return ""
}

// join runs ##meeting join and creates the pane; it returns the create
// message the tab received.
func (h *meetingCmdRig) join(t *testing.T, args ...string) *msg.MsgTabCreatePaneGroupInLane {
	t.Helper()
	tabInbox, err := h.nc.SubscribeSync(msg.T("tab", "tab-1", "inbox"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tabInbox.Unsubscribe() }()
	out := h.run(append([]string{"join"}, args...)...)
	if !strings.Contains(out, "opening") {
		t.Fatalf("join output %q", out)
	}
	create, ok := h.next(t).(*msgMeetingPaneCreate)
	if !ok {
		t.Fatal("expected msgMeetingPaneCreate")
	}
	h.w.handleMeetingPaneCreate(create)
	raw, err := tabInbox.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cp, ok := decodeEnvelope(t, h.codecs, raw).(*msg.MsgTabCreatePaneGroupInLane)
	if !ok || cp.PaneType != domain.PaneTypeMeeting || cp.PaneID != create.paneID || cp.LaneID != "lane-tab-1" {
		t.Fatalf("create %+v", cp)
	}
	if n := h.notice(t); !strings.Contains(n, "p1:") || !strings.Contains(n, "meeting "+cp.Meta["meeting.id"]+": joined "+cp.Meta["meeting.platform"]+" in pane "+shortID(cp.PaneID)) {
		t.Fatalf("join notice %q", n)
	}
	h.mu.Lock()
	h.extra = append(h.extra, domain.PaneSnapshot{ID: cp.PaneID, PaneType: domain.PaneTypeMeeting, Meta: cp.Meta})
	h.mu.Unlock()
	return cp
}

func TestMeetingJoinWebCreatesMeetingPaneWithContractMeta(t *testing.T) {
	h := newMeetingCmdRig(t)
	cp := h.join(t, "https://meet.google.com/abc-defg-hij")
	want := map[string]string{"meeting.kind": "web", "meeting.platform": "google_meet", "meeting.url": "https://meet.google.com/abc-defg-hij", "meeting.profile": "default"}
	for k, v := range want {
		if cp.Meta[k] != v {
			t.Errorf("meta %s = %q, want %q", k, cp.Meta[k], v)
		}
	}
	if !meeting.ValidMeetingID(cp.Meta["meeting.id"]) || cp.Meta["meeting.room"] != "" {
		t.Fatalf("meta %v", cp.Meta)
	}
	mt, ok := h.w.meeting.ByPane(cp.PaneID)
	if !ok || mt.ID != cp.Meta["meeting.id"] || len(mt.Bots) > 0 {
		t.Fatalf("registry %+v", mt)
	}
	cp2 := h.join(t, "https://zoom.us/j/123456789?pwd=abc", "--profile", "work")
	if cp2.Meta["meeting.profile"] != "work" || cp2.Meta["meeting.platform"] != "zoom" || cp2.Meta["meeting.id"] == cp.Meta["meeting.id"] {
		t.Fatalf("second meeting %v", cp2.Meta)
	}
	h.run("list")
	list := h.notice(t)
	if !strings.Contains(list, cp.Meta["meeting.id"]) || !strings.Contains(list, cp2.Meta["meeting.id"]) || !strings.Contains(list, "no bot") {
		t.Fatalf("list %q", list)
	}
}

// Teams links open a web meeting pane — the /meet/<id> short links too, which
// share their path shape with rysh room links (043 §"Zoom and Teams in panes").
func TestMeetingJoinTeamsLinksOpenWebPanes(t *testing.T) {
	for _, link := range []string{
		"https://teams.microsoft.com/meet/2345678901234?p=AbCdEfGh",
		"https://teams.live.com/meet/9876543210987?p=XyZ123",
		"https://teams.microsoft.com/l/meetup-join/19%3ameeting_NjQ1MjBkYzItYjU0Yi00ZDIxLWE4ZGEtOTc0YzQ5ZjQ2ZmZh%40thread.v2/0?context=%7b%22Tid%22%3a%22t%22%7d",
	} {
		h := newMeetingCmdRig(t)
		cp := h.join(t, link)
		if cp.Meta["meeting.kind"] != "web" || cp.Meta["meeting.platform"] != "microsoft_teams" || cp.Meta["meeting.url"] != link || cp.Meta["meeting.room"] != "" {
			t.Errorf("%s: meta %v", link, cp.Meta)
		}
	}
}

func TestMeetingJoinRefusesBadTargets(t *testing.T) {
	h := newMeetingCmdRig(t)
	for _, args := range [][]string{
		{"join"},
		{"join", "https://zoom.us/s/123456789?zak=secret"},
		{"join", "https://user:pw@meet.google.com/abc-defg-hij"},
		{"join", "https://evil.example/abc"},
		{"join", "https://evil.example/meet/room1"},
		{"join", "rysh:../x"},
		{"join", "https://meet.google.com/abc-defg-hij", "--title", "t"},
		{"join", "rysh:room1", "--profile", "work"},
		{"join", "https://meet.google.com/abc-defg-hij", "--profile", "../x"},
		{"join", "a", "b"},
	} {
		out := h.run(args...)
		if h.w.takeRyshFailure() == nil || strings.Contains(out, "opening") {
			t.Errorf("%v accepted: %q", args, out)
		}
	}
	select {
	case m := <-h.self:
		t.Fatalf("refused join still created %T", m)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestMeetingJoinRyshRooms(t *testing.T) {
	h := newMeetingCmdRig(t)
	cp := h.join(t, "rysh:room1")
	if c := <-h.calls; c != "GET /api/meeting-rooms/room1 Bearer rysh_upstream_key" {
		t.Fatalf("room check %q", c)
	}
	want := map[string]string{"meeting.kind": "livekit", "meeting.platform": "livekit", "meeting.room": "room1",
		"meeting.server": h.server.URL, "meeting.url": h.server.URL + "/meet/room1"}
	for k, v := range want {
		if cp.Meta[k] != v {
			t.Errorf("meta %s = %q, want %q", k, cp.Meta[k], v)
		}
	}
	if _, ok := cp.Meta["meeting.profile"]; ok {
		t.Error("livekit pane carries a browser profile")
	}

	cp = h.join(t, h.server.URL+"/meet/room2")
	<-h.calls
	if cp.Meta["meeting.room"] != "room2" {
		t.Fatal(cp.Meta)
	}

	cp = h.join(t, "new", "--title", "Standup")
	if c := <-h.calls; c != "POST /api/meeting-rooms Bearer rysh_upstream_key" {
		t.Fatalf("create %q", c)
	}
	if cp.Meta["meeting.room"] != "room-new" {
		t.Fatal(cp.Meta)
	}

	// A room the server refuses: no pane, the error in the invoking pane.
	h.run("join", "rysh:gone")
	<-h.calls
	if n := h.notice(t); !strings.Contains(n, "ended") {
		t.Fatalf("gone room %q", n)
	}
	if n := len(h.w.meeting.Do(meeting.Command{Action: "list"}, meeting.Credentials{}, nil).Meetings); n != 3 {
		t.Fatalf("%d meetings", n)
	}
}

// meetingToken sends MsgMeetingToken through a real request envelope.
func (h *meetingCmdRig) meetingToken(t *testing.T, paneID string) *msg.MsgMeetingTokenReply {
	t.Helper()
	return h.meetingTokenReq(t, &msg.MsgMeetingToken{PaneID: paneID})
}

func (h *meetingCmdRig) meetingTokenReq(t *testing.T, req *msg.MsgMeetingToken) *msg.MsgMeetingTokenReply {
	t.Helper()
	inbox := nats.NewInbox()
	sub, err := h.nc.SubscribeSync(inbox)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	h.w.handleMeetingToken(req, &msg.RequestEnvelope{ReplyTo: inbox, NC: h.nc, Codecs: h.codecs})
	raw, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := decodeEnvelope(t, h.codecs, raw).(*msg.MsgMeetingTokenReply)
	if !ok {
		t.Fatal("not a token reply")
	}
	return r
}

func TestMeetingTokenForMeetingPanesOnly(t *testing.T) {
	h := newMeetingCmdRig(t)
	lk := h.join(t, "rysh:room1")
	<-h.calls
	web := h.join(t, "https://meet.google.com/abc-defg-hij")

	r := h.meetingToken(t, lk.PaneID)
	if r.Error != "" || r.Token != "lk-secret-token" || r.URL != "wss://lk.example" || r.Identity != "u1" || r.ExpiresAt == "" || r.PaneID != lk.PaneID {
		t.Fatalf("%+v", r)
	}
	if c := <-h.calls; c != "POST /api/meeting-rooms/room1/token Bearer rysh_upstream_key" {
		t.Fatalf("token call %q", c)
	}
	for _, pane := range []string{web.PaneID, "p1", "", "nope"} {
		if r := h.meetingToken(t, pane); r.Error == "" || r.Token != "" {
			t.Errorf("pane %q got %+v", pane, r)
		}
	}
	// The token appears in no notice and no pane meta.
	close(h.notices)
	for n := range h.notices {
		if strings.Contains(n, "lk-secret-token") {
			t.Fatalf("token leaked into pane output: %q", n)
		}
	}
	h.notices = make(chan string, 16)
	for _, p := range h.extra {
		for _, v := range p.Meta {
			if strings.Contains(v, "lk-secret-token") {
				t.Fatal("token in pane meta")
			}
		}
	}
}

// E-87: the app's Meeting panel has no pane; it names a room on the
// session's rysh server, and the daemon mints the token with its own key.
func TestMeetingTokenForRoomWithoutPane(t *testing.T) {
	h := newMeetingCmdRig(t)
	r := h.meetingTokenReq(t, &msg.MsgMeetingToken{Room: "room9"})
	if r.Error != "" || r.Token != "lk-secret-token" || r.URL != "wss://lk.example" || r.Identity != "u1" || r.Room != "room9" || r.PaneID != "" {
		t.Fatalf("%+v", r)
	}
	if c := <-h.calls; c != "POST /api/meeting-rooms/room9/token Bearer rysh_upstream_key" {
		t.Fatalf("token call %q", c)
	}
	// A pane request still ignores any room it carries: the pane's room wins.
	lk := h.join(t, "rysh:room1")
	<-h.calls
	if r := h.meetingTokenReq(t, &msg.MsgMeetingToken{PaneID: lk.PaneID, Room: "room9"}); r.Error != "" {
		t.Fatalf("%+v", r)
	}
	if c := <-h.calls; c != "POST /api/meeting-rooms/room1/token Bearer rysh_upstream_key" {
		t.Fatalf("pane token call %q", c)
	}
	// Bad room ids never reach the server.
	for _, room := range []string{"../x", "a/b", strings.Repeat("r", 65)} {
		if r := h.meetingTokenReq(t, &msg.MsgMeetingToken{Room: room}); r.Error == "" || r.Token != "" || r.Room != room {
			t.Errorf("room %q got %+v", room, r)
		}
	}
	select {
	case c := <-h.calls:
		t.Fatalf("bad room reached the server: %q", c)
	case <-time.After(50 * time.Millisecond):
	}
	// No session server: a clear error, no request.
	t.Setenv(MeetingServerVar, "")
	if r := h.meetingTokenReq(t, &msg.MsgMeetingToken{Room: "room9"}); !strings.Contains(r.Error, "no rysh server") {
		t.Fatalf("%+v", r)
	}
}

func TestMeetingPaneClosedByHandUnregisters(t *testing.T) {
	h := newMeetingCmdRig(t)
	cp := h.join(t, "https://meet.google.com/abc-defg-hij")
	id := cp.Meta["meeting.id"]

	// The pane actor stopped but the pane is still in the snapshot: no leave.
	h.w.meetingPaneStopped(cp.PaneID)
	h.w.handleMeetingPaneGone(h.next(t).(*msgMeetingPaneGone))
	if _, ok := h.w.meeting.Lookup(id); !ok {
		t.Fatal("unregistered a pane that is still there")
	}

	h.mu.Lock()
	h.extra = nil
	h.mu.Unlock()
	h.w.meetingPaneStopped(cp.PaneID)
	h.w.handleMeetingPaneGone(h.next(t).(*msgMeetingPaneGone))
	if n := h.notice(t); !strings.Contains(n, "p1:") || !strings.Contains(n, "meeting "+id+": pane closed") {
		t.Fatalf("notice %q", n)
	}
	if _, ok := h.w.meeting.Lookup(id); ok {
		t.Fatal("closed pane's meeting still registered")
	}
	// Unknown panes are ignored.
	h.w.meetingPaneStopped("other")
	select {
	case m := <-h.self:
		t.Fatalf("stop of a non-meeting pane sent %T", m)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestMeetingLeaveClosesItsPane(t *testing.T) {
	h := newMeetingCmdRig(t)
	cp := h.join(t, "https://meet.google.com/abc-defg-hij")
	id := cp.Meta["meeting.id"]
	laneInbox, err := h.nc.SubscribeSync(msg.T("lane", "lane-tab-1", "inbox"))
	if err != nil {
		t.Fatal(err)
	}
	out := h.run("leave", id)
	if !strings.Contains(out, "closing pane "+shortID(cp.PaneID)) {
		t.Fatalf("leave %q", out)
	}
	raw, err := laneInbox.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatal("no group delete published:", err)
	}
	if del, ok := decodeEnvelope(t, h.codecs, raw).(*msg.MsgLaneDeletePaneGroup); !ok || del.PaneGroupID != "grp-"+cp.PaneID {
		t.Fatalf("lane got %+v", del)
	}
	if out := h.run("leave", "m-bogus"); !strings.Contains(out, "not a meeting id") {
		t.Fatalf("bad id %q", out)
	}
}

func TestMeetingRestoreReregistersPanesWithTheirIDs(t *testing.T) {
	h := newMeetingCmdRig(t)
	h.extra = []domain.PaneSnapshot{
		{ID: "mp1", PaneType: domain.PaneTypeMeeting, Meta: map[string]string{"meeting.id": "m-00aa11", "meeting.kind": "web", "meeting.url": "https://meet.google.com/abc-defg-hij"}},
		{ID: "mp2", PaneType: domain.PaneTypeMeeting, Meta: map[string]string{"meeting.id": "m-00bb22", "meeting.kind": "livekit", "meeting.room": "room1", "meeting.server": h.server.URL}},
		{ID: "mp3", PaneType: domain.PaneTypeMeeting, Meta: map[string]string{"meeting.id": "garbage", "meeting.kind": "web"}},
		{ID: "shell", Meta: map[string]string{"meeting.id": "m-00cc33", "meeting.kind": "web", "meeting.url": "https://meet.google.com/abc-defg-hij"}},
	}
	<-h.w.restoreMeetingPanes()
	for pane, id := range map[string]string{"mp1": "m-00aa11", "mp2": "m-00bb22"} {
		mt, ok := h.w.meeting.ByPane(pane)
		if !ok || mt.ID != id || len(mt.Bots) > 0 {
			t.Fatalf("%s: %+v %v", pane, mt, ok)
		}
	}
	if n := len(h.w.meeting.Do(meeting.Command{Action: "list"}, meeting.Credentials{}, nil).Meetings); n != 2 {
		t.Fatalf("%d meetings restored", n)
	}
	// Idempotent.
	<-h.w.restoreMeetingPanes()
	if r := h.w.meeting.Do(meeting.Command{Action: "status", ID: "m-00aa11"}, meeting.Credentials{}, nil); !strings.Contains(r.Notice, "restart") {
		t.Fatalf("%+v", r)
	}
	// A token request right after a restart finds the restored room.
	if r := h.meetingToken(t, "mp2"); r.Error != "" {
		t.Fatalf("%+v", r)
	}
}

// A foreign /meet/ link on a session with no rysh server is refused as not
// this session's server, not as a missing server.
func TestMeetingJoinForeignRoomLinkWithoutServer(t *testing.T) {
	h := newMeetingCmdRig(t)
	t.Setenv(MeetingServerVar, "")
	h.w.cfg.Upstream.URL = ""
	out := h.run("join", "https://evil.example/meet/x")
	if h.w.takeRyshFailure() == nil || !strings.Contains(out, "evil.example, which is not this session's rysh server") || strings.Contains(out, "opening") {
		t.Fatalf("%q", out)
	}
}

func TestMeetingAddRemoveRouteToAgentCommand(t *testing.T) {
	h := newMeetingCmdRig(t)
	for _, args := range [][]string{{"add"}, {"remove", "agent"}} {
		if out := h.run(args...); !strings.Contains(out, "##meeting add agent") {
			t.Errorf("%v: %q", args, out)
		}
	}
	if out := h.run("add", "agent", "m-00aa11", "scribe"); !strings.Contains(out, "no meeting m-00aa11") {
		t.Errorf("%q", out)
	}
	if out := h.run("wibble"); !strings.Contains(out, "unknown subcommand for ##meeting") {
		t.Errorf("%q", out)
	}
}
