// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/memory"
	"github.com/rysh-ai/rysh-cli-code/internal/memory/recall"
	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

type fakeEmb struct{ calls int }

func (e *fakeEmb) Dim() int   { return 8 }
func (e *fakeEmb) ID() string { return "fake:8" }
func (e *fakeEmb) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.calls += len(texts)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 8)
		v[0] = 1
		v[1+len(t)%7] = 1
		out[i] = v
	}
	return out, nil
}

func semanticRules() policy.MemoryRules {
	r := policy.Empty().MemoryRules()
	r.Semantic = policy.SemanticRules{Enabled: true, Model: "m", Dim: 8}
	return r
}

// TestSemanticSearchIsOffUnlessPolicyEnablesIt (design 010 §3 S4).
func TestSemanticSearchIsOffUnlessPolicyEnablesIt(t *testing.T) {
	s, _ := memorySetup(t)
	built := 0
	s.newEmbedder = func(policy.SemanticRules) (recall.Embedder, error) { built++; return &fakeEmb{}, nil }
	if _, err := s.semanticSearch(context.Background(), "x", 3); !errors.Is(err, errSemanticOff) {
		t.Errorf("default policy: err = %v, want errSemanticOff", err)
	}
	if built != 0 {
		t.Errorf("an embedder was built with semantic memory off")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.MemoryStore().Dir()), "vecindex.json")); err == nil {
		t.Errorf("index file written with semantic memory off")
	}
}

// TestSemanticSourcesSkipWithheldEntries: what is indexed is what the
// scrubber lets through; a withheld (credential-shaped) entry never is.
func TestSemanticSearchIndexesEntriesAndSkipsWithheld(t *testing.T) {
	s, store := memorySetup(t)
	emb := &fakeEmb{}
	s.newEmbedder = func(policy.SemanticRules) (recall.Embedder, error) { return emb, nil }
	s.SetMemoryRules(semanticRules())
	e, err := store.Add(memory.KindFact, "Deploy with the rollback script.", memory.SourceUser, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A hand-edited entry holding a credential shape is withheld.
	bad := "api" + "_key: " + strings.Repeat("Zx9", 12)
	if err := os.WriteFile(filepath.Join(store.Dir(), "leak.md"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	hits, err := s.semanticSearch(context.Background(), "deploy", 5)
	if err != nil {
		t.Fatalf("semanticSearch: %v", err)
	}
	var sawEntry bool
	for _, h := range hits {
		if h.Source == "memory:leak" || strings.Contains(h.Text, "Zx9") {
			t.Errorf("withheld entry was indexed")
		}
		if h.Source == "memory:"+e.ID {
			sawEntry = true
		}
	}
	if !sawEntry {
		t.Errorf("entry %s not found among %d hits", e.ID, len(hits))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(store.Dir()), "vecindex.json")); err != nil {
		t.Errorf("index file not saved: %v", err)
	}
	// Turning it off again stops search.
	s.SetMemoryRules(policy.Empty().MemoryRules())
	if _, err := s.semanticSearch(context.Background(), "x", 3); !errors.Is(err, errSemanticOff) {
		t.Errorf("after turning off: err = %v", err)
	}
}

// TestRecallBlockIsOptInCitedAndCapped (design 010 §3 S5).
func TestRecallBlockIsOptInCitedAndCapped(t *testing.T) {
	s, store := memorySetup(t)
	s.newEmbedder = func(policy.SemanticRules) (recall.Embedder, error) { return &fakeEmb{}, nil }
	if _, err := store.Add(memory.KindFact, "Deploy with the rollback script.", memory.SourceUser, nil); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Semantic on, auto-recall off (the default): nothing.
	s.SetMemoryRules(semanticRules())
	if got := s.recallBlock(ctx, "deploy"); got != "" {
		t.Errorf("auto_recall off: got a block %q", got)
	}

	r := semanticRules()
	r.Semantic.AutoRecall, r.Semantic.AutoRecallBytes = true, 200
	s.SetMemoryRules(r)
	got := s.recallBlock(ctx, "deploy")
	if !strings.Contains(got, "memory:") || !strings.Contains(got, "rollback") {
		t.Errorf("auto_recall on: block %q lacks the cited entry", got)
	}
	if len(got) > 200 {
		t.Errorf("block is %d bytes, cap 200", len(got))
	}

	// An embedder failure gives no block, never an error in the prompt.
	s.newEmbedder = func(policy.SemanticRules) (recall.Embedder, error) { return nil, errors.New("no ollama") }
	r.Semantic.Model = "other" // new rules key: the indexer is rebuilt
	s.SetMemoryRules(r)
	if got := s.recallBlock(ctx, "deploy"); got != "" {
		t.Errorf("embedder failure: got %q, want no block", got)
	}
}

// TestSemanticIndexerIsSharedByProviderCopies: humanoids and meeting agents
// copy the Setup to swap the provider. The copy must reuse the same indexer
// (one writer per vecindex.json), not a second one guarded by its own lock.
func TestSemanticIndexerIsSharedByProviderCopies(t *testing.T) {
	s, _ := memorySetup(t)
	s.newEmbedder = func(policy.SemanticRules) (recall.Embedder, error) { return &fakeEmb{}, nil }
	r := semanticRules().Semantic
	path := filepath.Join(t.TempDir(), "vecindex.json")
	// Copy before anything is opened, as setupForProvider does at any time.
	cp := new(Setup)
	*cp = *s
	ix1, err := s.semanticIndexer(r, path)
	if err != nil {
		t.Fatal(err)
	}
	ix2, err := cp.semanticIndexer(r, path)
	if err != nil {
		t.Fatal(err)
	}
	if ix1 != ix2 {
		t.Errorf("copied Setup opened a second indexer")
	}
	// A Setup built without memory (bare literal) reports search as off.
	if _, err := (&Setup{}).semanticIndexer(r, path); !errors.Is(err, errSemanticOff) {
		t.Errorf("bare Setup: err = %v, want errSemanticOff", err)
	}
}
