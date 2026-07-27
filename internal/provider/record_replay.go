package provider

// Record/replay determinism for the eval harness (design 009 §3.2).
//
// Recording wraps a live AgenticProvider and appends every successful provider
// interaction (both the agentic CompleteWithTools seam and the single-turn
// Complete seam) to a JSONL transcript. Replay is itself an AgenticProvider
// that serves those recorded turns back in order, so a recorded case re-runs
// deterministically with zero token spend and no API key.
//
// The seam is provider construction in the session daemon
// (internal/agentic/setup.go). `rysh run` and the daemon are separate
// processes, so the record/replay directories travel the same road as the
// --provider override: environment variables (RecordDirEnv / ReplayDirEnv)
// that the spawned daemon inherits.
//
// Honesty notes, deliberately not papered over:
//   - Only SUCCESSFUL provider calls are recorded. A run that died on a
//     provider error leaves a transcript that ends early; replaying it errors
//     loudly at the missing turn instead of inventing one.
//   - Replay serves turns strictly in recorded order. It checks the
//     conversation LENGTH against the recording (structure must match) but
//     not the content: tool outputs legitimately differ across machines
//     (timestamps, absolute paths) while the loop structure stays identical.
//     A length mismatch — the loop diverged, or compaction restructured the
//     conversation — is a hard error naming both lengths.
//   - Recording is per-process and append-ordered. Concurrent orchestrators
//     (parallel sub-agents) would interleave nondeterministically; replay is
//     only meaningful for single-loop runs.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"sync"
)

// RecordDirEnv / ReplayDirEnv carry the record/replay directory from the
// `rysh run` / `rysh eval` process into the spawned session daemon (which
// constructs the provider). Set by run/eval from --record / --replay flags.
const (
	RecordDirEnv = "RYSH_RECORD_DIR"
	ReplayDirEnv = "RYSH_REPLAY_DIR"
)

// TranscriptFileName is the transcript file inside a record/replay directory.
const TranscriptFileName = "transcript.jsonl"

// ReplayDirFromEnv returns the active replay directory ("" when replay is not
// requested). The daemon consults this at provider-construction time.
func ReplayDirFromEnv() string { return os.Getenv(ReplayDirEnv) }

// RecordDirFromEnv returns the active record directory ("" when recording is
// not requested).
func RecordDirFromEnv() string { return os.Getenv(RecordDirEnv) }

// transcriptEntry is one recorded provider interaction.
type transcriptEntry struct {
	Index int    `json:"index"`
	Kind  string `json:"kind"` // "agentic" (CompleteWithTools) | "complete" (Complete)
	// ConvLen is the conversation length of the recorded CompleteWithTools
	// call — the replay's structural divergence check.
	ConvLen  int              `json:"conv_len,omitempty"`
	Response recordedResponse `json:"response"`
}

// recordedResponse is the JSON shape of one provider response. AgenticResponse
// itself has no JSON tags on some nested types, so the transcript owns its
// schema explicitly and converts.
type recordedResponse struct {
	Text       []string           `json:"text,omitempty"`
	Thinking   []string           `json:"thinking,omitempty"`
	ToolCalls  []recordedToolCall `json:"tool_calls,omitempty"`
	StopReason string             `json:"stop_reason,omitempty"`
	Usage      recordedUsage      `json:"usage"`
	// CompleteText is the reply of a single-turn Complete call ("complete").
	CompleteText string `json:"complete_text,omitempty"`
}

type recordedToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type recordedUsage struct {
	InTokens   int `json:"in_tokens"`
	OutTokens  int `json:"out_tokens"`
	CacheRead  int `json:"cache_read,omitempty"`
	CacheWrite int `json:"cache_write,omitempty"`
}

func toRecordedResponse(r *AgenticResponse) recordedResponse {
	out := recordedResponse{
		StopReason: string(r.StopReason),
		Usage: recordedUsage{
			InTokens:   r.Usage.InputTokens,
			OutTokens:  r.Usage.OutputTokens,
			CacheRead:  r.Usage.CacheReadInputTokens,
			CacheWrite: r.Usage.CacheCreationInputTokens,
		},
	}
	for _, tb := range r.TextBlocks {
		out.Text = append(out.Text, tb.Text)
	}
	for _, th := range r.ThinkingBlocks {
		out.Thinking = append(out.Thinking, th.Text)
	}
	for _, tc := range r.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, recordedToolCall{ID: tc.ID, Name: tc.Name, Input: tc.Input})
	}
	return out
}

func (r recordedResponse) toAgenticResponse() *AgenticResponse {
	out := &AgenticResponse{
		StopReason: StopReason(r.StopReason),
		Usage: Usage{
			InputTokens:              r.Usage.InTokens,
			OutputTokens:             r.Usage.OutTokens,
			CacheReadInputTokens:     r.Usage.CacheRead,
			CacheCreationInputTokens: r.Usage.CacheWrite,
		},
	}
	for _, t := range r.Text {
		out.TextBlocks = append(out.TextBlocks, TextBlock{Text: t})
	}
	for _, tc := range r.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCallRequest{ID: tc.ID, Name: tc.Name, Input: tc.Input})
	}
	// Thinking blocks are deliberately NOT replayed: recorded thinking lacks
	// the provider signature needed to resubmit it, and the eval harness
	// grades output/commands/files, none of which depend on thinking.
	return out
}

// ---------------------------------------------------------------------------
// Recording wrapper
// ---------------------------------------------------------------------------

// recordingProvider wraps a live AgenticProvider and appends every successful
// interaction to <dir>/transcript.jsonl.
type recordingProvider struct {
	inner AgenticProvider

	mu   sync.Mutex
	path string
	next int
}

// NewRecording wraps inner so every successful provider call is appended to
// dir's transcript. The directory is created; a pre-existing transcript is
// truncated (a recording is one run, not an accumulation).
func NewRecording(inner AgenticProvider, dir string) (AgenticProvider, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("record: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, TranscriptFileName)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		return nil, fmt.Errorf("record: truncate %s: %w", path, err)
	}
	return &recordingProvider{inner: inner, path: path}, nil
}

func (p *recordingProvider) Name() string { return p.inner.Name() }

func (p *recordingProvider) Complete(ctx context.Context, prompt string) (string, error) {
	reply, err := p.inner.Complete(ctx, prompt)
	if err != nil {
		return reply, err
	}
	p.append(transcriptEntry{Kind: "complete", Response: recordedResponse{CompleteText: reply}})
	return reply, nil
}

func (p *recordingProvider) CompleteWithTools(
	ctx context.Context,
	conversation []ConversationTurn,
	tools []ToolSpec,
	systemPrompt string,
) (*AgenticResponse, error) {
	resp, err := p.inner.CompleteWithTools(ctx, conversation, tools, systemPrompt)
	if err != nil || resp == nil {
		return resp, err
	}
	p.append(transcriptEntry{Kind: "agentic", ConvLen: len(conversation), Response: toRecordedResponse(resp)})
	return resp, nil
}

// append writes one entry. Failures are surfaced on stderr but never fail the
// live call: the run's own result must not depend on the recorder's disk.
func (p *recordingProvider) append(e transcriptEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e.Index = p.next
	p.next++
	// Encoder with HTML escaping off: tool inputs are code (`>`class shell
	// redirects, comparisons) and must round-trip byte-identically.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(e); err != nil {
		fmt.Fprintf(os.Stderr, "[record] WARNING: marshal transcript entry %d: %v\n", e.Index, err)
		return
	}
	f, err := os.OpenFile(p.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[record] WARNING: open transcript: %v\n", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(buf.Bytes()); err != nil {
		fmt.Fprintf(os.Stderr, "[record] WARNING: write transcript entry %d: %v\n", e.Index, err)
	}
}

// ---------------------------------------------------------------------------
// Replay provider
// ---------------------------------------------------------------------------

// replayProvider serves a recorded transcript in order. It needs no API key.
// Construction is lazy about errors: a missing/corrupt transcript is stored
// and returned on the first call, because the daemon constructs its provider
// where it cannot return an error — the run then fails loudly (exit 1)
// instead of silently degrading to another provider.
type replayProvider struct {
	mu      sync.Mutex
	loadErr error
	entries []transcriptEntry
	pos     int
	dir     string
}

// NewReplay builds a replay provider over dir's transcript.
func NewReplay(dir string) AgenticProvider {
	p := &replayProvider{dir: dir}
	p.entries, p.loadErr = loadTranscript(dir)
	return p
}

func loadTranscript(dir string) ([]transcriptEntry, error) {
	path := filepath.Join(dir, TranscriptFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf(progname.Rewrite("replay: read %s: %w (record one with `rysh run --record %s`)"), path, err, dir)
	}
	var entries []transcriptEntry
	dec := json.NewDecoder(bytes.NewReader(data))
	for dec.More() {
		var e transcriptEntry
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("replay: parse %s entry %d: %w", path, len(entries), err)
		}
		entries = append(entries, e)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("replay: %s holds no recorded turns", path)
	}
	return entries, nil
}

func (p *replayProvider) Name() string { return "replay" }

// take pops the next entry, requiring the given kind.
func (p *replayProvider) take(kind string) (transcriptEntry, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loadErr != nil {
		return transcriptEntry{}, p.loadErr
	}
	if p.pos >= len(p.entries) {
		return transcriptEntry{}, fmt.Errorf(
			"replay: transcript %s exhausted after %d turn(s) — the re-run asked for more provider calls than were recorded (the run diverged)",
			filepath.Join(p.dir, TranscriptFileName), len(p.entries))
	}
	e := p.entries[p.pos]
	if e.Kind != kind {
		return transcriptEntry{}, fmt.Errorf(
			"replay: turn %d was recorded as %q but the re-run asked for %q (the run diverged)",
			e.Index, e.Kind, kind)
	}
	p.pos++
	return e, nil
}

func (p *replayProvider) Complete(_ context.Context, _ string) (string, error) {
	e, err := p.take("complete")
	if err != nil {
		return "", err
	}
	return e.Response.CompleteText, nil
}

func (p *replayProvider) CompleteWithTools(
	_ context.Context,
	conversation []ConversationTurn,
	_ []ToolSpec,
	_ string,
) (*AgenticResponse, error) {
	e, err := p.take("agentic")
	if err != nil {
		return nil, err
	}
	// Structural divergence check: same turn index must see the same
	// conversation LENGTH (content legitimately differs — tool outputs carry
	// timestamps/paths — but the loop shape must match).
	if e.ConvLen != 0 && e.ConvLen != len(conversation) {
		return nil, fmt.Errorf(
			progname.Rewrite("replay: turn %d expected a conversation of %d turn(s), got %d — the run diverged from the recording (or compaction restructured it); re-record with `rysh run --record`"),
			e.Index, e.ConvLen, len(conversation))
	}
	return e.Response.toAgenticResponse(), nil
}
