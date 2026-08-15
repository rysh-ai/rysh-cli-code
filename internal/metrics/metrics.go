// SPDX-License-Identifier: Apache-2.0

// Package metrics implements an in-process metrics sink for the rysh
// agentic system. It satisfies rysh-shared/agentic.MetricsSink and
// exposes a simple text dump usable from `##agent metrics`.
//
// Follow-up item 3, MVP form: in-memory aggregation. A Prometheus
// exporter is a natural drop-in via the same interface (add a
// PrometheusSink that wraps client_golang's Registry); kept out of this
// commit to avoid pulling in a new dependency.
package metrics

import (
	"fmt"
	"sort"
	"sync"
	"time"

	provider "github.com/rysh-ai/rysh-cli-shared/provider"
	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// Sink is an in-process MetricsSink. Safe for concurrent use.
type Sink struct {
	mu sync.Mutex

	tools       map[string]*toolStats
	llms        map[string]*llmStats
	compactions int
	compactSum  time.Duration
	approvals   map[string]int // "decision" -> count
}

// New constructs a fresh in-memory sink.
func New() *Sink {
	return &Sink{
		tools:     make(map[string]*toolStats),
		llms:      make(map[string]*llmStats),
		approvals: make(map[string]int),
	}
}

// toolStats tracks per-tool aggregates. We keep the durations and output
// sizes as sorted slices so quantiles are exact (cheap at the ~1000s of
// calls we expect per session).
type toolStats struct {
	calls       int
	errors      int
	errKinds    map[string]int
	durations   []time.Duration
	outputBytes []int
}

func (t *toolStats) record(dur time.Duration, out *sharedtools.ToolOutput, err error) {
	t.calls++
	t.durations = append(t.durations, dur)
	if out != nil {
		t.outputBytes = append(t.outputBytes, len(out.Content))
		if out.Error != "" {
			t.errors++
			kind := out.ErrorKind
			if kind == "" {
				kind = "unknown"
			}
			if t.errKinds == nil {
				t.errKinds = make(map[string]int)
			}
			t.errKinds[kind]++
		}
	}
	if err != nil {
		t.errors++
		if t.errKinds == nil {
			t.errKinds = make(map[string]int)
		}
		t.errKinds["internal"]++
	}
}

// llmStats tracks per-model token + duration aggregates.
type llmStats struct {
	calls     int
	durations []time.Duration
	inputTok  int
	outputTok int
	cacheRead int
	cacheNew  int
}

// RecordTool implements agentic.MetricsSink.
func (s *Sink) RecordTool(name string, dur time.Duration, out *sharedtools.ToolOutput, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.tools[name]
	if !ok {
		ts = &toolStats{}
		s.tools[name] = ts
	}
	ts.record(dur, out, err)
}

// RecordLLMCall implements agentic.MetricsSink.
func (s *Sink) RecordLLMCall(model string, dur time.Duration, usage provider.Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ls, ok := s.llms[model]
	if !ok {
		ls = &llmStats{}
		s.llms[model] = ls
	}
	ls.calls++
	ls.durations = append(ls.durations, dur)
	ls.inputTok += usage.InputTokens
	ls.outputTok += usage.OutputTokens
	ls.cacheRead += usage.CacheReadInputTokens
	ls.cacheNew += usage.CacheCreationInputTokens
}

// RecordCompaction implements agentic.MetricsSink.
func (s *Sink) RecordCompaction(droppedTurns int, dur time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.compactions++
	s.compactSum += dur
}

// RecordApproval implements agentic.MetricsSink.
func (s *Sink) RecordApproval(toolName, decision string, dur time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.approvals[decision]++
}

// Dump returns a human-readable text summary of accumulated metrics.
// Used by the `##agent metrics` command.
func (s *Sink) Dump() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out string

	// Tools, alphabetised.
	toolNames := make([]string, 0, len(s.tools))
	for n := range s.tools {
		toolNames = append(toolNames, n)
	}
	sort.Strings(toolNames)

	if len(toolNames) > 0 {
		out += "[tools]\n"
		out += fmt.Sprintf("  %-22s %6s  %8s  %8s  %8s  %8s  %s\n",
			"name", "calls", "p50", "p95", "p99", "errors", "error_kinds")
		for _, n := range toolNames {
			ts := s.tools[n]
			p50, p95, p99 := percentile(ts.durations, 0.50), percentile(ts.durations, 0.95), percentile(ts.durations, 0.99)
			out += fmt.Sprintf("  %-22s %6d  %8s  %8s  %8s  %8d  %s\n",
				n, ts.calls, p50, p95, p99, ts.errors, kindsDisplay(ts.errKinds))
		}
		out += "\n"
	}

	// LLM, alphabetised by model.
	llmNames := make([]string, 0, len(s.llms))
	for n := range s.llms {
		llmNames = append(llmNames, n)
	}
	sort.Strings(llmNames)
	if len(llmNames) > 0 {
		out += "[llm]\n"
		out += fmt.Sprintf("  %-26s %6s  %8s  %12s  %12s  %12s  %12s\n",
			"model", "calls", "p95", "input_tok", "output_tok", "cache_read", "cache_new")
		for _, n := range llmNames {
			ls := s.llms[n]
			out += fmt.Sprintf("  %-26s %6d  %8s  %12d  %12d  %12d  %12d\n",
				n, ls.calls, percentile(ls.durations, 0.95),
				ls.inputTok, ls.outputTok, ls.cacheRead, ls.cacheNew)
		}
		out += "\n"
	}

	if s.compactions > 0 {
		mean := s.compactSum / time.Duration(s.compactions)
		out += fmt.Sprintf("[compaction] events=%d  total=%s  mean=%s\n\n", s.compactions, s.compactSum, mean)
	}
	if len(s.approvals) > 0 {
		out += "[approvals]\n"
		decisions := make([]string, 0, len(s.approvals))
		for d := range s.approvals {
			decisions = append(decisions, d)
		}
		sort.Strings(decisions)
		for _, d := range decisions {
			out += fmt.Sprintf("  %s: %d\n", d, s.approvals[d])
		}
	}
	if out == "" {
		out = "(no metrics collected yet)\n"
	}
	return out
}

// percentile returns the q-th percentile of a duration slice. q in [0,1].
// Returns 0 for empty slices.
func percentile(xs []time.Duration, q float64) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(xs))
	copy(sorted, xs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)-1) * q)
	return sorted[idx]
}

func kindsDisplay(kinds map[string]int) string {
	if len(kinds) == 0 {
		return ""
	}
	out := ""
	first := true
	keys := make([]string, 0, len(kinds))
	for k := range kinds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !first {
			out += ","
		}
		out += fmt.Sprintf("%s=%d", k, kinds[k])
		first = false
	}
	return out
}
