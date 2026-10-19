// SPDX-License-Identifier: Apache-2.0

package meetingexec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/rysh-ai/rysh-cli-shared/provider"
)

type fakeProvider struct {
	calls atomic.Int32
	fail  bool
}

func (*fakeProvider) Name() string { return "meeting-test" }
func (*fakeProvider) Complete(context.Context, string) (string, error) {
	return "", errors.New("unexpected Complete")
}
func (p *fakeProvider) CompleteWithTools(ctx context.Context, history []provider.ConversationTurn, specs []provider.ToolSpec, system string) (*provider.AgenticResponse, error) {
	n := p.calls.Add(1)
	if p.fail {
		return nil, errors.New("fixture provider failed")
	}
	if len(specs) != 1 || specs[0].Name != "meeting_read_file" {
		return nil, errors.New("unscoped tool exposure")
	}
	if n == 1 {
		return &provider.AgenticResponse{StopReason: provider.StopReasonToolUse, ToolCalls: []provider.ToolCallRequest{{ID: "read-1", Name: "meeting_read_file", Input: json.RawMessage(`{"path":"report.txt"}`)}}}, nil
	}
	found := false
	for _, turn := range history {
		if turn.Role == "tool" && turn.Content == "fixture report" {
			found = true
		}
	}
	if !found {
		return nil, errors.New("real tool result missing")
	}
	return &provider.AgenticResponse{StopReason: provider.StopReasonEndTurn, TextBlocks: []provider.TextBlock{{Text: "The report says fixture report."}}}, nil
}
func TestExistingExecutorReadsScopedFixtureAndReturnsOnlyCompletedAnswer(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "report.txt"), []byte("fixture report"), 0600)
	system := actor.NewActorSystem()
	defer system.Shutdown()
	p := &fakeProvider{}
	execute, err := New(system, p, root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := execute(ctx, "read report.txt")
	if err != nil {
		t.Fatal(err)
	}
	if out != "The report says fixture report." || p.calls.Load() != 2 {
		t.Fatalf("%q calls %d", out, p.calls.Load())
	}
	// Only the fixture remains: no disk/KV transcript or execution history.
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatal("executor wrote artifacts")
	}
}
func TestScopedReadsDenyEscapeSymlinkAndCredentialFiles(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	_ = os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0600)
	_ = os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "visible.txt"))
	tool := &readFile{dir: dir}
	for _, path := range []string{"../secret.txt", "/etc/passwd", ".env", "x/.rysh/config", "visible.txt", "credentials.json", "private.key"} {
		p, _ := json.Marshal(map[string]string{"path": path})
		out, err := tool.Execute(context.Background(), p)
		if err == nil && (out.Error == "" || out.Content != "") {
			t.Fatalf("allowed %s", path)
		}
	}
}
func TestExecutorFailureDoesNotReturnSpeech(t *testing.T) {
	system := actor.NewActorSystem()
	defer system.Shutdown()
	execute, _ := New(system, &fakeProvider{fail: true}, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := execute(ctx, "test")
	if err == nil || out != "" {
		t.Fatal("accepted failed execution")
	}
}

type namedProvider struct{ fakeProvider }

func (*namedProvider) Name() string { return "mock-agentic" }

type replayProvider struct{ fakeProvider }

func (*replayProvider) Name() string { return "replay" }

// The founder's MacBook run hit the mock fallback and was told about
// RYSH_RECORD_DIR/RYSH_REPLAY_DIR; each refusal must name its real cause.
func TestLiveProviderErrorNamesTheCause(t *testing.T) {
	t.Setenv("RYSH_RECORD_DIR", "")
	t.Setenv("RYSH_REPLAY_DIR", "")
	cases := []struct {
		name string
		prov provider.AgenticProvider
		env  string
		want string // substring; "" = accepted
	}{
		{"live", &fakeProvider{}, "", ""},
		{"nil", nil, "", "no provider key"},
		{"mock", &namedProvider{}, "", "no provider key"},
		{"replay provider", &replayProvider{}, "", "replayed transcript"},
		{"replay env", &fakeProvider{}, "RYSH_REPLAY_DIR", "replayed transcript"},
		{"record env", &fakeProvider{}, "RYSH_RECORD_DIR", "RYSH_RECORD_DIR records"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv(tc.env, t.TempDir())
			}
			err := LiveProviderError(tc.prov)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused a live provider: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			case tc.want == "no provider key" && strings.Contains(err.Error(), "RYSH_RECORD_DIR"):
				t.Fatalf("mock refusal still blames record/replay: %v", err)
			}
		})
	}
	system := actor.NewActorSystem()
	defer system.Shutdown()
	if _, err := New(system, &namedProvider{}, ""); err == nil || !strings.Contains(err.Error(), "no provider key") {
		t.Fatalf("New on the mock provider: err = %v", err)
	}
}

func TestExecutorRefusesSessionRecordingAndReplay(t *testing.T) {
	system := actor.NewActorSystem()
	defer system.Shutdown()
	for _, name := range []string{"RYSH_RECORD_DIR", "RYSH_REPLAY_DIR"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, t.TempDir())
			if _, err := New(system, &fakeProvider{}, ""); err == nil {
				t.Fatal("accepted recording/replay session")
			}
		})
	}
}
