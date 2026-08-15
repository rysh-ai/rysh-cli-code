// SPDX-License-Identifier: Apache-2.0

package fleet

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// Durability: the registry SURVIVES A DAEMON RESTART, and that is most of the
// reason it moved out of `.rysh/fleet/*.json` (design 028 §6.5).
//
// One KV entry per fleet, keyed by name. Unlike the board's history there is no
// ordinal and no ordering to preserve — a fleet is a record that is overwritten
// in place, so a second writer here costs a lost UPDATE rather than deleted
// history. That is a real difference and it is why this file is short: the
// board's whole single-writer apparatus exists to protect an append-only
// sequence, and this is not one.
//
// RETENTION IS DELIBERATELY LONGER THAN THE BOARD'S. Board history is
// monitoring and ages out in a week; a fleet's identity is what lets a human
// resume `claude -r <session_id>` in a worktree weeks later, and losing it
// turns a set of live agents into anonymous panes. It is bounded rather than
// unbounded because a session that registers and forgets fleets forever should
// still not grow without limit.
const (
	// RetentionTTL is how long one fleet record lives without being rewritten.
	RetentionTTL = 90 * 24 * time.Hour
	// RetentionMaxBytes bounds the bucket on disk.
	RetentionMaxBytes = 8 << 20
)

// BucketName is the KV bucket for a session's fleet registry, namespaced by
// session exactly like every other bucket in internal/bus.
func BucketName(session string) string {
	if session == "" {
		session = "default"
	}
	return "rysh-fleet-" + session
}

// BucketConfig is the bucket the bus should create. It lives here, beside the
// retention constants, so the numbers and the thing they configure cannot drift
// apart.
func BucketConfig(session string) *nats.KeyValueConfig {
	return &nats.KeyValueConfig{
		Bucket:   BucketName(session),
		Storage:  nats.FileStorage,
		TTL:      RetentionTTL,
		MaxBytes: RetentionMaxBytes,
	}
}

// KV is the slice of nats.KeyValue this package uses. Narrow on purpose: it
// keeps internal/fleet out of the bus's dependency tree and makes the store
// testable against a fake as well as against a real JetStream.
type KV interface {
	Put(key string, value []byte) (uint64, error)
	Get(key string) (nats.KeyValueEntry, error)
	Delete(key string, opts ...nats.DeleteOpt) error
	Keys(opts ...nats.WatchOpt) ([]string, error)
}

// Persistence writes fleet records to a KV and replays them on restart.
//
// The nil *Persistence is usable and does nothing — that is what a session
// without JetStream gets, and it degrades to a registry that forgets on restart
// rather than a session that refuses to start.
type Persistence struct {
	kv KV
	mu sync.Mutex
}

// NewPersistence returns a Persistence over kv. A nil kv yields a nil
// Persistence, whose methods are no-ops.
func NewPersistence(kv KV) *Persistence {
	if kv == nil {
		return nil
	}
	return &Persistence{kv: kv}
}

const fleetKeyPrefix = "fleet-"

func fleetKey(name string) string { return fleetKeyPrefix + name }

// Save writes one fleet record, overwriting the previous one.
func (p *Persistence) Save(f *Fleet) error {
	if p == nil || f == nil {
		return nil
	}
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err = p.kv.Put(fleetKey(f.Name), data)
	return err
}

// Delete removes one fleet record. A missing key is not an error: forgetting a
// fleet that was never persisted is the same outcome the caller asked for.
func (p *Persistence) Delete(name string) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.kv.Delete(fleetKey(name)); err != nil && err != nats.ErrKeyNotFound {
		return err
	}
	return nil
}

// Restore replays every persisted fleet into r and returns how many were
// applied.
//
// A record that fails to fetch or decode is SKIPPED, not fatal, and the count
// returned is what really loaded — the same rule the board's restore follows. A
// registry that refuses to start because one fleet's record went bad is worse
// than one missing a fleet, because it takes every other fleet down with it.
func (p *Persistence) Restore(r *Registry) (int, error) {
	if p == nil || r == nil {
		return 0, nil
	}
	keys, err := p.kv.Keys()
	if err != nil {
		// An empty bucket reports ErrNoKeysFound rather than an empty slice. A
		// session with no fleets is the normal first-run case, not an error.
		if err == nats.ErrNoKeysFound {
			return 0, nil
		}
		return 0, err
	}

	loaded := 0
	for _, k := range keys {
		if !strings.HasPrefix(k, fleetKeyPrefix) {
			continue
		}
		entry, gerr := p.kv.Get(k)
		if gerr != nil {
			continue
		}
		var f Fleet
		if json.Unmarshal(entry.Value(), &f) != nil {
			continue
		}
		// Through Register, not straight into the map, so a record written by
		// an older or newer build is held to the same validation as a live
		// registration — and a fleet whose name stopped being a legal board id
		// is dropped here rather than surfacing later as a board nobody can
		// subscribe to.
		if _, rerr := r.Register(f); rerr != nil {
			continue
		}
		// Register keeps existing members and ignores the incoming ones, which
		// is right for a re-registration and wrong for a restore: on a cold
		// registry there is nothing to keep, so the members have to be put back
		// explicitly.
		for _, m := range f.Members {
			_ = r.UpsertMember(f.Name, m)
		}
		loaded++
	}
	return loaded, nil
}
