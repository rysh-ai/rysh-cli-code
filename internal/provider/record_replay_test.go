// SPDX-License-Identifier: Apache-2.0

package provider

// Hermetic tests for record/replay determinism (design 009 §3.2): a scripted
// fake provider stands in for the live model; recording captures its turns
// and replay must serve them back byte-identically — tool calls, text, stop
// reasons, and usage included — with no key and no network.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// scriptedAgentic serves a fixed sequence of agentic responses and Complete
// replies — the fake-model seam.
type scriptedAgentic struct {
	name      string
	responses []*AgenticResponse
	replies   []string
	calls     int
	completes int
	err       error
}

func (s *scriptedAgentic) Name() string { return s.name }

func (s *scriptedAgentic) Complete(_ context.Context, _ string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	if s.completes >= len(s.replies) {
		return "", errors.New("scripted replies exhausted")
	}
	r := s.replies[s.completes]
	s.completes++
	return r, nil
}

func (s *scriptedAgentic) CompleteWithTools(
	_ context.Context, _ []ConversationTurn, _ []ToolSpec, _ string,
) (*AgenticResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.calls >= len(s.responses) {
		return nil, errors.New("scripted responses exhausted")
	}
	r := s.responses[s.calls]
	s.calls++
	return r, nil
}

// scriptedTwoTurn is the canonical two-turn agent run: one tool_use turn,
// then an end_turn answer — both with real usage numbers.
func scriptedTwoTurn() *scriptedAgentic {
	return &scriptedAgentic{
		name: "scripted",
		responses: []*AgenticResponse{
			{
				ToolCalls: []ToolCallRequest{{
					ID: "call_1", Name: "bash",
					Input: json.RawMessage(`{"command":"echo hello > hello.txt"}`),
				}},
				StopReason: StopReasonToolUse,
				Usage:      Usage{InputTokens: 120, OutputTokens: 30, CacheReadInputTokens: 10},
			},
			{
				TextBlocks: []TextBlock{{Text: "DONE"}},
				StopReason: StopReasonEndTurn,
				Usage:      Usage{InputTokens: 200, OutputTokens: 15},
			},
		},
		replies: []string{"PASS\nlooks good"},
	}
}

// conv builds a conversation of n turns (only the length matters to replay).
func conv(n int) []ConversationTurn {
	turns := make([]ConversationTurn, n)
	for i := range turns {
		turns[i] = ConversationTurn{Role: "user", Content: "x"}
	}
	return turns
}

func TestRecordThenReplay_ServesIdenticalTurns(t *testing.T) {
	dir := t.TempDir()
	live := scriptedTwoTurn()
	rec, err := NewRecording(live, dir)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Name() != "scripted" {
		t.Fatalf("recording must be name-transparent, got %q", rec.Name())
	}

	ctx := context.Background()
	var recorded []*AgenticResponse
	for i, n := range []int{1, 3} {
		r, err := rec.CompleteWithTools(ctx, conv(n), nil, "sys")
		if err != nil {
			t.Fatalf("recorded call %d: %v", i, err)
		}
		recorded = append(recorded, r)
	}
	reply, err := rec.Complete(ctx, "judge prompt")
	if err != nil || reply != "PASS\nlooks good" {
		t.Fatalf("recorded Complete = %q, %v", reply, err)
	}

	replay := NewReplay(dir)
	if replay.Name() != "replay" {
		t.Fatalf("replay provider name = %q", replay.Name())
	}
	for i, n := range []int{1, 3} {
		got, err := replay.CompleteWithTools(ctx, conv(n), nil, "different sys prompt is fine")
		if err != nil {
			t.Fatalf("replayed call %d: %v", i, err)
		}
		// Thinking blocks are documented as not replayed; everything the
		// eval harness grades from must round-trip identically.
		want := *recorded[i]
		want.ThinkingBlocks = nil
		if !reflect.DeepEqual(&want, got) {
			t.Fatalf("replayed turn %d differs:\nwant %+v\ngot  %+v", i, want, got)
		}
	}
	if reply, err := replay.Complete(ctx, "judge prompt"); err != nil || reply != "PASS\nlooks good" {
		t.Fatalf("replayed Complete = %q, %v", reply, err)
	}
}

func TestReplay_DivergenceAndExhaustionFailLoudly(t *testing.T) {
	dir := t.TempDir()
	rec, err := NewRecording(scriptedTwoTurn(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := rec.CompleteWithTools(ctx, conv(1), nil, ""); err != nil {
		t.Fatal(err)
	}

	// Conversation-length divergence.
	replay := NewReplay(dir)
	if _, err := replay.CompleteWithTools(ctx, conv(5), nil, ""); err == nil ||
		!strings.Contains(err.Error(), "diverged") {
		t.Fatalf("length divergence must error loudly, got %v", err)
	}

	// Kind divergence: the transcript's next turn is agentic, not complete.
	replay = NewReplay(dir)
	if _, err := replay.Complete(ctx, "p"); err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("kind divergence must error loudly, got %v", err)
	}

	// Exhaustion: one recorded turn, two requested.
	replay = NewReplay(dir)
	if _, err := replay.CompleteWithTools(ctx, conv(1), nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := replay.CompleteWithTools(ctx, conv(3), nil, ""); err == nil ||
		!strings.Contains(err.Error(), "exhausted") {
		t.Fatalf("exhaustion must error loudly, got %v", err)
	}
}

func TestReplay_MissingOrEmptyTranscriptFailsAtCallTime(t *testing.T) {
	// Missing dir: construction succeeds (the daemon cannot return an
	// error), the first call fails with a how-to-record hint.
	replay := NewReplay(filepath.Join(t.TempDir(), "nope"))
	if _, err := replay.CompleteWithTools(context.Background(), conv(1), nil, ""); err == nil ||
		!strings.Contains(err.Error(), "--record") {
		t.Fatalf("missing transcript must fail with a recording hint, got %v", err)
	}

	// A transcript with zero recorded turns (e.g. the recorded run errored
	// before its first successful provider call) is equally unusable.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, TranscriptFileName), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	replay = NewReplay(dir)
	if _, err := replay.CompleteWithTools(context.Background(), conv(1), nil, ""); err == nil ||
		!strings.Contains(err.Error(), "no recorded turns") {
		t.Fatalf("empty transcript must fail loudly, got %v", err)
	}
}

// Recording only successful calls: a provider error leaves no transcript
// entry (an errored turn cannot be replayed as if it had succeeded).
func TestRecording_SkipsFailedCalls(t *testing.T) {
	dir := t.TempDir()
	rec, err := NewRecording(&scriptedAgentic{name: "s", err: errors.New("429")}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rec.CompleteWithTools(context.Background(), conv(1), nil, ""); err == nil {
		t.Fatal("inner error must propagate")
	}
	data, err := os.ReadFile(filepath.Join(dir, TranscriptFileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Fatalf("failed call must not be recorded, transcript: %s", data)
	}
}

// A fresh recording truncates any stale transcript: one recording = one run.
func TestRecording_TruncatesPreviousTranscript(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, TranscriptFileName), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := NewRecording(scriptedTwoTurn(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rec.CompleteWithTools(context.Background(), conv(1), nil, ""); err != nil {
		t.Fatal(err)
	}
	replay := NewReplay(dir)
	if _, err := replay.CompleteWithTools(context.Background(), conv(1), nil, ""); err != nil {
		t.Fatalf("stale content must be gone; replay failed: %v", err)
	}
}
