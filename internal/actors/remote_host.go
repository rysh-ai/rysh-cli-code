// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/remote"
)

// ---------------------------------------------------------------------------
// Remote hosts (feature #14, design 070)
//
// `##remote add <ssh-alias>` spawns one RemoteHostActor. It runs
// `ssh <alias> rysh_local remote-serve` through the user's own ssh_config
// (internal/remote), and turns each tab of the remote session into a local
// CONTROL-mode mirror tab: the workspace renders it and routes keystrokes,
// lines, structural ops and maximize to this actor exactly as it does for a
// control-mode shared tab (MsgUpstreamSendCommand), so no second renderer and
// no second input path exist. The local rysh draws the panes; nothing nests.
//
// Fail closed: input is relayed only over a live, handshaken connection and
// every command is acked by the remote. Anything else — not connected, queue
// full, no ack — is reported to the workspace as "not delivered" and shown on
// the tab; it is never routed to a local pane.
// ---------------------------------------------------------------------------

// Host states reported to the workspace.
const (
	remoteStateConnecting   = "connecting"
	remoteStateConnected    = "connected"
	remoteStateDisconnected = "disconnected"
	remoteStateError        = "error" // will not retry (protocol mismatch)
)

// remoteHostSharePrefix marks the synthetic share ids of remote-host tabs.
const remoteHostSharePrefix = "rh."

// remoteHostShareID is the stable local share id of one remote tab: the same
// after a reconnect, so the reattached tab is the same local tab. It never
// contains ':' (mirrorPaneID splits on it); host aliases and rysh tab ids do
// not contain one either.
func remoteHostShareID(host, remoteTabID string) string {
	return remoteHostSharePrefix + host + "." + strings.ReplaceAll(remoteTabID, ":", "_")
}

// remoteConn is a dialed transport: the ssh subprocess in production.
type remoteConn interface {
	io.ReadWriteCloser
	Stderr() string
	Exited() <-chan struct{}
}

type remoteDialFunc func(remote.SSHOptions) (remoteConn, error)

func dialRemoteSSH(o remote.SSHOptions) (remoteConn, error) { return remote.DialSSH(o) }

// RemoteHostTab is one remote tab as the workspace should show it.
type RemoteHostTab struct {
	ShareID string
	Title   string
	Tab     domain.TabSnapshot // remote pane ids; Output carries text panes' tails
	Deltas  map[string]string  // source pane id -> newly appended text (for republish)
}

// MsgRemoteHostLayout carries a host's full current tab list to the workspace.
type MsgRemoteHostLayout struct {
	Host string
	Tabs []RemoteHostTab
}

// MsgRemoteHostStatus reports a host's connection state, or (Note only, State
// unchanged) a problem such as input that was not delivered.
type MsgRemoteHostStatus struct {
	Host  string
	State string
	Note  string
	// SSHPID is the live ssh process (connected state only), shown by
	// ##remote list so a user can see — and if needed end — the transport.
	SSHPID int
}

// internal mailbox messages
type (
	remoteHostConnected struct {
		gen    int
		client *remote.Client
		conn   remoteConn
		hello  remote.Hello
	}
	remoteHostFrame struct {
		gen int
		f   remote.Frame
	}
	remoteHostDown struct {
		gen    int
		err    error
		stderr string
	}
	remoteHostRetry     struct{ gen int }
	remoteHostReconnect struct{}
	remoteHostTick      struct{}
	remoteHostSendFail  struct {
		pane string
		err  error
	}
	remoteHostResize struct {
		mirrorPaneID string
		rows, cols   int
	}
)

// remotePaneVT is the per-pane push state for an interactive remote pane.
type remotePaneVT struct {
	interactive  bool
	rows         []string
	cursorRow    int
	cursorCol    int
	seq          uint64
	lastKeyframe time.Time
}

// RemoteHostActor owns one ssh connection to one remote rysh session.
type RemoteHostActor struct {
	host string
	opts remote.SSHOptions

	pub          *msg.NATSPublisher
	nc           *nats.Conn
	workspacePID *actor.PID
	system       *actor.ActorSystem
	self         *actor.PID

	dial                   remoteDialFunc
	ackTimeout             time.Duration
	backoffMin, backoffMax time.Duration
	backoff                time.Duration

	gen      int
	state    string
	client   *remote.Client
	conn     remoteConn
	sendQ    chan remote.Command
	connStop chan struct{}
	stopped  bool
	tickStop chan struct{}

	layout    []domain.TabSnapshot
	paneShare map[string]string // source pane id -> share id
	text      map[string]string // source pane id -> text tail
	vts       map[string]*remotePaneVT
	sizes     map[string][2]int // source pane id -> last size sent
	resizeSub map[string]*nats.Subscription
	needsYou  map[string]bool // source pane id -> announced as needing you
}

// NewRemoteHostActor builds the actor for one `##remote add`.
func NewRemoteHostActor(host string, opts remote.SSHOptions, pub *msg.NATSPublisher, nc *nats.Conn, workspacePID *actor.PID) *RemoteHostActor {
	return &RemoteHostActor{
		host: host, opts: opts, pub: pub, nc: nc, workspacePID: workspacePID,
		dial:       dialRemoteSSH,
		ackTimeout: 5 * time.Second,
		backoffMin: time.Second, backoffMax: 30 * time.Second,
		paneShare: map[string]string{}, text: map[string]string{},
		vts: map[string]*remotePaneVT{}, sizes: map[string][2]int{},
		resizeSub: map[string]*nats.Subscription{},
		needsYou:  map[string]bool{},
	}
}

// Receive implements actor.Actor.
func (a *RemoteHostActor) Receive(ctx actor.Context) {
	switch m := ctx.Message().(type) {
	case *actor.Started:
		a.system, a.self = ctx.ActorSystem(), ctx.Self()
		a.backoff = a.backoffMin
		a.tickStop = make(chan struct{})
		go a.tickLoop(a.tickStop)
		a.connect()

	case *actor.Stopping:
		a.stopped = true
		close(a.tickStop)
		a.dropConn()
		for id, s := range a.resizeSub {
			_ = s.Unsubscribe()
			delete(a.resizeSub, id)
		}

	case *remoteHostConnected:
		if m.gen != a.gen || a.stopped {
			_ = m.client.Close()
			return
		}
		a.client, a.conn = m.client, m.conn
		a.backoff = a.backoffMin
		a.sendQ = make(chan remote.Command, 1024)
		a.connStop = make(chan struct{})
		go a.sendLoop(m.client, a.sendQ, a.connStop)
		go a.pingLoop(m.client, a.connStop)
		a.setState(remoteStateConnected, "")
		// Re-claim the sizes the local viewports asked for before the drop.
		for pane, d := range a.sizes {
			a.enqueue(remote.Command{Type: remote.CmdResize, Pane: pane, Payload: resizePayload(d[0], d[1])})
		}

	case *remoteHostFrame:
		if m.gen != a.gen {
			return
		}
		a.handleFrame(m.f)

	case *remoteHostDown:
		if m.gen != a.gen || a.stopped {
			return
		}
		a.dropConn()
		if errors.Is(m.err, remote.ErrProtoMismatch) {
			a.setState(remoteStateError, m.err.Error())
			return
		}
		a.setState(remoteStateDisconnected, a.downReason(m))
		wait, gen := a.backoff, a.gen
		a.backoff = min(a.backoff*2, a.backoffMax)
		time.AfterFunc(wait, func() { a.system.Root.Send(a.self, &remoteHostRetry{gen: gen}) })

	case *remoteHostRetry:
		if m.gen == a.gen && !a.stopped && a.client == nil {
			a.connect()
		}

	case *remoteHostReconnect:
		a.backoff = a.backoffMin
		if a.client == nil {
			a.connect() // a new gen invalidates any pending retry
		}

	case *remoteHostTick:
		a.refreshKeyframes()

	case *remoteHostResize:
		a.handleResize(m)

	case *remoteHostSendFail:
		slog.Warn("remote-host: input not delivered", "host", a.host, "pane", m.pane, "err", m.err)
		a.note(fmt.Sprintf("input not delivered: %v", m.err))

	case *msg.MsgUpstreamSendCommand:
		a.relay(m)
	}
}

func (a *RemoteHostActor) setState(state, note string) {
	a.state = state
	st := &MsgRemoteHostStatus{Host: a.host, State: state, Note: note}
	if p, ok := a.conn.(interface{ PID() int }); ok && state == remoteStateConnected {
		st.SSHPID = p.PID()
	}
	a.system.Root.Send(a.workspacePID, st)
}

func (a *RemoteHostActor) note(note string) {
	a.system.Root.Send(a.workspacePID, &MsgRemoteHostStatus{Host: a.host, State: a.state, Note: note})
}

func (a *RemoteHostActor) downReason(m *remoteHostDown) string {
	if strings.TrimSpace(m.stderr) != "" {
		return remote.ClassifySSHError(m.stderr)
	}
	if m.err != nil {
		return m.err.Error()
	}
	return "connection closed"
}

// connect starts a new connection generation in the background.
func (a *RemoteHostActor) connect() {
	a.gen++
	gen := a.gen
	a.setState(remoteStateConnecting, "")
	sys, self, opts, dial, ack := a.system, a.self, a.opts, a.dial, a.ackTimeout
	go func() {
		conn, err := dial(opts)
		if err != nil {
			sys.Root.Send(self, &remoteHostDown{gen: gen, err: err})
			return
		}
		c := remote.NewClient(conn, remote.ClientOptions{AckTimeout: ack})
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		h, err := c.Handshake(ctx)
		cancel()
		if err != nil {
			_ = c.Close()
			sys.Root.Send(self, &remoteHostDown{gen: gen, err: err, stderr: stderrAfterExit(conn)})
			return
		}
		sys.Root.Send(self, &remoteHostConnected{gen: gen, client: c, conn: conn, hello: h})
		for f := range c.Frames() {
			sys.Root.Send(self, &remoteHostFrame{gen: gen, f: f})
		}
		_ = c.Close()
		sys.Root.Send(self, &remoteHostDown{gen: gen, err: c.Err(), stderr: stderrAfterExit(conn)})
	}()
}

// stderrAfterExit waits briefly for ssh to exit so its last words are read.
func stderrAfterExit(conn remoteConn) string {
	select {
	case <-conn.Exited():
	case <-time.After(2 * time.Second):
	}
	return conn.Stderr()
}

func (a *RemoteHostActor) dropConn() {
	if a.connStop != nil {
		close(a.connStop)
		a.connStop = nil
	}
	if a.client != nil {
		_ = a.client.Close()
		a.client = nil
	}
	a.conn = nil
	a.sendQ = nil
}

// sendLoop writes queued commands in order. Each is acked asynchronously; a
// failed ack comes back as remoteHostSendFail.
func (a *RemoteHostActor) sendLoop(c *remote.Client, q chan remote.Command, stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case cmd := <-q:
			res := c.DoAsync(cmd)
			go func(pane string) {
				if err := <-res; err != nil {
					a.system.Root.Send(a.self, &remoteHostSendFail{pane: pane, err: err})
				}
			}(cmd.Pane)
		}
	}
}

// pingLoop notices a silently dead link (laptop sleep, NAT timeout) and ends
// the connection so the reconnect path takes over.
func (a *RemoteHostActor) pingLoop(c *remote.Client, stop chan struct{}) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-c.Done():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := c.Ping(ctx)
			cancel()
			if err != nil {
				_ = c.Close()
				return
			}
		}
	}
}

func (a *RemoteHostActor) tickLoop(stop chan struct{}) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			a.system.Root.Send(a.self, &remoteHostTick{})
		}
	}
}

// relay hands one command from the workspace to the remote, or reports why it
// cannot.
func (a *RemoteHostActor) relay(m *msg.MsgUpstreamSendCommand) {
	pane := m.TargetPaneID
	if pane == "" || strings.HasPrefix(pane, "mirror-pending:") {
		a.note("input not delivered: no remote pane selected")
		return
	}
	if !remote.KnownCommand(m.CommandType) {
		a.note("input not delivered: unsupported command " + m.CommandType)
		return
	}
	if a.client == nil {
		a.note(fmt.Sprintf("input not delivered: %s is %s", a.host, a.state))
		return
	}
	// The local daemon log records every remote action with its host; the
	// keystroke bytes themselves are not logged.
	if m.CommandType == remote.CmdRawKeystroke {
		slog.Debug("remote-host: input", "host", a.host, "pane", pane, "type", m.CommandType)
	} else {
		slog.Info("remote-host: command", "host", a.host, "pane", pane, "type", m.CommandType)
	}
	a.enqueue(remote.Command{Type: m.CommandType, Pane: pane, Payload: m.Payload})
}

func (a *RemoteHostActor) enqueue(cmd remote.Command) {
	if a.sendQ == nil {
		a.note("input not delivered: not connected")
		return
	}
	select {
	case a.sendQ <- cmd:
	default:
		a.note("input not delivered: send queue full")
	}
}

func resizePayload(rows, cols int) string {
	return fmt.Sprintf(`{"rows":%d,"cols":%d}`, rows, cols)
}

func (a *RemoteHostActor) handleResize(m *remoteHostResize) {
	share, src := parseMirrorPaneID(m.mirrorPaneID)
	if src == "" || a.paneShare[src] != share || m.rows <= 0 || m.cols <= 0 {
		return
	}
	d := [2]int{m.rows, m.cols}
	if a.sizes[src] == d {
		return
	}
	a.sizes[src] = d
	if a.client != nil {
		a.enqueue(remote.Command{Type: remote.CmdResize, Pane: src, Payload: resizePayload(m.rows, m.cols)})
	}
}

func (a *RemoteHostActor) handleFrame(f remote.Frame) {
	switch f.T {
	case remote.FrameLayout:
		a.layout = f.Tabs
		share := map[string]string{}
		for i := range f.Tabs {
			sid := remoteHostShareID(a.host, f.Tabs[i].ID)
			for _, id := range domain.PaneIDsInTab(&f.Tabs[i]) {
				share[id] = sid
			}
		}
		for id := range a.paneShare {
			if share[id] == "" {
				delete(a.text, id)
				delete(a.vts, id)
				delete(a.sizes, id)
			}
		}
		a.syncAttention(share)
		a.paneShare = share
		a.syncResizeSubs()
		a.pushLayout(nil)
	case remote.FrameScreen:
		a.handleScreen(f)
	case remote.FrameError:
		a.note("remote: " + f.Error)
	}
}

// syncAttention raises (or acks) the local attention for remote panes whose
// needs-you state changed — the event a local pane publishes for itself
// (attentionChanged), on the mirror pane id the local TUI draws.
func (a *RemoteHostActor) syncAttention(share map[string]string) {
	now := map[string]bool{}
	for i := range a.layout {
		for p := range domain.PanesInTab(&a.layout[i]) {
			if _, ok := domain.PaneNeedsYou(p.Meta); ok {
				now[p.ID] = true
			}
		}
	}
	for id := range now {
		if a.needsYou[id] {
			continue
		}
		a.needsYou[id] = true
		_ = a.pub.Send(msg.T("ws", "attention"), &msg.MsgAttentionEvent{
			PaneID:    mirrorPaneID(share[id], id),
			Category:  msg.AttentionPrompt,
			Priority:  msg.AttentionPriorityHigh,
			Title:     "Needs you on " + a.host,
			Timestamp: time.Now().Unix(),
		})
	}
	for id := range a.needsYou {
		if now[id] {
			continue
		}
		delete(a.needsYou, id)
		sid := share[id]
		if sid == "" {
			sid = a.paneShare[id]
		}
		_ = a.pub.Send(msg.T("ws", "attention"), &msg.MsgAttentionAck{PaneID: mirrorPaneID(sid, id)})
	}
}

// pushLayout sends the whole current tab list, text tails merged in.
func (a *RemoteHostActor) pushLayout(deltas map[string]string) {
	out := &MsgRemoteHostLayout{Host: a.host}
	for _, t := range a.layout {
		tab := t
		tab.Lanes = make([]domain.LaneSnapshot, len(t.Lanes))
		var td map[string]string
		for li, lane := range t.Lanes {
			nl := lane
			nl.PaneGroups = make([]domain.PaneGroupSnapshot, len(lane.PaneGroups))
			for gi, g := range lane.PaneGroups {
				ng := g
				ng.Panes = make([]domain.PaneSnapshot, len(g.Panes))
				for pi, p := range g.Panes {
					p.Output = a.text[p.ID]
					meta := make(map[string]string, len(p.Meta)+1)
					for k, v := range p.Meta {
						meta[k] = v
					}
					meta[domain.PaneMetaRemoteHost] = a.host
					p.Meta = meta
					ng.Panes[pi] = p
					if d := deltas[p.ID]; d != "" {
						if td == nil {
							td = map[string]string{}
						}
						td[p.ID] = d
					}
				}
				nl.PaneGroups[gi] = ng
			}
			tab.Lanes[li] = nl
		}
		title := t.Title
		if title == "" {
			title = t.ID
		}
		out.Tabs = append(out.Tabs, RemoteHostTab{ShareID: remoteHostShareID(a.host, t.ID), Title: title, Tab: tab, Deltas: td})
	}
	a.system.Root.Send(a.workspacePID, out)
}

func (a *RemoteHostActor) handleScreen(f remote.Frame) {
	src := f.Pane
	share := a.paneShare[src]
	if share == "" {
		return
	}
	pv := a.vts[src]
	if pv == nil {
		pv = &remotePaneVT{}
		a.vts[src] = pv
	}
	if f.Interactive {
		if !pv.interactive {
			pv.interactive = true
			pv.rows = nil
			a.system.Root.Send(a.workspacePID, &MsgMirrorPaneVTUpdate{ShareID: share, SourcePaneID: src, Interactive: true})
		}
		a.emitVT(share, src, pv, f.Screen, f.CursorRow, f.CursorCol, false)
		return
	}
	if pv.interactive {
		pv.interactive = false
		pv.rows = nil
		pv.seq++
		_ = msg.SendMirrorPaneVTFrame(a.pub, mirrorPaneID(share, src), &msg.MsgMirrorPaneVTFrame{
			PaneID: mirrorPaneID(share, src), Seq: pv.seq, Interactive: false,
		})
		a.system.Root.Send(a.workspacePID, &MsgMirrorPaneVTUpdate{ShareID: share, SourcePaneID: src, Interactive: false})
	}
	if old := a.text[src]; f.Text != old {
		a.text[src] = f.Text
		a.pushLayout(map[string]string{src: outputDelta(old, f.Text)})
	}
}

// emitVT pushes a keyframe or delta on the mirror pane's vtframe topic — the
// same plane MirrorTabListenerActor feeds, so the TUI renders it unchanged.
func (a *RemoteHostActor) emitVT(share, src string, pv *remotePaneVT, rows []string, cr, cc int, forceKey bool) {
	changed := diffMirrorScreen(pv.rows, rows)
	key := forceKey || decideMirrorKeyframe(len(pv.rows), len(rows), len(changed), time.Since(pv.lastKeyframe), mirrorVTKeyframeInterval)
	mid := mirrorPaneID(share, src)
	f := &msg.MsgMirrorPaneVTFrame{PaneID: mid, Interactive: true, Rows: len(rows), CursorRow: cr, CursorCol: cc}
	if key {
		f.Full = rows
		pv.lastKeyframe = time.Now()
	} else {
		if len(changed) == 0 && cr == pv.cursorRow && cc == pv.cursorCol {
			return
		}
		f.BaseSeq = pv.seq
		f.Changed = changed
	}
	pv.seq++
	f.Seq = pv.seq
	pv.rows, pv.cursorRow, pv.cursorCol = rows, cr, cc
	_ = msg.SendMirrorPaneVTFrame(a.pub, mid, f)
}

// refreshKeyframes re-sends a keyframe for every interactive pane, so a TUI
// that attached after the last change (or missed a frame) is not left blank
// waiting for the remote program to redraw.
func (a *RemoteHostActor) refreshKeyframes() {
	for src, pv := range a.vts {
		if pv.interactive && pv.rows != nil && time.Since(pv.lastKeyframe) >= 2*time.Second {
			if share := a.paneShare[src]; share != "" {
				a.emitVT(share, src, pv, pv.rows, pv.cursorRow, pv.cursorCol, true)
			}
		}
	}
}

// syncResizeSubs keeps one exact subscription on each remote pane's local
// mirror inbox, where the TUI sends its size claim (MsgPaneResize) for every
// visible pane.
func (a *RemoteHostActor) syncResizeSubs() {
	if a.nc == nil {
		return
	}
	want := map[string]bool{}
	for src, share := range a.paneShare {
		want[mirrorPaneID(share, src)] = true
	}
	for id, s := range a.resizeSub {
		if !want[id] {
			_ = s.Unsubscribe()
			delete(a.resizeSub, id)
		}
	}
	codecs := a.pub.Codecs()
	for id := range want {
		if _, ok := a.resizeSub[id]; ok {
			continue
		}
		mid := id
		sub, err := a.nc.Subscribe(msg.T("pane", mid, "inbox"), func(nm *nats.Msg) {
			var env msg.NATSEnvelope
			if json.Unmarshal(nm.Data, &env) != nil {
				return
			}
			dec, err := codecs.Decode(env.TypeTag, env.Payload)
			if err != nil {
				return
			}
			if r, ok := dec.(*msg.MsgPaneResize); ok {
				a.system.Root.Send(a.self, &remoteHostResize{mirrorPaneID: mid, rows: r.Rows, cols: r.Cols})
			}
		})
		if err == nil {
			a.resizeSub[mid] = sub
		}
	}
}
