// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/memory"
	"github.com/rysh-ai/rysh-cli-code/internal/memory/recall"
	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// qa coverage proposal 2 (2026-11-01): memory outlives a daemon restart. An
// entry added in one daemon is, after a restart (a new Setup over the same
// .rysh dir), still in the system prompt and still reaches the next turn's
// prompt as the S5 auto-recall block, and the saved index is reused: only the
// query is embedded again, not the unchanged entry.
func TestMemorySurvivesARestartIntoPromptAndRecall(t *testing.T) {
	ryshDir := filepath.Join(t.TempDir(), ".rysh")
	workDir := t.TempDir()
	rules := semanticRules()
	rules.Semantic.AutoRecall, rules.Semantic.AutoRecallBytes = true, 400
	ctx := context.Background()

	daemon := func(emb *fakeEmb) (*Setup, *memory.Store) {
		store := memory.Open(ryshDir)
		s := &Setup{workDir: workDir}
		s.initMemory(store, policy.Empty().MemoryRules(), "base prompt")
		s.newEmbedder = func(policy.SemanticRules) (recall.Embedder, error) { return emb, nil }
		s.SetMemoryRules(rules)
		return s, store
	}

	first, store := daemon(&fakeEmb{})
	e, err := store.Add(memory.KindFact, "Deploy with the rollback script.", memory.SourceUser, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := first.recallBlock(ctx, "deploy"); !strings.Contains(got, "memory:"+e.ID) {
		t.Fatalf("before restart: recall block %q lacks memory:%s", got, e.ID)
	}

	// Restart: nothing carried over but the .rysh dir.
	emb := &fakeEmb{}
	second, _ := daemon(emb)
	second.RebuildMemory()
	if !strings.Contains(second.SystemPrompt, "Deploy with the rollback script.") || !strings.Contains(second.SystemPrompt, "["+e.ID+"]") {
		t.Errorf("after restart the system prompt lacks entry %s", e.ID)
	}
	got := second.recallBlock(ctx, "deploy")
	if !strings.Contains(got, "memory:"+e.ID) || !strings.Contains(got, "rollback") {
		t.Errorf("after restart: recall block %q lacks the cited entry", got)
	}
	if emb.calls != 1 {
		t.Errorf("after restart %d texts were embedded, want 1 (the query; the saved index holds the entry)", emb.calls)
	}
}
