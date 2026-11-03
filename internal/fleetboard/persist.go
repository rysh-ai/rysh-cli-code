// SPDX-License-Identifier: Apache-2.0

package fleetboard

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// Storage (design 030 §9.2): KV rysh-fleetboard-<session>, one key per
// fleet, file storage, the same retention as the fleet registry. Rows are
// rebuilt from here at start, so the board does not depend on ABLA's lazy
// restore of named boards.
const (
	RetentionTTL      = 90 * 24 * time.Hour
	RetentionMaxBytes = 8 << 20
)

// BucketName is the KV bucket for one session.
func BucketName(session string) string {
	if session == "" {
		session = "default"
	}
	return "rysh-fleetboard-" + session
}

// BucketConfig is the KV config for one session.
func BucketConfig(session string) *nats.KeyValueConfig {
	return &nats.KeyValueConfig{
		Bucket:   BucketName(session),
		Storage:  nats.FileStorage,
		TTL:      RetentionTTL,
		MaxBytes: RetentionMaxBytes,
	}
}

// KV is the slice of nats.KeyValue this package uses.
type KV interface {
	Put(key string, value []byte) (uint64, error)
	Get(key string) (nats.KeyValueEntry, error)
	Delete(key string, opts ...nats.DeleteOpt) error
	Keys(opts ...nats.WatchOpt) ([]string, error)
}

// Persistence writes rows to a KV. The nil *Persistence is usable and does
// nothing: a session without JetStream gets a board that forgets on restart.
type Persistence struct {
	kv KV
	mu sync.Mutex
}

// NewPersistence returns a Persistence over kv, or nil for a nil kv.
func NewPersistence(kv KV) *Persistence {
	if kv == nil {
		return nil
	}
	return &Persistence{kv: kv}
}

const rowKeyPrefix = "fb-"

// Save writes one row.
func (p *Persistence) Save(r Row) error {
	if p == nil || r.Fleet == "" {
		return nil
	}
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err = p.kv.Put(rowKeyPrefix+r.Fleet, data)
	return err
}

// Delete removes one fleet's row. A missing key is not an error.
func (p *Persistence) Delete(fleet string) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.kv.Delete(rowKeyPrefix + fleet); err != nil && err != nats.ErrKeyNotFound {
		return err
	}
	return nil
}

// LoadAll reads every stored row. A record that fails to fetch or decode is
// skipped, not fatal.
func (p *Persistence) LoadAll() ([]Row, error) {
	if p == nil {
		return nil, nil
	}
	keys, err := p.kv.Keys()
	if err != nil {
		if err == nats.ErrNoKeysFound {
			return nil, nil
		}
		return nil, err
	}
	var rows []Row
	for _, k := range keys {
		if !strings.HasPrefix(k, rowKeyPrefix) {
			continue
		}
		e, gerr := p.kv.Get(k)
		if gerr != nil {
			continue
		}
		var r Row
		if json.Unmarshal(e.Value(), &r) != nil || r.Fleet == "" {
			continue
		}
		rows = append(rows, r)
	}
	return rows, nil
}
