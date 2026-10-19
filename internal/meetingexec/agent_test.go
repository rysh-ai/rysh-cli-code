// SPDX-License-Identifier: Apache-2.0

package meetingexec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	sharedagentic "github.com/rysh-ai/rysh-cli-shared/agentic"
	"github.com/rysh-ai/rysh-cli-shared/msg"
	"github.com/rysh-ai/rysh-cli-shared/provider"
	"github.com/rysh-ai/rysh-cli-shared/tools"

	"github.com/rysh-ai/rysh-cli-code/internal/meeting"
	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// scriptProvider asks for one bash call, then answers. It records the first
// user turn so tests can check the meeting framing.
type scriptProvider struct {
	mu      sync.Mutex
	calls   int
	request string
	command string
}

func (*scriptProvider) Name() string { return "meeting-test" }
func (*scriptProvider) Complete(context.Context, string) (string, error) {
	return "", errors.New("unexpected Complete")
}
func (p *scriptProvider) CompleteWithTools(_ context.Context, history []provider.ConversationTurn, _ []provider.ToolSpec, _ string) (*provider.AgenticResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls == 1 {
		for _, t := range history {
			if t.Role == "user" {
				p.request = t.Content
				break
			}
		}
		in, _ := json.Marshal(map[string]string{"command": p.command})
		return &provider.AgenticResponse{StopReason: provider.StopReasonToolUse, ToolCalls: []provider.ToolCallRequest{{ID: "t1", Name: "bash", Input: in}}}, nil
	}
	return &provider.AgenticResponse{StopReason: provider.StopReasonEndTurn, TextBlocks: []provider.TextBlock{{Text: "Deployed to staging."}}}, nil
}
func (p *scriptProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// gatedTool always requires approval: in a meeting it must run anyway.
type gatedTool struct {
	runs    atomic.Int32
	block   bool
	stopped chan struct{}
}

func (*gatedTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: "bash", Description: "run a command", Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)}
}
func (*gatedTool) RequiresApproval(json.RawMessage) bool { return true }
func (g *gatedTool) Execute(ctx context.Context, _ json.RawMessage) (*tools.ToolOutput, error) {
	g.runs.Add(1)
	if g.block {
		<-ctx.Done()
		close(g.stopped)
		return nil, ctx.Err()
	}
	return &tools.ToolOutput{Content: "ok"}, nil
}

// sessionBus is an in-process NATS standing in for the session bus.
func sessionBus(t *testing.T) (*nats.Conn, *msg.NATSPublisher) {
	t.Helper()
	ns, err := server.NewServer(&server.Options{DontListen: true, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close(); ns.Shutdown() })
	return nc, msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
}

func toolAgent(t *testing.T, system *actor.ActorSystem, p *scriptProvider, tool *gatedTool, live func() error) meeting.Execute {
	t.Helper()
	nc, pub := sessionBus(t)
	execute, err := NewToolAgent(system, ToolAgent{
		Name: "scribe",
		Live: live,
		Build: func(id string) *sharedagentic.LLMPromptExecutionActor {
			if !strings.HasPrefix(id, "meeting-scribe-") {
				t.Errorf("turn bus id %q", id)
			}
			reg := tools.NewToolRegistry()
			reg.Register("bash", tool)
			return sharedagentic.NewLLMPromptExecutionActor(id, 8, pub, nc, p, reg, "You are Scribe.", "", "", nil)
		},
		NC:     nc,
		Codecs: pub.Codecs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return execute
}

type trail struct {
	mu       sync.Mutex
	entries  []meeting.AuditEntry
	activity []string
}

func (tr *trail) ctx(parent context.Context) context.Context {
	ctx := meeting.WithAuditRecorder(parent, "scribe", "Host", func(e meeting.AuditEntry) {
		tr.mu.Lock()
		tr.entries = append(tr.entries, e)
		tr.mu.Unlock()
	})
	return meeting.WithActivityReporter(ctx, func(l string) {
		tr.mu.Lock()
		tr.activity = append(tr.activity, l)
		tr.mu.Unlock()
	})
}

// Founder decision 2026-09-28: an attached agent runs its own tools with no
// approval — even one that always asks — and every call is audited.
func TestToolAgentRunsToolsWithoutApprovalAndAudits(t *testing.T) {
	system := actor.NewActorSystem()
	defer system.Shutdown()
	p := &scriptProvider{command: "deploy staging --token sk-livesecret0123456789"}
	tool := &gatedTool{}
	execute := toolAgent(t, system, p, tool, nil)
	tr := &trail{}
	ctx, cancel := context.WithTimeout(tr.ctx(context.Background()), 10*time.Second)
	defer cancel()
	out, err := execute(ctx, "deploy the branch")
	if err != nil || out != "Deployed to staging." || tool.runs.Load() != 1 {
		t.Fatalf("out %q err %v runs %d", out, err, tool.runs.Load())
	}
	if !strings.Contains(p.request, `labelled "Host"`) || !strings.Contains(p.request, "not a verified identity") || !strings.HasSuffix(p.request, "deploy the branch") {
		t.Fatalf("request framing %q", p.request)
	}
	waitUntil(t, func() bool { tr.mu.Lock(); defer tr.mu.Unlock(); return len(tr.entries) == 2 })
	tr.mu.Lock()
	defer tr.mu.Unlock()
	start, end := tr.entries[0], tr.entries[1]
	if start.Tool != "bash" || start.Status != "started" || end.Status != "ok" || end.Agent != "scribe" || end.Speaker != "Host" {
		t.Fatalf("audit %+v", tr.entries)
	}
	for _, e := range tr.entries {
		if strings.Contains(e.Target, "sk-livesecret") || !strings.Contains(e.Target, "deploy staging") {
			t.Fatalf("target not redacted or missing: %q", e.Target)
		}
	}
	// The wall activity is best-effort LIVE progress, not a record (ruling A,
	// root-0459): it starts with "thinking with scribe"; any further labels, if
	// they arrived before the turn ended, appear in order. The audit above is exact.
	full := []string{"thinking with scribe", "running bash", "thinking with scribe"}
	if len(tr.activity) == 0 || len(tr.activity) > len(full) {
		t.Fatalf("activity %q", tr.activity)
	}
	for i, l := range tr.activity {
		if l != full[i] {
			t.Fatalf("activity %q", tr.activity)
		}
	}
}

// "stop" cancels the turn context: a tool already running is cancelled and
// the turn returns no answer.
func TestToolAgentStopCancelsRunningTool(t *testing.T) {
	system := actor.NewActorSystem()
	defer system.Shutdown()
	p := &scriptProvider{command: "sleep 600"}
	tool := &gatedTool{block: true, stopped: make(chan struct{})}
	execute := toolAgent(t, system, p, tool, nil)
	tr := &trail{}
	ctx, cancel := context.WithCancel(tr.ctx(context.Background()))
	done := make(chan error, 1)
	go func() { _, err := execute(ctx, "run the long job"); done <- err }()
	waitUntil(t, func() bool { return tool.runs.Load() == 1 })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not stop")
	}
	select {
	case <-tool.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("running tool was not cancelled")
	}
}

func TestToolAgentFailsClosed(t *testing.T) {
	system := actor.NewActorSystem()
	defer system.Shutdown()
	p := &scriptProvider{command: "x"}
	tool := &gatedTool{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gone := errors.New("agent scribe is no longer loaded")
	if out, err := toolAgent(t, system, p, tool, func() error { return gone })(ctx, "hi"); !errors.Is(err, gone) || out != "" {
		t.Fatalf("deactivated agent answered: %q %v", out, err)
	}
	policy.SetBlocked("test posture unknown")
	defer policy.ClearBlocked()
	if out, err := toolAgent(t, system, p, tool, nil)(ctx, "hi"); err == nil || out != "" {
		t.Fatal("policy-blocked agent turn ran")
	}
	if plain, _ := New(system, p, ""); plain != nil {
		if _, err := plain(ctx, "hi"); err == nil {
			t.Fatal("policy-blocked rysh turn ran")
		}
	}
	if p.count() != 0 || tool.runs.Load() != 0 {
		t.Fatal("provider or tool called")
	}
	if _, err := NewToolAgent(system, ToolAgent{Name: "scribe"}); err == nil {
		t.Fatal("accepted an agent without an executor builder")
	}
}

// Plain "hey rysh" keeps its scope and reports "thinking"; the scoped reader
// reports the base name of a file it opened, nothing for a denied path.
func TestExecutorReportsActivity(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "docs"), 0o700)
	_ = os.WriteFile(filepath.Join(root, "docs", "report.txt"), []byte("fixture report"), 0o600)
	tr := &trail{}
	ctx := tr.ctx(context.Background())
	tool := &readFile{dir: root, report: func(l string) { meeting.ReportActivity(ctx, l) }}
	for _, path := range []string{"docs/report.txt", ".env", "../x"} {
		b, _ := json.Marshal(map[string]string{"path": path})
		_, _ = tool.Execute(ctx, b)
	}
	system := actor.NewActorSystem()
	defer system.Shutdown()
	plain, _ := New(system, &fakeProvider{fail: true}, "")
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, _ = plain(c, "hi")
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if strings.Join(tr.activity, "|") != "reading report.txt|thinking" {
		t.Fatalf("labels %q", tr.activity)
	}
}

func waitUntil(t *testing.T, fn func() bool) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition timed out")
}
