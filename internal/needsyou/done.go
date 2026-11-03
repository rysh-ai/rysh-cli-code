// SPDX-License-Identifier: Apache-2.0

package needsyou

import (
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// "Task done" alerts (gap02 slice 2, design 074). A pane that was busy for at
// least `done_after` and then stops is one alert: the long build, test run or
// agent you walked away from has finished. Off unless done_after is set.
//
// Busy is a program in the pane's foreground (PaneSnapshot.Program, polled
// twice a second by internal/actors/pane_program.go; empty at the shell
// prompt), or rysh's own provider query. The pane's status word alone is not
// enough: "running shell" is set when a command is typed and is never cleared
// when it ends. Like needs-you, an alert carries the pane's name and a reason
// word, never anything from its screen.

const (
	EventNeedsYou = "needs_you"
	EventDone     = "done"

	ReasonDone   = "done"   // the run ended normally
	ReasonFailed = "failed" // the pane's status went to "error"
)

// statusQuerying is the status of a pane whose rysh agent is waiting on its
// model (internal/actors/pane_exec.go); no foreground program shows it.
const statusQuerying = "querying provider"

// PaneState is one pane's status, as the done watcher reads it.
type PaneState struct {
	ID       string
	Name     string
	Program  string
	Status   string
	NeedsYou bool
}

func (p PaneState) busy() bool { return p.Program != "" || p.Status == statusQuerying }

// StatesFromTabs lists every pane of a layout snapshot with its status,
// skipping mirror tabs (a remote pane is reported by its own daemon).
func StatesFromTabs(tabs []domain.TabSnapshot) []PaneState {
	var out []PaneState
	for i := range tabs {
		if strings.HasPrefix(tabs[i].ID, mirrorPrefix) {
			continue
		}
		for p := range domain.PanesInTab(&tabs[i]) {
			_, needs := domain.PaneNeedsYou(p.Meta)
			out = append(out, PaneState{ID: p.ID, Name: paneName(p), Program: p.Program, Status: p.Status, NeedsYou: needs})
		}
	}
	return out
}

// Done is one finished run.
type Done struct {
	ID     string
	Name   string
	Reason string // ReasonDone or ReasonFailed
	Ran    time.Duration
}

// DoneWatcher decides which finished runs to alert for. Pure, like Watcher.
type DoneWatcher struct {
	after, cooldown time.Duration
	busySince       map[string]time.Time
	lastAlert       map[string]time.Time
}

// NewDoneWatcher alerts for runs of at least after (0 = off), at most once per
// pane per cooldown. A run that ends inside the cooldown is dropped, not held:
// a later "done" for a run that has already been replaced means nothing.
func NewDoneWatcher(after, cooldown time.Duration) *DoneWatcher {
	return &DoneWatcher{after: after, cooldown: cooldown, busySince: map[string]time.Time{}, lastAlert: map[string]time.Time{}}
}

// Observe takes every pane now and returns the runs that just finished.
func (w *DoneWatcher) Observe(now time.Time, panes []PaneState) []Done {
	if w.after <= 0 {
		return nil
	}
	seen := make(map[string]bool, len(panes))
	var out []Done
	for _, p := range panes {
		seen[p.ID] = true
		since, tracked := w.busySince[p.ID]
		if p.busy() {
			if !tracked {
				w.busySince[p.ID] = now
			}
			continue
		}
		if !tracked {
			continue
		}
		delete(w.busySince, p.ID)
		ran := now.Sub(since)
		if ran < w.after || p.NeedsYou {
			continue
		}
		if last, ok := w.lastAlert[p.ID]; ok && now.Sub(last) < w.cooldown {
			continue
		}
		w.lastAlert[p.ID] = now
		reason := ReasonDone
		if p.Status == "error" {
			reason = ReasonFailed
		}
		out = append(out, Done{ID: p.ID, Name: p.Name, Reason: reason, Ran: ran})
	}
	for id := range w.busySince {
		if !seen[id] {
			delete(w.busySince, id)
		}
	}
	for id, last := range w.lastAlert {
		if !seen[id] && now.Sub(last) >= w.cooldown {
			delete(w.lastAlert, id)
		}
	}
	return out
}

// Busy reports whether any run is being timed.
func (w *DoneWatcher) Busy() bool { return len(w.busySince) > 0 }
