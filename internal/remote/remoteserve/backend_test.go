// SPDX-License-Identifier: Apache-2.0

package remoteserve

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/remote"
)

type sent struct {
	subject string
	m       interface{}
}

// fakeSession records sends and answers requests from canned replies.
type fakeSession struct {
	mu      sync.Mutex
	sends   []sent
	replies map[string]func(m interface{}) interface{}
	subs    map[string]func(interface{})
	sendErr error
}

func newFakeSession() *fakeSession {
	return &fakeSession{replies: map[string]func(interface{}) interface{}{}, subs: map[string]func(interface{}){}}
}

func (f *fakeSession) RequestFromSubject(subject string, m interface{}, _ time.Duration) (interface{}, error) {
	f.mu.Lock()
	fn := f.replies[subject]
	f.mu.Unlock()
	if fn == nil {
		return nil, errNoResponders
	}
	return fn(m), nil
}

type errString string

func (e errString) Error() string { return string(e) }

const errNoResponders = errString("no responders")

func (f *fakeSession) SendToSubject(subject string, m interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sends = append(f.sends, sent{subject, m})
	return nil
}

func (f *fakeSession) SubscribeExact(subject string, fn func(interface{})) (func(), error) {
	if strings.ContainsAny(subject, "*>") {
		panic("wildcard subscription: " + subject)
	}
	f.mu.Lock()
	f.subs[subject] = fn
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		delete(f.subs, subject)
		f.mu.Unlock()
	}, nil
}

func (f *fakeSession) fire(subject string) {
	f.mu.Lock()
	fn := f.subs[subject]
	f.mu.Unlock()
	if fn != nil {
		fn(nil)
	}
}

func (f *fakeSession) lastSend(t *testing.T) sent {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sends) == 0 {
		t.Fatal("nothing sent")
	}
	return f.sends[len(f.sends)-1]
}

func snapshotWith(tabs ...domain.TabSnapshot) func(interface{}) interface{} {
	return func(interface{}) interface{} {
		return &msg.MsgWorkspaceSnapshotReply{Snapshot: domain.WorkspaceSnapshot{Tabs: tabs}}
	}
}

func tab(id string, panes ...domain.PaneSnapshot) domain.TabSnapshot {
	return domain.TabSnapshot{ID: id, Title: id, Lanes: []domain.LaneSnapshot{{ID: "l", PaneGroups: []domain.PaneGroupSnapshot{{ID: "g", Panes: panes}}}}}
}

func TestLayoutDropsMirrorTabsAndHeavyFields(t *testing.T) {
	msg.SetSessionPrefix("rsTest")
	f := newFakeSession()
	f.replies[msg.T("ws", "snapshot")] = snapshotWith(
		tab("t1", domain.PaneSnapshot{ID: "p1", Title: "claude", Program: "claude", RawMode: true,
			VTScreen: []string{"big"}, Output: "lots", ShellHistory: []string{"h"},
			Meta: map[string]string{domain.PaneMetaAttention: domain.AttentionNeedsYou, "supervisor.note": "private"}}),
		tab("mirror:share1", domain.PaneSnapshot{ID: "mirror:share1:x"}),
	)
	b, err := New(f)
	if err != nil {
		t.Fatal(err)
	}
	tabs, err := b.Layout()
	if err != nil {
		t.Fatal(err)
	}
	if len(tabs) != 1 || tabs[0].ID != "t1" {
		t.Fatalf("tabs = %+v", tabs)
	}
	p := tabs[0].Lanes[0].PaneGroups[0].Panes[0]
	if p.VTScreen != nil || p.Output != "" || p.ShellHistory != nil {
		t.Fatalf("heavy fields kept: %+v", p)
	}
	if !p.RawMode || p.Program != "claude" || p.Meta[domain.PaneMetaAttention] != domain.AttentionNeedsYou {
		t.Fatalf("needed fields dropped: %+v", p)
	}
	if _, ok := p.Meta["supervisor.note"]; ok {
		t.Fatal("non-attention meta must stay on the remote")
	}
}

func TestScreenInteractiveAndText(t *testing.T) {
	msg.SetSessionPrefix("rsTest")
	f := newFakeSession()
	f.replies[msg.T("pane", "p1", "snapshot")] = func(m interface{}) interface{} {
		if _, ok := m.(*msg.MsgGetPaneVT); ok {
			return &msg.MsgPaneVTReply{PaneID: "p1", Interactive: true, Screen: []string{"a", "b"}, CursorRow: 1, CursorCol: 1}
		}
		t.Fatal("interactive pane needs no full snapshot")
		return nil
	}
	f.replies[msg.T("pane", "p2", "snapshot")] = func(m interface{}) interface{} {
		if _, ok := m.(*msg.MsgGetPaneVT); ok {
			return &msg.MsgPaneVTReply{PaneID: "p2"}
		}
		if req, ok := m.(*msg.MsgGetPaneSnapshot); !ok || !req.NoHistories {
			t.Fatalf("text pane read must skip histories: %#v", m)
		}
		return &msg.MsgPaneSnapshotReply{Snapshot: domain.PaneSnapshot{ID: "p2", Output: strings.Repeat("x\n", 20000)}}
	}
	b, _ := New(f)
	s, err := b.Screen("p1")
	if err != nil || !s.Interactive || len(s.Rows) != 2 {
		t.Fatalf("p1 = %+v %v", s, err)
	}
	s, err = b.Screen("p2")
	if err != nil || s.Interactive || len(s.Text) > maxText || !strings.HasPrefix(s.Text, "x\n") {
		t.Fatalf("p2 text len=%d err=%v", len(s.Text), err)
	}
}

func TestApplyMapsCommandsToSessionMessages(t *testing.T) {
	msg.SetSessionPrefix("rsTest")
	f := newFakeSession()
	f.replies[msg.T("ws", "snapshot")] = snapshotWith(tab("t1", domain.PaneSnapshot{ID: "p1"}))
	b, _ := New(f)
	if _, err := b.Layout(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		cmd     remote.Command
		subject string
		check   func(interface{}) bool
	}{
		{remote.Command{Type: remote.CmdRawKeystroke, Pane: "p1", Payload: "aGk="}, msg.T("pane", "p1", "rawinput"),
			func(m interface{}) bool {
				r, ok := m.(*msg.MsgRawKeyInput)
				return ok && string(r.Data) == "hi" && r.PaneID == "p1"
			}},
		{remote.Command{Type: remote.CmdExecShell, Pane: "p1", Payload: "ls"}, msg.T("pane", "p1", "inbox"),
			func(m interface{}) bool { r, ok := m.(*msg.MsgPaneExecShell); return ok && r.Command == "ls" }},
		{remote.Command{Type: remote.CmdExecPrompt, Pane: "p1", Payload: "hi"}, msg.T("pane", "p1", "inbox"),
			func(m interface{}) bool { r, ok := m.(*msg.MsgPaneExecPrompt); return ok && r.Prompt == "hi" }},
		{remote.Command{Type: remote.CmdExecRysh, Pane: "p1", Payload: "pane info"}, msg.T("ws", "inbox"),
			func(m interface{}) bool {
				r, ok := m.(*msg.MsgExecRyshOnPane)
				return ok && r.Command == "pane info" && r.PaneID == "p1"
			}},
		{remote.Command{Type: remote.CmdTabOp, Pane: "p1", Payload: `{"op":"create_pane"}`}, msg.T("ws", "inbox"),
			func(m interface{}) bool {
				r, ok := m.(*msg.MsgMirrorTabOp)
				return ok && r.TabID == "t1" && r.Op == "create_pane"
			}},
		{remote.Command{Type: remote.CmdResize, Pane: "p1", Payload: `{"rows":30,"cols":100}`}, msg.T("pane", "p1", "inbox"),
			func(m interface{}) bool {
				r, ok := m.(*msg.MsgPaneResize)
				return ok && r.Rows == 30 && r.Cols == 100 && !r.Override && strings.HasPrefix(r.ClientID, "tui:")
			}},
	}
	for _, c := range cases {
		if err := b.Apply(c.cmd); err != nil {
			t.Fatalf("%s: %v", c.cmd.Type, err)
		}
		s := f.lastSend(t)
		if s.subject != c.subject || !c.check(s.m) {
			t.Fatalf("%s sent %s %#v", c.cmd.Type, s.subject, s.m)
		}
	}
	for _, bad := range []remote.Command{
		{Type: remote.CmdRawKeystroke, Pane: "p1", Payload: "%%%"},
		{Type: remote.CmdResize, Pane: "p1", Payload: `{"rows":0,"cols":1}`},
		{Type: remote.CmdTabOp, Pane: "p1", Payload: `nope`},
		{Type: "rm_rf", Pane: "p1"},
	} {
		if err := b.Apply(bad); err == nil {
			t.Fatalf("%+v accepted", bad)
		}
	}
	// Close releases the size claim it made.
	b.Close()
	s := f.lastSend(t)
	if r, ok := s.m.(*msg.MsgPaneReleaseSize); !ok || !strings.HasPrefix(r.ClientID, "tui:") {
		t.Fatalf("close sent %#v", s.m)
	}
}

func TestApplyReportsUndeliveredSend(t *testing.T) {
	msg.SetSessionPrefix("rsTest")
	f := newFakeSession()
	f.sendErr = errString("nats: connection closed")
	b, _ := New(f)
	if err := b.Apply(remote.Command{Type: remote.CmdExecShell, Pane: "p1", Payload: "ls"}); err == nil {
		t.Fatal("a failed publish must be reported, never swallowed")
	}
}

func TestWatchSubscribesExactRawDirtyAndEmits(t *testing.T) {
	msg.SetSessionPrefix("rsTest")
	f := newFakeSession()
	b, _ := New(f)
	b.Watch([]string{"p1", "p2"})
	f.fire(msg.T("pane", "p2", "rawDirty"))
	select {
	case ev := <-b.Events():
		if ev.Pane != "p2" {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	b.Watch([]string{"p1"})
	f.mu.Lock()
	_, still := f.subs[msg.T("pane", "p2", "rawDirty")]
	f.mu.Unlock()
	if still {
		t.Fatal("unwatched pane still subscribed")
	}
	f.fire(msg.T("ws", "layoutDirty"))
	select {
	case ev := <-b.Events():
		if !ev.Layout {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no layout event")
	}
}

func TestLayoutReportsActivityAndPaneType(t *testing.T) {
	msg.SetSessionPrefix("rsTest")
	f := newFakeSession()
	f.replies[msg.T("ws", "snapshot")] = snapshotWith(tab("t1",
		domain.PaneSnapshot{ID: "busy", Program: "claude", PaneType: "agent"},
		domain.PaneSnapshot{ID: "quiet", Program: "claude"},
		domain.PaneSnapshot{ID: "shell"},
	))
	b, _ := New(f)
	b.Watch([]string{"busy", "quiet", "shell"})
	f.fire(msg.T("pane", "busy", "rawDirty")) // output just now
	tabs, err := b.Layout()
	if err != nil {
		t.Fatal(err)
	}
	ps := tabs[0].Lanes[0].PaneGroups[0].Panes
	if ps[0].Meta[domain.PaneMetaActivity] != domain.PaneActivityWorking || ps[0].PaneType != "agent" {
		t.Fatalf("busy = %+v", ps[0])
	}
	if ps[1].Meta[domain.PaneMetaActivity] != domain.PaneActivityIdle {
		t.Fatalf("quiet = %+v", ps[1])
	}
	if ps[2].Meta[domain.PaneMetaActivity] != domain.PaneActivityIdle {
		t.Fatalf("shell = %+v", ps[2])
	}
}

func TestTextGrowthCountsAsWorking(t *testing.T) {
	msg.SetSessionPrefix("rsTest")
	f := newFakeSession()
	out := "tick1\n"
	f.replies[msg.T("ws", "snapshot")] = snapshotWith(tab("t1", domain.PaneSnapshot{ID: "p"}))
	f.replies[msg.T("pane", "p", "snapshot")] = func(m interface{}) interface{} {
		if _, ok := m.(*msg.MsgGetPaneVT); ok {
			return &msg.MsgPaneVTReply{PaneID: "p"}
		}
		return &msg.MsgPaneSnapshotReply{Snapshot: domain.PaneSnapshot{ID: "p", Output: out}}
	}
	b, _ := New(f)
	b.Watch([]string{"p"})
	_, _ = b.Screen("p")
	tabs, _ := b.Layout()
	if a := tabs[0].Lanes[0].PaneGroups[0].Panes[0].Meta[domain.PaneMetaActivity]; a != domain.PaneActivityIdle {
		t.Fatalf("first read = %q", a)
	}
	out += "tick2\n"
	_, _ = b.Screen("p")
	tabs, _ = b.Layout()
	if a := tabs[0].Lanes[0].PaneGroups[0].Panes[0].Meta[domain.PaneMetaActivity]; a != domain.PaneActivityWorking {
		t.Fatalf("after growth = %q", a)
	}
}
