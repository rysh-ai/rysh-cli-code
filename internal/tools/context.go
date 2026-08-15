// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"
)

// ContextTool unifies the former context_store and context_recall tools behind
// a single `action` switch (store | recall | list). It delegates to the
// underlying executors so the persistence logic lives in one place.
type ContextTool struct {
	store  *ContextStoreTool
	recall *ContextRecallTool
}

// NewContextTool builds a ContextTool backed by the given JetStream context.
func NewContextTool(js nats.JetStreamContext) *ContextTool {
	return &ContextTool{
		store:  NewContextStoreTool(js),
		recall: NewContextRecallTool(js),
	}
}

// Spec returns the unified tool specification for the LLM.
func (t *ContextTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["store", "recall", "list"], "description": "store=save a key/value; recall=read a key (or keys with a prefix); list=list all keys"},
    "key": {"type": "string", "description": "Key name for store/recall (alphanumeric, dots, dashes, underscores)"},
    "value": {"type": "string", "description": "Value to store (action=store)"},
    "prefix": {"type": "string", "description": "List keys with this prefix (action=recall/list)"}
  },
  "required": ["action"]
}`)
	return ToolSpec{
		Name:             "context",
		Description:      "Durable per-session key/value scratch memory (JetStream KV). action=store saves key→value; action=recall reads a key or a prefix; action=list lists keys.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval is always false — context storage is non-destructive.
func (t *ContextTool) RequiresApproval(_ json.RawMessage) bool { return false }

// Execute dispatches to the store or recall executor based on `action`.
func (t *ContextTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p struct {
		Action string `json:"action"`
		Key    string `json:"key,omitempty"`
		Value  string `json:"value,omitempty"`
		Prefix string `json:"prefix,omitempty"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid context params: %w", err)
	}

	switch p.Action {
	case "store":
		sub, _ := json.Marshal(ContextStoreParams{Key: p.Key, Value: p.Value})
		return t.store.Execute(ctx, sub)
	case "recall":
		sub, _ := json.Marshal(ContextRecallParams{Key: p.Key, Prefix: p.Prefix})
		return t.recall.Execute(ctx, sub)
	case "list":
		sub, _ := json.Marshal(ContextRecallParams{Prefix: p.Prefix, List: true})
		return t.recall.Execute(ctx, sub)
	default:
		return ErrOutputf(ErrKindValidation, "unknown action %q (use store|recall|list)", p.Action), nil
	}
}
