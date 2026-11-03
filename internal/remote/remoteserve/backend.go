// SPDX-License-Identifier: Apache-2.0

// Package remoteserve is the remote end of feature #14: a remote.Backend over
// a running rysh session's own loopback NATS, which `rysh remote-serve` serves
// on its stdio. It reads only what the session already publishes for its own
// TUI, and it applies input exactly as a control-mode share subscriber's input
// is applied (upstream_share.go handleRemoteCommand) — so remote panes run
// under the remote session's own policy, which this package never touches.
package remoteserve

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/remote"
)

// SessionClient is the part of cli.Client the backend uses.
type SessionClient interface {
	RequestFromSubject(subject string, m interface{}, timeout time.Duration) (interface{}, error)
	SendToSubject(subject string, m interface{}) error
	// SubscribeExact subscribes to one exact subject (never a wildcard).
	SubscribeExact(subject string, fn func(interface{})) (unsubscribe func(), err error)
}

const (
	requestTimeout = 3 * time.Second
	// maxText bounds a text pane's output tail, as the mirror path does.
	maxText = 16 * 1024
	// mirrorPrefix marks the remote session's own mirror tabs (shares it
	// subscribed to, remote hosts it shows). They are not forwarded: a remote
	// pane is always a real pane of the remote session.
	mirrorPrefix = "mirror:"
)

// Backend implements remote.Backend over a session client.
type Backend struct {
	c        SessionClient
	clientID string
	events   chan remote.Event

	mu       sync.Mutex
	subs     map[string]func()
	layout   func()
	paneTab  map[string]string // pane id -> tab id, from the last layout
	claimed  map[string]bool   // panes holding our size claim
	lastOut  map[string]time.Time
	lastText map[string]string
}

// workingWindow: a pane that produced output this recently is working
// (an agent's spinner, a build, a loop); otherwise idle. Needs-you is the
// pane's own attention meta.
const workingWindow = 3 * time.Second

// New subscribes to the session's layout signal and returns the backend.
// The size-claim id is "tui:<pid>", so the remote panes drop our claim on
// their own if remote-serve dies without Close (the pane prunes dead tui pids).
func New(c SessionClient) (*Backend, error) {
	b := &Backend{
		c:        c,
		clientID: fmt.Sprintf("tui:%d", os.Getpid()),
		events:   make(chan remote.Event, 256),
		subs:     map[string]func(){},
		paneTab:  map[string]string{},
		claimed:  map[string]bool{},
		lastOut:  map[string]time.Time{},
		lastText: map[string]string{},
	}
	unsub, err := c.SubscribeExact(msg.T("ws", "layoutDirty"), func(interface{}) {
		b.emit(remote.Event{Layout: true})
	})
	if err != nil {
		return nil, fmt.Errorf("subscribe layout: %w", err)
	}
	b.layout = unsub
	return b, nil
}

func (b *Backend) emit(ev remote.Event) {
	select {
	case b.events <- ev:
	default: // full: the server's poll re-reads everything anyway
	}
}

// Events implements remote.Backend.
func (b *Backend) Events() <-chan remote.Event { return b.events }

// Layout implements remote.Backend.
func (b *Backend) Layout() ([]domain.TabSnapshot, error) {
	reply, err := b.c.RequestFromSubject(msg.T("ws", "snapshot"),
		&msg.MsgGetWorkspaceSnapshot{LayoutOnly: true, NoHistories: true}, requestTimeout)
	if err != nil {
		return nil, err
	}
	r, ok := reply.(*msg.MsgWorkspaceSnapshotReply)
	if !ok {
		return nil, fmt.Errorf("unexpected snapshot reply %T", reply)
	}
	var tabs []domain.TabSnapshot
	paneTab := map[string]string{}
	for _, t := range r.Snapshot.Tabs {
		if strings.HasPrefix(t.ID, mirrorPrefix) {
			continue
		}
		tt := TrimTab(t)
		for p := range domain.PanesInTab(&tt) {
			paneTab[p.ID] = tt.ID
			if p.Meta == nil {
				p.Meta = map[string]string{}
			}
			p.Meta[domain.PaneMetaActivity] = b.activity(p)
		}
		tabs = append(tabs, tt)
	}
	b.mu.Lock()
	b.paneTab = paneTab
	b.mu.Unlock()
	return tabs, nil
}

// activity is the working/idle observation for one pane.
func (b *Backend) activity(p *domain.PaneSnapshot) string {
	b.mu.Lock()
	last := b.lastOut[p.ID]
	b.mu.Unlock()
	if !last.IsZero() && time.Since(last) < workingWindow {
		return domain.PaneActivityWorking
	}
	return domain.PaneActivityIdle
}

// TrimTab keeps what a local rysh needs to draw a remote tab: structure,
// titles, modes, status, the foreground program and the attention meta. Screens
// and output travel in screen frames; histories and conversations stay remote.
func TrimTab(src domain.TabSnapshot) domain.TabSnapshot {
	out := domain.TabSnapshot{ID: src.ID, Title: src.Title, ActivePaneID: src.ActivePaneID}
	for _, lane := range src.Lanes {
		nl := domain.LaneSnapshot{ID: lane.ID, Flex: lane.Flex, Name: lane.Name, ActivePaneID: lane.ActivePaneID}
		for _, g := range lane.PaneGroups {
			ng := domain.PaneGroupSnapshot{ID: g.ID, RowFlex: g.RowFlex, ActivePaneID: g.ActivePaneID}
			for _, p := range g.Panes {
				ps := domain.PaneSnapshot{
					ID: p.ID, Title: p.Title, GivenName: p.GivenName, Mode: p.Mode, Status: p.Status, PaneType: p.PaneType,
					LastCommand: p.LastCommand, ProviderName: p.ProviderName, Program: p.Program,
					RawMode: p.RawMode, MouseEnabled: p.MouseEnabled, MouseProto: p.MouseProto, MouseSGR: p.MouseSGR,
					PTYRows: p.PTYRows, PTYCols: p.PTYCols,
				}
				for _, k := range []string{domain.PaneMetaAttention, domain.PaneMetaAttentionReason, domain.PaneMetaAttentionSince} {
					if v, ok := p.Meta[k]; ok {
						if ps.Meta == nil {
							ps.Meta = map[string]string{}
						}
						ps.Meta[k] = v
					}
				}
				ng.Panes = append(ng.Panes, ps)
			}
			nl.PaneGroups = append(nl.PaneGroups, ng)
		}
		out.Lanes = append(out.Lanes, nl)
	}
	return out
}

// Screen implements remote.Backend.
func (b *Backend) Screen(paneID string) (remote.Screen, error) {
	reply, err := b.c.RequestFromSubject(msg.T("pane", paneID, "snapshot"), &msg.MsgGetPaneVT{}, requestTimeout)
	if err != nil {
		return remote.Screen{}, err
	}
	vt, ok := reply.(*msg.MsgPaneVTReply)
	if !ok {
		return remote.Screen{}, fmt.Errorf("unexpected VT reply %T", reply)
	}
	if vt.Interactive {
		return remote.Screen{Interactive: true, Rows: vt.Screen, CursorRow: vt.CursorRow, CursorCol: vt.CursorCol}, nil
	}
	reply, err = b.c.RequestFromSubject(msg.T("pane", paneID, "snapshot"), &msg.MsgGetPaneSnapshot{NoHistories: true}, requestTimeout)
	if err != nil {
		return remote.Screen{}, err
	}
	ps, ok := reply.(*msg.MsgPaneSnapshotReply)
	if !ok {
		return remote.Screen{}, fmt.Errorf("unexpected pane reply %T", reply)
	}
	text := tail(ps.Snapshot.Output, maxText)
	// A text pane's output can grow without a rawDirty; a changed tail is
	// output too.
	b.mu.Lock()
	if prev, seen := b.lastText[paneID]; seen && prev != text {
		b.lastOut[paneID] = time.Now()
	}
	b.lastText[paneID] = text
	b.mu.Unlock()
	return remote.Screen{Text: text}, nil
}

// tail returns at most the last n bytes of s, starting at a line boundary.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	t := s[len(s)-n:]
	if i := strings.IndexByte(t, '\n'); i >= 0 {
		t = t[i+1:]
	}
	return t
}

// Watch implements remote.Backend: one exact rawDirty subscription per pane.
func (b *Backend) Watch(ids []string) {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for id := range b.lastOut {
		if !want[id] {
			delete(b.lastOut, id)
		}
	}
	for id := range b.lastText {
		if !want[id] {
			delete(b.lastText, id)
		}
	}
	for id, unsub := range b.subs {
		if !want[id] {
			unsub()
			delete(b.subs, id)
		}
	}
	for id := range want {
		if _, ok := b.subs[id]; ok {
			continue
		}
		pane := id
		unsub, err := b.c.SubscribeExact(msg.T("pane", pane, "rawDirty"), func(interface{}) {
			b.mu.Lock()
			b.lastOut[pane] = time.Now()
			b.mu.Unlock()
			b.emit(remote.Event{Pane: pane})
		})
		if err == nil {
			b.subs[pane] = unsub
		}
	}
}

// Apply implements remote.Backend. It mirrors handleRemoteCommand: the same
// messages, on the same subjects, a control-mode share subscriber's input
// produces. An error means the message did not reach the session's NATS.
func (b *Backend) Apply(c remote.Command) error {
	pane := c.Pane
	inbox := msg.T("pane", pane, "inbox")
	switch c.Type {
	case remote.CmdRawKeystroke:
		data, err := base64.StdEncoding.DecodeString(c.Payload)
		if err != nil {
			return errors.New("invalid base64 payload")
		}
		return b.c.SendToSubject(msg.T("pane", pane, "rawinput"), &msg.MsgRawKeyInput{PaneID: pane, Data: data})
	case remote.CmdExecShell, remote.CmdSubmitInput:
		return b.c.SendToSubject(inbox, &msg.MsgPaneExecShell{Command: c.Payload})
	case remote.CmdExecPrompt:
		return b.c.SendToSubject(inbox, &msg.MsgPaneExecPrompt{Prompt: c.Payload})
	case remote.CmdExecChat:
		return b.c.SendToSubject(inbox, &msg.MsgPaneExecChat{Message: c.Payload, SenderName: "remote"})
	case remote.CmdExecRysh:
		return b.c.SendToSubject(msg.T("ws", "inbox"), &msg.MsgExecRyshOnPane{PaneID: pane, Command: c.Payload})
	case remote.CmdTabOp:
		var op struct {
			Op    string `json:"op"`
			Dir   string `json:"dir"`
			Delta int    `json:"delta"`
			Name  string `json:"name"`
		}
		if err := json.Unmarshal([]byte(c.Payload), &op); err != nil {
			return errors.New("invalid tab_op payload")
		}
		tab := b.tabOf(pane)
		if tab == "" {
			return errors.New("pane has no tab")
		}
		return b.c.SendToSubject(msg.T("ws", "inbox"), &msg.MsgMirrorTabOp{
			TabID: tab, PaneID: pane, Op: op.Op, Dir: op.Dir, Delta: op.Delta, Name: op.Name,
		})
	case remote.CmdMaximize:
		var p struct {
			On   bool `json:"on"`
			Rows int  `json:"rows"`
			Cols int  `json:"cols"`
		}
		if err := json.Unmarshal([]byte(c.Payload), &p); err != nil {
			return errors.New("invalid maximize payload")
		}
		return b.c.SendToSubject(msg.T("ws", "remoteFullscreen"), &msg.MsgRemotePaneFullscreen{
			TabID: b.tabOf(pane), PaneID: pane, On: p.On, Rows: p.Rows, Cols: p.Cols,
		})
	case remote.CmdResize:
		var d struct {
			Rows int `json:"rows"`
			Cols int `json:"cols"`
		}
		if err := json.Unmarshal([]byte(c.Payload), &d); err != nil || d.Rows <= 0 || d.Cols <= 0 {
			return errors.New("invalid resize payload")
		}
		// A claim, not an override: a viewport attached on the remote keeps its
		// say, and the pane renders at the smallest of the two.
		if err := b.c.SendToSubject(inbox, &msg.MsgPaneResize{Rows: d.Rows, Cols: d.Cols, ClientID: b.clientID}); err != nil {
			return err
		}
		b.mu.Lock()
		b.claimed[pane] = true
		b.mu.Unlock()
		return nil
	}
	return fmt.Errorf("unknown command type %s", c.Type)
}

func (b *Backend) tabOf(pane string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.paneTab[pane]
}

// Close implements remote.Backend: drop subscriptions and release size claims.
func (b *Backend) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.layout != nil {
		b.layout()
		b.layout = nil
	}
	for id, unsub := range b.subs {
		unsub()
		delete(b.subs, id)
	}
	for pane := range b.claimed {
		_ = b.c.SendToSubject(msg.T("pane", pane, "inbox"), &msg.MsgPaneReleaseSize{ClientID: b.clientID})
		delete(b.claimed, pane)
	}
}
