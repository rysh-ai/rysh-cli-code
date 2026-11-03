// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/memory/embed"
	"github.com/rysh-ai/rysh-cli-code/internal/memory/recall"
	"github.com/rysh-ai/rysh-cli-code/internal/memory/vecindex"
	"github.com/rysh-ai/rysh-cli-code/internal/policy"
	"github.com/rysh-ai/rysh-cli-code/internal/tools"
)

// Semantic recall (design 010 §3, slice S4). Off unless the resolved policy
// says memory.semantic.enabled; then the context tool's action=search syncs
// the index with the memory sources and answers cited top-k queries.

// errSemanticOff is returned while policy keeps semantic memory off.
var errSemanticOff = errors.New("semantic memory is off (policy memory.semantic.enabled)")

// semanticState holds the indexer for the semantic rules it was built under;
// a rules change (model, dim, URL, remote) opens a new one.
type semanticState struct {
	mu  sync.Mutex
	key string
	ix  *recall.Indexer
}

// newContextTool builds the context tool with action=search bound to this
// Setup. The hook checks policy on every call, so turning semantic memory
// on or off takes effect without new panes.
func (s *Setup) newContextTool(js nats.JetStreamContext) *tools.ContextTool {
	t := tools.NewContextTool(js)
	t.SetSearch(s.semanticSearch)
	return t
}

// defaultEmbedder is the loopback Ollama embedder (S3). A remote host is
// allowed only when policy says allow_remote.
func defaultEmbedder(r policy.SemanticRules) (recall.Embedder, error) {
	var opts []embed.Option
	if r.AllowRemote {
		opts = append(opts, embed.AllowRemoteHost())
	}
	return embed.NewOllama(r.EmbedderURL, r.Model, r.Dim, opts...)
}

// semanticSearch answers action=search, or errSemanticOff.
func (s *Setup) semanticSearch(ctx context.Context, query string, k int) ([]vecindex.Hit, error) {
	r := s.MemoryRules()
	store := s.MemoryStore()
	if !r.Semantic.Enabled || store == nil {
		return nil, errSemanticOff
	}
	ix, err := s.semanticIndexer(r.Semantic, filepath.Join(filepath.Dir(store.Dir()), vecindex.FileName))
	if err != nil {
		return nil, err
	}
	return ix.Search(ctx, s.semanticSources(), query, k)
}

func (s *Setup) semanticIndexer(r policy.SemanticRules, path string) (*recall.Indexer, error) {
	if s.sem == nil {
		return nil, errSemanticOff
	}
	s.sem.mu.Lock()
	defer s.sem.mu.Unlock()
	key := fmt.Sprintf("%+v", r)
	if s.sem.ix != nil && s.sem.key == key {
		return s.sem.ix, nil
	}
	build := s.newEmbedder
	if build == nil {
		build = defaultEmbedder
	}
	emb, err := build(r)
	if err != nil {
		return nil, fmt.Errorf("semantic memory: %w", err)
	}
	s.sem.ix, s.sem.key = recall.Open(path, emb), key
	return s.sem.ix, nil
}

// semanticSources is what gets indexed: the memory entries the scrubber lets
// through (a withheld entry never is) and the RYSH.md files already loaded
// into the prompt. Nothing else (no pane history or output).
func (s *Setup) semanticSources() []recall.Source {
	var out []recall.Source
	if store := s.MemoryStore(); store != nil {
		if entries, err := store.List(s.knownSecrets()); err == nil {
			for _, e := range entries {
				if e.Withheld == "" && e.Text != "" {
					out = append(out, recall.Source{Name: "memory:" + e.ID, Text: e.Text})
				}
			}
		}
	}
	global, project := projectMemoryPaths(s.workDir)
	for _, p := range append([]string{global}, project...) {
		if p == "" {
			continue
		}
		if text := readMemoryFile(p); text != "" {
			out = append(out, recall.Source{Name: p, Text: text})
		}
	}
	return out
}

// autoRecallK / autoRecallMinScore: how many passages auto-recall considers,
// and the cosine below which a passage is not worth a prompt's bytes.
const (
	autoRecallK        = 3
	autoRecallMinScore = 0.3
)

// recallBlock is the per-turn auto-recall block (design 010 §3 S5): the best
// cited passages for this prompt, within policy's byte cap, or "". It is ""
// unless both semantic memory and auto_recall are on, and on any error (a
// missing embedder must never break or delay a prompt beyond the actor's
// deadline).
func (s *Setup) recallBlock(ctx context.Context, prompt string) string {
	r := s.MemoryRules().Semantic
	if !r.Enabled || !r.AutoRecall || strings.TrimSpace(prompt) == "" {
		return ""
	}
	hits, err := s.semanticSearch(ctx, prompt, autoRecallK)
	if err != nil {
		return ""
	}
	header := "# Recalled from memory (automatic, cited; a suggestion, verify before relying on it)\n"
	var b strings.Builder
	for _, h := range hits {
		if h.Score < autoRecallMinScore {
			continue
		}
		cite := h.Source
		if h.Heading != "" {
			cite += "#" + h.Heading
		}
		line := fmt.Sprintf("- %s (%.2f): %s\n", cite, h.Score, strings.Join(strings.Fields(h.Text), " "))
		if len(header)+b.Len()+len(line) > r.AutoRecallBytes {
			break
		}
		b.WriteString(line)
	}
	if b.Len() == 0 {
		return ""
	}
	return header + strings.TrimRight(b.String(), "\n")
}
