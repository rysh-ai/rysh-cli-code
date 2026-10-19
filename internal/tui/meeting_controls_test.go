// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// E17 (h), MTG-6: the design-040 lifecycle controls the TUI lacked — pending,
// approve/reject, speak, pause/resume, cancel — on a meeting pane, sent with
// the SAME daemon command the CLI uses (MsgMeetingCommand to ws.inbox), each
// addressed to the pane's own meeting id in the daemon's registry (E30 PM-1),
// so two meetings never cross. list/status/leave are PM-1/PM-4's and are not
// reimplemented here. Driven against fakes: no meeting, no Recall session.

func mtgPaneFor(paneID, meetingID string) domain.PaneSnapshot {
	return domain.PaneSnapshot{ID: paneID, PaneType: domain.PaneTypeMeeting, Meta: map[string]string{
		"meeting.id": meetingID, "meeting.kind": "web", "meeting.platform": "google_meet",
	}}
}

var mtgPane = mtgPaneFor("mp", "m-3f9a2c")

func meetingModel() Model {
	m := paneModel(mtgPane)
	m.snapshot.ActiveTabID = "t1" // paneModel leaves it unset; focus needs it
	m.meetingControls = map[string]*meetingCtl{}
	return m
}

func TestMeetingCommandFor(t *testing.T) {
	ctl := &meetingCtl{}
	if c, ok := meetingCommandFor(mtgPane, "pending", ctl); !ok || c.Action != "pending" || c.ID != "m-3f9a2c" {
		t.Fatalf("pending command = %+v ok=%v, want pending for the pane's meeting m-3f9a2c", c, ok)
	}
	if _, ok := meetingCommandFor(mtgPane, "approve", ctl); ok {
		t.Fatal("approve with no pending turn produced a command")
	}
	ctl.reply = &meeting.Reply{State: "awaiting_approval", TurnID: "t-1"}
	if c, ok := meetingCommandFor(mtgPane, "approve", ctl); !ok || c.TurnID != "t-1" || c.ID != "m-3f9a2c" {
		t.Fatalf("approve command = %+v ok=%v, want turn t-1 of m-3f9a2c", c, ok)
	}
	// Without a registry id the panel sends nothing: the daemon's "" default
	// means "the only meeting", which is exactly the cross-talk to avoid.
	if _, ok := meetingCommandFor(domain.PaneSnapshot{ID: "x", PaneType: domain.PaneTypeMeeting}, "pending", ctl); ok {
		t.Fatal("a meeting pane with no meeting id produced a command")
	}
}

func TestMeetingPaneKeys(t *testing.T) {
	m := meetingModel()

	handled, cmd := m.updateMeetingPaneInput(key("g"))
	ctl := m.meetingControls["mp"]
	if !handled || cmd == nil || ctl == nil || ctl.inflight != "pending" {
		t.Fatalf("g: handled=%v cmd=%v, want a pending refresh", handled, cmd != nil)
	}

	ctl.inflight = ""
	ctl.reply = &meeting.Reply{State: "listening", Phase: "listening"}
	if handled, cmd := m.updateMeetingPaneInput(key("a")); !handled || cmd != nil || !strings.Contains(ctl.note, "no turn") {
		t.Fatalf("a with nothing pending: handled=%v cmd=%v note=%q", handled, cmd != nil, ctl.note)
	}

	for _, tc := range []struct{ state, k, want string }{
		{"awaiting_approval", "a", "approve"},
		{"awaiting_approval", "x", "reject"},
		{"awaiting_speech", "s", "speak"},
		{"awaiting_speech", "x", "reject"},
		{"listening", "p", "pause"},
		{"paused", "p", "resume"},
		{"executing", "c", "cancel"},
	} {
		ctl.inflight = ""
		ctl.reply = &meeting.Reply{State: tc.state, TurnID: "t-9"}
		handled, cmd := m.updateMeetingPaneInput(key(tc.k))
		if !handled || cmd == nil || ctl.inflight != tc.want {
			t.Errorf("state %s key %q: handled=%v cmd=%v inflight=%q, want %s", tc.state, tc.k, handled, cmd != nil, ctl.inflight, tc.want)
		}
	}

	// Leave belongs to ##meeting leave (PM-4); the panel does not claim it.
	if handled, _ := m.updateMeetingPaneInput(key("L")); handled {
		t.Fatal("L was claimed: leave is not this panel's control")
	}
	if handled, _ := m.updateMeetingPaneInput(key("enter")); !handled {
		t.Fatal("enter on a meeting pane was not swallowed (there is no shell)")
	}
	if handled, _ := m.updateMeetingPaneInput(key("esc")); handled {
		t.Fatal("esc was claimed by the meeting pane")
	}
}

func TestMeetingKeysOnlyOnMeetingPanes(t *testing.T) {
	m := paneModel(domain.PaneSnapshot{ID: "sh"})
	m.snapshot.ActiveTabID = "t1"
	m.meetingControls = map[string]*meetingCtl{}
	if handled, _ := m.updateMeetingPaneInput(key("a")); handled {
		t.Fatal("a normal pane's keys were claimed")
	}
}

func TestMeetingPendingReplyRendersControls(t *testing.T) {
	m := meetingModel()
	m.applyMeetingReply(meetingReplyMsg{paneID: "mp", action: "pending", reply: meeting.Reply{
		ID: "m-3f9a2c", State: "awaiting_approval", Phase: "awaiting_approval", Mode: "approve",
		TurnID: "t-1", Speaker: "Ana", Agent: "scribe", Request: "summarise the sprint",
	}})
	out := ansi.Strip(m.buildMeetingPanelWithControls(mtgPane, 80, 24))
	for _, want := range []string{
		"meeting m-3f9a2c", // E30's placeholder is still drawn, unmodified
		"awaiting_approval", "Ana", "scribe", "t-1", "summarise the sprint",
		"a approve", "x reject",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("panel lacks %q:\n%s", want, out)
		}
	}
	if n := strings.Count(out, "\n"); n != 23 {
		t.Errorf("panel has %d lines, want the pane height 24", n+1)
	}

	cmd := m.applyMeetingReply(meetingReplyMsg{paneID: "mp", action: "pause", reply: meeting.Reply{State: "paused"}})
	if cmd == nil || !strings.Contains(m.meetingControls["mp"].note, "pause") {
		t.Fatalf("pause reply: cmd=%v note=%q, want a note and a pending refresh", cmd != nil, m.meetingControls["mp"].note)
	}
	m.applyMeetingReply(meetingReplyMsg{paneID: "mp", action: "approve", err: "turn t-1 expired"})
	if !strings.Contains(m.meetingControls["mp"].note, "turn t-1 expired") {
		t.Fatalf("an action error was not shown: %q", m.meetingControls["mp"].note)
	}
}

func TestMeetingPollStartsOncePerFocus(t *testing.T) {
	m := meetingModel()
	if cmd := m.meetingPollStart(); cmd == nil {
		t.Fatal("focusing a meeting pane did not start polling")
	}
	if cmd := m.meetingPollStart(); cmd != nil {
		t.Fatal("a second poll chain started while one is running")
	}
	// Focus moved to another meeting pane: the chain carries on there.
	other := mtgPaneFor("mp2", "m-bbbbbb")
	m.snapshot.ActivePaneID = other.ID
	m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes = []domain.PaneSnapshot{mtgPane, other}
	m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].ActivePaneID = other.ID
	if next, cmd := m.Update(meetingPollTick{paneID: "mp"}); cmd == nil || !next.(Model).meetingPolling {
		t.Fatal("poll chain stopped when focus moved to another meeting pane")
	}
	// Focus on a normal pane: the next tick ends the chain.
	m.snapshot.ActivePaneID = "sh"
	m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes = []domain.PaneSnapshot{mtgPane, {ID: "sh"}}
	m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].ActivePaneID = "sh"
	next, cmd := m.Update(meetingPollTick{paneID: "mp2"})
	if cmd != nil || next.(Model).meetingPolling {
		t.Fatalf("tick with no meeting pane focused kept polling (cmd=%v)", cmd != nil)
	}
}

// --- two meetings, a fake daemon ------------------------------------------

// fakeMeetingDaemon answers MsgMeetingCommand on ws.inbox like the
// workspace does, recording every command it receives.
type fakeMeetingDaemon struct {
	mu   sync.Mutex
	cmds []meeting.Command
}

func (f *fakeMeetingDaemon) received() []meeting.Command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]meeting.Command(nil), f.cmds...)
}

func startFakeMeetingDaemon(t *testing.T) (*msgpkg.NATSPublisher, *fakeMeetingDaemon) {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	codecs := msgpkg.DefaultCodecRegistry()
	f := &fakeMeetingDaemon{}
	_, err = nc.Subscribe(msgpkg.T("ws", "inbox"), func(m *nats.Msg) {
		var env msgpkg.NATSEnvelope
		_ = json.Unmarshal(m.Data, &env)
		d, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		mc, ok := d.(*msgpkg.MsgMeetingCommand)
		if !ok {
			return
		}
		f.mu.Lock()
		f.cmds = append(f.cmds, mc.Command)
		f.mu.Unlock()
		re := &msgpkg.RequestEnvelope{ReplyTo: env.ReplyTo, NC: nc, Codecs: codecs}
		_ = re.Reply(&msgpkg.MsgMeetingReply{Reply: meeting.Reply{ID: mc.Command.ID, State: "ok-" + mc.Command.Action}})
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	return msgpkg.NewNATSPublisher(nc, codecs), f
}

// TestMeetingControlsAddressTheirOwnMeeting: two meeting panes, two meetings
// in the registry. Approve, speak and pause on meeting B's pane reach the
// daemon carrying B's id — never A's, never the "" only-meeting default — and
// the replies land on B's panel only.
func TestMeetingControlsAddressTheirOwnMeeting(t *testing.T) {
	pub, daemon := startFakeMeetingDaemon(t)
	a, b := mtgPaneFor("pa", "m-aaaaaa"), mtgPaneFor("pb", "m-bbbbbb")
	m := paneModel(b)
	m.snapshot.ActiveTabID = "t1"
	m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes = []domain.PaneSnapshot{a, b}
	m.pub = pub
	m.workspaceInbox = msgpkg.T("ws", "inbox")
	m.meetingControls = map[string]*meetingCtl{
		"pa": {reply: &meeting.Reply{ID: "m-aaaaaa", State: "awaiting_approval", TurnID: "ta"}, note: "A untouched"},
	}

	run := func(k string, state string) meetingReplyMsg {
		t.Helper()
		ctl := m.meetingCtlFor("pb")
		ctl.reply = &meeting.Reply{ID: "m-bbbbbb", State: state, TurnID: "tb"}
		_, cmd := m.updateMeetingPaneInput(key(k))
		if cmd == nil {
			t.Fatalf("key %q on B sent nothing", k)
		}
		r, ok := cmd().(meetingReplyMsg)
		if !ok {
			t.Fatalf("key %q: command returned %T, want a meetingReplyMsg", k, cmd())
		}
		m.applyMeetingReply(r)
		return r
	}
	run("a", "awaiting_approval")
	run("s", "awaiting_speech")
	run("p", "listening")

	got := daemon.received()
	if len(got) != 3 {
		t.Fatalf("daemon received %d commands %+v, want 3", len(got), got)
	}
	for i, want := range []string{"approve", "speak", "pause"} {
		if got[i].Action != want || got[i].ID != "m-bbbbbb" {
			t.Errorf("command %d = %s for %q, want %s for meeting B m-bbbbbb", i, got[i].Action, got[i].ID, want)
		}
	}
	if got[0].TurnID != "tb" || got[1].TurnID != "tb" {
		t.Errorf("approve/speak carried turns %q/%q, want B's turn tb", got[0].TurnID, got[1].TurnID)
	}
	if ctlA := m.meetingControls["pa"]; ctlA.note != "A untouched" || ctlA.reply.TurnID != "ta" {
		t.Fatalf("meeting A's panel changed: %+v", ctlA)
	}
}
