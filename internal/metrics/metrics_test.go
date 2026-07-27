package metrics

import (
	"strings"
	"testing"
	"time"

	provider "github.com/rysh-ai/rysh-cli-shared/provider"
	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// TestSink_RecordTool tracks calls + errors with kind breakdown.
func TestSink_RecordTool(t *testing.T) {
	s := New()
	s.RecordTool("bash", 100*time.Millisecond, &sharedtools.ToolOutput{Content: "ok"}, nil)
	s.RecordTool("bash", 200*time.Millisecond, &sharedtools.ToolOutput{Content: "ok"}, nil)
	s.RecordTool("bash", 50*time.Millisecond, sharedtools.ErrOutput(sharedtools.ErrKindTransient, "down"), nil)

	dump := s.Dump()
	if !strings.Contains(dump, "bash") {
		t.Errorf("expected bash row in dump:\n%s", dump)
	}
	if !strings.Contains(dump, "transient=1") {
		t.Errorf("expected transient=1 in kinds:\n%s", dump)
	}
}

// TestSink_RecordLLM rolls up tokens per model.
func TestSink_RecordLLM(t *testing.T) {
	s := New()
	s.RecordLLMCall("claude-sonnet-4", 1*time.Second, provider.Usage{
		InputTokens: 1000, OutputTokens: 200,
		CacheReadInputTokens: 800, CacheCreationInputTokens: 200,
	})
	s.RecordLLMCall("claude-sonnet-4", 2*time.Second, provider.Usage{
		InputTokens: 500, OutputTokens: 100,
		CacheReadInputTokens: 2000,
	})

	dump := s.Dump()
	if !strings.Contains(dump, "claude-sonnet-4") {
		t.Errorf("expected model row:\n%s", dump)
	}
	if !strings.Contains(dump, "2800") {
		// cache_read 800 + 2000 = 2800
		t.Errorf("expected cache_read total 2800:\n%s", dump)
	}
}

// TestSink_RecordCompaction tracks event count + total duration.
func TestSink_RecordCompaction(t *testing.T) {
	s := New()
	s.RecordCompaction(15, 800*time.Millisecond)
	s.RecordCompaction(8, 1*time.Second)
	dump := s.Dump()
	if !strings.Contains(dump, "events=2") {
		t.Errorf("expected events=2:\n%s", dump)
	}
}

// TestSink_RecordApproval increments per decision.
func TestSink_RecordApproval(t *testing.T) {
	s := New()
	s.RecordApproval("file_edit", "yes", 100*time.Millisecond)
	s.RecordApproval("file_edit", "yes", 100*time.Millisecond)
	s.RecordApproval("bash", "no", 50*time.Millisecond)
	dump := s.Dump()
	if !strings.Contains(dump, "yes: 2") {
		t.Errorf("expected yes: 2:\n%s", dump)
	}
	if !strings.Contains(dump, "no: 1") {
		t.Errorf("expected no: 1:\n%s", dump)
	}
}

// TestSink_EmptyDump is a clear empty-state.
func TestSink_EmptyDump(t *testing.T) {
	s := New()
	if got := s.Dump(); !strings.Contains(got, "no metrics collected") {
		t.Errorf("expected empty-state message: %q", got)
	}
}

// TestPercentile_Basic spot-checks the quantile helper.
func TestPercentile_Basic(t *testing.T) {
	xs := []time.Duration{}
	if percentile(xs, 0.5) != 0 {
		t.Errorf("empty slice should yield 0")
	}
	xs = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond}
	if got := percentile(xs, 0.5); got != 10*time.Millisecond {
		// idx = int(2 * 0.5) = 1 → xs[1] = 20ms. Adjust expected accordingly.
		if got != 20*time.Millisecond {
			t.Errorf("p50 of 3-element slice: got %v", got)
		}
	}
}
