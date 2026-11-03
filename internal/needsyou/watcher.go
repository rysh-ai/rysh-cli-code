// SPDX-License-Identifier: Apache-2.0

// Package needsyou tells you, away from the screen, that an agent is waiting
// on you (gap02 / G6: "agents keep running off the laptop").
//
// The daemon already knows when a pane needs you (root-order-044 #3,
// internal/actors/attention.go): an approval held at the gate, an ask_user
// question, or an agent CLI's prompt on screen. Until now only an attached TUI
// acted on it (a bell or OSC 9 in its own terminal), so a session left running
// on another machine, or on a laptop with no UI attached, waited in silence.
//
// This package runs inside the daemon, reads the same attention meta from the
// session's own layout snapshot, and sends an alert to the sinks the user
// configured (`notify: needs_you:` in rysh.config.yaml). It is opt-in, and it
// is deliberately small:
//
//   - An alert goes out only if the pane STILL needs you `after` it was first
//     seen (default 60s). Someone at the keyboard answers first and nothing is
//     sent; that is the "away" test, without tracking attached clients.
//   - One alert per raise, and at most one per pane per `cooldown`.
//   - Mirror tabs (another host's panes shown here, a share) are skipped: a
//     remote pane is notified by the daemon it runs on, never twice.
//   - No screen text ever leaves: an alert carries the pane's given name (or
//     a short id) and a reason word, exactly what the TUI's own notification
//     carries (internal/tui/attention_notify.go).
//
// READ-ONLY toward the session: nothing here answers, dismisses or delays a
// prompt.
package needsyou

import (
	"strconv"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// mirrorPrefix marks tabs whose panes run elsewhere (remote hosts, shares).
const mirrorPrefix = "mirror:"

// nameMax caps the pane name in an alert, as the TUI's notification does.
const nameMax = 40

// Pane is one pane that needs you.
type Pane struct {
	ID     string
	Name   string // given name, else a short id; never the auto title
	Reason string // domain.AttentionReason*
	Since  int64  // unix millis, as the pane reports it
}

// PanesFromTabs lists the panes of a layout snapshot that need you, skipping
// mirror tabs.
func PanesFromTabs(tabs []domain.TabSnapshot) []Pane {
	var out []Pane
	for i := range tabs {
		if strings.HasPrefix(tabs[i].ID, mirrorPrefix) {
			continue
		}
		for p := range domain.PanesInTab(&tabs[i]) {
			reason, ok := domain.PaneNeedsYou(p.Meta)
			if !ok {
				continue
			}
			since, _ := strconv.ParseInt(p.Meta[domain.PaneMetaAttentionSince], 10, 64)
			out = append(out, Pane{ID: p.ID, Name: paneName(p), Reason: reason, Since: since})
		}
	}
	return out
}

// paneName is the pane's given name with control bytes dropped and capped,
// else the first 8 characters of its id. Never the title, which can be a
// command line.
func paneName(p *domain.PaneSnapshot) string {
	var b strings.Builder
	for _, r := range p.GivenName {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			continue
		}
		b.WriteRune(r)
	}
	name := []rune(b.String())
	if len(name) > nameMax {
		name = name[:nameMax]
	}
	if len(name) > 0 {
		return string(name)
	}
	if len(p.ID) > 8 {
		return p.ID[:8]
	}
	return p.ID
}

// track is one raise of one pane.
type track struct {
	since     int64
	firstSeen time.Time
	alerted   bool
}

// Watcher decides which panes to alert for. Pure: the runner feeds it the
// current panes and the time, and it is tested without a daemon or a clock.
type Watcher struct {
	after, cooldown time.Duration
	tracks          map[string]*track
	lastAlert       map[string]time.Time
}

// NewWatcher returns a watcher that alerts after a pane has needed you for
// `after`, at most once per raise and once per `cooldown` per pane.
func NewWatcher(after, cooldown time.Duration) *Watcher {
	return &Watcher{after: after, cooldown: cooldown, tracks: map[string]*track{}, lastAlert: map[string]time.Time{}}
}

// Observe takes every pane that needs you now and returns those to alert for.
// A pane missing from panes no longer needs you; its raise is forgotten.
func (w *Watcher) Observe(now time.Time, panes []Pane) []Pane {
	seen := make(map[string]bool, len(panes))
	var out []Pane
	for _, p := range panes {
		seen[p.ID] = true
		tr := w.tracks[p.ID]
		if tr == nil || tr.since != p.Since {
			// A new raise: first seen now. A changed since without a clear in
			// between is an answered request followed by a new one.
			tr = &track{since: p.Since, firstSeen: now}
			w.tracks[p.ID] = tr
		}
		if tr.alerted || now.Sub(tr.firstSeen) < w.after {
			continue
		}
		if last, ok := w.lastAlert[p.ID]; ok && now.Sub(last) < w.cooldown {
			continue // held, not dropped: it goes out once the cooldown passes
		}
		tr.alerted = true
		w.lastAlert[p.ID] = now
		out = append(out, p)
	}
	for id := range w.tracks {
		if !seen[id] {
			delete(w.tracks, id)
		}
	}
	return out
}

// Pending reports whether a raise is waiting for its delay or cooldown, i.e.
// whether the runner must look again even with no layout change.
func (w *Watcher) Pending() bool {
	for _, tr := range w.tracks {
		if !tr.alerted {
			return true
		}
	}
	return false
}
