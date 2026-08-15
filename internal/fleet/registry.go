// SPDX-License-Identifier: Apache-2.0

// Package fleet is the session's registry of agent fleets (design 028 §6.5):
// which fleets exist, who is in them, and which board each one talks on.
//
// THE PACKAGE BOUNDARY IS THE POINT, and it is the same one internal/board
// draws. This package must not import internal/actors or internal/tui, and it
// does no NATS, no I/O and no clock of its own. What it holds is a MODEL — a
// registry that can be tested exhaustively without standing anything up — while
// the actor around it owns subscriptions, persistence and the question of who
// is alive.
//
// WHY THIS EXISTS AT ALL, since the fleet layer is a Python script and worked
// without it. The registry was `.rysh/fleet/<name>.json`: untracked, shared by
// every fleet in the workspace, and written by whichever `fleetctl` happened to
// run. Two failures are on record from live sessions — a sibling fleet's
// teardown DELETED a live fleet's manifest, taking the tool down for everyone;
// and `fleetctl worktree` reported `created: true` for a directory that did not
// exist. Both are registry-truth problems, and both get linearly worse with the
// number of fleets. The manifest stays as a cache; this is the authority.
package fleet

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// MaxUp is how many fleets may be `up` at once (founder gate `D-14`, ruled 3 on
// 2026-08-11).
//
// THE CAP IS ENFORCED HERE, NOT IN THE DRIVER, and that is the point of the
// number living in this file. A cap the driver respects is a cap that holds
// until somebody runs `fleetctl up` twice by hand, or a second driver starts,
// or a script loops — and the failure it prevents is not tidiness: twenty-five
// fleets is roughly 150 panes and 125 live claude sessions, against a pane
// limit that refuses first and load failures that report success they did not
// have (design 028 §6.8).
//
// REGISTERED IS FREE. The cap counts only StateUp, so a driver may register all
// twenty-five and promote them three at a time.
const MaxUp = 3

// State is where a fleet is in its lifecycle.
//
// REGISTERED IS NOT A LESSER FORM OF UP, it is the cheap half of the split that
// makes 25 fleets affordable: a registered fleet is a row, a board id and a
// roadmap — no panes, no claude sessions, no spend. Bringing it up is what
// costs, which is what the concurrency cap in design 028 §6.8 (`D-14`) bounds.
const (
	StateRegistered = "registered"
	StateUp         = "up"
	StateDown       = "down"
)

// Member is one agent in a fleet.
//
// PaneID is the identity and the only field this package treats as unique — the
// same rule the board applies to posts, and for the same reason: a label is a
// given-name, given-names are unique per LANE rather than per session
// (TabActor.IsGivenNameTakenInLane), and two fleets in different lanes may
// legally hold the same one.
type Member struct {
	PaneID    string `json:"pane_id"`
	Label     string `json:"label,omitempty"`
	Role      string `json:"role,omitempty"`
	Unit      string `json:"unit,omitempty"`
	Parent    string `json:"parent,omitempty"` // parent's pane id
	Worktree  string `json:"worktree,omitempty"`
	SessionID string `json:"session_id,omitempty"` // resumable with `claude -r`
}

// Fleet is one fleet: its identity, where its work is described, and who is in
// it.
type Fleet struct {
	Name       string `json:"name"`
	BoardID    string `json:"board_id"`
	Tab        string `json:"tab,omitempty"`
	Source     string `json:"source,omitempty"` // the epic doc it was cut from
	RoadmapDir string `json:"roadmap_dir,omitempty"`
	State      string `json:"state"`
	Created    int64  `json:"created,omitempty"` // unix millis, stamped by the caller

	Members []Member `json:"members"`
}

// Registry is the set of fleets in one session. Safe for concurrent use: the
// actor's subscription goroutines read it while its reconcile loop writes.
type Registry struct {
	mu     sync.Mutex
	fleets map[string]*Fleet
}

// New returns an empty Registry.
func New() *Registry { return &Registry{fleets: map[string]*Fleet{}} }

// ValidateName rejects a fleet name that cannot also be a board id.
//
// ONE NAMESPACE, DELIBERATELY. A fleet's board id defaults to its name (design
// 028 §6.5), and a board id becomes a NATS subject token — so a fleet called
// "epic.07" would name a board nobody can subscribe to. Rejecting at
// registration is the only place this can be caught before the fleet exists;
// afterwards the failure is a board that is silently empty.
func ValidateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("a fleet needs a name")
	}
	if err := msg.ValidateBoardID(name); err != nil {
		return fmt.Errorf("a fleet name must also be a legal board id: %w", err)
	}
	return nil
}

// Normalise fills in what a caller may leave blank and returns the fleet it
// will be stored as. It does not validate; Register does.
func Normalise(f Fleet) Fleet {
	f.Name = msg.NormalizeBoardID(f.Name)
	if strings.TrimSpace(f.BoardID) == "" {
		f.BoardID = f.Name
	}
	f.BoardID = msg.NormalizeBoardID(f.BoardID)
	if strings.TrimSpace(f.State) == "" {
		f.State = StateRegistered
	}
	if f.Members == nil {
		f.Members = []Member{}
	}
	return f
}

// Register adds or REPLACES a fleet's identity, keeping its members.
//
// Members are kept rather than replaced because registration and membership
// arrive from different places at different times: `fleetctl up` registers the
// fleet, then adds each agent as its pane is created. A Register that emptied
// the roster would make a re-registration — which happens on every restart of
// the driver — silently forget everyone who was already there.
func (r *Registry) Register(f Fleet) (*Fleet, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := ValidateName(f.Name); err != nil {
		return nil, err
	}
	f = Normalise(f)
	if f.BoardID != f.Name {
		if err := msg.ValidateBoardID(f.BoardID); err != nil {
			return nil, fmt.Errorf("board id: %w", err)
		}
	}

	existing := r.fleets[f.Name]
	if existing != nil {
		f.Members = existing.Members
		if f.Created == 0 {
			f.Created = existing.Created
		}
	}
	stored := f
	r.fleets[f.Name] = &stored
	return copyFleet(&stored), nil
}

// Get returns a copy of one fleet, or nil.
//
// A COPY, so a caller cannot mutate the registry by holding a pointer it was
// handed for reading. The board store makes the opposite trade and documents it;
// here the objects are small and the readers are many (a CLI, a ## verb, the
// interrupt path), so copying is cheaper than the discipline.
func (r *Registry) Get(name string) *Fleet {
	r.mu.Lock()
	defer r.mu.Unlock()

	f := r.fleets[msg.NormalizeBoardID(name)]
	if f == nil {
		return nil
	}
	return copyFleet(f)
}

// List returns every fleet, ordered by name so a listing is stable. Go
// randomises map iteration, and a fleet list that reorders itself between two
// calls looks like the session changed.
func (r *Registry) List() []Fleet {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Fleet, 0, len(r.fleets))
	for _, f := range r.fleets {
		out = append(out, *copyFleet(f))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Forget removes a fleet from the registry and reports whether it was there.
//
// IT DOES NOT TOUCH PANES. Forgetting is a registry operation; the panes of a
// forgotten fleet keep running, which is exactly what happened live when a
// sibling's teardown deleted a manifest — the agents carried on while the tool
// that could address them lost their names. Naming that here is the point:
// callers that want the agents stopped must stop them, and this is not that.
func (r *Registry) Forget(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	name = msg.NormalizeBoardID(name)
	if _, ok := r.fleets[name]; !ok {
		return false
	}
	delete(r.fleets, name)
	return true
}

// SetState moves a fleet through its lifecycle.
func (r *Registry) SetState(name, state string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	f := r.fleets[msg.NormalizeBoardID(name)]
	if f == nil {
		return fmt.Errorf("no fleet named %q", name)
	}
	switch state {
	case StateRegistered, StateUp, StateDown:
	default:
		return fmt.Errorf("unknown fleet state %q (want %s, %s or %s)",
			state, StateRegistered, StateUp, StateDown)
	}

	// The cap, refused rather than queued. A queue would leave the caller
	// believing its fleet is coming up when nothing will bring it up, which is
	// the receipt-without-delivery shape; a refusal that NAMES what is already
	// up tells the operator exactly which fleet to take down first.
	if state == StateUp && f.State != StateUp {
		var up []string
		for _, other := range r.fleets {
			if other.Name != f.Name && other.State == StateUp {
				up = append(up, other.Name)
			}
		}
		if len(up) >= MaxUp {
			sort.Strings(up)
			return fmt.Errorf(
				"%d fleets are already up (%s) and the cap is %d — take one down first, "+
					"or register %q and promote it when a slot frees. Registering costs "+
					"nothing; a fleet that is up costs panes and live claude sessions",
				len(up), strings.Join(up, ", "), MaxUp, f.Name)
		}
	}

	f.State = state
	return nil
}

// UpsertMember adds a member or updates the one with the same pane id.
//
// Keyed on PaneID and never on Label: a fleet legitimately holds two agents
// called `wkr-01` in different lanes, and keying on the label would silently
// merge them into one — which reads as a fleet half its real size.
func (r *Registry) UpsertMember(name string, m Member) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	f := r.fleets[msg.NormalizeBoardID(name)]
	if f == nil {
		return fmt.Errorf("no fleet named %q", name)
	}
	if strings.TrimSpace(m.PaneID) == "" {
		return fmt.Errorf("a fleet member needs a pane id")
	}
	for i := range f.Members {
		if f.Members[i].PaneID == m.PaneID {
			f.Members[i] = m
			return nil
		}
	}
	f.Members = append(f.Members, m)
	return nil
}

// RemoveMember drops one member by pane id.
func (r *Registry) RemoveMember(name, paneID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	f := r.fleets[msg.NormalizeBoardID(name)]
	if f == nil {
		return false
	}
	for i := range f.Members {
		if f.Members[i].PaneID == paneID {
			f.Members = append(f.Members[:i], f.Members[i+1:]...)
			return true
		}
	}
	return false
}

// FleetOfPane reports which fleet a pane belongs to, or "".
func (r *Registry) FleetOfPane(paneID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if paneID == "" {
		return ""
	}
	for _, f := range r.fleets {
		for _, m := range f.Members {
			if m.PaneID == paneID {
				return f.Name
			}
		}
	}
	return ""
}

// Reconcile drops members whose panes no longer exist and reports how many were
// dropped.
//
// THE EMPTY SET MEANS "THE CALLER DOES NOT KNOW" and retains everything. This
// is board.Store.RetainRoster's rule, restated because the failure it prevents
// is worse here: a momentarily empty or failed snapshot would empty every
// fleet's roster, and a registry that reports a fleet as having no members is
// indistinguishable from a fleet that finished. A registry that overcounts is
// bad; one that silently empties itself is unrecoverable.
//
// It never removes the FLEET, only members. A fleet whose panes have all gone is
// a fleet that is down — which is a state, decided by whoever owns the
// lifecycle, not something to infer from a snapshot.
func (r *Registry) Reconcile(livePaneIDs map[string]bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(livePaneIDs) == 0 {
		return 0
	}
	dropped := 0
	for _, f := range r.fleets {
		kept := f.Members[:0]
		for _, m := range f.Members {
			if livePaneIDs[m.PaneID] {
				kept = append(kept, m)
				continue
			}
			dropped++
		}
		f.Members = kept
	}
	return dropped
}

// Len is how many fleets the registry holds.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.fleets)
}

func copyFleet(f *Fleet) *Fleet {
	out := *f
	out.Members = make([]Member, len(f.Members))
	copy(out.Members, f.Members)
	return &out
}
