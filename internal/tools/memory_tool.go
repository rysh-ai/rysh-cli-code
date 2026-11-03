// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/memory"
)

// MemoryTool lets the agent keep short facts, preferences and notes across
// sessions (design 010 §3a). The agent may add (each add asks the user for
// approval, because an entry is injected into every later session) and list.
// The tool has no forget: that is ##memory forget (which an agent can reach
// only through rysh_command, with approval) or deleting the file.
type MemoryTool struct {
	store *memory.Store
	gate  func() (bool, string)
	known func() []string
}

// MemoryToolParams holds the parameters for memory operations.
type MemoryToolParams struct {
	Action string `json:"action"`         // "add", "list"
	Kind   string `json:"kind,omitempty"` // fact | preference | note (add; default note)
	Text   string `json:"text,omitempty"` // the memory (add)
}

// NewMemoryTool creates an unbound tool; it refuses until Bind is called.
func NewMemoryTool() *MemoryTool { return &MemoryTool{} }

// Bind wires the workspace store, the policy gate (enabled, reason) and the
// session's registered secret values for the scrubber. Called once, before
// any pane uses the tool.
func (t *MemoryTool) Bind(store *memory.Store, gate func() (bool, string), known func() []string) {
	t.store, t.gate, t.known = store, gate, known
}

// Spec returns the tool specification for the LLM.
func (t *MemoryTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["add", "list"], "description": "add (remember one short item; the user approves it) or list (what is remembered)"},
    "kind": {"type": "string", "enum": ["fact", "preference", "note"], "description": "What the item is (add; default note)"},
    "text": {"type": "string", "description": "The item to remember, one short statement (add; at most 1 KB)"}
  },
  "required": ["action"]
}`)
	return ToolSpec{
		Name: "memory",
		Description: "Remember short facts, user preferences and notes across sessions in this workspace (injected into later sessions). " +
			"Use it for stable things worth knowing next time, never for secrets (credential-shaped text is refused) or transient task state. " +
			"The user lists and forgets entries with ##memory.",
		Parameters:       schema,
		RequiresApproval: true,
	}
}

// RequiresApproval asks before every add; list is read-only. Unparseable
// params ask (fail closed).
func (t *MemoryTool) RequiresApproval(params json.RawMessage) bool {
	var p MemoryToolParams
	if json.Unmarshal(params, &p) != nil {
		return true
	}
	return p.Action != "list"
}

// Execute performs the memory operation.
func (t *MemoryTool) Execute(_ context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p MemoryToolParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid memory params: %w", err)
	}
	if t.store == nil || t.gate == nil {
		return ErrOutput(ErrKindPermissionDenied, "cross-session memory is not available in this session"), nil
	}
	// Memory off (by policy, or because the policy failed to load) means no
	// writes and no reads: the model sees nothing it would not see injected.
	if ok, reason := t.gate(); !ok {
		return ErrOutputf(ErrKindPermissionDenied, "memory is disabled: %s", reason), nil
	}
	var known []string
	if t.known != nil {
		known = t.known()
	}
	switch p.Action {
	case "list":
		list, err := t.store.List(known)
		if err != nil {
			return ErrOutputf(ErrKindInternal, "could not read memory: %v", err), nil
		}
		return &ToolOutput{Content: renderMemoryList(list)}, nil
	case "add":
		kind := memory.Kind(strings.TrimSpace(p.Kind))
		if kind == "" {
			kind = memory.KindNote
		}
		e, err := t.store.Add(kind, p.Text, memory.SourceAgent, known)
		if err != nil {
			return ErrOutputf(ErrKindValidation, "not remembered: %v", err), nil
		}
		return &ToolOutput{
			Content:  fmt.Sprintf("Remembered as %s [%s]. The user can remove it with ##memory forget %s.", e.Kind, e.ID, e.ID),
			Metadata: map[string]string{"id": e.ID},
		}, nil
	default:
		return ErrOutputf(ErrKindValidation, "invalid action %q (add or list; only the user can forget)", p.Action), nil
	}
}

// renderMemoryList prints one line per entry; a withheld entry shows its rule,
// never its content.
func renderMemoryList(list []memory.Entry) string {
	if len(list) == 0 {
		return "(nothing remembered in this workspace yet)"
	}
	var b strings.Builder
	for _, e := range list {
		if e.Withheld != "" {
			fmt.Fprintf(&b, "- [%s] (%s) withheld: %s\n", e.ID, e.Kind, e.Withheld)
			continue
		}
		fmt.Fprintf(&b, "- [%s] (%s) %s\n", e.ID, e.Kind, strings.Join(strings.Fields(e.Text), " "))
	}
	return strings.TrimRight(b.String(), "\n")
}
