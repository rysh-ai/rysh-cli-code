// SPDX-License-Identifier: Apache-2.0

package board

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Durability (founder gate 2, 2026-08-08): the board SURVIVES A DAEMON RESTART.
//
// It rides the JetStream KV that internal/bus already provisions — no second
// NATS server, no second store, no sidecar file. One KV entry per post, keyed by
// a zero-padded arrival ordinal so that listing the keys in lexical order
// replays the board in the order it actually happened.
//
// THE RETENTION BOUND, because an unbounded board is a defect:
//
//   - RetentionTTL = 7 days per entry, enforced by JetStream itself. A fleet
//     that ran last month is archaeology, not monitoring; a week survives a
//     weekend outage and any plausible restart.
//   - RetentionMaxBytes = 64 MiB on the bucket, so a runaway poster bounds the
//     disk rather than the disk bounding the machine.
//   - Restore replays at most MaxPosts (5000) — the newest entries. The store's
//     in-memory cap is the same number, so *what you can see is what is kept*,
//     and a restore cannot be slower than a cold board is big.
//
// WHAT IS DURABLE AND WHAT IS STILL LOST — stated exactly, because the earlier
// non-goal "no persistence beyond the session" is now wrong and must not be
// replaced by an over-claim:
//
//   - DURABLE: every post a running board process receives. The KV write is
//     UNCONDITIONAL on the NATS callback — it is never inside the render
//     buffer's drop branch — so a full render buffer costs a live update, never
//     the record. Subscriber.Dropped therefore means "not shown live", not
//     "lost": the view can recover those from the KV.
//   - STILL LOST, and this limit stands: posts published while NO board process
//     is running. Publishing is plain-NATS fire-and-forget (msg.SendBoardPost →
//     nats.Conn.Publish), which has no store-and-forward; nobody subscribed
//     means nobody wrote. Also lost: anything NATS itself drops when it declares
//     a slow consumer.
//   - STILL LOST: everything older than RetentionTTL, and everything past
//     RetentionMaxBytes or the MaxPosts restore window.
const (
	// RetentionTTL is how long one board entry lives in the KV.
	RetentionTTL = 7 * 24 * time.Hour
	// RetentionMaxBytes bounds the bucket on disk.
	RetentionMaxBytes = 64 << 20
)

// BucketName is the KV bucket for a session's board. Namespaced by session
// exactly like the buckets in internal/bus, so several sessions can share one
// NATS server without sharing a board.
func BucketName(session string) string {
	if session == "" {
		session = "default"
	}
	return "rysh-board-" + session
}

// BucketConfig is the bucket the bus should create for the board. It is here,
// next to the retention constants, so the numbers and the thing they configure
// cannot drift apart.
func BucketConfig(session string) *nats.KeyValueConfig {
	return &nats.KeyValueConfig{
		Bucket:   BucketName(session),
		Storage:  nats.FileStorage,
		TTL:      RetentionTTL,
		MaxBytes: RetentionMaxBytes,
	}
}

// KV is the slice of nats.KeyValue this package uses. Narrow on purpose: it
// keeps internal/board out of the bus's dependency tree and makes the store
// testable against a fake as well as against a real JetStream.
type KV interface {
	Put(key string, value []byte) (uint64, error)
	Get(key string) (nats.KeyValueEntry, error)
	Keys(opts ...nats.WatchOpt) ([]string, error)
}

// Persistence writes board messages to a KV and replays them on restart.
//
// The zero value is not usable; the nil *Persistence is, and does nothing —
// that is what a board running without JetStream gets, and it degrades to a
// live-only board rather than refusing to start.
// ONE PERSISTENCE PER BOARD, AND THE ORDINAL IS PER BOARD TOO (design 028).
//
// Two Persistence values over one board that read the KV at the same moment
// agree on the next free ordinal and both write that key: the second Put
// OVERWRITES the first, so a second writer does not duplicate history, it
// destroys it (TestTwoWritersThatBothPrimedFirstStillDestroyHistory).
//
// prime() narrows the hazard to that concurrent case — a writer created LATER
// continues where the previous one stopped instead of restarting at 1 — but it
// cannot close it, and nothing here should be read as making a second writer
// safe. Board ids do not weaken the rule either, they SCOPE it: the invariant
// is one writer per BOARD, which is why the key prefix and the ordinal move
// together.
type Persistence struct {
	kv    KV
	board string // "" for the default board — see keyPrefix

	mu     sync.Mutex
	seq    uint64 // arrival ordinal, continued across restarts by Restore/prime
	primed bool   // has the ordinal been read back from the KV yet?
}

// NewPersistence returns a Persistence over kv for one board. A nil kv yields a
// nil Persistence, whose methods are no-ops.
//
// board is a board id (msg.DefaultBoardID / "" for the session board). An
// INVALID id is treated as the default board rather than being allowed to
// become a key prefix, on the same defence-in-depth argument as the subject
// builders: the edges validate, and this bounds what happens if one did not.
func NewPersistence(kv KV, board string) *Persistence {
	if kv == nil {
		return nil
	}
	if msg.ValidateBoardID(board) != nil {
		board = msg.DefaultBoardID
	}
	return &Persistence{kv: kv, board: msg.NormalizeBoardID(board)}
}

const (
	postKeyPrefix = "post-"
	regKeyPrefix  = "reg-"
	// boardKeyPrefix namespaces a NAMED board's keys inside the session's
	// bucket: "b.<id>.post-…". The default board keeps the flat "post-…" keys
	// it has always had, so a bucket written before board ids existed restores
	// exactly as it did — a prefix on every key would have made every existing
	// board's history invisible on upgrade while looking like an empty board.
	boardKeyPrefix = "b."
	// keyDigits zero-pads the ordinal so lexical key order IS arrival order.
	// 18 digits is more ordinals than a session can produce.
	keyDigits = 18
)

// keyPrefix is what this board's keys start with: "" for the default board,
// "b.<id>." for a named one.
func (p *Persistence) keyPrefix() string {
	if p == nil || p.board == "" || p.board == msg.DefaultBoardID {
		return ""
	}
	return boardKeyPrefix + p.board + "."
}

func (p *Persistence) postKey(seq uint64) string {
	return fmt.Sprintf("%s%s%0*d", p.keyPrefix(), postKeyPrefix, keyDigits, seq)
}

// regKey is derived from the pane uuid so re-registration overwrites rather
// than accumulating. KV keys may not contain every byte a persona might, but a
// pane uuid is already KV-safe.
func (p *Persistence) regKey(paneID string) string {
	return p.keyPrefix() + regKeyPrefix + paneID
}

// owns reports whether a key belongs to this board.
//
// The default board's test is not just "has no b. prefix": it must also reject
// every NAMED board's keys, which is what the boardKeyPrefix check does. Get
// this wrong in the lenient direction and the session board silently replays 25
// fleets' history as its own.
func (p *Persistence) owns(key string) bool {
	pre := p.keyPrefix()
	if pre == "" {
		return !strings.HasPrefix(key, boardKeyPrefix)
	}
	return strings.HasPrefix(key, pre)
}

// SavePost persists one post. Called unconditionally from the subscriber's NATS
// callback — never from inside the render buffer's drop branch — so a full
// render buffer never costs durability.
func (p *Persistence) SavePost(post *msg.MsgBoardPost) error {
	if p == nil || post == nil {
		return nil
	}
	data, err := json.Marshal(post)
	if err != nil {
		return err
	}
	// Prime BEFORE taking the next ordinal. A Persistence created for a board
	// that already has history starts at seq 0, and its first write would land
	// on post-…0001 — a key that is already there. Restore primes it when the
	// caller restores first; this covers the caller that does not, which is the
	// live path for a board whose first message arrives before anything has
	// read its history back.
	p.prime()

	p.mu.Lock()
	p.seq++
	key := p.postKey(p.seq)
	p.mu.Unlock()

	_, err = p.kv.Put(key, data)
	return err
}

// prime reads this board's highest ordinal back from the KV, once.
//
// It is the cheap half of Restore: keys only, no Get, no decode, nothing
// applied to a store. That matters because it runs on the NATS callback
// goroutine (SavePost) the first time a board is written to, and the expensive
// half — replaying history into a store — belongs to whoever owns the store.
func (p *Persistence) prime() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.primed {
		p.mu.Unlock()
		return
	}
	p.primed = true
	p.mu.Unlock()

	keys, err := p.kv.Keys()
	if err != nil {
		// ErrNoKeysFound is the normal first-run case; any other error leaves
		// the ordinal at 0, which is the same position a fresh board starts
		// from. A write that then collides is visible as a lost post rather
		// than as a corrupted one, and the alternative — refusing to record
		// because the bucket could not be listed — loses strictly more.
		return
	}
	var maxSeq uint64
	for _, k := range keys {
		if !p.owns(k) {
			continue
		}
		if n, perr := p.parsePostKey(k); perr == nil && n > maxSeq {
			maxSeq = n
		}
	}
	p.mu.Lock()
	if maxSeq > p.seq {
		p.seq = maxSeq
	}
	p.mu.Unlock()
}

// SaveRegister persists a persona announcement, keyed by pane so the roster
// does not grow with every re-announcement.
func (p *Persistence) SaveRegister(r *msg.MsgBoardRegister) error {
	if p == nil || r == nil || r.PaneID == "" {
		return nil
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = p.kv.Put(p.regKey(r.PaneID), data)
	return err
}

// Restore replays the persisted board into s and returns how many posts and
// registrations were applied.
//
// Posts replay in key order, which is arrival order, so threading and orphan
// re-parenting reconstruct exactly as they happened live. Only the newest
// MaxPosts entries are read: the store would evict anything older on the way in
// anyway, so fetching them would be work whose only product is garbage.
//
// A key that fails to fetch or decode is SKIPPED, not fatal. A board that
// refuses to start because one entry went bad is worse than a board missing one
// message, and the count returned tells the caller what it really got.
func (p *Persistence) Restore(s *Store) (posts int, registrations int, err error) {
	if p == nil || s == nil {
		return 0, 0, nil
	}
	keys, err := p.kv.Keys()
	if err != nil {
		// An empty bucket reports ErrNoKeysFound rather than an empty slice.
		// A board with no history is the normal first-run case, not an error.
		if err == nats.ErrNoKeysFound {
			return 0, 0, nil
		}
		return 0, 0, err
	}

	// THIS BOARD'S KEYS ONLY. One bucket holds every board in the session, so a
	// restore that did not filter would replay every fleet's history onto
	// whichever board restored first — and it would look like a busy board
	// rather than like a bug.
	pre := p.keyPrefix()
	var postKeys, regKeys []string
	for _, k := range keys {
		if !p.owns(k) {
			continue
		}
		rest := strings.TrimPrefix(k, pre)
		switch {
		case strings.HasPrefix(rest, postKeyPrefix):
			postKeys = append(postKeys, k)
		case strings.HasPrefix(rest, regKeyPrefix):
			regKeys = append(regKeys, k)
		}
	}
	sort.Strings(postKeys)
	if len(postKeys) > MaxPosts {
		postKeys = postKeys[len(postKeys)-MaxPosts:]
	}

	// Registrations first: the roster should be populated before the posts that
	// reference it, so a view never renders a frame with a known agent missing.
	for _, k := range regKeys {
		entry, gerr := p.kv.Get(k)
		if gerr != nil {
			continue
		}
		var r msg.MsgBoardRegister
		if json.Unmarshal(entry.Value(), &r) != nil {
			continue
		}
		s.Register(&r)
		registrations++
	}

	var maxSeq uint64
	for _, k := range postKeys {
		entry, gerr := p.kv.Get(k)
		if gerr != nil {
			continue
		}
		var post msg.MsgBoardPost
		if json.Unmarshal(entry.Value(), &post) != nil {
			continue
		}
		if s.Apply(&post) {
			posts++
		}
		if n, perr := p.parsePostKey(k); perr == nil && n > maxSeq {
			maxSeq = n
		}
	}

	// Continue the ordinal where the previous process left off, so a restart
	// cannot rewrite entries that are still live in the bucket. Derived from the
	// keys we actually saw, including any beyond the replay window — and from
	// this board's keys only, because another board's higher ordinal says
	// nothing about where this one may safely write.
	for _, k := range keys {
		if !p.owns(k) {
			continue
		}
		if n, perr := p.parsePostKey(k); perr == nil && n > maxSeq {
			maxSeq = n
		}
	}
	p.mu.Lock()
	p.primed = true
	if maxSeq > p.seq {
		p.seq = maxSeq
	}
	p.mu.Unlock()

	return posts, registrations, nil
}

// parsePostKey reads the arrival ordinal out of one of THIS board's post keys.
// A key belonging to another board is not a parse failure to be logged, it is
// simply not ours — owns() is the caller's guard and this repeats it so a new
// call site cannot skip it.
func (p *Persistence) parsePostKey(k string) (uint64, error) {
	want := p.keyPrefix() + postKeyPrefix
	if !p.owns(k) || !strings.HasPrefix(k, want) {
		return 0, fmt.Errorf("not a post key for board %q: %q", p.board, k)
	}
	var n uint64
	if _, err := fmt.Sscanf(k[len(want):], "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}
