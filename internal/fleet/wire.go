// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The registry's read and write path.
//
// THE RULE, AND IT IS THE ONE THE BOARD'S READ PATH ALREADY ESTABLISHED: a
// reader ASKS THE OWNER. Nothing outside the fleet actor opens the registry's
// KV bucket, because a second call site that derives a bucket name is precisely
// how F-23 happened — the read failed while looking perfectly healthy, since an
// empty answer and a wrong bucket render identically.
//
// AND THE SECOND RULE, which is what most of the error handling below is for:
// AN UNANSWERED QUERY IS NOT AN EMPTY SESSION. Ask returns ErrNoRegistry and
// never a zero Reply, because "no fleets are registered" and "nothing answered"
// lead to opposite actions — one says stand a fleet up, the other says find out
// why the daemon is not listening.

// ErrNoRegistry means nobody answered. Callers MUST branch on it.
var ErrNoRegistry = errors.New("fleet: the registry did not answer")

// Query asks for one fleet, or for all of them.
type Query struct {
	// Name selects one fleet. Empty means every fleet.
	Name string `json:"name,omitempty"`
}

// Reply is what the fleet actor answers.
type Reply struct {
	Fleets []Fleet `json:"fleets"`

	// ReconciledAt is when membership was last checked against the panes that
	// actually exist, in unix millis; 0 means never.
	//
	// IT IS REPORTED RATHER THAN HIDDEN because the answer is served from the
	// actor's cache and is therefore as old as the last reconcile. That is a
	// deliberate design constraint, not an oversight — see ServeQueries — and a
	// caller that cannot tell a fresh roster from a stale one would eventually
	// act on a member that closed ten minutes ago. Same discipline as the
	// board's RosterReconciled.
	ReconciledAt int64 `json:"reconciled_at,omitempty"`

	// Err carries a refusal. A registry that cannot answer says so; it never
	// answers with an empty list.
	Err string `json:"err,omitempty"`
}

// Update operations.
const (
	OpRegister     = "register"
	OpForget       = "forget"
	OpState        = "state"
	OpMemberUpsert = "member-upsert"
	OpMemberRemove = "member-remove"
)

// Update is a write against the registry.
type Update struct {
	Op string `json:"op"`

	Fleet  Fleet  `json:"fleet,omitempty"`  // OpRegister
	Name   string `json:"name,omitempty"`   // every other op
	State  string `json:"state,omitempty"`  // OpState
	Member Member `json:"member,omitempty"` // OpMemberUpsert
	PaneID string `json:"pane_id,omitempty"`
}

// UpdateReply reports what the registry did.
type UpdateReply struct {
	OK    bool   `json:"ok"`
	Fleet *Fleet `json:"fleet,omitempty"`
	Err   string `json:"err,omitempty"`
}

// LivePanesFunc reports the panes that exist in the session right now.
//
// THE ERROR MUST NOT BE SWALLOWED INTO AN EMPTY MAP. "I could not ask" and "I
// asked and nobody is there" are different facts and only one of them licenses
// dropping members — Registry.Reconcile treats the empty set as "caller does not
// know" for exactly this reason.
type LivePanesFunc func() (map[string]bool, error)

// ServeQueries answers registry reads for as long as the returned subscription
// lives.
//
// IT ANSWERS FROM THE CACHED REGISTRY AND NEVER CALLS BACK INTO THE WORKSPACE,
// and that is a correctness requirement rather than a performance choice. The
// WORKSPACE is a caller here — `##fleet list` runs inside it — and the fleet
// actor's reconcile asks the workspace for a snapshot. If this handler
// reconciled on demand, the workspace would block in Receive waiting for an
// answer that requires the workspace to answer a snapshot first: a deadlock
// broken only by a timeout, which would surface as "the registry did not
// answer" on a healthy session. Reconciliation therefore happens on the actor's
// own schedule, and the answer says how old it is.
func ServeQueries(nc *nats.Conn, r *Registry, reconciledAt func() int64) (*nats.Subscription, error) {
	if nc == nil || r == nil {
		return nil, errors.New("fleet: ServeQueries needs a connection and a registry")
	}
	return nc.Subscribe(msg.FleetQuerySubject(), func(m *nats.Msg) {
		var q Query
		if len(m.Data) > 0 {
			if err := json.Unmarshal(m.Data, &q); err != nil {
				// REFUSE rather than answer with an empty list: an unreadable
				// query answered with `{"fleets":[]}` tells the caller this
				// session runs no fleets, which is a lie it cannot detect.
				respondQuery(m, Reply{Err: "unreadable query: " + err.Error()})
				return
			}
		}
		reply := Reply{Fleets: []Fleet{}}
		if reconciledAt != nil {
			reply.ReconciledAt = reconciledAt()
		}
		if q.Name != "" {
			if f := r.Get(q.Name); f != nil {
				reply.Fleets = append(reply.Fleets, *f)
			}
		} else {
			reply.Fleets = r.List()
		}
		respondQuery(m, reply)
	})
}

// ServeUpdates accepts registry writes. onChange is called after every accepted
// update, so the caller can persist without this file knowing what a KV is.
func ServeUpdates(nc *nats.Conn, r *Registry, onChange func(op string, f *Fleet, name string)) (*nats.Subscription, error) {
	if nc == nil || r == nil {
		return nil, errors.New("fleet: ServeUpdates needs a connection and a registry")
	}
	return nc.Subscribe(msg.FleetUpdateSubject(), func(m *nats.Msg) {
		var u Update
		if err := json.Unmarshal(m.Data, &u); err != nil {
			respondUpdate(m, UpdateReply{Err: "unreadable update: " + err.Error()})
			return
		}
		reply := apply(r, u)
		if reply.OK && onChange != nil {
			name := u.Name
			if name == "" && reply.Fleet != nil {
				name = reply.Fleet.Name
			}
			onChange(u.Op, reply.Fleet, name)
		}
		respondUpdate(m, reply)
	})
}

// apply is the registry's write switch, pure apart from the Registry it
// mutates — so every refusal below is testable without a bus.
func apply(r *Registry, u Update) UpdateReply {
	switch u.Op {
	case OpRegister:
		f, err := r.Register(u.Fleet)
		if err != nil {
			return UpdateReply{Err: err.Error()}
		}
		return UpdateReply{OK: true, Fleet: f}

	case OpForget:
		if !r.Forget(u.Name) {
			// A refusal, not a silent success. "Forget a fleet that is not
			// there" is usually a typo, and reporting it as done sends the
			// caller away believing a fleet it can still see is gone.
			return UpdateReply{Err: fmt.Sprintf("no fleet named %q", u.Name)}
		}
		return UpdateReply{OK: true}

	case OpState:
		if err := r.SetState(u.Name, u.State); err != nil {
			return UpdateReply{Err: err.Error()}
		}
		return UpdateReply{OK: true, Fleet: r.Get(u.Name)}

	case OpMemberUpsert:
		if err := r.UpsertMember(u.Name, u.Member); err != nil {
			return UpdateReply{Err: err.Error()}
		}
		return UpdateReply{OK: true, Fleet: r.Get(u.Name)}

	case OpMemberRemove:
		if !r.RemoveMember(u.Name, u.PaneID) {
			return UpdateReply{Err: fmt.Sprintf("fleet %q has no member with pane id %q", u.Name, u.PaneID)}
		}
		return UpdateReply{OK: true, Fleet: r.Get(u.Name)}

	default:
		return UpdateReply{Err: fmt.Sprintf("unknown fleet update op %q", u.Op)}
	}
}

func respondQuery(m *nats.Msg, reply Reply) {
	data, err := json.Marshal(reply)
	if err != nil {
		// Nothing useful can be sent, so send nothing: the caller times out and
		// reports ErrNoRegistry, which is at least true. An empty payload would
		// decode to a session with no fleets.
		return
	}
	_ = m.Respond(data)
}

func respondUpdate(m *nats.Msg, reply UpdateReply) {
	data, err := json.Marshal(reply)
	if err != nil {
		return
	}
	_ = m.Respond(data)
}

// Ask puts a Query to the registry and returns its answer.
//
// It returns (nil, ErrNoRegistry) when nobody answers — no responder, a
// timeout, or no connection — and NEVER a usable-looking empty reply alongside
// an error.
func Ask(nc *nats.Conn, q Query, timeout time.Duration) (*Reply, error) {
	if nc == nil {
		return nil, fmt.Errorf("%w: no connection to the session bus", ErrNoRegistry)
	}
	body, err := json.Marshal(q)
	if err != nil {
		return nil, fmt.Errorf("fleet: encode query: %w", err)
	}
	m, err := nc.Request(msg.FleetQuerySubject(), body, timeout)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrNoRegistry, err)
	}
	var reply Reply
	if err := json.Unmarshal(m.Data, &reply); err != nil {
		return nil, fmt.Errorf("fleet: unreadable answer from the registry: %w", err)
	}
	if reply.Err != "" {
		return nil, fmt.Errorf("fleet: the registry refused the query: %s", reply.Err)
	}
	return &reply, nil
}

// Send puts an Update to the registry.
//
// A refusal from the registry comes back as an error, so a caller cannot mistake
// "recorded" for "sent and ignored" — `fleetctl` puts agents into a fleet
// immediately after registering it, and a registration that quietly failed
// would produce a fleet whose members belong to nothing.
func Send(nc *nats.Conn, u Update, timeout time.Duration) (*UpdateReply, error) {
	if nc == nil {
		return nil, fmt.Errorf("%w: no connection to the session bus", ErrNoRegistry)
	}
	body, err := json.Marshal(u)
	if err != nil {
		return nil, fmt.Errorf("fleet: encode update: %w", err)
	}
	m, err := nc.Request(msg.FleetUpdateSubject(), body, timeout)
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", ErrNoRegistry, err)
	}
	var reply UpdateReply
	if err := json.Unmarshal(m.Data, &reply); err != nil {
		return nil, fmt.Errorf("fleet: unreadable answer from the registry: %w", err)
	}
	if !reply.OK {
		if reply.Err == "" {
			reply.Err = "the registry refused the update without saying why"
		}
		return &reply, fmt.Errorf("fleet: %s", reply.Err)
	}
	return &reply, nil
}
