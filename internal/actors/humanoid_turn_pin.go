// SPDX-License-Identifier: Apache-2.0

package actors

// turnPinLife decides when the turn pin ends (N1). The executor runs the
// newest prompt it is sent: a prompt sent while a run is active cancels that
// run (it ends "paused") or replaces the prompt queued behind it. Sub-agent
// runs report on the same status subject, and a paused run can be continued.
// So the pin ends only when the newest prompt's own top-level run is done or
// failed; until then pane_send stays held to the pinned pane (fail-closed).
type turnPinLife struct {
	inFlight bool            // the newest prompt's run has not ended yet
	skip     int             // top-level runs that end before it does
	active   map[string]bool // runs started and not ended
	children map[string]bool // of those, sub-agent runs
}

// sent records a prompt sent to the executor.
func (l *turnPinLife) sent() {
	l.skip = 0
	if l.inFlight {
		l.skip = 1 // the run in flight ends first
	}
	l.inFlight = true
}

// resumed records a continue sent to the executor; it starts a run only when
// none is in flight.
func (l *turnPinLife) resumed() {
	if !l.inFlight {
		l.inFlight, l.skip = true, 0
	}
}

// status follows one MsgAgenticStatus and reports whether the pin ends now.
func (l *turnPinLife) status(orch, phase string) bool {
	if l.active == nil {
		l.active, l.children = map[string]bool{}, map[string]bool{}
	}
	if phase != "done" && phase != "error" && phase != "paused" {
		if !l.active[orch] {
			if len(l.active) > 0 {
				l.children[orch] = true
			}
			l.active[orch] = true
		}
		return false
	}
	child := l.children[orch]
	delete(l.active, orch)
	delete(l.children, orch)
	if child || len(l.active) > 0 {
		return false
	}
	if l.inFlight && l.skip > 0 {
		l.skip--
		return false
	}
	l.inFlight = false
	return phase == "done" || phase == "error"
}

// pinTurn records a prompt about to be sent; a non-empty pane pins the turn
// at once. An unpinned prompt leaves an earlier pin until its own run ends.
func (h *HumanoidActor) pinTurn(pane string) {
	if pane != "" {
		h.turnPin.Store(pane)
	}
	h.pinLife.sent()
}
