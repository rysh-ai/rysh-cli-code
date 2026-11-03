// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/memory"
)

func boundMemoryTool(t *testing.T, enabled bool) (*MemoryTool, *memory.Store) {
	t.Helper()
	store := memory.Open(filepath.Join(t.TempDir(), ".rysh"))
	tool := NewMemoryTool()
	tool.Bind(store, func() (bool, string) {
		if enabled {
			return true, ""
		}
		return false, "memory is disabled by policy"
	}, nil)
	return tool, store
}

func runMemoryTool(t *testing.T, tool *MemoryTool, params string) *ToolOutput {
	t.Helper()
	out, err := tool.Execute(context.Background(), json.RawMessage(params))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return out
}

func TestMemoryToolAddAndList(t *testing.T) {
	tool, store := boundMemoryTool(t, true)
	out := runMemoryTool(t, tool, `{"action":"add","kind":"fact","text":"integration tests need a throwaway Postgres"}`)
	if out.Error != "" {
		t.Fatalf("add failed: %s", out.Error)
	}
	list, _ := store.List(nil)
	if len(list) != 1 || list[0].Source != memory.SourceAgent || list[0].Kind != memory.KindFact {
		t.Fatalf("stored entries = %+v", list)
	}
	out = runMemoryTool(t, tool, `{"action":"list"}`)
	if !strings.Contains(out.Content, "throwaway Postgres") || !strings.Contains(out.Content, list[0].ID) {
		t.Fatalf("list output does not show the entry and its id")
	}
}

func TestMemoryToolAddAsksForApprovalAndListDoesNot(t *testing.T) {
	tool, _ := boundMemoryTool(t, true)
	if !tool.RequiresApproval(json.RawMessage(`{"action":"add","text":"x"}`)) {
		t.Fatalf("an agent write to cross-session memory must ask for approval")
	}
	if tool.RequiresApproval(json.RawMessage(`{"action":"list"}`)) {
		t.Fatalf("list is read-only")
	}
	if !tool.RequiresApproval(json.RawMessage(`not json`)) {
		t.Fatalf("unparseable params must fail closed (ask)")
	}
}

func TestMemoryToolCannotForget(t *testing.T) {
	tool, _ := boundMemoryTool(t, true)
	out := runMemoryTool(t, tool, `{"action":"forget","id":"x"}`)
	if out.Error == "" {
		t.Fatalf("forget must not be available to the agent")
	}
}

func TestMemoryToolRefusesCredentialShapedText(t *testing.T) {
	tool, store := boundMemoryTool(t, true)
	params, _ := json.Marshal(map[string]string{"action": "add", "text": "deploy key " + "gh" + "p_" + strings.Repeat("aB3", 12)})
	out := runMemoryTool(t, tool, string(params))
	if out.Error == "" || !strings.Contains(out.Error, memory.RulePrefix) {
		t.Fatalf("credential-shaped add not refused with its rule")
	}
	if strings.Contains(out.Error+out.Content, "aB3aB3") {
		t.Fatalf("the refusal echoed the value")
	}
	if list, _ := store.List(nil); len(list) != 0 {
		t.Fatalf("refused add stored %d entries", len(list))
	}
}

func TestMemoryToolFailsClosedWhenDisabledOrUnbound(t *testing.T) {
	tool, store := boundMemoryTool(t, false)
	out := runMemoryTool(t, tool, `{"action":"add","text":"x y z"}`)
	if out.Error == "" || !strings.Contains(out.Error, "disabled") {
		t.Fatalf("disabled memory accepted a write")
	}
	if _, err := os.Stat(store.Dir()); err == nil {
		t.Fatalf("disabled memory created the store")
	}
	unbound := NewMemoryTool()
	if out := runMemoryTool(t, unbound, `{"action":"list"}`); out.Error == "" {
		t.Fatalf("an unbound tool must refuse")
	}
}

// With memory off (policy says so, or failed to load) the model must not read
// entries either: list is gated like add.
func TestMemoryToolListFailsClosedWhenDisabled(t *testing.T) {
	store := memory.Open(filepath.Join(t.TempDir(), ".rysh"))
	if _, err := store.Add(memory.KindFact, "the staging host is called kestrel", memory.SourceUser, nil); err != nil {
		t.Fatal(err)
	}
	tool := NewMemoryTool()
	tool.Bind(store, func() (bool, string) { return false, "the policy failed to load" }, nil)
	out := runMemoryTool(t, tool, `{"action":"list"}`)
	if out.Error == "" || !strings.Contains(out.Error, "disabled") {
		t.Fatalf("list with memory off must refuse")
	}
	if strings.Contains(out.Content, "kestrel") || strings.Contains(out.Error, "kestrel") {
		t.Fatalf("list with memory off leaked an entry")
	}
}
