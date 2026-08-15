// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

const contextKVBucket = "rysh-context"

// ContextStoreTool stores key-value pairs in JetStream KV for cross-turn persistence.
type ContextStoreTool struct {
	js nats.JetStreamContext
}

// ContextStoreParams holds the parameters for storing context.
type ContextStoreParams struct {
	Key   string `json:"key"`   // key name (alphanumeric, dots, dashes, underscores)
	Value string `json:"value"` // value to store
}

// NewContextStoreTool creates a ContextStoreTool with the given JetStream context.
func NewContextStoreTool(js nats.JetStreamContext) *ContextStoreTool {
	return &ContextStoreTool{js: js}
}

// Spec returns the tool specification for the LLM.
func (t *ContextStoreTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "key": {"type": "string", "description": "Key name for the stored value (e.g., 'project.goal', 'user.preference')"},
    "value": {"type": "string", "description": "Value to store"}
  },
  "required": ["key", "value"]
}`)
	return ToolSpec{
		Name:             "context_store",
		Description:      "Store a key-value pair that persists across conversation turns. Useful for remembering findings, decisions, or state during long tasks.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval always returns false.
func (t *ContextStoreTool) RequiresApproval(_ json.RawMessage) bool {
	return false
}

// Execute stores the key-value pair.
func (t *ContextStoreTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p ContextStoreParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid context_store params: %w", err)
	}

	if p.Key == "" {
		return ErrOutput(ErrKindValidation, "key is required"), nil
	}
	if p.Value == "" {
		return ErrOutput(ErrKindValidation, "value is required"), nil
	}

	if t.js == nil {
		return ErrOutput(ErrKindMissing, "JetStream not available"), nil
	}

	kv, err := t.getOrCreateBucket()
	if err != nil {
		return ErrOutputf(ErrKindInternal, "failed to access context store: %v", err), nil
	}

	// Store with timestamp metadata.
	entry := contextEntry{
		Value:     p.Value,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return ErrOutputf(ErrKindInternal, "failed to marshal value: %v", err), nil
	}

	if _, err := kv.Put(p.Key, data); err != nil {
		return ErrOutputf(ErrKindInternal, "failed to store key '%s': %v", p.Key, err), nil
	}

	return &ToolOutput{
		Content: fmt.Sprintf("Stored: %s = %s", p.Key, truncateStr(p.Value, 200)),
		Metadata: map[string]string{
			"key": p.Key,
		},
	}, nil
}

func (t *ContextStoreTool) getOrCreateBucket() (nats.KeyValue, error) {
	kv, err := t.js.KeyValue(contextKVBucket)
	if err == nil {
		return kv, nil
	}
	return t.js.CreateKeyValue(&nats.KeyValueConfig{
		Bucket:  contextKVBucket,
		Storage: nats.FileStorage,
	})
}

type contextEntry struct {
	Value     string `json:"value"`
	UpdatedAt string `json:"updated_at"`
}

// ContextRecallTool retrieves values from the context KV store.
type ContextRecallTool struct {
	js nats.JetStreamContext
}

// ContextRecallParams holds the parameters for recalling context.
type ContextRecallParams struct {
	Key    string `json:"key,omitempty"`    // specific key to recall
	Prefix string `json:"prefix,omitempty"` // list keys with this prefix
	List   bool   `json:"list,omitempty"`   // list all keys
}

// NewContextRecallTool creates a ContextRecallTool with the given JetStream context.
func NewContextRecallTool(js nats.JetStreamContext) *ContextRecallTool {
	return &ContextRecallTool{js: js}
}

// Spec returns the tool specification for the LLM.
func (t *ContextRecallTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "key": {"type": "string", "description": "Specific key to recall"},
    "prefix": {"type": "string", "description": "List all keys with this prefix (e.g., 'project.')"},
    "list": {"type": "boolean", "description": "List all stored keys and values"}
  }
}`)
	return ToolSpec{
		Name:             "context_recall",
		Description:      "Recall previously stored key-value pairs. Retrieve a specific key, filter by prefix, or list all stored context.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval always returns false — read-only.
func (t *ContextRecallTool) RequiresApproval(_ json.RawMessage) bool {
	return false
}

// Execute retrieves the requested context.
func (t *ContextRecallTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p ContextRecallParams
	if params != nil {
		_ = json.Unmarshal(params, &p)
	}

	if t.js == nil {
		return ErrOutput(ErrKindMissing, "JetStream not available"), nil
	}

	kv, err := t.js.KeyValue(contextKVBucket)
	if err != nil {
		return &ToolOutput{Content: "(no context stored yet)"}, nil
	}

	// Specific key.
	if p.Key != "" {
		entry, err := kv.Get(p.Key)
		if err != nil {
			return &ToolOutput{Content: fmt.Sprintf("Key '%s' not found.", p.Key)}, nil
		}
		var ce contextEntry
		if json.Unmarshal(entry.Value(), &ce) == nil {
			return &ToolOutput{
				Content: fmt.Sprintf("%s = %s\n(updated: %s)", p.Key, ce.Value, ce.UpdatedAt),
				Metadata: map[string]string{
					"key":        p.Key,
					"updated_at": ce.UpdatedAt,
				},
			}, nil
		}
		return &ToolOutput{Content: fmt.Sprintf("%s = %s", p.Key, string(entry.Value()))}, nil
	}

	// List all or filter by prefix.
	keys, err := kv.Keys()
	if err != nil {
		return &ToolOutput{Content: "(no context stored yet)"}, nil
	}

	var sb strings.Builder
	count := 0
	for _, key := range keys {
		if p.Prefix != "" && !strings.HasPrefix(key, p.Prefix) {
			continue
		}
		entry, err := kv.Get(key)
		if err != nil {
			continue
		}
		var ce contextEntry
		if json.Unmarshal(entry.Value(), &ce) == nil {
			sb.WriteString(fmt.Sprintf("%s = %s\n", key, ce.Value))
		} else {
			sb.WriteString(fmt.Sprintf("%s = %s\n", key, string(entry.Value())))
		}
		count++
	}

	if count == 0 {
		if p.Prefix != "" {
			return &ToolOutput{Content: fmt.Sprintf("No keys with prefix '%s' found.", p.Prefix)}, nil
		}
		return &ToolOutput{Content: "(no context stored yet)"}, nil
	}

	return &ToolOutput{
		Content: sb.String(),
		Metadata: map[string]string{
			"count": fmt.Sprintf("%d", count),
		},
	}, nil
}
