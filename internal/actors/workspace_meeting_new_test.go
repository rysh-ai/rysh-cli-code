// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// open runs a ##meeting command that opens a pane (args include the verb) and
// creates the pane; it returns the create message and the "joined" notice.
func (h *meetingCmdRig) open(t *testing.T, args ...string) (*msg.MsgTabCreatePaneGroupInLane, string) {
	t.Helper()
	tabInbox, err := h.nc.SubscribeSync(msg.T("tab", "tab-1", "inbox"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tabInbox.Unsubscribe() }()
	if out := h.run(args...); !strings.Contains(out, "opening") {
		t.Fatalf("%v output %q", args, out)
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
	if !ok || cp.PaneType != domain.PaneTypeMeeting {
		t.Fatalf("create %+v", cp)
	}
	n := h.notice(t)
	if !strings.Contains(n, "meeting "+cp.Meta["meeting.id"]+": joined") {
		t.Fatalf("joined notice %q", n)
	}
	return cp, n
}

func TestMeetingNewArgsAndProvider(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		fromVar string
		target  string // "" = refused
		why     string
	}{
		{nil, "", "new", ""},
		{[]string{"--title", "Weekly", "sync", "--hours", "3"}, "", "new", ""},
		{[]string{"--provider", "rysh"}, "", "new", ""},
		{nil, "livekit", "new", ""},
		{[]string{"--provider", "LiveKit"}, "meet", "new", ""},
		{[]string{"--provider", "meet", "--link", "https://meet.google.com/abc-defg-hij"}, "", "https://meet.google.com/abc-defg-hij", ""},
		{[]string{"--link", "https://zoom.us/j/123456789"}, "zoom", "https://zoom.us/j/123456789", ""},
		{[]string{"--provider", "meet"}, "", "", "https://meet.new"},
		{nil, "teams", "", "Meet now"},
		{nil, "webex", "", "the session var MEETING_PROVIDER"},
		{[]string{"--provider", "zoom", "--link", "https://meet.google.com/abc-defg-hij"}, "", "", "is a Google Meet link, not Zoom"},
		{[]string{"--link", "https://meet.google.com/abc-defg-hij"}, "", "", "a LiveKit room is created here"},
		{[]string{"--profile", "work"}, "", "", "--profile applies only"},
		{[]string{"--provider", "meet", "--link", "https://meet.google.com/abc-defg-hij", "--invite", "guest"}, "", "", "invite people to a Google Meet meeting in Google Meet"},
		{[]string{"--provider", "meet", "--link", "https://meet.google.com/abc-defg-hij", "--hours", "2"}, "", "", "--hours applies"},
		{[]string{"--hours", "73"}, "", "", "1–72"},
		{[]string{"--invite", "bob"}, "", "", "invite whom"},
		{[]string{"--invite", "Bob <bob@example.test>"}, "", "", ""},
		{[]string{"--agent"}, "", "", "usage"},
		{[]string{"stray"}, "", "", "usage"},
	} {
		a, err := parseMeetingNewArgs(tc.args)
		if err == nil {
			err = a.resolveProvider(tc.fromVar)
		}
		if tc.target != "" {
			if err != nil || a.join.target != tc.target {
				t.Errorf("%v (var %q): target %q err %v", tc.args, tc.fromVar, a.join.target, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.why) {
			t.Errorf("%v (var %q): err %v, want %q", tc.args, tc.fromVar, err, tc.why)
		}
	}
	a, _ := parseMeetingNewArgs([]string{"--title", "\"Weekly", "sync\"", "--agent", "alice", "--agent", "bob", "--invite", "guest", "--consent"})
	if a.join.title != "Weekly sync" || strings.Join(a.then.agents, ",") != "alice,bob" || a.then.invites[0] != "guest" || !a.then.consent {
		t.Fatalf("parsed %+v", a)
	}
}

func TestMeetingNewDefaultsToALiveKitRoom(t *testing.T) {
	h := newMeetingCmdRig(t)
	cp, n := h.open(t, "new", "--title", "Standup", "--hours", "3")
	if c := <-h.calls; c != "POST /api/meeting-rooms Bearer rysh_upstream_key" {
		t.Fatalf("create %q", c)
	}
	h.mu.Lock()
	body := h.bodies["POST /api/meeting-rooms"]
	h.mu.Unlock()
	if body["title"] != "Standup" || body["ttl_hours"] != float64(3) {
		t.Fatalf("create body %v", body)
	}
	if cp.Meta["meeting.kind"] != "livekit" || cp.Meta["meeting.room"] != "room-new" {
		t.Fatalf("meta %v", cp.Meta)
	}
	id := cp.Meta["meeting.id"]
	if !strings.Contains(n, "room link (rysh accounts): "+h.server.URL+"/meet/room-new · ends ") ||
		!strings.Contains(n, "next: ##meeting add agent "+id+" <agent> · ##meeting invite "+id+" <email|guest>") {
		t.Fatalf("created notice %q", n)
	}
	// The founder's spelling is the same command.
	var out strings.Builder
	h.w.handleRyshSelfCommand(nil, &out, "p1", []string{"meeting", "list"})
	if l := h.notice(t); !strings.Contains(l, id) {
		t.Fatalf("##rysh meeting list %q", l)
	}
}

func TestMeetingNewWebNeedsALink(t *testing.T) {
	h := newMeetingCmdRig(t)
	out := h.run("new", "--provider", "meet")
	if h.w.takeRyshFailure() == nil || !strings.Contains(out, "https://meet.new") || !strings.Contains(out, "##meeting new --provider meet --link <url>") {
		t.Fatalf("meet without a link: %q", out)
	}
	cp, n := h.open(t, "new", "--provider", "meet", "--link", "https://meet.google.com/abc-defg-hij", "--profile", "work")
	if cp.Meta["meeting.kind"] != "web" || cp.Meta["meeting.profile"] != "work" || strings.Contains(n, "room link") {
		t.Fatalf("meet pane %v %q", cp.Meta, n)
	}
	// Invites to a Meet meeting happen in Meet: said, not failed.
	out = h.run("invite", cp.Meta["meeting.id"], "jane@example.test")
	if h.w.takeRyshFailure() != nil || !strings.Contains(out, "invite people in Google Meet itself") || !strings.Contains(out, "https://meet.google.com/abc-defg-hij") {
		t.Fatalf("web invite %q", out)
	}
}

func TestMeetingNewRunsAgentAndInviteFollowups(t *testing.T) {
	h := newMeetingCmdRig(t)
	old := meetingAgentResolver
	t.Cleanup(func() { meetingAgentResolver = old })
	var asked []string
	meetingAgentResolver = func(_ *WorkspaceActor, name string) (meeting.Agent, error) {
		asked = append(asked, name)
		return meeting.Agent{}, errors.New("agent " + name + " is not spawned")
	}
	cp, n := h.open(t, "new", "--agent", "alice", "--invite", "jane@example.test", "--invite", "guest")
	<-h.calls // room create
	id := cp.Meta["meeting.id"]
	if strings.Contains(n, "next:") {
		t.Fatalf("next hint despite follow-ups: %q", n)
	}
	var notices []string
	for range 5 { // the agent's result, then per invite "inviting…" and its link
		notices = append(notices, h.notice(t))
	}
	all := strings.Join(notices, "")
	if len(asked) != 1 || asked[0] != "alice" || !strings.Contains(all, "agent alice is not spawned") {
		t.Fatalf("agent follow-up %v %q", asked, all)
	}
	for _, want := range []string{
		id + ": inviting jane@example.test", id + ": inviting a guest",
		"invite for jane@example.test — one person", "https://rysh.test/join/room-new#invite=rysh_mi_s",
		"as a guest they are guest-3f9a2c1e7b4d (agents answer them with --allow host,guest-3f9a2c1e7b4d or anyone)",
		"rysh sends no email", "revoke: ##meeting uninvite " + id + " 3f9a2c1e-7b4d-4e0a-9c11-000000000abc",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("follow-up notices lack %q:\n%s", want, all)
		}
	}
	h.mu.Lock()
	body := h.bodies["POST /api/meeting-rooms/room-new/invites"]
	h.mu.Unlock()
	if body == nil {
		t.Fatal("no invite request")
	}
	// A failed follow-up does not undo the meeting or fail a later command.
	if _, ok := h.w.meeting.Lookup(id); !ok || h.w.takeRyshFailure() != nil {
		t.Fatal("follow-up failure leaked")
	}
}

func TestMeetingInviteCommands(t *testing.T) {
	h := newMeetingCmdRig(t)
	cp := h.join(t, "rysh:room1")
	<-h.calls
	id := cp.Meta["meeting.id"]

	h.run("invite", id, "jane@example.test", "--name", "Jane", "Doe", "--hours", "5")
	if n := h.notice(t); !strings.Contains(n, "https://rysh.test/join/room1#invite=") {
		t.Fatalf("invite %q", n)
	}
	if c := <-h.calls; c != "POST /api/meeting-rooms/room1/invites Bearer rysh_upstream_key" {
		t.Fatalf("invite call %q", c)
	}
	h.mu.Lock()
	body := h.bodies["POST /api/meeting-rooms/room1/invites"]
	h.mu.Unlock()
	if body["email"] != "jane@example.test" || body["name"] != "Jane Doe" || body["ttl_minutes"] != float64(300) {
		t.Fatalf("invite body %v", body)
	}

	h.run("invite", id, "link")
	if n := h.notice(t); !strings.Contains(n, "invite link for rysh accounts (up to 20 people") || !strings.Contains(n, "/meet/room1#invite=") {
		t.Fatalf("link %q", n)
	}
	<-h.calls

	h.run("invites", id)
	n := h.notice(t)
	<-h.calls
	if !strings.Contains(n, "2 invite(s)") || !strings.Contains(n, "personal  jane@example.test  used 0/1") || !strings.Contains(n, "shared    rysh accounts  used 3/20  revoked") {
		t.Fatalf("invites %q", n)
	}

	h.run("uninvite", id, "3f9a2c1e-7b4d-4e0a-9c11-000000000abc")
	if n := h.notice(t); !strings.Contains(n, "revoked; its guest, if in the room, was removed") {
		t.Fatalf("uninvite %q", n)
	}
	if c := <-h.calls; c != "DELETE /api/meeting-rooms/room1/invites/3f9a2c1e-7b4d-4e0a-9c11-000000000abc Bearer rysh_upstream_key" {
		t.Fatalf("uninvite call %q", c)
	}

	for _, args := range [][]string{
		{"invite"}, {"invite", id}, {"invite", id, "bob"}, {"invite", id, "link", "--name", "x"},
		{"invite", id, "guest", "--hours", "200"}, {"invite", "m-000000", "guest"}, {"invite", "nope", "guest"},
		{"invites"}, {"uninvite", id, "abc"}, {"uninvite", id},
	} {
		out := h.run(args...)
		if h.w.takeRyshFailure() == nil || strings.Contains(out, "inviting") {
			t.Errorf("%v accepted: %q", args, out)
		}
	}
	select {
	case c := <-h.calls:
		t.Fatalf("a refused command reached the server: %s", c)
	case <-time.After(50 * time.Millisecond):
	}
}
