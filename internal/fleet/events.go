// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"strconv"
	"sync"
)

// Status-ledger change events (design 093 §12.3 H3).
//
// The fleet actor publishes one LedgerEvent on msg.FleetEventSubject for each
// member whose visible row changed, so a bridge such as `rysh family serve`
// can stream status without polling the registry. The model stays here and
// pure — LedgerTracker diffs registry snapshots — and the actor owns the bus.

// LedgerEvent is one member's ledger row after a change.
type LedgerEvent struct {
	Fleet  string `json:"fleet"`
	PaneID string `json:"pane_id"`
	Label  string `json:"label,omitempty"`
	// State is the member's work state as one word: the declared work when
	// there is one, else derived from the observed activity (working →
	// progress, anything else → idle). Always one of the Work* constants.
	State    string `json:"state"`
	Work     string `json:"work,omitempty"`
	Activity string `json:"activity,omitempty"`
	Health   string `json:"health,omitempty"`
	Task     string `json:"task,omitempty"`
	Note     string `json:"note,omitempty"`
	Pct      *int   `json:"pct,omitempty"`
	At       int64  `json:"at"` // unix millis the change was seen
}

// LedgerState is the one-word state of a ledger row.
func LedgerState(s *MemberStatus) string {
	if s == nil {
		return WorkIdle
	}
	if ValidWork(s.Work) {
		return s.Work
	}
	if s.Activity == ActivityWorking {
		return WorkProgress
	}
	return WorkIdle
}

// LedgerTracker remembers the last row it reported per member. Safe for
// concurrent use: the actor calls it from its update, watch and reconcile
// goroutines.
type LedgerTracker struct {
	mu   sync.Mutex
	last map[string]string
}

// NewLedgerTracker returns an empty tracker; its first Changes reports every
// member once.
func NewLedgerTracker() *LedgerTracker { return &LedgerTracker{last: map[string]string{}} }

// Changes returns an event for every member whose state, task, note, pct or
// health differs from the last call, and forgets members that left.
func (t *LedgerTracker) Changes(fleets []Fleet, now int64) []LedgerEvent {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []LedgerEvent
	present := map[string]bool{}
	for _, f := range fleets {
		for _, m := range f.Members {
			key := f.Name + "\x00" + m.PaneID
			present[key] = true
			ev := LedgerEvent{Fleet: f.Name, PaneID: m.PaneID, Label: m.Label, State: LedgerState(m.Status), At: now}
			if s := m.Status; s != nil {
				ev.Work, ev.Activity, ev.Health, ev.Task, ev.Note = s.Work, s.Activity, s.Health, s.Task, s.Note
				if s.Pct != nil {
					p := *s.Pct
					ev.Pct = &p
				}
			}
			sig := ev.State + "\x00" + ev.Task + "\x00" + ev.Note + "\x00" + ev.Health + "\x00" + pctSig(ev.Pct)
			if t.last[key] == sig {
				continue
			}
			t.last[key] = sig
			out = append(out, ev)
		}
	}
	for k := range t.last {
		if !present[k] {
			delete(t.last, k)
		}
	}
	return out
}

func pctSig(p *int) string {
	if p == nil {
		return "-"
	}
	return strconv.Itoa(*p)
}
