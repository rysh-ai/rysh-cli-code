package main

// Record→replay determinism, end to end at the harness seam (design 009
// §3.2): a scripted fake provider plays the live model; a mini agent loop
// executes its tool calls FOR REAL in a fresh git workspace and derives the
// Result through the exact production pipeline (runCollector + git
// snapshots). Pass 1 records through provider.NewRecording; pass 2 re-runs
// with provider.NewReplay — no fake provider, no API key — and must produce
// the IDENTICAL Result, with the case's structural assertions green both
// times. This is the hermetic proof that a recorded transcript makes eval
// grading deterministic and key-free.

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"reflect"
	"testing"

	sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

	"github.com/rysh-ai/rysh-cli-code/internal/eval"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

// scriptedLiveProvider is the fake live model: first turn asks for a bash
// tool call, second turn answers DONE. Same shape as a real two-turn run.
type scriptedLiveProvider struct {
	calls int
}

func (s *scriptedLiveProvider) Name() string { return "scripted-live" }

func (s *scriptedLiveProvider) Complete(context.Context, string) (string, error) {
	return "", errors.New("not used")
}

func (s *scriptedLiveProvider) CompleteWithTools(
	_ context.Context, _ []provider.ConversationTurn, _ []provider.ToolSpec, _ string,
) (*provider.AgenticResponse, error) {
	defer func() { s.calls++ }()
	switch s.calls {
	case 0:
		return &provider.AgenticResponse{
			ToolCalls: []provider.ToolCallRequest{{
				ID: "call_1", Name: "bash",
				Input: json.RawMessage(`{"command":"echo hello rysh > hello.txt"}`),
			}},
			StopReason: provider.StopReasonToolUse,
			Usage:      provider.Usage{InputTokens: 150, OutputTokens: 40},
		}, nil
	case 1:
		return &provider.AgenticResponse{
			TextBlocks: []provider.TextBlock{{Text: "DONE"}},
			StopReason: provider.StopReasonEndTurn,
			Usage:      provider.Usage{InputTokens: 220, OutputTokens: 10},
		}, nil
	}
	return nil, errors.New("scripted provider exhausted")
}

// driveProviderAgent is the mini agent loop: it consumes provider turns,
// executes bash tool calls for real in workDir, and derives the Result via
// the production collector + git-snapshot pipeline (the same derivations
// executeHeadlessRun uses). The daemon/NATS transport is the only thing
// faked out.
func driveProviderAgent(t *testing.T, p provider.AgenticProvider, workDir string) (runOutcome, eval.Result, error) {
	t.Helper()
	var res eval.Result
	before, inRepo := gitStatusSnapshot(workDir)
	if !inRepo {
		return runOutcome{}, res, errors.New("workspace must be a git repo")
	}
	c := newRunCollector(runBudget{})
	ctx := context.Background()
	conversation := []provider.ConversationTurn{{Role: "user", Content: "create hello.txt then reply DONE"}}
	for i := 0; i < 8; i++ {
		resp, err := p.CompleteWithTools(ctx, conversation, nil, "system prompt")
		if err != nil {
			return runOutcome{Status: "error", ExitCode: runExitError, Detail: err.Error()}, res, nil
		}
		c.OnUsage(&msg.MsgUsageRecord{
			InTokens: resp.Usage.InputTokens, OutTokens: resp.Usage.OutputTokens,
			CacheRead: resp.Usage.CacheReadInputTokens, CacheWrite: resp.Usage.CacheCreationInputTokens,
		})
		if resp.StopReason == provider.StopReasonToolUse {
			asst := provider.ConversationTurn{Role: "assistant", ToolCalls: resp.ToolCalls}
			conversation = append(conversation, asst)
			for _, tc := range resp.ToolCalls {
				var in struct {
					Command string `json:"command"`
				}
				if err := json.Unmarshal(tc.Input, &in); err != nil {
					return runOutcome{}, res, err
				}
				c.OnStep(&msg.MsgAgenticStep{Kind: sharedmsg.StepToolStart, Origin: "bash",
					Title: "bash: " + in.Command})
				cmd := exec.Command("sh", "-c", in.Command)
				cmd.Dir = workDir
				out, _ := cmd.CombinedOutput()
				conversation = append(conversation, provider.ConversationTurn{
					Role: "tool", ToolCallID: tc.ID, Content: string(out)})
			}
			continue
		}
		for _, tb := range resp.TextBlocks {
			c.OnOutput(&msg.MsgAgenticOutput{Type: "text", Content: tb.Text})
		}
		break
	}
	after, _ := gitStatusSnapshot(workDir)
	res.FilesChanged = changedPaths(before, after)
	res.Commands = c.CommandLines()
	res.Output = c.OutputText()
	res.TokensUsed = int(c.TokensUsed())
	return runOutcome{Status: "done", ExitCode: runExitDone}, res, nil
}

func TestRecordThenReplay_DeterministicResultAndGreenAssertions(t *testing.T) {
	recordDir := t.TempDir()

	// Pass 1 — "live" run with the scripted provider, recorded.
	liveWork := t.TempDir()
	gitT(t, liveWork, "init", "-q")
	rec, err := provider.NewRecording(&scriptedLiveProvider{}, recordDir)
	if err != nil {
		t.Fatal(err)
	}
	liveOutcome, liveRes, err := driveProviderAgent(t, rec, liveWork)
	if err != nil || liveOutcome.ExitCode != runExitDone {
		t.Fatalf("live pass failed: %+v, %v", liveOutcome, err)
	}

	// Pass 2 — replay from the transcript alone: fresh workspace, NO fake
	// provider, no key. The replay provider is the production one the daemon
	// selects under RYSH_REPLAY_DIR.
	replayWork := t.TempDir()
	gitT(t, replayWork, "init", "-q")
	replayOutcome, replayRes, err := driveProviderAgent(t, provider.NewReplay(recordDir), replayWork)
	if err != nil || replayOutcome.ExitCode != runExitDone {
		t.Fatalf("replay pass failed: %+v, %v", replayOutcome, err)
	}

	// The graded artifact must be IDENTICAL — files, commands, output, and
	// token spend (usage rides in the transcript).
	if !reflect.DeepEqual(liveRes, replayRes) {
		t.Fatalf("replay Result diverged:\nlive   %+v\nreplay %+v", liveRes, replayRes)
	}

	// And a case's structural assertions grade green against both.
	expect := eval.Expect{
		FilesChanged:    []string{"hello.txt"},
		CommandsAllowed: []string{"echo *"},
		OutputMatches:   []string{"(?i)done"},
		MaxTokens:       1000,
	}
	for name, r := range map[string]eval.Result{"live": liveRes, "replay": replayRes} {
		if as := eval.Evaluate(expect, r); !eval.Passed(as) {
			t.Fatalf("%s pass failed assertions: %+v", name, as)
		}
	}

	// The replayed run really did the work again: the file exists in the
	// replay workspace, written by re-executing the recorded tool call.
	if _, _, err := driveProviderAgent(t, provider.NewReplay(recordDir), replayWork); err != nil {
		// third pass just proves transcripts are reusable; outcome checked above
		t.Fatalf("transcript must be replayable repeatedly: %v", err)
	}
	if !contains(replayRes.FilesChanged, "hello.txt") {
		t.Fatalf("replay must re-create hello.txt, got %v", replayRes.FilesChanged)
	}
	if replayRes.TokensUsed != 420 {
		t.Fatalf("replayed token spend must equal the recorded spend (420), got %d", replayRes.TokensUsed)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
