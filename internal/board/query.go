// SPDX-License-Identifier: Apache-2.0

package board

// The board's READ path (design 027, agent-fleet epic wave 1).
//
// The agents board was write-only to agents: `rysh board post` and
// `rysh board reply` put messages on it, `##board open` renders it in a TUI
// pane, and nothing let a claude in a pane READ what the fleet had been
// saying. This file is the missing half.
//
// THE ONE DESIGN RULE HERE, AND IT IS NOT NEGOTIABLE: a reader ASKS THE
// RECORDER. It does not open the board's KV bucket. See msg.BoardQuerySubject
// for the two reasons (design 026 §5.4, and F-23).
//
// THE SECOND RULE, which is what most of this file's error handling is for:
// AN UNANSWERED QUERY IS NOT AN EMPTY BOARD. Ask returns (nil, ErrNoRecorder)
// and never a zero QueryReply, because a zero QueryReply renders as a clean,
// confident, empty board — the exact shape of F-20 and F-23, where every link
// was present and the failure looked like health.

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ErrNoRecorder means nobody answered the query.
//
// It is a distinct error and not an empty result on purpose. "The recorder did
// not answer" and "the recorder answered, and the board is empty" are different
// facts about the world, and a caller that cannot tell them apart will print
// silence as health. Every caller must branch on this.
var ErrNoRecorder = errors.New("board: the recorder did not answer")

// Query is a read request against the recorder's board.
//
// Both bounds are optional and the zero Query means "everything you have".
type Query struct {
	// Since keeps only threads with activity at or after this unix-millis
	// instant. It is compared against the poster's clock (MsgBoardPost.TS), so
	// it inherits that field's honest limit: arrival order, not causal order.
	Since int64 `json:"since,omitempty"`

	// Limit bounds how many THREADS come back, keeping the most recent — this
	// is `tail`, so the end of the board is what you want. Zero means unbounded.
	//
	// Threads, not posts, because the store's own eviction is by whole thread
	// (store.go evictLocked): splitting a thread to hit a post count would
	// manufacture exactly the orphan pile both bounds exist to avoid.
	Limit int `json:"limit,omitempty"`
}

// QueryReply is what the recorder answers.
//
// Stats describe the WHOLE board; Threads is the window Since/Limit asked for.
// That is deliberate — the counters are how a caller says "N more exist that I
// am not showing you" instead of silently presenting a window as the whole
// thing (the discipline design 025 §7.1a puts on eviction).
type QueryReply struct {
	Threads []Thread      `json:"threads"`
	Roster  []RosterEntry `json:"roster"`
	Stats   Stats         `json:"stats"`

	// Filtered counts threads dropped by Since; Withheld counts threads dropped
	// by Limit. Both are here so a truncated answer can say so out loud.
	Filtered int `json:"filtered"`
	Withheld int `json:"withheld"`

	// RosterReconciled reports whether Roster was checked against the panes
	// that actually exist, or is being served as recorded.
	//
	// False is not an error and does not invalidate the answer — it means the
	// recorder could not confirm who is alive, so the roster may still contain
	// panes that have closed (F-26). It is here because the alternative to
	// disclosing it is a caller that cannot tell an authoritative roster from a
	// best-effort one, which is the same shape of defect as the ghost itself:
	// something that looks certain and is not.
	RosterReconciled bool `json:"roster_reconciled"`

	// Err carries a refusal from the recorder. A recorder that cannot answer
	// says so; it never answers with an empty board, for the reason at the top
	// of this file. Ask turns a non-empty Err into an error.
	Err string `json:"err,omitempty"`
}

// Answer builds the reply to a Query from a Store. Pure: no NATS, no clock, so
// the filtering rules are testable without standing anything up.
//
// KNOWN LIMIT, stated rather than implied: this is NOT an atomic snapshot.
// Threads, Roster and Stats each take the store's lock separately, so a post
// arriving mid-answer can be counted in Stats and absent from Threads. The
// board is a monitoring view and not an audit log (design 025 §7.2), and the
// alternative — holding the store's lock across the whole answer — would let a
// reader stall the collector that feeds it. A reader that sees Stats.Posts one
// ahead of what it can enumerate is seeing a live board, not a broken one.
func Answer(s *Store, q Query) QueryReply {
	reply := QueryReply{Threads: []Thread{}, Roster: []RosterEntry{}}
	if s == nil {
		return reply
	}

	all := s.Threads()
	kept := make([]Thread, 0, len(all))
	for _, t := range all {
		if q.Since > 0 && threadActivity(t) < q.Since {
			reply.Filtered++
			continue
		}
		kept = append(kept, t)
	}

	// Keep the TAIL. Store.Threads() is in arrival order, so the newest threads
	// are at the end.
	if q.Limit > 0 && len(kept) > q.Limit {
		reply.Withheld = len(kept) - q.Limit
		kept = kept[len(kept)-q.Limit:]
	}

	reply.Threads = kept
	reply.Roster = s.Roster()
	reply.Stats = s.Stats()
	return reply
}

// threadActivity is the most recent post in a thread, root or reply.
//
// Filtering by the thread's LATEST activity and keeping the thread WHOLE is the
// choice here: a --since that sliced replies off their root would hand back a
// root that reads as unanswered, or an orphan pile — a filter that changes the
// meaning of what it returns is worse than one that returns too much.
func threadActivity(t Thread) int64 {
	var latest int64
	if t.Root != nil {
		latest = t.Root.TS
	}
	for _, r := range t.Replies {
		if r.TS > latest {
			latest = r.TS
		}
	}
	return latest
}

// LivePanesFunc reports the panes that exist in the session RIGHT NOW, as a set
// of pane ids.
//
// THE ERROR IS NOT DECORATION AND MUST NOT BE SWALLOWED INTO AN EMPTY MAP.
// "I could not ask" and "I asked and nobody is here" are different facts, and
// only one of them licenses deleting anything. Collapsing them is the whole of
// F-26's danger: reconciling against a failed round trip would wipe the entire
// roster the first time the workspace was busy, which is strictly worse than
// the ghost it fixes.
type LivePanesFunc func() (map[string]bool, error)

// reconcileRoster drops roster entries for panes that no longer exist, and
// reports whether it was able to.
//
// F-26: registration is persistent (founder gate 2), so a roster served
// verbatim accumulates GHOSTS — a pane that registered and then closed keeps
// its entry forever. Observed live: `rysh board tail --json` returned 7 roster
// entries for 6 live panes while the rendered board showed 5 agents at the same
// instant. Two surfaces over one store, disagreeing about who exists.
//
// IT IS DONE HERE, AT THE SOURCE, AND NOT IN THE CLI. If the caller
// reconciled, the rule would exist in two places and the two would drift —
// which is exactly how F-18 happened, and why design 025 §8b puts a rule in one
// predicate. The rule itself is Store.RetainRoster and is not restated here:
// live pane ids are authoritative for WHO EXISTS, registrations stay
// authoritative for WHAT THEY ARE CALLED.
//
// THE PRECONDITION, WHICH IS THE POINT: a failed lookup leaves the roster
// completely untouched. The zero-pane case is left to RetainRoster, which
// already owns it — duplicating that guard here would be the second copy this
// function exists to avoid.
func reconcileRoster(s *Store, live LivePanesFunc) bool {
	if s == nil || live == nil {
		return false
	}
	ids, err := live()
	if err != nil {
		// COULD NOT ASK. Serve the roster as it stands, ghosts and all, and say
		// so through QueryReply.RosterReconciled. A stale roster a caller knows
		// is stale beats an empty one it believes.
		return false
	}
	if len(ids) == 0 {
		// ASKED, AND WAS TOLD NOTHING. A different input from the error above
		// and equally non-destructive: RetainRoster retains everything on an
		// empty set. Reported as unreconciled because it is.
		return false
	}
	s.RetainRoster(ids)
	return true
}

// ServeQueries answers board queries from s for as long as the returned
// subscription lives.
//
// The caller owns the subscription's lifetime, and that is the whole liveness
// contract: ABLA unsubscribes when it stops, so a dead recorder GOES QUIET and
// a caller's timeout is the honest answer rather than a stale artifact's. Same
// property as msg.BoardAliveSubject, for the same reason.
//
// live may be nil, in which case the roster is served unreconciled and says so.
// storeFor resolves the board a query names. Returning nil is an ANSWER — "I
// record that board and it is empty" — and not a refusal, because the recorder
// hears every board in the session through one wildcard (subscriber.go). A
// board nobody has posted to yet is empty, and saying so is true.
//
// It must not be used to mean "I do not serve that board": a recorder that
// refuses a board it is in fact recording would make an honest reader report
// ErrNoRecorder for a board that works.
func ServeQueries(nc *nats.Conn, storeFor func(board string) *Store, live LivePanesFunc) ([]*nats.Subscription, error) {
	if nc == nil || storeFor == nil {
		return nil, errors.New("board: ServeQueries needs a connection and a store resolver")
	}

	handler := func(m *nats.Msg) {
		board, ok := msg.BoardIDFromSubject(m.Subject, "query")
		if !ok {
			respond(m, QueryReply{Err: "unreadable board subject: " + m.Subject})
			return
		}
		var q Query
		if len(m.Data) > 0 {
			if err := json.Unmarshal(m.Data, &q); err != nil {
				// REFUSE, do not answer with an empty board. An unreadable
				// query answered with `{"threads":[]}` would tell the caller
				// the fleet has said nothing, which is a lie the caller has no
				// way to detect.
				respond(m, QueryReply{Err: "unreadable query: " + err.Error()})
				return
			}
		}
		s := storeFor(board)
		// Reconcile BEFORE reading, so the answer describes who exists now
		// rather than everyone who ever announced.
		reconciled := reconcileRoster(s, live)
		reply := Answer(s, q)
		reply.RosterReconciled = reconciled
		respond(m, reply)
	}

	var subs []*nats.Subscription
	for _, subject := range []string{msg.BoardQuerySubject(msg.DefaultBoardID), msg.BoardQueryPattern()} {
		sub, err := nc.Subscribe(subject, handler)
		if err != nil {
			for _, done := range subs {
				_ = done.Unsubscribe()
			}
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, nil
}

func respond(m *nats.Msg, reply QueryReply) {
	data, err := json.Marshal(reply)
	if err != nil {
		// Nothing useful can be sent, so send nothing: the caller times out and
		// reports ErrNoRecorder, which is at least true — it did not get an
		// answer. An empty payload would decode to an empty board.
		return
	}
	_ = m.Respond(data)
}

// Ask puts a Query to the recorder and returns its answer.
//
// It returns (nil, ErrNoRecorder) when nobody answers — no responder, a
// timeout, or no connection at all. It NEVER returns a usable-looking empty
// reply alongside an error, so a caller that forgets to check err has nothing
// to render rather than an empty board to render confidently.
// board names which board to read; "" is the session board.
func Ask(nc *nats.Conn, board string, q Query, timeout time.Duration) (*QueryReply, error) {
	if nc == nil {
		return nil, fmt.Errorf("%w: no connection to the session bus", ErrNoRecorder)
	}
	if err := msg.ValidateBoardID(board); err != nil {
		// Refused here rather than sanitised into the default board: a reader
		// that asked for one board and was quietly shown another is the same
		// class of lie as an empty board that should not be empty.
		return nil, fmt.Errorf("board: %w", err)
	}
	body, err := json.Marshal(q)
	if err != nil {
		return nil, fmt.Errorf("board: encode query: %w", err)
	}
	m, err := nc.Request(msg.BoardQuerySubject(board), body, timeout)
	if err != nil {
		// nats.ErrNoResponders (nobody subscribed) and nats.ErrTimeout (nobody
		// answered in time) are the same fact to a caller: the board's contents
		// are UNKNOWN. Neither is an empty board.
		return nil, fmt.Errorf("%w (%v)", ErrNoRecorder, err)
	}
	var reply QueryReply
	if err := json.Unmarshal(m.Data, &reply); err != nil {
		return nil, fmt.Errorf("board: unreadable answer from the recorder: %w", err)
	}
	if reply.Err != "" {
		return nil, fmt.Errorf("board: the recorder refused the query: %s", reply.Err)
	}
	return &reply, nil
}
