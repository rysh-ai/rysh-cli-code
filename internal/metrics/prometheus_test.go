package metrics

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	provider "github.com/rysh-ai/rysh-cli-shared/provider"
	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// scrape renders the PromSink's /metrics text.
func scrape(t *testing.T, p *PromSink) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("metrics handler returned %d", rec.Code)
	}
	return rec.Body.String()
}

func TestPromSink_RecordsAllFamilies(t *testing.T) {
	p := NewPrometheus()

	p.RecordTool("bash", 120*time.Millisecond, &sharedtools.ToolOutput{Content: "ok"}, nil)
	p.RecordTool("file_read", 5*time.Millisecond, &sharedtools.ToolOutput{Error: "file not found", ErrorKind: sharedtools.ErrKindMissing}, nil)
	p.RecordTool("glob", 1*time.Millisecond, nil, errors.New("boom"))
	p.RecordLLMCall("claude-x", 2*time.Second, provider.Usage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 10, CacheCreationInputTokens: 5})
	p.RecordCompaction(7, 300*time.Millisecond)
	p.RecordApproval("file_edit", "yes", 0)

	body := scrape(t, p)

	wants := []string{
		`rysh_tool_calls_total{status="ok",tool="bash"} 1`,
		`rysh_tool_calls_total{status="error",tool="file_read"} 1`,
		`rysh_tool_errors_total{kind="missing",tool="file_read"} 1`,
		`rysh_tool_errors_total{kind="internal",tool="glob"} 1`,
		`rysh_llm_calls_total{model="claude-x"} 1`,
		`rysh_llm_input_tokens_total{model="claude-x"} 100`,
		`rysh_llm_output_tokens_total{model="claude-x"} 50`,
		`rysh_llm_cache_read_tokens_total{model="claude-x"} 10`,
		`rysh_llm_cache_creation_tokens_total{model="claude-x"} 5`,
		`rysh_compaction_events_total 1`,
		`rysh_compaction_dropped_turns_total 7`,
		`rysh_approvals_total{decision="yes",tool="file_edit"} 1`,
	}
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("metrics output missing %q", w)
		}
	}
}

func TestMulti_FansOutAndDumps(t *testing.T) {
	inproc := New()
	prom := NewPrometheus()
	multi := NewMulti(inproc, prom)

	multi.RecordTool("grep", 10*time.Millisecond, &sharedtools.ToolOutput{Content: "x"}, nil)
	multi.RecordLLMCall("m", time.Second, provider.Usage{InputTokens: 3})

	// In-process sink saw it (Dump reflects the tool + delegation).
	dump := multi.Dump()
	if !strings.Contains(dump, "grep") {
		t.Errorf("Multi.Dump should include the tool from the in-process sink, got:\n%s", dump)
	}
	if dump != inproc.Dump() {
		t.Errorf("Multi.Dump should delegate to the in-process sink")
	}

	// Prometheus sink saw it too.
	if body := scrape(t, prom); !strings.Contains(body, `rysh_tool_calls_total{status="ok",tool="grep"} 1`) {
		t.Errorf("Prometheus sink missing the fanned-out tool call")
	}
}

func TestNewMulti_NilPrimary(t *testing.T) {
	prom := NewPrometheus()
	multi := NewMulti(nil, prom)
	// Must not panic and Dump returns a placeholder.
	multi.RecordTool("x", time.Millisecond, nil, nil)
	if got := multi.Dump(); !strings.Contains(got, "no metrics") {
		t.Errorf("expected placeholder Dump with nil primary, got %q", got)
	}
}
