// SPDX-License-Identifier: Apache-2.0

package fleet

// Goals (design 055): what a fleet is for, and what each of its agents is for.
//
// WHY THEY LIVE HERE, beside the ledger and not in the epic doc or the brief.
// A goal in prose can be read; it cannot be asked "is the fleet going to make
// it?". The goal executor judges direction — progress toward these, in order,
// against their due times — and a goal it could not read back after a restart
// would be a goal it silently stopped judging. So goals are registry state,
// persisted with the fleet exactly like Watch, and set only through the
// registry's own write path.
//
// Pure like the rest of the package: every function takes `now`.

import (
	"fmt"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Goal states.
const (
	GoalOpen    = "open"
	GoalMet     = "met"
	GoalDropped = "dropped"
)

// Goal is a fleet's or a member's goal.
type Goal struct {
	Text     string `json:"text"`
	DoneWhen string `json:"done_when,omitempty"` // the acceptance line: how anyone can tell it is met
	DueAt    int64  `json:"due_at,omitempty"`    // unix millis; 0 = no deadline
	State    string `json:"state,omitempty"`     // open (default) | met | dropped
	SetBy    string `json:"set_by,omitempty"`    // pane id, or "human"
	SetAt    int64  `json:"set_at,omitempty"`
	MetAt    int64  `json:"met_at,omitempty"`
	Note     string `json:"note,omitempty"`
}

// IsOpen reports whether g is a goal still being worked toward.
func (g *Goal) IsOpen() bool {
	return g != nil && strings.TrimSpace(g.Text) != "" && (g.State == "" || g.State == GoalOpen)
}

// IsMet reports whether g was met.
func (g *Goal) IsMet() bool { return g != nil && g.State == GoalMet }

// GoalUpdate sets or closes a fleet's goal, or one member's.
type GoalUpdate struct {
	// Member selects a member (pane id, prefix, label); empty means the
	// fleet's own goal.
	Member   string `json:"member,omitempty"`
	Text     string `json:"text,omitempty"`
	DoneWhen string `json:"done_when,omitempty"`
	DueSec   int    `json:"due_sec,omitempty"` // seconds from now; 0 keeps the due time (or none)
	// State: "" sets the goal (Text required); met / dropped closes it.
	State string `json:"state,omitempty"`
	Note  string `json:"note,omitempty"`
	SetBy string `json:"set_by,omitempty"`
}

// Goal executor modes.
const (
	// GoalExecAdaptive looks every Min while anything is wrong, and backs off
	// toward Max while the fleet is on track.
	GoalExecAdaptive = "adaptive"
	// GoalExecFixed looks every Min.
	GoalExecFixed = "fixed"
	// GoalExecOff runs no executor for the fleet.
	GoalExecOff = "off"
)

// Executor cadence defaults — the brief's "every 5 minutes or every 30".
const (
	DefaultGoalExecMinSec = 300
	DefaultGoalExecMaxSec = 1800
	// minGoalExecSec bounds the cadence from below: every directive costs an
	// agent a turn, and a coordinator that looks more often than once a
	// minute is a source of noise, not of order.
	minGoalExecSec = 60
)

// GoalExec configures a fleet's goal executor. Zero values mean the defaults.
type GoalExec struct {
	Mode   string `json:"mode,omitempty"`
	MinSec int    `json:"min_sec,omitempty"`
	MaxSec int    `json:"max_sec,omitempty"`
}

// Resolved returns the config with every default filled in.
func (g *GoalExec) Resolved() GoalExec {
	out := GoalExec{Mode: GoalExecAdaptive, MinSec: DefaultGoalExecMinSec, MaxSec: DefaultGoalExecMaxSec}
	if g == nil {
		return out
	}
	if g.Mode != "" {
		out.Mode = g.Mode
	}
	if g.MinSec > 0 {
		out.MinSec = g.MinSec
	}
	if g.MaxSec > 0 {
		out.MaxSec = g.MaxSec
	}
	if out.MaxSec < out.MinSec {
		out.MaxSec = out.MinSec
	}
	return out
}

// Validate refuses a config the executor cannot run.
func (g GoalExec) Validate() error {
	switch g.Mode {
	case "", GoalExecAdaptive, GoalExecFixed, GoalExecOff:
	default:
		return fmt.Errorf("unknown goal executor mode %q (want %s, %s or %s)", g.Mode, GoalExecAdaptive, GoalExecFixed, GoalExecOff)
	}
	if g.MinSec < 0 || g.MaxSec < 0 {
		return fmt.Errorf("a cadence cannot be negative")
	}
	if g.MinSec != 0 && g.MinSec < minGoalExecSec {
		return fmt.Errorf("--min %ds is below the %ds floor: every directive costs an agent a turn", g.MinSec, minGoalExecSec)
	}
	if g.MaxSec != 0 && g.MinSec != 0 && g.MaxSec < g.MinSec {
		return fmt.Errorf("--max %ds is below --min %ds", g.MaxSec, g.MinSec)
	}
	if g.MaxSec != 0 && g.MinSec == 0 && g.MaxSec < DefaultGoalExecMinSec {
		return fmt.Errorf("--max %ds is below the default --min %ds; set --min too", g.MaxSec, DefaultGoalExecMinSec)
	}
	return nil
}

// SetGoal sets or closes a goal. It returns a copy of the fleet as stored.
//
// It REFUSES rather than repairs: a goal with no text, closing a goal that was
// never set, an unknown member. Every assessment the executor makes is measured
// against these, and a goal that was quietly half-applied is a fleet judged
// against the wrong target.
func (r *Registry) SetGoal(fleetName string, gu GoalUpdate, now int64) (*Fleet, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.fleets[msg.NormalizeBoardID(fleetName)]
	if f == nil {
		return nil, fmt.Errorf("no fleet named %q", fleetName)
	}
	slot := &f.Goal
	whose := fmt.Sprintf("fleet %q", f.Name)
	if strings.TrimSpace(gu.Member) != "" {
		m, err := ResolveMember(f, gu.Member)
		if err != nil {
			return nil, err
		}
		slot = &m.Goal
		whose = fmt.Sprintf("%s in fleet %q", MemberLabel(f, m.PaneID), f.Name)
	}

	state := strings.ToLower(strings.TrimSpace(gu.State))
	switch state {
	case "", GoalOpen:
		text := strings.TrimSpace(gu.Text)
		if text == "" {
			return nil, fmt.Errorf("a goal needs --goal '<what done looks like>'")
		}
		if gu.DueSec < 0 {
			return nil, fmt.Errorf("--due cannot be negative")
		}
		g := &Goal{Text: text, DoneWhen: strings.TrimSpace(gu.DoneWhen), State: GoalOpen,
			SetBy: strings.TrimSpace(gu.SetBy), SetAt: now, Note: strings.TrimSpace(gu.Note)}
		if gu.DueSec > 0 {
			g.DueAt = now + int64(gu.DueSec)*1000
		} else if *slot != nil && (*slot).Text == text {
			g.DueAt = (*slot).DueAt // restating the same goal keeps its deadline
		}
		*slot = g
	case GoalMet, GoalDropped:
		if *slot == nil {
			return nil, fmt.Errorf("%s has no goal to mark %s", whose, state)
		}
		g := copyGoal(*slot)
		g.State = state
		if state == GoalMet {
			g.MetAt = now
		}
		if n := strings.TrimSpace(gu.Note); n != "" {
			g.Note = n
		}
		*slot = g
	default:
		return nil, fmt.Errorf("unknown goal state %q (want %s, %s or %s)", gu.State, GoalOpen, GoalMet, GoalDropped)
	}
	return copyFleet(f), nil
}

// SetGoalExec sets a fleet's goal executor config.
func (r *Registry) SetGoalExec(name string, g GoalExec) (*Fleet, error) {
	g.Mode = strings.ToLower(strings.TrimSpace(g.Mode))
	if err := g.Validate(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.fleets[msg.NormalizeBoardID(name)]
	if f == nil {
		return nil, fmt.Errorf("no fleet named %q", name)
	}
	cp := g
	f.GoalExec = &cp
	return copyFleet(f), nil
}

// GoalAskSent records the cmd id of an executor ask (HURRY / REALIGN), so the
// member's next declaration answers it on its thread.
func (r *Registry) GoalAskSent(fleetName, paneID, cmdID string, now int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, m, err := r.memberLocked(fleetName, paneID); err == nil {
		if m.Status == nil {
			m.Status = &MemberStatus{JoinedAt: now}
		}
		m.Status.GoalAskCmd, m.Status.GoalAskAt = cmdID, now
	}
}

// GoalStatus is the goal executor's latest verdict on a fleet, written by the
// executor after every cycle so any process can read it with the fleet
// (`rysh fleet goal show`). A cache of the executor's mind, never an input to
// it: the executor judges from the ledger and the board, not from this.
type GoalStatus struct {
	State      string   `json:"state"`
	Pct        int      `json:"pct"`                 // -1 unknown
	FinishAt   int64    `json:"finish_at,omitempty"` // critical-path finish; 0 unknown
	Unknown    int      `json:"unknown,omitempty"`   // open paths with no estimate
	Bottleneck string   `json:"bottleneck,omitempty"`
	Waiting    int      `json:"waiting,omitempty"`
	Summary    string   `json:"summary,omitempty"` // the report's first line
	Why        []string `json:"why,omitempty"`
	AssessedAt int64    `json:"assessed_at"`
	NextAt     int64    `json:"next_at,omitempty"`
	Sent       int      `json:"sent,omitempty"` // directives sent in that cycle
	// Members is the primary condition per member pane id.
	Members map[string]string `json:"members,omitempty"`
}

// SetGoalStatus records the executor's latest verdict.
func (r *Registry) SetGoalStatus(name string, gs GoalStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f := r.fleets[msg.NormalizeBoardID(name)]; f != nil {
		cp := gs
		f.GoalStatus = &cp
	}
}

func copyGoal(g *Goal) *Goal {
	if g == nil {
		return nil
	}
	out := *g
	return &out
}

// DurationText renders a duration for a message: 10m, 1h, 1h30m, 45s.
func DurationText(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	d = d.Round(time.Second)
	if d >= time.Minute {
		d = d.Round(time.Minute)
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
