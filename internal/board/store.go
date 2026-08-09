// Package board is the agents-board store (design 025): the in-memory model of
// a threaded, push-based stream of what every agent in a session is doing.
//
// THE PACKAGE BOUNDARY IS THE POINT. This package must not import
// internal/tui or internal/actors. The founder has not yet ruled on whether the
// board is a ##mode or a PaneType (design 025 §8.1), and a store that knows
// nothing about bubbletea, panes or modes keeps that decision a small change in
// the view layer instead of a rewrite. Everything here is pure: no NATS, no
// I/O, no clock. The only dependency is the message contract in internal/msg.
package board

import (
	"strings"
	"sync"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// MaxPosts bounds the store so a long session cannot grow it without limit.
//
// The number is sized from the case that motivated the epic: a 44-agent fleet.
// At 44 agents this is ~110 messages each, which is more than a full day of
// milestone-grade posting. Eviction is by whole thread, oldest first (see
// evictLocked), because evicting a root while keeping its replies would turn a
// coherent thread into a pile of orphans — the one shape this store exists to
// avoid.
//
// EVICTION IS A DOCUMENTED LOSS FROM THE VIEW, not from the record. Since
// founder gate 2 the board persists (persist.go): an evicted message is out of
// the in-memory board but still in the KV until its retention expires. The
// earlier non-goal "no persistence beyond the session" is dead — do not
// reintroduce it in a comment. Store.Stats().Evicted counts evictions so a view
// can say "N older messages are no longer shown" rather than silently
// shortening history.
const MaxPosts = 5000

// Thread is one root and its replies, as a view renders it.
//
// The messages are owned by the Store and MUST be treated as read-only by
// callers; the slices are copies, so appending to them is safe, but mutating a
// *msg.MsgBoardPost mutates what every other reader sees. Copying ~5000
// messages on every render to prevent that would cost more than the discipline
// does.
type Thread struct {
	// Key addresses the thread. For a normal thread it is the poster-minted
	// "<pane-uuid>/<n>" that replies carry. For a standalone post (one that
	// arrived with no ThreadID and can therefore never be replied to) it is an
	// internal key that no agent can mint — see standaloneKey.
	Key string

	// Root is nil while the thread is Provisional.
	Root    *msg.MsgBoardPost
	Replies []*msg.MsgBoardPost

	// Provisional marks a thread whose replies arrived before their root.
	// This is expected, not an error: thread ids are minted agent-side with no
	// round trip (design 025 §4.3), so ordering between two panes is not
	// guaranteed. The view renders a provisional thread as a root; when the
	// real root lands the thread stops being provisional and re-positions
	// itself to where the ROOT's arrival puts it.
	Provisional bool
}

// RosterEntry is one agent the board knows about, from MsgBoardRegister.
//
// Deliberately just pane + persona + clock. There is no Fleet, Role or Unit
// here because gate 4 removed them from the contract: a board post is CHAT —
// who spoke, under which thread — and the fleet's FROM/TO routing envelope is a
// fleet concern. Their absence is what makes gate 3 ("every claude in the
// session may post, not only fleet members") real rather than aspirational:
// with no fleet fields at all, there is no field that can be missing for a
// non-fleet claude, so nothing downstream can drift into treating a fleet
// poster as more first-class than any other.
type RosterEntry struct {
	PaneID  string
	Persona string
	TS      int64
}

// Stats are the counters a view needs to be honest with the human about what it
// is not showing.
type Stats struct {
	Threads     int // total threads, provisional included
	Provisional int // threads still missing their root
	Posts       int // roots + replies currently held
	Duplicates  uint64
	Evicted     uint64
	// UnknownVersion counts posts whose V is not BoardSchemaVersion. They are
	// KEPT, not dropped — a board that discards a message because it was
	// written by a newer agent is worse than one that renders it plainly.
	UnknownVersion uint64
}

// Store holds the board. It is safe for concurrent use: a Subscriber pump may
// feed it from the NATS callback goroutine while a view reads it from the
// render goroutine, and that is the intended deployment.
type Store struct {
	mu sync.Mutex

	max     int
	seq     uint64 // monotonic arrival counter; also positions threads
	threads map[string]*thread
	roster  map[string]RosterEntry

	// dedup maps a post's identity to the thread holding it, so that evicting a
	// thread also forgets its dedup keys. Without that link the dedup set would
	// be the unbounded thing the cap exists to prevent.
	dedup map[string]string

	duplicates     uint64
	evicted        uint64
	unknownVersion uint64
	versions       map[int]int
}

type thread struct {
	key     string
	root    *msg.MsgBoardPost
	replies []*msg.MsgBoardPost

	// order positions the thread among its peers. It is the arrival sequence of
	// the ROOT once the root exists, and of the first orphan before that — so a
	// re-parented thread moves to where the root's own arrival implies, exactly
	// as if the reply had never arrived early.
	order uint64

	dedupKeys []string
}

func (t *thread) count() int {
	n := len(t.replies)
	if t.root != nil {
		n++
	}
	return n
}

// New returns an empty Store bounded at max posts. A max <= 0 means MaxPosts.
func New(max int) *Store {
	if max <= 0 {
		max = MaxPosts
	}
	return &Store{
		max:      max,
		threads:  make(map[string]*thread),
		roster:   make(map[string]RosterEntry),
		dedup:    make(map[string]string),
		versions: make(map[int]int),
	}
}

// Apply files one post. It reports whether the post was accepted; false means
// it was a duplicate of one already held (or nil).
//
// Nothing is rejected for being unfamiliar. An unknown Kind renders as-is —
// Kind is free-form by contract, because fleetctl's --kind already is. An
// unknown schema version is counted and kept.
func (s *Store) Apply(p *msg.MsgBoardPost) bool {
	if p == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dk := dedupKey(p)
	if _, dup := s.dedup[dk]; dup {
		s.duplicates++
		return false
	}

	s.seq++
	seq := s.seq

	s.versions[p.V]++
	if p.V != msg.BoardSchemaVersion {
		s.unknownVersion++
	}
	sanitisePersona(p)

	key, isRoot := classify(p, seq)

	t := s.threads[key]
	if t == nil {
		t = &thread{key: key, order: seq}
		s.threads[key] = t
	}

	switch {
	case isRoot && t.root == nil:
		t.root = p
		// Re-position: the thread belongs where its root arrived, not where an
		// early orphan happened to land.
		t.order = seq
	case isRoot:
		// The thread's opener posting again under its own key. It is not a
		// second root — a thread has exactly one — so it files as a reply.
		t.replies = append(t.replies, p)
	default:
		t.replies = append(t.replies, p)
	}

	t.dedupKeys = append(t.dedupKeys, dk)
	s.dedup[dk] = key

	s.evictLocked()
	return true
}

// ApplyEvent files whichever message an Event carries, so a consumer draining a
// Subscriber does not have to switch on the kind itself.
func (s *Store) ApplyEvent(ev Event) {
	switch {
	case ev.Post != nil:
		s.Apply(ev.Post)
	case ev.Register != nil:
		s.Register(ev.Register)
	}
}

// Register records a persona announcement. Registration is ADVISORY: a post
// from a pane that never registered is still rendered (see Apply). A board that
// drops messages from agents which forgot to introduce themselves is worse than
// one with a thin roster.
//
// Last announcement wins — a pane that is renamed re-announces.
func (s *Store) Register(r *msg.MsgBoardRegister) {
	if r == nil || r.PaneID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	persona := r.Persona
	if containsUnitSep(persona) {
		persona = msg.BoardPersona("", "", r.PaneID)
	}
	s.roster[r.PaneID] = RosterEntry{
		PaneID:  r.PaneID,
		Persona: persona,
		TS:      r.TS,
	}
}

// Threads returns every thread in arrival order — roots by their own arrival,
// replies in the order they arrived under the root.
func (s *Store) Threads() []Thread {
	s.mu.Lock()
	defer s.mu.Unlock()

	ordered := make([]*thread, 0, len(s.threads))
	for _, t := range s.threads {
		ordered = append(ordered, t)
	}
	// Insertion sort on `order`: the slice is nearly sorted in practice (only a
	// re-parented thread moves) and this avoids pulling in a comparator for a
	// key that is a plain uint64.
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && ordered[j-1].order > ordered[j].order; j-- {
			ordered[j-1], ordered[j] = ordered[j], ordered[j-1]
		}
	}

	out := make([]Thread, 0, len(ordered))
	for _, t := range ordered {
		replies := make([]*msg.MsgBoardPost, len(t.replies))
		copy(replies, t.replies)
		out = append(out, Thread{
			Key:         t.key,
			Root:        t.root,
			Replies:     replies,
			Provisional: t.root == nil,
		})
	}
	return out
}

// Roster returns the announced agents, ordered by pane id so a view renders a
// stable list.
func (s *Store) Roster() []RosterEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RosterEntry, 0, len(s.roster))
	for _, e := range s.roster {
		out = append(out, e)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].PaneID > out[j].PaneID; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Stats reports the counters a view needs to disclose what it is not showing.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{
		Threads:        len(s.threads),
		Duplicates:     s.duplicates,
		Evicted:        s.evicted,
		UnknownVersion: s.unknownVersion,
	}
	for _, t := range s.threads {
		st.Posts += t.count()
		if t.root == nil {
			st.Provisional++
		}
	}
	return st
}

// VersionCounts reports how many posts arrived at each schema version, so a
// view (or an operator) can see a fleet mid-upgrade rather than guess.
func (s *Store) VersionCounts() map[int]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int]int, len(s.versions))
	for v, n := range s.versions {
		out[v] = n
	}
	return out
}

// evictLocked drops whole threads, oldest first, until the store is within its
// cap. Caller holds the mutex.
func (s *Store) evictLocked() {
	total := 0
	for _, t := range s.threads {
		total += t.count()
	}
	for total > s.max {
		var oldest *thread
		for _, t := range s.threads {
			if oldest == nil || t.order < oldest.order {
				oldest = t
			}
		}
		if oldest == nil {
			return
		}
		n := oldest.count()
		total -= n
		s.evicted += uint64(n)
		for _, dk := range oldest.dedupKeys {
			delete(s.dedup, dk)
		}
		delete(s.threads, oldest.key)
	}
}

// classify decides a post's thread key and whether the post is that thread's
// root.
//
// THIS IS THE ONE RULE THAT MAKES BLIND THREADING WORK, so it is spelled out.
// The contract has no "is-root" flag; it has ThreadID and PaneID. Thread ids are
// minted by the poster as MintThreadID(paneID, n) = "<pane-uuid>/<n>"
// (internal/msg/messages_board.go), so the key CARRIES ITS OWNER. Therefore:
//
//   - ThreadID == ""            -> a standalone post. It is a root that can
//     never be replied to, because no agent can address it. Keyed internally.
//   - ThreadID owned by PaneID  -> the poster opened this thread, so this post
//     is its ROOT (the first such post; a later one from the same opener is a
//     reply to its own thread).
//   - ThreadID owned by another -> a REPLY. If its root has not arrived, the
//     thread stands provisionally until it does.
//
// The alternative — deriving a root's key from "the Nth root this pane posted"
// — was rejected: it couples every later reply to an ordinal that shifts if a
// single post is evicted or dropped (both are documented behaviours), and the
// mis-parenting would be silent.
func classify(p *msg.MsgBoardPost, seq uint64) (key string, isRoot bool) {
	if p.ThreadID == "" {
		return standaloneKey(seq), true
	}
	return p.ThreadID, ownsThread(p.PaneID, p.ThreadID)
}

// standaloneKey names a thread no agent can address. The prefix is a byte an
// agent cannot put in a pane uuid, so it can never collide with a minted id.
func standaloneKey(seq uint64) string {
	var b strings.Builder
	b.WriteByte(0)
	b.WriteString("standalone/")
	// seq is already unique per store.
	writeUint(&b, seq)
	return b.String()
}

func writeUint(b *strings.Builder, n uint64) {
	if n == 0 {
		b.WriteByte('0')
		return
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	b.Write(buf[i:])
}

// ownsThread reports whether threadID was minted by paneID, i.e. whether it has
// the MintThreadID shape "<paneID>/<n>".
func ownsThread(paneID, threadID string) bool {
	if paneID == "" {
		return false
	}
	rest, ok := strings.CutPrefix(threadID, paneID+"/")
	return ok && rest != ""
}

// dedupKey identifies a post for idempotency.
//
// WHY THESE FIELDS ARE ENOUGH: a retry re-sends the SAME message — NewBoardPost
// stamps TS once, at creation — so pane + clock + thread + kind + text is stable
// across retries. Two genuinely distinct posts that collide would have to come
// from the same pane, in the same millisecond, in the same thread, with the same
// kind and byte-identical text; collapsing those two is the lesser error.
//
// WHERE IT IS NOT ENOUGH, stated rather than implied: a producer that REBUILDS
// the message on retry (calling NewBoardPost again, taking a fresh clock) defeats
// this entirely and its post will appear twice. Producers must retry the message
// they already built. The board has no auth (design 025 §7.5), so it also cannot
// tell a retry from a deliberate replay by a third party.
func dedupKey(p *msg.MsgBoardPost) string {
	var b strings.Builder
	b.Grow(len(p.PaneID) + len(p.ThreadID) + len(p.Kind) + len(p.Text) + 24)
	b.WriteString(p.PaneID)
	b.WriteByte(0)
	writeUint(&b, uint64(p.TS))
	b.WriteByte(0)
	b.WriteString(p.ThreadID)
	b.WriteByte(0)
	b.WriteString(p.Kind)
	b.WriteByte(0)
	b.WriteString(p.Text)
	return b.String()
}

// sanitisePersona repairs a display name the store must never render.
//
// Approval panes OVERLOAD GivenName to carry "requestID\x1FresponseSubject"
// (internal/actors/approval_pane.go), so a producer that reads a pane's
// given-name naively can put a NATS subject in Persona. msg.BoardPersona guards
// the producer side; this guards the store, because the store is what every view
// renders from and a second view must not have to remember the rule.
func sanitisePersona(p *msg.MsgBoardPost) {
	if containsUnitSep(p.Persona) {
		p.Persona = msg.BoardPersona("", "", p.PaneID)
	}
	if p.Persona == "" {
		p.Persona = msg.BoardPersona("", "", p.PaneID)
	}
}

func containsUnitSep(s string) bool {
	return strings.IndexByte(s, 0x1f) >= 0
}

// RetainRoster drops roster entries for panes that no longer exist.
//
// Registration is persistent (founder gate 2), so without this the roster
// accumulates GHOSTS: a pane that registered, then closed — or that belonged to
// an earlier session entirely — keeps its entry forever. Observed live before
// this existed: a board reporting "3 agents" in a session with two, the third
// being a pane id from a previous run.
//
// A roster that overcounts is worse than no roster, by the same argument that a
// stale name is worse than no name — it looks authoritative and is wrong. Live
// pane ids are therefore authoritative for WHO EXISTS, while registrations stay
// authoritative for WHAT THEY ARE CALLED.
//
// An empty or nil set is treated as "caller does not know" and retains
// everything, so a momentarily empty snapshot cannot wipe the roster.
func (s *Store) RetainRoster(livePaneIDs map[string]bool) {
	if len(livePaneIDs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.roster {
		if !livePaneIDs[id] {
			delete(s.roster, id)
		}
	}
}
