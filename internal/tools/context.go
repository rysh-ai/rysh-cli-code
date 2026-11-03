// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/memory/vecindex"
)

// DefaultSearchK is how many hits action=search returns when k is omitted.
const DefaultSearchK = 5

// SearchFunc answers action=search (design 010 §3 S4). Nil means semantic
// memory is off.
type SearchFunc func(ctx context.Context, query string, k int) ([]vecindex.Hit, error)

// ContextTool unifies the former context_store and context_recall tools behind
// a single `action` switch (store | recall | list). It delegates to the
// underlying executors so the persistence logic lives in one place.
type ContextTool struct {
	store  *ContextStoreTool
	recall *ContextRecallTool
	search SearchFunc
}

// SetSearch wires action=search; nil leaves it refusing.
func (t *ContextTool) SetSearch(f SearchFunc) { t.search = f }

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
    "action": {"type": "string", "enum": ["store", "recall", "list", "search"], "description": "store=save a key/value; recall=read a key (or keys with a prefix); list=list all keys; search=meaning-based search over remembered memories and RYSH.md, cited (only when semantic memory is enabled by policy)"},
    "query": {"type": "string", "description": "What to look for (action=search)"},
    "k": {"type": "integer", "description": "How many results, 1-20 (action=search, default 5)"},
    "key": {"type": "string", "description": "Key name for store/recall (alphanumeric, dots, dashes, underscores)"},
    "value": {"type": "string", "description": "Value to store (action=store)"},
    "prefix": {"type": "string", "description": "List keys with this prefix (action=recall/list)"}
  },
  "required": ["action"]
}`)
	return ToolSpec{
		Name:             "context",
		Description:      "Durable per-session key/value scratch memory (JetStream KV). action=store saves key→value; action=recall reads a key or a prefix; action=list lists keys; action=search finds remembered memories and RYSH.md passages by meaning, each cited by source (off unless policy memory.semantic.enabled).",
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
		Query  string `json:"query,omitempty"`
		K      int    `json:"k,omitempty"`
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
	case "search":
		return t.runSearch(ctx, p.Query, p.K), nil
	default:
		return ErrOutputf(ErrKindValidation, "unknown action %q (use store|recall|list|search)", p.Action), nil
	}
}

// runSearch answers action=search with cited hits: "source#heading (score)"
// then the passage, so the model and the user can check where it came from.
func (t *ContextTool) runSearch(ctx context.Context, query string, k int) *ToolOutput {
	if t.search == nil {
		return ErrOutput(ErrKindValidation, "semantic memory is off; enable it with policy memory.semantic.enabled (design 010 §3)")
	}
	if k == 0 {
		k = DefaultSearchK
	}
	hits, err := t.search(ctx, query, k)
	if err != nil {
		return ErrOutputf(ErrKindValidation, "search failed: %v", err)
	}
	if len(hits) == 0 {
		return &ToolOutput{Content: "No remembered passage matches."}
	}
	var b strings.Builder
	for i, h := range hits {
		cite := h.Source
		if h.Heading != "" {
			cite += "#" + h.Heading
		}
		fmt.Fprintf(&b, "%d. %s (%.2f)\n%s\n\n", i+1, cite, h.Score, strings.TrimSpace(h.Text))
	}
	return &ToolOutput{Content: strings.TrimRight(b.String(), "\n")}
}
