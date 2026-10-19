// SPDX-License-Identifier: Apache-2.0

package web

import (
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// streamPaneVT used to run a full structural snapshot — one request to every
// tab, lane, group and pane — ten times a second for as long as any browser
// was connected, only to learn which panes were interactive, and then pulled
// every visible interactive pane's frame each tick to discover it had not
// changed. On a 22-pane session that was ~300 bus requests a second with
// nobody typing. The daemon already announces both kinds of change, and the
// TUI has long been driven by them:
//
//   - pane.<id>.rawDirty   a pane's VT screen changed (local raw output, or a
//     remote-interactive pane's screen update);
//   - pane.<id>.shareMode  a pane entered or left interactive mode;
//   - ws.layoutDirty       the layout changed.
//
// vtSignals collects them between ticks. Signals are hints, not the only
// source of truth: streamPaneVT still re-reads the structure every
// vtStructureResync and sweeps every visible pane every vtSweepInterval, so a
// dropped message costs at most that much staleness, never a stuck screen.
const (
	vtStructureResync = 2 * time.Second
	vtSweepInterval   = time.Second
)

type vtSignals struct {
	mu        sync.Mutex
	dirty     map[string]bool
	structure bool
	subs      []*nats.Subscription
}

// subscribeVTSignals starts collecting change signals. The first take reports
// a structure change, so the first tick always reads the layout.
func (s *Server) subscribeVTSignals() *vtSignals {
	v := &vtSignals{dirty: map[string]bool{}, structure: true}
	if s.nc == nil {
		return v
	}
	onPane := func(markStructure bool) nats.MsgHandler {
		return func(m *nats.Msg) {
			id := paneIDFromSubject(m.Subject)
			v.mu.Lock()
			if id != "" {
				v.dirty[id] = true
			}
			if markStructure {
				v.structure = true
			}
			v.mu.Unlock()
		}
	}
	for _, sub := range []struct {
		subject string
		handler nats.MsgHandler
	}{
		{msg.T("pane", "*", "rawDirty"), onPane(false)},
		{msg.T("pane", "*", "shareMode"), onPane(true)},
		{msg.T("ws", "layoutDirty"), func(*nats.Msg) {
			v.mu.Lock()
			v.structure = true
			v.mu.Unlock()
		}},
	} {
		if s, err := s.nc.Subscribe(sub.subject, sub.handler); err == nil {
			v.subs = append(v.subs, s)
		}
	}
	return v
}

// take returns the panes signalled since the last take and whether the
// structure may have changed, and resets both.
func (v *vtSignals) take() (dirty map[string]bool, structure bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	dirty, structure = v.dirty, v.structure
	v.dirty, v.structure = map[string]bool{}, false
	return dirty, structure
}

// reset marks everything stale, for the next stream client to start fresh.
func (v *vtSignals) reset() {
	v.mu.Lock()
	v.dirty, v.structure = map[string]bool{}, true
	v.mu.Unlock()
}

func (v *vtSignals) close() {
	for _, s := range v.subs {
		_ = s.Unsubscribe()
	}
}
