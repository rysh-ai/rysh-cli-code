// SPDX-License-Identifier: Apache-2.0

package main

// Tests for the shareable run record (`rysh run --share <dir>`, root-order-044
// item #12; E9 design note out/e9-run-record-design.md). Failures report
// counts only: never a planted secret, nor anything derived from it
// (rr-msg-240).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-shared/secretnat"

	"github.com/rysh-ai/rysh-cli-code/internal/provider"
	"github.com/rysh-ai/rysh-cli-code/internal/proxy/wirecheck"
)

// testHome / testUser stand in for the machine identity the record masks.
const (
	testHome = "/home/e9tester"
	testUser = "e9tester"
)

func defaultRegistry(t *testing.T) *secretnat.Registry {
	t.Helper()
	reg, err := secretnat.NewDefaultRegistry(nil, nil)
	if err != nil {
		t.Fatalf("secretnat registry: %v", err)
	}
	return reg
}

// recordTranscript records a real two-turn transcript through the production
// recording provider and returns its bytes.
func recordTranscript(t *testing.T, p provider.AgenticProvider) []byte {
	t.Helper()
	dir := t.TempDir()
	rec, err := provider.NewRecording(p, dir)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	gitT(t, work, "init", "-q")
	if _, _, err := driveProviderAgent(t, rec, work); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, provider.TranscriptFileName))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func baseCommit(s string) *string { return &s }

func testRecordInput(t *testing.T, audit *runAudit, transcript []byte) runRecordInput {
	t.Helper()
	return runRecordInput{
		Audit:      audit,
		Transcript: transcript,
		Repo:       runRecordRepo{BaseCommit: baseCommit("0123456789abcdef0123456789abcdef01234567")},
		Version:    "9.9.9-test",
		CreatedAt:  time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC),
		Registry:   defaultRegistry(t),
		Home:       testHome,
		User:       testUser,
	}
}

func minimalAudit() *runAudit {
	return &runAudit{
		SchemaVersion: runAuditSchemaVersion,
		Session:       "run-1",
		Provider:      "scripted-live",
		Prompt:        "create hello.txt then reply DONE",
		Status:        "done",
		ToolCalls: []auditToolCall{{Tool: "bash", InputSummary: "echo hello rysh > hello.txt",
			StartedAtMs: 1000, EndedAtMs: 1200, DurationMs: 200}},
		Approvals: []auditApproval{},
		Usage:     auditUsage{Records: 2, InTokens: 370, OutTokens: 50, TotalTokens: 420, CostMicroUSD: 1234},
		Budget:    auditBudget{SpentTokens: 420, SpentMicroUSD: 1234},
		Diff:      auditDiff{GitAvailable: true, ChangedPaths: []string{"hello.txt"}},
		Limits:    []string{"audit limit kept"},
	}
}

// decodeRecord parses record.json into the typed record.
func decodeRecord(t *testing.T, b []byte) runRecord {
	t.Helper()
	var r runRecord
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("record.json does not parse: %v", err)
	}
	return r
}

// 1. The join: audit unchanged, base commit, transcript facts.
func TestBuildRunRecord_JoinsAuditTranscriptAndRepo(t *testing.T) {
	transcript := recordTranscript(t, &scriptedLiveProvider{})
	audit := minimalAudit()
	b, err := buildRunRecord(testRecordInput(t, audit, transcript))
	if err != nil {
		t.Fatalf("buildRunRecord: %v", err)
	}
	r := decodeRecord(t, b.Record)
	if r.SchemaVersion != runRecordSchemaVersion || runRecordSchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", r.SchemaVersion)
	}
	if r.RyshVersion != "9.9.9-test" || !r.CreatedAt.Equal(time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("version/created_at not carried: %q %v", r.RyshVersion, r.CreatedAt)
	}
	if r.Repo.BaseCommit == nil || *r.Repo.BaseCommit != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatal("base_commit not carried")
	}
	if r.Audit == nil || r.Audit.Session != "run-1" || len(r.Audit.ToolCalls) != 1 ||
		r.Audit.Usage.TotalTokens != 420 || r.Audit.Usage.CostMicroUSD != 1234 ||
		r.Audit.ToolCalls[0].InputSummary != "echo hello rysh > hello.txt" {
		t.Fatalf("audit not carried unchanged: %+v", r.Audit)
	}
	if !r.Replay.Available || r.Replay.TranscriptEntries != 2 || !r.Replay.SingleLoop {
		t.Fatalf("replay facts wrong: %+v", r.Replay)
	}
	if !strings.Contains(r.Replay.Command, "rysh run --replay") {
		t.Fatalf("replay command missing: %q", r.Replay.Command)
	}
	if len(b.Transcript) == 0 {
		t.Fatal("transcript.jsonl missing from bundle")
	}
	// The record's limits keep the audit's and add the record's own.
	joined := strings.Join(r.Limits, "\n")
	for _, want := range []string{"audit limit kept", "not signed", "re-executes", "detector"} {
		if !strings.Contains(joined, want) {
			t.Errorf("limits miss %q", want)
		}
	}
}

func TestBuildRunRecord_SubAgentsAreNotSingleLoop(t *testing.T) {
	audit := minimalAudit()
	audit.ToolCalls = append(audit.ToolCalls, auditToolCall{Tool: "bash", Depth: 1})
	b, err := buildRunRecord(testRecordInput(t, audit, recordTranscript(t, &scriptedLiveProvider{})))
	if err != nil {
		t.Fatal(err)
	}
	if decodeRecord(t, b.Record).Replay.SingleLoop {
		t.Fatal("a run with depth>0 tool calls must not claim single_loop")
	}
}

func TestBuildRunRecord_NoTranscriptMeansNoReplay(t *testing.T) {
	b, err := buildRunRecord(testRecordInput(t, minimalAudit(), nil))
	if err != nil {
		t.Fatal(err)
	}
	r := decodeRecord(t, b.Record)
	if r.Replay.Available || r.Replay.Note == "" || b.Transcript != nil {
		t.Fatalf("absent transcript must give available=false with a note and no file: %+v", r.Replay)
	}
}

func TestBuildRunRecord_NoBaseCommitHasReason(t *testing.T) {
	in := testRecordInput(t, minimalAudit(), nil)
	in.Repo = runRecordRepo{BaseNote: "not a git repository"}
	b, err := buildRunRecord(in)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Repo map[string]any `json:"repo"`
	}
	if err := json.Unmarshal(b.Record, &raw); err != nil {
		t.Fatal(err)
	}
	if v, ok := raw.Repo["base_commit"]; !ok || v != nil {
		t.Fatal("base_commit must be present and null when there is none")
	}
	if raw.Repo["base_note"] == "" {
		t.Fatal("a null base_commit needs its reason")
	}
}

// 2. Redaction: the planted value occurs 0 times in all three files.
func TestBuildRunRecord_RedactsPlantedSecretEverywhere(t *testing.T) {
	secret := wirecheck.PlantedSecret()
	transcript := recordTranscript(t, &plantedLiveProvider{secret: secret})

	audit := minimalAudit()
	audit.Prompt = "deploy with key " + secret
	audit.ToolCalls[0].InputSummary = "curl -H 'Authorization: " + secret + "'"
	audit.Diff.Patch = "+API_KEY=" + secret + "\n"
	audit.Result.Output = "used " + secret
	audit.Approvals = []auditApproval{{RequestID: "r1", Description: "send " + secret + "?", Decision: "fail_closed"}}

	b, err := buildRunRecord(testRecordInput(t, audit, transcript))
	if err != nil {
		t.Fatalf("buildRunRecord: %v", err)
	}
	for name, data := range map[string][]byte{"record.json": b.Record, "transcript.jsonl": b.Transcript, "index.html": b.Index} {
		if n := bytes.Count(data, []byte(secret)); n != 0 {
			t.Errorf("%s still holds the planted secret %dx", name, n)
		}
	}
	r := decodeRecord(t, b.Record)
	if r.Redaction.DetectorHits["stripe"] < 6 {
		t.Errorf("stripe detector hits = %d, want >= 6 (prompt, summary, patch, output, approval, transcript)",
			r.Redaction.DetectorHits["stripe"])
	}
	if !strings.Contains(string(b.Record), "[REDACTED:stripe]") || !strings.Contains(string(b.Transcript), "[REDACTED:stripe]") {
		t.Error("redacted spans must read [REDACTED:<detector>]")
	}
	// The redacted transcript still parses as a replay transcript.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, provider.TranscriptFileName), b.Transcript, 0o644); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	gitT(t, work, "init", "-q")
	if out, _, err := driveProviderAgent(t, provider.NewReplay(dir), work); err != nil || out.ExitCode != runExitDone {
		t.Fatalf("redacted transcript must replay: %+v %v", out, err)
	}
}

// plantedLiveProvider is scriptedLiveProvider with the secret in the tool
// input and the final text.
type plantedLiveProvider struct {
	secret string
	calls  int
}

func (s *plantedLiveProvider) Name() string { return "planted-live" }

func (s *plantedLiveProvider) Complete(context.Context, string) (string, error) {
	return "", errors.New("not used")
}

func (s *plantedLiveProvider) CompleteWithTools(
	_ context.Context, _ []provider.ConversationTurn, _ []provider.ToolSpec, _ string,
) (*provider.AgenticResponse, error) {
	defer func() { s.calls++ }()
	switch s.calls {
	case 0:
		in, _ := json.Marshal(map[string]string{"command": "echo " + s.secret + " > /dev/null"})
		return &provider.AgenticResponse{
			ToolCalls:  []provider.ToolCallRequest{{ID: "call_1", Name: "bash", Input: in}},
			StopReason: provider.StopReasonToolUse,
			Usage:      provider.Usage{InputTokens: 10, OutputTokens: 5},
		}, nil
	case 1:
		return &provider.AgenticResponse{
			TextBlocks: []provider.TextBlock{{Text: "DONE with " + s.secret}},
			StopReason: provider.StopReasonEndTurn,
			Usage:      provider.Usage{InputTokens: 12, OutputTokens: 6},
		}, nil
	}
	return nil, errors.New("planted provider exhausted")
}

// 3. $HOME and the username are masked.
func TestBuildRunRecord_MasksHomeAndUser(t *testing.T) {
	audit := minimalAudit()
	audit.ToolCalls[0].InputSummary = "cat " + testHome + "/project/README.md"
	audit.Detail = "run by " + testUser
	b, err := buildRunRecord(testRecordInput(t, audit, nil))
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"record.json": b.Record, "index.html": b.Index} {
		if n := bytes.Count(data, []byte(testUser)); n != 0 {
			t.Errorf("%s holds the username %dx", name, n)
		}
	}
	r := decodeRecord(t, b.Record)
	if r.Audit.ToolCalls[0].InputSummary != "cat $HOME/project/README.md" {
		t.Errorf("home not masked: %q", r.Audit.ToolCalls[0].InputSummary)
	}
	if r.Redaction.HomePathsMasked != 1 || r.Redaction.UsernameMasked != 1 {
		t.Errorf("mask counts = home %d user %d, want 1/1", r.Redaction.HomePathsMasked, r.Redaction.UsernameMasked)
	}
}

// 4. Fail closed: no registry, no bundle.
func TestBuildRunRecord_NilRegistryFailsClosed(t *testing.T) {
	in := testRecordInput(t, minimalAudit(), nil)
	in.Registry = nil
	if b, err := buildRunRecord(in); err == nil || b != nil {
		t.Fatal("a nil registry must fail closed with no bundle")
	}
}

func TestBuildRunRecord_BadTranscriptFailsClosed(t *testing.T) {
	if b, err := buildRunRecord(testRecordInput(t, minimalAudit(), []byte("{not json\n"))); err == nil || b != nil {
		t.Fatal("an unparseable transcript must fail closed with no bundle")
	}
}

func TestWriteShareBundle_RegistryErrorWritesNothing(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "share")
	prev := runRecordRegistry
	runRecordRegistry = func() (*secretnat.Registry, error) { return nil, errors.New("injected") }
	defer func() { runRecordRegistry = prev }()

	err := writeShareBundle(dest, shareInput{Audit: minimalAudit(), CreatedAt: time.Now()})
	if err == nil {
		t.Fatal("registry error must fail")
	}
	assertNothingWritten(t, parent)
}

func TestWriteShareBundle_WriteErrorLeavesNothing(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "share")
	prev := runRecordWriteFile
	runRecordWriteFile = func(path string, data []byte, perm os.FileMode) error {
		if strings.HasSuffix(path, "index.html") {
			return errors.New("injected write error")
		}
		return os.WriteFile(path, data, perm)
	}
	defer func() { runRecordWriteFile = prev }()

	if err := writeShareBundle(dest, shareInput{Audit: minimalAudit(), CreatedAt: time.Now()}); err == nil {
		t.Fatal("write error must fail")
	}
	assertNothingWritten(t, parent)
}

func TestWriteShareBundle_RefusesExistingDir(t *testing.T) {
	dest := t.TempDir() // exists
	if err := writeShareBundle(dest, shareInput{Audit: minimalAudit(), CreatedAt: time.Now()}); err == nil {
		t.Fatal("an existing --share dir must be refused")
	}
}

func TestWriteShareBundle_WritesAllFiles(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "share")
	rec := t.TempDir()
	if err := os.WriteFile(filepath.Join(rec, provider.TranscriptFileName),
		recordTranscript(t, &scriptedLiveProvider{}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeShareBundle(dest, shareInput{Audit: minimalAudit(), RecordDir: rec, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("writeShareBundle: %v", err)
	}
	for _, f := range []string{"record.json", provider.TranscriptFileName, "index.html"} {
		if _, err := os.Stat(filepath.Join(dest, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	ents, _ := os.ReadDir(parent)
	if len(ents) != 1 {
		t.Errorf("parent holds %d entries, want only the bundle (no staging left)", len(ents))
	}
}

func assertNothingWritten(t *testing.T, parent string) {
	t.Helper()
	ents, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("fail-closed path left %d entries in the parent dir", len(ents))
	}
}

// Fail closed at the exit code (root decision (a)).
func TestShareExitCode(t *testing.T) {
	if got := shareExitCode(runExitDone, nil); got != runExitDone {
		t.Errorf("no share error keeps the run's code, got %d", got)
	}
	if got := shareExitCode(runExitGateBlocked, nil); got != runExitGateBlocked {
		t.Errorf("no share error keeps the run's code, got %d", got)
	}
	if got := shareExitCode(runExitDone, errors.New("x")); got != runExitError {
		t.Errorf("a share error must exit runExitError, got %d", got)
	}
	if got := shareExitCode(runExitGateBlocked, errors.New("x")); got == runExitDone {
		t.Errorf("a share error must never exit 0")
	}
}

// 5. The viewer is inert.
func TestRunRecordViewer_Inert(t *testing.T) {
	audit := minimalAudit()
	audit.Prompt = `</script><script>alert(1)</script><img src=x onerror=alert(2)>`
	b, err := buildRunRecord(testRecordInput(t, audit, nil))
	if err != nil {
		t.Fatal(err)
	}
	page := string(b.Index)
	// The attribute text itself may survive as inert JSON string content; what
	// must never appear is a raw tag that could open or close an element.
	for _, bad := range []string{"</script><script>alert", "<img"} {
		if n := strings.Count(page, bad); n != 0 {
			t.Errorf("viewer holds raw %q %dx", bad, n)
		}
	}
	if !strings.Contains(page, `http-equiv="Content-Security-Policy"`) || !strings.Contains(page, "default-src 'none'") {
		t.Error("viewer must carry a default-src 'none' CSP")
	}
	if strings.Contains(page, "script-src 'unsafe-inline'") {
		t.Error("script-src must be pinned by hash, not unsafe-inline")
	}
	if n := strings.Count(page, "http://") + strings.Count(page, "https://"); n != 0 {
		t.Errorf("viewer holds %d external URLs", n)
	}
	if n := strings.Count(runRecordViewerJS, "innerHTML") + strings.Count(runRecordViewerJS, "outerHTML") +
		strings.Count(runRecordViewerJS, "insertAdjacentHTML") + strings.Count(runRecordViewerJS, "document.write") +
		strings.Count(runRecordViewerJS, "fetch(") + strings.Count(runRecordViewerJS, "eval("); n != 0 {
		t.Errorf("viewer script uses %d HTML-injection or network sinks", n)
	}
	if n := strings.Count(page, `id="run-record"`); n != 1 {
		t.Errorf("want exactly one embedded record block, got %d", n)
	}
	// The script hash in the CSP matches the script actually embedded.
	if !strings.Contains(page, "'"+viewerScriptHash()+"'") {
		t.Error("CSP script hash does not match the embedded script")
	}
}

func TestRunRecordViewer_TemplatePlaceholdersOnce(t *testing.T) {
	for _, ph := range []string{"{{CSP_SCRIPT_HASH}}", "{{VIEWER_JS}}", "{{RECORD_JSON}}"} {
		if n := strings.Count(runRecordViewerHTML, ph); n != 1 {
			t.Errorf("template holds %s %dx, want 1", ph, n)
		}
	}
	if strings.Contains(runRecordViewerJS, "{{") {
		t.Error("viewer script must hold no template placeholders")
	}
}

// 6. Flags.
func TestParseRunArgs_ShareFlag(t *testing.T) {
	opts, err := parseRunArgs([]string{"p", "--share", "out/rec"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.ShareDir != "out/rec" {
		t.Fatalf("--share not parsed: %+v", opts)
	}
	for name, args := range map[string][]string{
		"share missing arg":      {"p", "--share"},
		"share+replay exclusive": {"p", "--share", "a", "--replay", "b"},
	} {
		if _, err := parseRunArgs(args); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
	// --share with --record keeps the caller's own raw recording.
	opts, err = parseRunArgs([]string{"p", "--share", "a", "--record", "r"})
	if err != nil || opts.RecordDir != "r" || opts.ShareDir != "a" {
		t.Fatalf("--share + --record: %+v %v", opts, err)
	}
}

// 7. Base commit capture.
func TestCaptureRunRecordRepo(t *testing.T) {
	dir := t.TempDir()
	if r := captureRunRecordRepo(dir, nil, false); r.BaseCommit != nil || r.BaseNote == "" {
		t.Fatalf("non-repo: %+v", r)
	}
	gitT(t, dir, "init", "-q")
	if r := captureRunRecordRepo(dir, map[string]string{}, true); r.BaseCommit != nil || r.BaseNote == "" {
		t.Fatalf("unborn HEAD: %+v", r)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "a.txt")
	gitT(t, dir, "-c", "user.email=t@example.invalid", "-c", "user.name=t", "commit", "-q", "-m", "a")
	r := captureRunRecordRepo(dir, map[string]string{}, true)
	if r.BaseCommit == nil || len(*r.BaseCommit) != 40 || r.DirtyBefore {
		t.Fatalf("clean repo: %+v", r)
	}
	if r := captureRunRecordRepo(dir, map[string]string{"b.txt": "??"}, true); !r.DirtyBefore {
		t.Fatal("a dirty before-snapshot must set dirty_before")
	}
}
