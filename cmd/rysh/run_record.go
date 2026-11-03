// SPDX-License-Identifier: Apache-2.0

package main

// The shareable run record: `rysh run --share <dir>` (root-order-044 item
// #12; design 009 §3.4). One folder per headless run that a reviewer can open
// without rysh and replay with it:
//
//	record.json       the run record (the --json-audit artifact, unchanged,
//	                  plus the base commit, replay facts and redaction counts)
//	transcript.jsonl  the redacted provider transcript, so
//	                  `rysh run --replay <dir>` works on the bundle as-is
//	index.html        a self-contained viewer with the record embedded
//
// Governance, deliberately strict:
//   - Every string in the record and the transcript goes through the
//     built-in secretnat detectors; a match becomes [REDACTED:<detector>].
//     Only per-detector COUNTS are kept — never a value, length, prefix or
//     hash. $HOME and the username are masked.
//   - Fail closed: no registry, an unparseable transcript or any write error
//     means no bundle at all (staging is removed, nothing is renamed), and
//     the run exits runExitError (shareExitCode).
//   - The viewer is inert: CSP default-src 'none', its one script pinned by
//     hash, no external URL, rendering via textContent only.
//   - The record is not signed and claims no integrity; `limits` says so.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-shared/secretnat"

	"github.com/rysh-ai/rysh-cli-code/internal/provider"
	"github.com/rysh-ai/rysh-cli-code/internal/sandbox"
)

// runRecordSchemaVersion identifies the record.json schema.
const runRecordSchemaVersion = 1

//go:embed run_record_viewer.html
var runRecordViewerHTML string

//go:embed run_record_viewer.js
var runRecordViewerJS string

// runRecordRepo is the repository state the run started from — what a
// reviewer checks out before replaying.
type runRecordRepo struct {
	// BaseCommit is HEAD before the run; null when there is none (BaseNote
	// says why).
	BaseCommit  *string `json:"base_commit"`
	BaseNote    string  `json:"base_note,omitempty"`
	DirtyBefore bool    `json:"dirty_before"`
}

// runRecordReplay says whether and how the bundle replays.
type runRecordReplay struct {
	Available         bool   `json:"available"`
	TranscriptEntries int    `json:"transcript_entries"`
	SingleLoop        bool   `json:"single_loop"`
	Command           string `json:"command,omitempty"`
	Note              string `json:"note,omitempty"`
}

// runRecordRedaction holds counts only.
type runRecordRedaction struct {
	DetectorHits    map[string]int `json:"detector_hits"`
	HomePathsMasked int            `json:"home_paths_masked"`
	UsernameMasked  int            `json:"username_masked"`
}

// runRecord is record.json.
type runRecord struct {
	SchemaVersion int                `json:"schema_version"`
	RyshVersion   string             `json:"rysh_version"`
	CreatedAt     time.Time          `json:"created_at"`
	Repo          runRecordRepo      `json:"repo"`
	Audit         *runAudit          `json:"audit"`
	Replay        runRecordReplay    `json:"replay"`
	Redaction     runRecordRedaction `json:"redaction"`
	// Sandboxed is "<backend>/<profile>" when the run's process was started
	// inside a rysh sandbox (design 066), else "none". Self-reported: it is
	// read from the RYSH_SANDBOX marker the sandbox launcher sets.
	Sandboxed string   `json:"sandboxed"`
	Limits    []string `json:"limits"`
}

// runRecordInput is everything buildRunRecord needs; it does no I/O.
type runRecordInput struct {
	Audit      *runAudit
	Transcript []byte // nil: no transcript was recorded
	Repo       runRecordRepo
	Worktree   bool // the run used --worktree (the replay should too)
	Version    string
	CreatedAt  time.Time
	Registry   *secretnat.Registry
	Home, User string
	// Sandbox is the RYSH_SANDBOX marker ("" when not sandboxed).
	Sandbox string
}

// runRecordBundle is the three files' bytes. Transcript is nil when absent.
type runRecordBundle struct {
	Record, Transcript, Index []byte
}

// runRecordLimits are the record's own documented limits, added to the
// audit's.
var runRecordLimits = []string{
	"not signed: this record carries no integrity or tamper evidence",
	"secret redaction uses the built-in secretnat detectors only; a secret no detector recognises is not redacted — review the record before sharing it",
	"replay re-executes the recorded tool calls on the replaying machine, under the same policy and fail-closed approval gates as any headless run; redacted tool inputs replay as their [REDACTED:…] placeholder text",
	"replay serves the recorded provider turns in order and is only meaningful for single-loop runs; only successful provider calls are recorded",
}

// buildRunRecord joins the audit, the transcript and the repo state into the
// bundle, redacting every string. Pure over its inputs. Any failure returns
// no bundle (fail closed).
func buildRunRecord(in runRecordInput) (*runRecordBundle, error) {
	if in.Registry == nil {
		return nil, errors.New("run record: no secret detector registry — refusing to write an unredacted record")
	}
	if in.Audit == nil {
		return nil, errors.New("run record: no audit to record")
	}
	red := newRecordRedactor(in.Registry, in.Home, in.User)

	// The transcript first: its entry count feeds the record.
	var transcript []byte
	entries := 0
	if in.Transcript != nil {
		var out bytes.Buffer
		sc := bufio.NewScanner(bytes.NewReader(in.Transcript))
		sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
		for sc.Scan() {
			line := bytes.TrimSpace(sc.Bytes())
			if len(line) == 0 {
				continue
			}
			doc, err := red.jsonDoc(line)
			if err != nil {
				return nil, fmt.Errorf("run record: transcript entry %d does not parse: %w", entries+1, err)
			}
			b, err := json.Marshal(doc)
			if err != nil {
				return nil, fmt.Errorf("run record: re-encode transcript entry %d: %w", entries+1, err)
			}
			out.Write(b)
			out.WriteByte('\n')
			entries++
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("run record: read transcript: %w", err)
		}
		transcript = out.Bytes()
		if transcript == nil {
			transcript = []byte{}
		}
	}

	singleLoop := true
	for _, c := range in.Audit.ToolCalls {
		if c.Depth > 0 {
			singleLoop = false
			break
		}
	}
	replay := runRecordReplay{TranscriptEntries: entries, SingleLoop: singleLoop}
	switch {
	case in.Transcript == nil:
		replay.Note = "no provider transcript was recorded for this run — it cannot be replayed"
	case entries == 0:
		replay.Note = "the provider transcript is empty (no successful provider call) — nothing to replay"
	default:
		replay.Available = true
		wt := ""
		if in.Worktree {
			wt = " --worktree"
		}
		replay.Command = "git checkout <repo.base_commit> && rysh run --replay <this folder>" + wt + " \"<audit.prompt>\""
		if !singleLoop {
			replay.Note = "this run used sub-agents (tool calls at depth > 0); replay order is not deterministic"
		}
	}

	rec := runRecord{
		SchemaVersion: runRecordSchemaVersion,
		RyshVersion:   in.Version,
		CreatedAt:     in.CreatedAt.UTC(),
		Repo:          in.Repo,
		Audit:         in.Audit,
		Replay:        replay,
		Sandboxed:     "none",
		Limits:        append(append([]string{}, in.Audit.Limits...), runRecordLimits...),
	}
	if sb := strings.TrimSpace(in.Sandbox); sb != "" {
		rec.Sandboxed = sb
		rec.Limits = append(rec.Limits, "sandboxed is self-reported: it is the "+sandbox.MarkerEnv+" marker the rysh sandbox launcher sets, and any process can set it")
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return nil, fmt.Errorf("run record: encode: %w", err)
	}
	doc, err := red.jsonDoc(raw)
	if err != nil {
		return nil, fmt.Errorf("run record: redact: %w", err)
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("run record: encoded record is not an object")
	}
	// Counts are set AFTER the walk, so they are not themselves scanned;
	// they are detector names and integers only.
	m["redaction"] = red.counts()

	record, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("run record: encode: %w", err)
	}
	record = append(record, '\n')

	// The viewer embeds the compact form. encoding/json escapes <, > and &
	// (and U+2028/9), so the embedded text cannot close its script element.
	compact, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("run record: encode: %w", err)
	}
	index, err := renderRunRecordViewer(compact)
	if err != nil {
		return nil, err
	}
	return &runRecordBundle{Record: record, Transcript: transcript, Index: index}, nil
}

// viewerScriptHash is the CSP source for the viewer's one inline script.
func viewerScriptHash() string {
	sum := sha256.Sum256([]byte(runRecordViewerJS))
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

// renderRunRecordViewer fills the viewer template. The record is substituted
// LAST, so placeholder-looking text inside it is never expanded.
func renderRunRecordViewer(recordJSON []byte) ([]byte, error) {
	for _, ph := range []string{"{{CSP_SCRIPT_HASH}}", "{{VIEWER_JS}}", "{{RECORD_JSON}}"} {
		if strings.Count(runRecordViewerHTML, ph) != 1 {
			return nil, fmt.Errorf("run record: viewer template must hold %s exactly once", ph)
		}
	}
	if bytes.Contains(recordJSON, []byte("</")) || bytes.ContainsAny(recordJSON, "<>") {
		return nil, errors.New("run record: embedded record is not HTML-escaped — refusing to render the viewer")
	}
	page := strings.Replace(runRecordViewerHTML, "{{CSP_SCRIPT_HASH}}", viewerScriptHash(), 1)
	page = strings.Replace(page, "{{VIEWER_JS}}", runRecordViewerJS, 1)
	page = strings.Replace(page, "{{RECORD_JSON}}", string(recordJSON), 1)
	return []byte(page), nil
}

// recordRedactor walks decoded JSON and redacts every string (keys too).
type recordRedactor struct {
	reg    *secretnat.Registry
	home   string
	userRe *regexp.Regexp
	hits   map[string]int
	homeN  int
	userN  int
}

func newRecordRedactor(reg *secretnat.Registry, home, user string) *recordRedactor {
	r := &recordRedactor{reg: reg, hits: map[string]int{}}
	if home = strings.TrimRight(home, "/"); len(home) > 1 {
		r.home = home
	}
	// Short usernames would mangle ordinary words; three characters is the
	// floor for whole-word masking.
	if len(user) >= 3 {
		r.userRe = regexp.MustCompile(`\b` + regexp.QuoteMeta(user) + `\b`)
	}
	return r
}

func (r *recordRedactor) str(s string) string {
	if ms := r.reg.DetectAll(s); len(ms) > 0 {
		var b strings.Builder
		last := 0
		for _, m := range ms {
			b.WriteString(s[last:m.Start])
			b.WriteString("[REDACTED:" + m.Type + "]")
			last = m.End
			r.hits[m.Type]++
		}
		b.WriteString(s[last:])
		s = b.String()
	}
	if r.home != "" {
		if n := strings.Count(s, r.home); n > 0 {
			s = strings.ReplaceAll(s, r.home, "$HOME")
			r.homeN += n
		}
	}
	if r.userRe != nil {
		if n := len(r.userRe.FindAllStringIndex(s, -1)); n > 0 {
			s = r.userRe.ReplaceAllLiteralString(s, "$USER")
			r.userN += n
		}
	}
	return s
}

func (r *recordRedactor) walk(v any) any {
	switch t := v.(type) {
	case string:
		return r.str(t)
	case []any:
		for i := range t {
			t[i] = r.walk(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[r.str(k)] = r.walk(val)
		}
		return out
	default:
		return v // numbers (json.Number), bools, null
	}
}

// jsonDoc decodes one JSON document (numbers kept exact) and redacts it.
func (r *recordRedactor) jsonDoc(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after JSON document")
	}
	return r.walk(v), nil
}

func (r *recordRedactor) counts() runRecordRedaction {
	hits := make(map[string]int, len(r.hits))
	names := make([]string, 0, len(r.hits))
	for k := range r.hits {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		hits[k] = r.hits[k]
	}
	return runRecordRedaction{DetectorHits: hits, HomePathsMasked: r.homeN, UsernameMasked: r.userN}
}

// captureRunRecordRepo records HEAD and whether the tree was dirty, from the
// before-snapshot the run already takes.
func captureRunRecordRepo(dir string, before map[string]string, inRepo bool) runRecordRepo {
	if !inRepo {
		return runRecordRepo{BaseNote: "the run directory is not a git repository"}
	}
	r := runRecordRepo{DirtyBefore: len(before) > 0}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "-q", "HEAD").Output()
	if err != nil {
		r.BaseNote = "the repository has no commits (unborn HEAD)"
		return r
	}
	sha := strings.TrimSpace(string(out))
	r.BaseCommit = &sha
	if r.DirtyBefore {
		r.BaseNote = "the working tree had uncommitted changes before the run; base_commit alone does not reproduce it"
	}
	return r
}

// Seams for the fail-closed tests.
var (
	runRecordRegistry = func() (*secretnat.Registry, error) {
		// All built-in detectors, none disabled by config: the share bundle
		// is stricter than the live session.
		return secretnat.NewDefaultRegistry(nil, nil)
	}
	runRecordWriteFile = os.WriteFile
	runRecordIdentity  = func() (home, name string) {
		home, _ = os.UserHomeDir()
		if u, err := user.Current(); err == nil {
			name = u.Username
		}
		return home, name
	}
)

// shareInput is what the run hands writeShareBundle.
type shareInput struct {
	Audit     *runAudit
	RecordDir string // where the raw transcript was recorded ("" = none)
	Repo      runRecordRepo
	Worktree  bool
	CreatedAt time.Time
}

// checkShareDir refuses a --share destination that already exists, before
// the run spends anything.
func checkShareDir(dir string) error {
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("--share %s already exists — choose a new folder", dir)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("--share %s: %w", dir, err)
	}
	return nil
}

// writeShareBundle builds the bundle and writes it to dir atomically: into a
// private staging folder beside dir, then one rename. Any failure removes the
// staging folder and leaves dir absent.
func writeShareBundle(dir string, in shareInput) error {
	if err := checkShareDir(dir); err != nil {
		return err
	}
	reg, err := runRecordRegistry()
	if err != nil {
		return fmt.Errorf("run record: secret detectors unavailable: %w", err)
	}
	var transcript []byte
	if in.RecordDir != "" {
		b, rerr := os.ReadFile(filepath.Join(in.RecordDir, provider.TranscriptFileName))
		switch {
		case rerr == nil:
			transcript = b
		case !os.IsNotExist(rerr):
			return fmt.Errorf("run record: read transcript: %w", rerr)
		}
	}
	home, name := runRecordIdentity()
	b, err := buildRunRecord(runRecordInput{
		Audit: in.Audit, Transcript: transcript, Repo: in.Repo, Worktree: in.Worktree,
		Version: version, CreatedAt: in.CreatedAt, Registry: reg, Home: home, User: name,
		Sandbox: os.Getenv(sandbox.MarkerEnv),
	})
	if err != nil {
		return err
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("run record: resolve %s: %w", dir, err)
	}
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("run record: create %s: %w", parent, err)
	}
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(abs)+".staging-")
	if err != nil {
		return fmt.Errorf("run record: staging folder: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(stage)
		}
	}()
	files := []struct {
		name string
		data []byte
	}{{"record.json", b.Record}}
	if b.Transcript != nil {
		files = append(files, struct {
			name string
			data []byte
		}{provider.TranscriptFileName, b.Transcript})
	}
	files = append(files, struct {
		name string
		data []byte
	}{"index.html", b.Index})
	for _, f := range files {
		if err := runRecordWriteFile(filepath.Join(stage, f.name), f.data, 0o644); err != nil {
			return fmt.Errorf("run record: write %s: %w", f.name, err)
		}
	}
	if err := os.Chmod(stage, 0o755); err != nil {
		return fmt.Errorf("run record: chmod: %w", err)
	}
	if err := os.Rename(stage, abs); err != nil {
		return fmt.Errorf("run record: move into place: %w", err)
	}
	ok = true
	return nil
}

// shareExitCode applies root decision (a): when --share was asked for and the
// bundle could not be written, the run fails closed with runExitError.
func shareExitCode(runCode int, shareErr error) int {
	if shareErr != nil {
		return runExitError
	}
	return runCode
}
