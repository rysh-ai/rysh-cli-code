// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/remote"
)

// ---------------------------------------------------------------------------
// A fake remote: remote.Serve over an in-process pipe, on a scripted backend.
// ---------------------------------------------------------------------------

type rhBackend struct {
	mu      sync.Mutex
	tabs    []domain.TabSnapshot
	screens map[string]remote.Screen
	applied []remote.Command
	events  chan remote.Event
}

func newRHBackend(tabs ...domain.TabSnapshot) *rhBackend {
	return &rhBackend{tabs: tabs, screens: map[string]remote.Screen{}, events: make(chan remote.Event, 64)}
}

func (b *rhBackend) Layout() ([]domain.TabSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]domain.TabSnapshot(nil), b.tabs...), nil
}
func (b *rhBackend) Screen(id string) (remote.Screen, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.screens[id], nil
}
func (b *rhBackend) Apply(c remote.Command) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.applied = append(b.applied, c)
	return nil
}
func (b *rhBackend) Watch([]string)              {}
func (b *rhBackend) Events() <-chan remote.Event { return b.events }
func (b *rhBackend) Close()                      {}
func (b *rhBackend) setScreen(id string, s remote.Screen) {
	b.mu.Lock()
	b.screens[id] = s
	b.mu.Unlock()
	b.events <- remote.Event{Pane: id}
}
func (b *rhBackend) appliedCmds() []remote.Command {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]remote.Command(nil), b.applied...)
}

// pipeConn is the local end of an in-process "ssh".
type pipeConn struct {
	r      *io.PipeReader
	w      *io.PipeWriter
	exited chan struct{}
	once   sync.Once
	stderr string
}

func (p *pipeConn) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeConn) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeConn) Close() error {
	p.once.Do(func() { _ = p.r.Close(); _ = p.w.Close() })
	return nil
}
func (p *pipeConn) Stderr() string          { return p.stderr }
func (p *pipeConn) Exited() <-chan struct{} { return p.exited }

// fakeRemote dials in-process remote-serves on one backend and lets a test
// drop the live connection or refuse new ones.
type fakeRemote struct {
	mu     sync.Mutex
	b      *rhBackend
	proto  int
	refuse string // non-empty: dial "fails" with this ssh stderr
	live   *pipeConn
	dials  int
}

func (f *fakeRemote) dial(remote.SSHOptions) (remoteConn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials++
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	pc := &pipeConn{r: s2cR, w: c2sW, exited: make(chan struct{})}
	if f.refuse != "" {
		pc.stderr = f.refuse
		_ = s2cW.Close()
		_ = c2sR.Close()
		close(pc.exited)
		return pc, nil
	}
	f.live = pc
	go func() {
		_ = remote.Serve(context.Background(), c2sR, s2cW, f.b, remote.ServeOptions{
			Proto: f.proto, Session: "far", PollInterval: time.Hour, ScreenThrottle: time.Millisecond,
		})
		_ = s2cW.Close()
		_ = c2sR.Close()
		close(pc.exited)
	}()
	return pc, nil
}

func (f *fakeRemote) drop() {
	f.mu.Lock()
	pc := f.live
	f.mu.Unlock()
	if pc != nil {
		_ = pc.Close()
	}
}

func (f *fakeRemote) dialCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dials
}

// probe stands in for the WorkspaceActor and records what the host actor
// tells it.
type probe struct {
	ch chan interface{}
}

func (p *probe) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *MsgRemoteHostLayout, *MsgRemoteHostStatus, *MsgMirrorPaneVTUpdate:
		p.ch <- m
	}
}

type rhRig struct {
	t      *testing.T
	sys    *actor.ActorSystem
	nc     *nats.Conn
	pub    *msg.NATSPublisher
	probe  *probe
	host   *actor.PID
	remote *fakeRemote
}

func newRHRig(t *testing.T, b *rhBackend, proto int) *rhRig {
	t.Helper()
	msg.SetSessionPrefix("rhtest")
	return newRHRigOn(t, b, proto, newBoardNATS(t))
}

func newRHRigOn(t *testing.T, b *rhBackend, proto int, nc *nats.Conn) *rhRig {
	t.Helper()
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	sys := actor.NewActorSystem()
	t.Cleanup(sys.Shutdown)
	pr := &probe{ch: make(chan interface{}, 256)}
	ppid := sys.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return pr }))
	fr := &fakeRemote{b: b, proto: proto}
	ha := NewRemoteHostActor("devbox", remote.SSHOptions{Host: "devbox", Session: "far"}, pub, nc, ppid)
	ha.dial = fr.dial
	ha.backoffMin, ha.backoffMax = 20*time.Millisecond, 80*time.Millisecond
	ha.ackTimeout = 2 * time.Second
	hpid := sys.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return ha }))
	t.Cleanup(func() { sys.Root.Stop(hpid) })
	return &rhRig{t: t, sys: sys, nc: nc, pub: pub, probe: pr, host: hpid, remote: fr}
}

// next returns the next probe message satisfying ok.
func (r *rhRig) next(what string, ok func(interface{}) bool) interface{} {
	r.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-r.probe.ch:
			if ok(m) {
				return m
			}
		case <-deadline:
			r.t.Fatalf("timeout waiting for %s", what)
		}
	}
}

func (r *rhRig) layout() *MsgRemoteHostLayout {
	r.t.Helper()
	return r.next("layout", func(m interface{}) bool { _, ok := m.(*MsgRemoteHostLayout); return ok }).(*MsgRemoteHostLayout)
}

func (r *rhRig) status(state string) *MsgRemoteHostStatus {
	r.t.Helper()
	return r.next("status "+state, func(m interface{}) bool {
		s, ok := m.(*MsgRemoteHostStatus)
		return ok && s.State == state
	}).(*MsgRemoteHostStatus)
}

func rhTab(id string, panes ...string) domain.TabSnapshot {
	g := domain.PaneGroupSnapshot{ID: "g-" + id, RowFlex: 10, ActivePaneID: panes[0]}
	for _, p := range panes {
		g.Panes = append(g.Panes, domain.PaneSnapshot{ID: p, Title: p, Mode: "shell"})
	}
	return domain.TabSnapshot{ID: id, Title: "remote " + id, ActivePaneID: panes[0],
		Lanes: []domain.LaneSnapshot{{ID: "l-" + id, Flex: 10, ActivePaneID: panes[0], PaneGroups: []domain.PaneGroupSnapshot{g}}}}
}

// ---------------------------------------------------------------------------

func TestRemoteHostShareIDsAreStableAndColonFree(t *testing.T) {
	a := remoteHostShareID("devbox", "tab-1")
	if a != remoteHostShareID("devbox", "tab-1") {
		t.Fatal("share id not stable across reconnects")
	}
	if strings.Contains(a, ":") {
		t.Fatalf("share id %q breaks mirrorPaneID parsing", a)
	}
	if a == remoteHostShareID("otherbox", "tab-1") {
		t.Fatal("two hosts' tabs collide")
	}
	if sh, src := parseMirrorPaneID(mirrorPaneID(a, "p1")); sh != a || src != "p1" {
		t.Fatalf("round trip = %q %q", sh, src)
	}
}

func TestRemoteHostLayoutAndTextReachWorkspace(t *testing.T) {
	b := newRHBackend(rhTab("t1", "p1", "p2"), rhTab("t2", "p3"))
	b.screens["p1"] = remote.Screen{Text: "$ uname\nDarwin\n"}
	r := newRHRig(t, b, 0)
	r.status(remoteStateConnected)
	l := r.layout()
	if l.Host != "devbox" || len(l.Tabs) != 2 {
		t.Fatalf("layout = %+v", l)
	}
	if l.Tabs[0].ShareID != remoteHostShareID("devbox", "t1") || l.Tabs[0].Title != "remote t1" {
		t.Fatalf("tab 0 = %+v", l.Tabs[0])
	}
	// The text pane's output arrives in a later layout push.
	for {
		l = r.layout()
		p := domain.FindPaneInTab(&l.Tabs[0].Tab, "p1")
		if p != nil && strings.Contains(p.Output, "Darwin") {
			if !strings.Contains(l.Tabs[0].Deltas["p1"], "Darwin") {
				t.Fatalf("delta for republish missing: %+v", l.Tabs[0].Deltas)
			}
			break
		}
	}
}

func TestRemoteHostInteractiveScreenStreamsVTFrames(t *testing.T) {
	b := newRHBackend(rhTab("t1", "p1"))
	r := newRHRig(t, b, 0)
	share := remoteHostShareID("devbox", "t1")
	frames := make(chan *msg.MsgMirrorPaneVTFrame, 16)
	sub, err := r.nc.Subscribe(msg.T("pane", mirrorPaneID(share, "p1"), "vtframe"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		var f msg.MsgMirrorPaneVTFrame
		if json.Unmarshal(env.Payload, &f) == nil {
			frames <- &f
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()
	_ = r.nc.Flush()
	r.status(remoteStateConnected)
	r.layout()
	b.setScreen("p1", remote.Screen{Interactive: true, Rows: []string{"claude>", ""}, CursorRow: 0, CursorCol: 7})
	vt := r.next("interactive enter", func(m interface{}) bool {
		u, ok := m.(*MsgMirrorPaneVTUpdate)
		return ok && u.SourcePaneID == "p1" && u.Interactive
	}).(*MsgMirrorPaneVTUpdate)
	if vt.ShareID != share {
		t.Fatalf("vt update share = %q", vt.ShareID)
	}
	select {
	case f := <-frames:
		if !f.Interactive || len(f.Full) != 2 || f.Full[0] != "claude>" || f.CursorCol != 7 {
			t.Fatalf("frame = %+v", f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no vtframe published")
	}
}

func TestRemoteHostRelaysInputToRemotePane(t *testing.T) {
	b := newRHBackend(rhTab("t1", "p1"))
	r := newRHRig(t, b, 0)
	r.status(remoteStateConnected)
	r.layout()
	r.sys.Root.Send(r.host, &msg.MsgUpstreamSendCommand{CommandType: "raw_keystroke", Payload: "bHM=", TargetPaneID: "p1"})
	r.sys.Root.Send(r.host, &msg.MsgUpstreamSendCommand{CommandType: "exec_shell", Payload: "pwd", TargetPaneID: "p1"})
	deadline := time.Now().Add(5 * time.Second)
	for len(b.appliedCmds()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("applied = %+v", b.appliedCmds())
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := b.appliedCmds()
	if got[0] != (remote.Command{Type: "raw_keystroke", Pane: "p1", Payload: "bHM="}) ||
		got[1] != (remote.Command{Type: "exec_shell", Pane: "p1", Payload: "pwd"}) {
		t.Fatalf("applied = %+v", got)
	}
}

func TestRemoteHostResizeFromLocalTUIReachesRemote(t *testing.T) {
	b := newRHBackend(rhTab("t1", "p1"))
	r := newRHRig(t, b, 0)
	r.status(remoteStateConnected)
	r.layout()
	mid := mirrorPaneID(remoteHostShareID("devbox", "t1"), "p1")
	deadline := time.Now().Add(5 * time.Second)
	for {
		// The TUI claims a size on the mirror pane's inbox, as for any pane.
		_ = r.pub.Send(msg.T("pane", mid, "inbox"), &msg.MsgPaneResize{Rows: 40, Cols: 120, ClientID: "tui:1"})
		time.Sleep(50 * time.Millisecond)
		for _, c := range b.appliedCmds() {
			if c.Type == remote.CmdResize && c.Pane == "p1" && c.Payload == `{"rows":40,"cols":120}` {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("resize never reached the remote: %+v", b.appliedCmds())
		}
	}
}

func TestRemoteHostFailsClosedWhileDisconnected(t *testing.T) {
	b := newRHBackend(rhTab("t1", "p1"))
	r := newRHRig(t, b, 0)
	r.status(remoteStateConnected)
	r.layout()
	r.remote.mu.Lock()
	r.remote.refuse = "ssh: connect to host devbox port 22: Connection refused"
	r.remote.mu.Unlock()
	r.remote.drop()
	r.status(remoteStateDisconnected)
	r.sys.Root.Send(r.host, &msg.MsgUpstreamSendCommand{CommandType: "raw_keystroke", Payload: "eA==", TargetPaneID: "p1"})
	st := r.next("input error", func(m interface{}) bool {
		s, ok := m.(*MsgRemoteHostStatus)
		return ok && strings.Contains(s.Note, "not delivered")
	}).(*MsgRemoteHostStatus)
	if st.Host != "devbox" {
		t.Fatalf("status = %+v", st)
	}
	for _, c := range b.appliedCmds() {
		if c.Payload == "eA==" {
			t.Fatal("input reached the remote while disconnected")
		}
	}
}

func TestRemoteHostReconnectsWithBackoffAndReattaches(t *testing.T) {
	b := newRHBackend(rhTab("t1", "p1"))
	r := newRHRig(t, b, 0)
	r.status(remoteStateConnected)
	first := r.layout()
	r.remote.drop()
	r.status(remoteStateDisconnected)
	r.status(remoteStateConnected)
	again := r.layout()
	if again.Tabs[0].ShareID != first.Tabs[0].ShareID {
		t.Fatalf("reattached to other tabs: %q vs %q", again.Tabs[0].ShareID, first.Tabs[0].ShareID)
	}
	if n := r.remote.dialCount(); n < 2 {
		t.Fatalf("dials = %d", n)
	}
	// Input works again after the reattach.
	r.sys.Root.Send(r.host, &msg.MsgUpstreamSendCommand{CommandType: "exec_shell", Payload: "again", TargetPaneID: "p1"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, c := range b.appliedCmds() {
			if c.Payload == "again" {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("input after reattach never applied")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRemoteHostHostKeyFailureIsReported(t *testing.T) {
	b := newRHBackend(rhTab("t1", "p1"))
	r := newRHRig(t, b, 0)
	r.status(remoteStateConnected)
	r.remote.mu.Lock()
	r.remote.refuse = "Host key verification failed."
	r.remote.mu.Unlock()
	r.remote.drop()
	st := r.next("host key note", func(m interface{}) bool {
		s, ok := m.(*MsgRemoteHostStatus)
		return ok && s.State == remoteStateDisconnected && strings.Contains(s.Note, "host key")
	}).(*MsgRemoteHostStatus)
	_ = st
}

func TestRemoteHostProtoMismatchStopsRetrying(t *testing.T) {
	b := newRHBackend(rhTab("t1", "p1"))
	r := newRHRig(t, b, remote.ProtoVersion+7)
	st := r.status(remoteStateError)
	if !strings.Contains(st.Note, "protocol") {
		t.Fatalf("note = %q", st.Note)
	}
	time.Sleep(300 * time.Millisecond) // several backoff periods
	if n := r.remote.dialCount(); n != 1 {
		t.Fatalf("a protocol mismatch was retried: %d dials", n)
	}
}

func TestRemoteHostTagsPanesAndRaisesAttention(t *testing.T) {
	needs := domain.PaneSnapshot{ID: "p1", Title: "claude", Meta: map[string]string{
		domain.PaneMetaAttention: domain.AttentionNeedsYou, domain.PaneMetaAttentionReason: domain.AttentionReasonPrompt}}
	tab := domain.TabSnapshot{ID: "t1", Title: "agents", ActivePaneID: "p1", Lanes: []domain.LaneSnapshot{{ID: "l",
		PaneGroups: []domain.PaneGroupSnapshot{{ID: "g", Panes: []domain.PaneSnapshot{needs}}}}}}
	b := newRHBackend(tab)
	msg.SetSessionPrefix("rhtest")
	nc := newBoardNATS(t)
	events := make(chan string, 8)
	sub, err := nc.Subscribe(msg.T("ws", "attention"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) == nil {
			var ev struct {
				PaneID string `json:"pane_id"`
			}
			_ = json.Unmarshal(env.Payload, &ev)
			events <- env.TypeTag + " " + ev.PaneID
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()
	_ = nc.Flush()
	r := newRHRigOn(t, b, 0, nc)
	r.status(remoteStateConnected)
	l := r.layout()
	p := domain.FindPaneInTab(&l.Tabs[0].Tab, "p1")
	if p == nil || p.Meta[domain.PaneMetaRemoteHost] != "devbox" || p.Meta[domain.PaneMetaAttention] != domain.AttentionNeedsYou {
		t.Fatalf("pane = %+v", p)
	}
	mid := mirrorPaneID(remoteHostShareID("devbox", "t1"), "p1")
	select {
	case ev := <-events:
		if ev != msg.TagAttentionEvent+" "+mid {
			t.Fatalf("event = %q", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("needs-you on the remote raised no local attention event")
	}
	// Cleared remotely: the local attention is acked.
	b.mu.Lock()
	b.tabs[0].Lanes[0].PaneGroups[0].Panes[0].Meta = nil
	b.mu.Unlock()
	b.events <- remote.Event{Layout: true}
	select {
	case ev := <-events:
		if ev != msg.TagAttentionAck+" "+mid {
			t.Fatalf("event = %q", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleared needs-you not acked locally")
	}
}
