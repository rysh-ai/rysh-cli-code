// SPDX-License-Identifier: Apache-2.0

// Package pipeline provides a thread-safe runtime registry for pipeline
// prompts.  Both the tools package (which writes entries) and the actors
// package (which reads them) import this package, avoiding an import cycle.
package pipeline

import (
	"encoding/json"
	"sync"

	"github.com/nats-io/nats.go"
)

var (
	mu      sync.RWMutex
	prompts = map[string]string{}
)

// RegisterPrompt adds or overwrites a prompt in the pipeline overlay.
// The key format is "language:phase" and the prompt must contain two %s
// placeholders (source pane alias, accumulated context) matching the
// convention of softdevPrompts in the actors package.
func RegisterPrompt(key, prompt string) {
	mu.Lock()
	defer mu.Unlock()
	prompts[key] = prompt
}

// ClearPrompts removes all entries from the pipeline overlay.
func ClearPrompts() {
	mu.Lock()
	defer mu.Unlock()
	prompts = map[string]string{}
}

// LookupPrompt returns the prompt for the given key, if one has been
// registered.  Thread-safe for concurrent reads.
func LookupPrompt(key string) (string, bool) {
	mu.RLock()
	defer mu.RUnlock()
	p, ok := prompts[key]
	return p, ok
}

// PersistToKV writes all pipeline prompts to the given KV bucket.
func PersistToKV(kv nats.KeyValue) error {
	mu.RLock()
	defer mu.RUnlock()
	data, err := json.Marshal(prompts)
	if err != nil {
		return err
	}
	_, err = kv.Put("prompts", data)
	return err
}

// RestoreFromKV loads pipeline prompts from the given KV bucket.
func RestoreFromKV(kv nats.KeyValue) error {
	entry, err := kv.Get("prompts")
	if err != nil {
		return err // nats.ErrKeyNotFound is fine — caller should handle
	}
	var loaded map[string]string
	if err := json.Unmarshal(entry.Value(), &loaded); err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()
	for k, v := range loaded {
		prompts[k] = v
	}
	return nil
}
