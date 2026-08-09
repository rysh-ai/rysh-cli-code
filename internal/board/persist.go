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
type Persistence struct {
	kv KV

	mu  sync.Mutex
	seq uint64 // arrival ordinal, continued across restarts by Restore
}

// NewPersistence returns a Persistence over kv. A nil kv yields a nil
// Persistence, whose methods are no-ops.
func NewPersistence(kv KV) *Persistence {
	if kv == nil {
		return nil
	}
	return &Persistence{kv: kv}
}

const (
	postKeyPrefix = "post-"
	regKeyPrefix  = "reg-"
	// keyDigits zero-pads the ordinal so lexical key order IS arrival order.
	// 18 digits is more ordinals than a session can produce.
	keyDigits = 18
)

func postKey(seq uint64) string {
	return fmt.Sprintf("%s%0*d", postKeyPrefix, keyDigits, seq)
}

// regKey is derived from the pane uuid so re-registration overwrites rather
// than accumulating. KV keys may not contain every byte a persona might, but a
// pane uuid is already KV-safe.
func regKey(paneID string) string {
	return regKeyPrefix + paneID
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
	p.mu.Lock()
	p.seq++
	key := postKey(p.seq)
	p.mu.Unlock()

	_, err = p.kv.Put(key, data)
	return err
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
	_, err = p.kv.Put(regKey(r.PaneID), data)
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

	var postKeys, regKeys []string
	for _, k := range keys {
		switch {
		case strings.HasPrefix(k, postKeyPrefix):
			postKeys = append(postKeys, k)
		case strings.HasPrefix(k, regKeyPrefix):
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
		if n, perr := parsePostKey(k); perr == nil && n > maxSeq {
			maxSeq = n
		}
	}

	// Continue the ordinal where the previous process left off, so a restart
	// cannot rewrite entries that are still live in the bucket. Derived from the
	// keys we actually saw, including any beyond the replay window.
	for _, k := range keys {
		if n, perr := parsePostKey(k); perr == nil && n > maxSeq {
			maxSeq = n
		}
	}
	p.mu.Lock()
	if maxSeq > p.seq {
		p.seq = maxSeq
	}
	p.mu.Unlock()

	return posts, registrations, nil
}

func parsePostKey(k string) (uint64, error) {
	if !strings.HasPrefix(k, postKeyPrefix) {
		return 0, fmt.Errorf("not a post key: %q", k)
	}
	var n uint64
	if _, err := fmt.Sscanf(k[len(postKeyPrefix):], "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}
