// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/memory/vecindex"
)

func runContext(t *testing.T, tool *ContextTool, params string) *ToolOutput {
	t.Helper()
	out, err := tool.Execute(context.Background(), json.RawMessage(params))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return out
}

// TestContextSearchIsOffByDefault: with no search wired (semantic memory off
// by policy, design 010 §3 S4) action=search refuses and says why.
func TestContextSearchIsOffByDefault(t *testing.T) {
	tool := NewContextTool(nil)
	out := runContext(t, tool, `{"action":"search","query":"deploy"}`)
	if out.Error == "" || !strings.Contains(out.Error, "memory.semantic") {
		t.Errorf("search with nothing wired: error = %q, want a refusal naming memory.semantic", out.Error)
	}
	if !strings.Contains(string(tool.Spec().Parameters), `"search"`) {
		t.Errorf("schema does not offer action=search")
	}
}

func TestContextSearchCitesHits(t *testing.T) {
	tool := NewContextTool(nil)
	var gotQ string
	var gotK int
	tool.SetSearch(func(_ context.Context, q string, k int) ([]vecindex.Hit, error) {
		gotQ, gotK = q, k
		return []vecindex.Hit{
			{Source: "RYSH.md", Heading: "Deploy", Text: "Deploy with the rollback script.", Score: 0.91},
			{Source: "memory:build", Text: "Build with make test.", Score: 0.42},
		}, nil
	})
	out := runContext(t, tool, `{"action":"search","query":"how to deploy","k":2}`)
	if out.Error != "" {
		t.Fatalf("search: %s", out.Error)
	}
	if gotQ != "how to deploy" || gotK != 2 {
		t.Errorf("hook got (%q, %d)", gotQ, gotK)
	}
	for _, want := range []string{"RYSH.md#Deploy", "0.91", "rollback script", "memory:build"} {
		if !strings.Contains(out.Content, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	// Default k when omitted.
	runContext(t, tool, `{"action":"search","query":"x"}`)
	if gotK != DefaultSearchK {
		t.Errorf("default k = %d, want %d", gotK, DefaultSearchK)
	}
	// A hook error is an error output, not a crash.
	tool.SetSearch(func(context.Context, string, int) ([]vecindex.Hit, error) { return nil, errors.New("embedder down") })
	if out := runContext(t, tool, `{"action":"search","query":"x"}`); out.Error == "" {
		t.Errorf("hook error not reported")
	}
}
