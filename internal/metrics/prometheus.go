// SPDX-License-Identifier: Apache-2.0

// Prometheus exporter for the agentic MetricsSink (follow-up 3b).
//
// PromSink is a drop-in MetricsSink that records into a private
// prometheus.Registry; the daemon serves it on /metrics behind the
// [metrics] config flag. It composes with the in-process Sink via Multi so
// ##agent metrics keeps working alongside scraping.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	sharedagentic "github.com/rysh-ai/rysh-cli-shared/agentic"
	provider "github.com/rysh-ai/rysh-cli-shared/provider"
	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// durationBuckets span sub-second tool calls through minute-long LLM turns.
var durationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

// PromSink implements sharedagentic.MetricsSink by recording into a private
// Prometheus registry. Use Handler() to serve it. Safe for concurrent use
// (Prometheus collectors are internally synchronised).
type PromSink struct {
	reg *prometheus.Registry

	toolCalls    *prometheus.CounterVec   // {tool,status}
	toolErrors   *prometheus.CounterVec   // {tool,kind}
	toolDuration *prometheus.HistogramVec // {tool}

	llmCalls         *prometheus.CounterVec   // {model}
	llmDuration      *prometheus.HistogramVec // {model}
	llmInputTokens   *prometheus.CounterVec   // {model}
	llmOutputTokens  *prometheus.CounterVec   // {model}
	llmCacheRead     *prometheus.CounterVec   // {model}
	llmCacheCreation *prometheus.CounterVec   // {model}

	compactionEvents       prometheus.Counter
	compactionDroppedTurns prometheus.Counter
	compactionDuration     prometheus.Histogram

	approvals *prometheus.CounterVec // {tool,decision}
}

// NewPrometheus builds a PromSink with all collectors registered into a fresh
// private registry (no global state; the /metrics output is rysh-only).
func NewPrometheus() *PromSink {
	reg := prometheus.NewRegistry()
	counterVec := func(name, help string, labels ...string) *prometheus.CounterVec {
		c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
		reg.MustRegister(c)
		return c
	}
	histVec := func(name, help string, labels ...string) *prometheus.HistogramVec {
		h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: durationBuckets}, labels)
		reg.MustRegister(h)
		return h
	}

	p := &PromSink{
		reg:          reg,
		toolCalls:    counterVec("rysh_tool_calls_total", "Tool invocations by tool and status (ok|error).", "tool", "status"),
		toolErrors:   counterVec("rysh_tool_errors_total", "Tool errors by tool and error kind.", "tool", "kind"),
		toolDuration: histVec("rysh_tool_duration_seconds", "Tool Execute wall-clock latency by tool.", "tool"),

		llmCalls:         counterVec("rysh_llm_calls_total", "LLM completions by model.", "model"),
		llmDuration:      histVec("rysh_llm_duration_seconds", "LLM completion latency by model.", "model"),
		llmInputTokens:   counterVec("rysh_llm_input_tokens_total", "Billed (non-cached) input tokens by model.", "model"),
		llmOutputTokens:  counterVec("rysh_llm_output_tokens_total", "Generated output tokens by model.", "model"),
		llmCacheRead:     counterVec("rysh_llm_cache_read_tokens_total", "Input tokens served from the prompt cache by model.", "model"),
		llmCacheCreation: counterVec("rysh_llm_cache_creation_tokens_total", "Input tokens written to the prompt cache by model.", "model"),

		compactionEvents:       prometheus.NewCounter(prometheus.CounterOpts{Name: "rysh_compaction_events_total", Help: "Conversation compaction events."}),
		compactionDroppedTurns: prometheus.NewCounter(prometheus.CounterOpts{Name: "rysh_compaction_dropped_turns_total", Help: "Conversation turns dropped by compaction."}),
		compactionDuration:     prometheus.NewHistogram(prometheus.HistogramOpts{Name: "rysh_compaction_duration_seconds", Help: "Compaction summarise latency.", Buckets: durationBuckets}),

		approvals: counterVec("rysh_approvals_total", "Approval decisions by tool and decision.", "tool", "decision"),
	}
	reg.MustRegister(p.compactionEvents, p.compactionDroppedTurns, p.compactionDuration)
	return p
}

// Handler returns an http.Handler that serves this sink's registry in
// Prometheus text format.
func (p *PromSink) Handler() http.Handler {
	return promhttp.HandlerFor(p.reg, promhttp.HandlerOpts{})
}

// RecordTool implements sharedagentic.MetricsSink.
func (p *PromSink) RecordTool(name string, dur time.Duration, out *sharedtools.ToolOutput, err error) {
	p.toolDuration.WithLabelValues(name).Observe(dur.Seconds())
	status := "ok"
	switch {
	case err != nil:
		status = "error"
		p.toolErrors.WithLabelValues(name, "internal").Inc()
	case out != nil && out.Error != "":
		status = "error"
		kind := out.ErrorKind
		if kind == "" {
			kind = "unknown"
		}
		p.toolErrors.WithLabelValues(name, kind).Inc()
	}
	p.toolCalls.WithLabelValues(name, status).Inc()
}

// RecordLLMCall implements sharedagentic.MetricsSink.
func (p *PromSink) RecordLLMCall(model string, dur time.Duration, usage provider.Usage) {
	p.llmCalls.WithLabelValues(model).Inc()
	p.llmDuration.WithLabelValues(model).Observe(dur.Seconds())
	p.llmInputTokens.WithLabelValues(model).Add(float64(usage.InputTokens))
	p.llmOutputTokens.WithLabelValues(model).Add(float64(usage.OutputTokens))
	p.llmCacheRead.WithLabelValues(model).Add(float64(usage.CacheReadInputTokens))
	p.llmCacheCreation.WithLabelValues(model).Add(float64(usage.CacheCreationInputTokens))
}

// RecordCompaction implements sharedagentic.MetricsSink.
func (p *PromSink) RecordCompaction(droppedTurns int, dur time.Duration) {
	p.compactionEvents.Inc()
	p.compactionDroppedTurns.Add(float64(droppedTurns))
	p.compactionDuration.Observe(dur.Seconds())
}

// RecordApproval implements sharedagentic.MetricsSink.
func (p *PromSink) RecordApproval(toolName, decision string, dur time.Duration) {
	p.approvals.WithLabelValues(toolName, decision).Inc()
}

// compile-time assertion that PromSink satisfies the interface.
var _ sharedagentic.MetricsSink = (*PromSink)(nil)

// ---------------------------------------------------------------------------
// Multi — fan-out MetricsSink.
// ---------------------------------------------------------------------------

// Multi fans every MetricsSink call out to several sinks (e.g. the in-process
// Sink for ##agent metrics plus a PromSink for scraping). It exposes Dump() by
// delegating to a designated dumper (the in-process Sink), so the existing
// ##agent metrics command keeps working when Prometheus is enabled.
type Multi struct {
	sinks  []sharedagentic.MetricsSink
	dumper interface{ Dump() string }
}

// NewMulti builds a fan-out. primary (the in-process Sink) is always first and
// is used for Dump(); extra sinks are appended. primary may be nil.
func NewMulti(primary *Sink, extra ...sharedagentic.MetricsSink) *Multi {
	sinks := make([]sharedagentic.MetricsSink, 0, 1+len(extra))
	var dumper interface{ Dump() string }
	if primary != nil {
		sinks = append(sinks, primary)
		dumper = primary
	}
	sinks = append(sinks, extra...)
	return &Multi{sinks: sinks, dumper: dumper}
}

// RecordTool implements sharedagentic.MetricsSink.
func (m *Multi) RecordTool(name string, dur time.Duration, out *sharedtools.ToolOutput, err error) {
	for _, s := range m.sinks {
		s.RecordTool(name, dur, out, err)
	}
}

// RecordLLMCall implements sharedagentic.MetricsSink.
func (m *Multi) RecordLLMCall(model string, dur time.Duration, usage provider.Usage) {
	for _, s := range m.sinks {
		s.RecordLLMCall(model, dur, usage)
	}
}

// RecordCompaction implements sharedagentic.MetricsSink.
func (m *Multi) RecordCompaction(droppedTurns int, dur time.Duration) {
	for _, s := range m.sinks {
		s.RecordCompaction(droppedTurns, dur)
	}
}

// RecordApproval implements sharedagentic.MetricsSink.
func (m *Multi) RecordApproval(toolName, decision string, dur time.Duration) {
	for _, s := range m.sinks {
		s.RecordApproval(toolName, decision, dur)
	}
}

// Dump delegates to the in-process Sink so ##agent metrics keeps working.
func (m *Multi) Dump() string {
	if m.dumper != nil {
		return m.dumper.Dump()
	}
	return "(no metrics collected yet)\n"
}

var _ sharedagentic.MetricsSink = (*Multi)(nil)
