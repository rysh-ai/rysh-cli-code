// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Tests for `rysh ansa keys` / `##ansa keys`: named keys to ONE pane.
//
// The properties under test, in the order they matter:
//  1. a refused send writes NOTHING to any PTY (asserted on what the transport
//     was asked to write, not on the receipt);
//  2. guarded keys (enter, tab, ctrl+c, …) need --force, and with --force the
//     pane must answer a probe;
//  3. every send is audited BEFORE the first key, and no audit means no keys;
//  4. one target only: an id, a unique @name, or a unique claude session id.

// keysFake extends the shared fake with a record of key writes.
type keysFake struct {
	*fakeAnsaTransport
	mu      sync.Mutex
	writes  map[string][][]byte
	keysErr error
}

func newKeysFake(panes []ansaPane) *keysFake {
	return &keysFake{fakeAnsaTransport: &fakeAnsaTransport{panes: panes}, writes: map[string][][]byte{}}
}

func (f *keysFake) SendKeys(paneID string, writes [][]byte) error {
	if f.keysErr != nil {
		return f.keysErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes[paneID] = append(f.writes[paneID], writes...)
	return nil
}

func (f *keysFake) wrote() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, w := range f.writes {
		n += len(w)
	}
	return n
}

// auditLog collects audit entries; failWith makes every append fail.
type auditLog struct {
	mu       sync.Mutex
	entries  []ansaKeysAuditEntry
	failWith error
}

func (a *auditLog) append(e ansaKeysAuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failWith != nil {
		return a.failWith
	}
	a.entries = append(a.entries, e)
	return nil
}

func (a *auditLog) all() []ansaKeysAuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]ansaKeysAuditEntry(nil), a.entries...)
}

func keysPanes() []ansaPane {
	return []ansaPane{
		{ID: "pane-aaaa", GivenName: "release-prep", Meta: map[string]string{"claude.session_id": "sid-1111"}},
		{ID: "pane-bbbb", GivenName: "wkr", Meta: map[string]string{"claude.session_id": "sid-2222"}},
		{ID: "pane-cccc", GivenName: "wkr", Meta: map[string]string{}},
		// Two panes claiming one claude session: a resumed copy, or a stale
		// stamp. Neither may be guessed.
		{ID: "pane-dddd", GivenName: "dup-a", Meta: map[string]string{"claude.session_id": "sid-dup"}},
		{ID: "pane-eeee", GivenName: "dup-b", Meta: map[string]string{"claude.session_id": "sid-dup"}},
	}
}

func runKeys(f *keysFake, a *auditLog, req ansaKeysRequest) *ansaKeysOutcome {
	return ansaKeysAtEdge(&ansaRouter{tr: f}, req, a.append)
}

func TestAnsaKeysSafeKeyByName(t *testing.T) {
	f := newKeysFake(keysPanes())
	f.program = "claude"
	a := &auditLog{}
	out := runKeys(f, a, ansaKeysRequest{Door: "cli", From: "pane-from", To: "@release-prep", Keys: []string{"esc*2"}})
	if !out.Result.OK {
		t.Fatalf("refused: %s", ansaRefusalLine(out.Result))
	}
	if got := f.writes["pane-aaaa"]; len(got) != 2 || !bytes.Equal(got[0], []byte{0x1b}) || !bytes.Equal(got[1], []byte{0x1b}) {
		t.Fatalf("writes to pane-aaaa = %q, want two ESC", got)
	}
	if f.wrote() != 2 {
		t.Fatalf("%d writes in total, want 2 (another pane got keys)", f.wrote())
	}
	if out.Keys != "esc*2" || out.Program != "claude" || !out.Probed {
		t.Errorf("outcome = %+v", out)
	}
	es := a.all()
	if len(es) != 1 {
		t.Fatalf("audit has %d entries, want 1: %+v", len(es), es)
	}
	e := es[0]
	if e.Status != "sent" || e.Target != "pane-aaaa" || e.From != "pane-from" || e.Keys != "esc*2" ||
		e.Door != "cli" || e.Ref != "@release-prep" || e.Program != "claude" || e.Force {
		t.Errorf("audit entry = %+v", e)
	}
}

func TestAnsaKeysByClaudeSession(t *testing.T) {
	f := newKeysFake(keysPanes())
	a := &auditLog{}
	out := runKeys(f, a, ansaKeysRequest{Door: "cli", ClaudeSession: "sid-2222", Keys: []string{"esc"}})
	if !out.Result.OK {
		t.Fatalf("refused: %s", ansaRefusalLine(out.Result))
	}
	if len(f.writes["pane-bbbb"]) != 1 || f.wrote() != 1 {
		t.Fatalf("writes = %v, want one ESC to pane-bbbb only", f.writes)
	}
	if es := a.all(); len(es) != 1 || es[0].Ref != "claude-session:sid-2222" {
		t.Errorf("audit = %+v", es)
	}
}

// Every refusal: the code, and NOTHING written to any pane.
func TestAnsaKeysRefusalsWriteNothing(t *testing.T) {
	cases := []struct {
		name  string
		req   ansaKeysRequest
		setup func(*keysFake)
		code  string
	}{
		{"no target", ansaKeysRequest{Keys: []string{"esc"}}, nil, msg.AnsaErrNoTarget},
		{"two targets", ansaKeysRequest{To: "pane-aaaa", ClaudeSession: "sid-1111", Keys: []string{"esc"}}, nil, msg.AnsaErrNoTarget},
		{"no keys", ansaKeysRequest{To: "pane-aaaa"}, nil, msg.AnsaErrBadKey},
		{"unknown key", ansaKeysRequest{To: "pane-aaaa", Keys: []string{"esc", "hello"}}, nil, msg.AnsaErrBadKey},
		{"printable", ansaKeysRequest{To: "pane-aaaa", Keys: []string{"1"}}, nil, msg.AnsaErrBadKey},
		{"unknown pane", ansaKeysRequest{To: "pane-zzzz", Keys: []string{"esc"}}, nil, msg.AnsaErrUnknownTarget},
		{"ambiguous name", ansaKeysRequest{To: "@wkr", Keys: []string{"esc"}}, nil, msg.AnsaErrAmbiguousTarget},
		{"wildcard", ansaKeysRequest{To: "*", Keys: []string{"esc"}}, nil, msg.AnsaErrUnknownTarget},
		{"unknown session", ansaKeysRequest{ClaudeSession: "sid-none", Keys: []string{"esc"}}, nil, msg.AnsaErrUnknownTarget},
		{"ambiguous session", ansaKeysRequest{ClaudeSession: "sid-dup", Keys: []string{"esc"}}, nil, msg.AnsaErrAmbiguousTarget},
		{"guarded without force", ansaKeysRequest{To: "pane-aaaa", Keys: []string{"esc", "enter"}}, nil, msg.AnsaErrNeedsForce},
		{"ctrl+c without force", ansaKeysRequest{To: "pane-aaaa", Keys: []string{"ctrl+c"}}, nil, msg.AnsaErrNeedsForce},
		{"guarded, force, pane silent", ansaKeysRequest{To: "pane-aaaa", Keys: []string{"enter"}, Force: true},
			func(f *keysFake) { f.probeErr = errors.New("nats: timeout") }, msg.AnsaErrUnreachable},
		{"directory down", ansaKeysRequest{To: "pane-aaaa", Keys: []string{"esc"}},
			func(f *keysFake) { f.panesErr = errors.New("tab did not answer") }, msg.AnsaErrDirectory},
		{"publish fails", ansaKeysRequest{To: "pane-aaaa", Keys: []string{"esc"}},
			func(f *keysFake) { f.keysErr = errors.New("nats: closed") }, msg.AnsaErrPublishFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newKeysFake(keysPanes())
			f.program = "claude"
			if tc.setup != nil {
				tc.setup(f)
			}
			a := &auditLog{}
			out := runKeys(f, a, tc.req)
			if out.Result.OK {
				t.Fatalf("sent, want refusal %s", tc.code)
			}
			if out.Result.Code != tc.code {
				t.Errorf("code = %s (%s), want %s", out.Result.Code, out.Result.Error, tc.code)
			}
			if n := f.wrote(); n != 0 {
				t.Errorf("%d key writes reached a pane on a refusal", n)
			}
			// The refusal itself is audited, and never as "sent" last.
			es := a.all()
			if len(es) == 0 || es[len(es)-1].Status != "refused" || es[len(es)-1].Code != tc.code {
				t.Errorf("audit = %+v, want a final refused/%s line", es, tc.code)
			}
		})
	}
}

func TestAnsaKeysAmbiguousListsCandidates(t *testing.T) {
	f := newKeysFake(keysPanes())
	out := runKeys(f, &auditLog{}, ansaKeysRequest{ClaudeSession: "sid-dup", Keys: []string{"esc"}})
	if strings.Join(out.Result.Candidates, ",") != "pane-dddd,pane-eeee" {
		t.Errorf("candidates = %v", out.Result.Candidates)
	}
}

func TestAnsaKeysGuardedWithForce(t *testing.T) {
	f := newKeysFake(keysPanes())
	f.program = "claude"
	a := &auditLog{}
	out := runKeys(f, a, ansaKeysRequest{To: "pane-aaaa", Keys: []string{"ctrl+u", "enter"}, Force: true})
	if !out.Result.OK {
		t.Fatalf("refused: %s", ansaRefusalLine(out.Result))
	}
	got := f.writes["pane-aaaa"]
	if len(got) != 2 || !bytes.Equal(got[0], []byte{0x15}) || !bytes.Equal(got[1], []byte{'\r'}) {
		t.Fatalf("writes = %q", got)
	}
	if es := a.all(); len(es) != 1 || !es[0].Force || es[0].Keys != "ctrl+u enter" {
		t.Errorf("audit = %+v", es)
	}
}

// A hung pane may not answer the probe; safe keys still go, because Esc is the
// thing that unsticks it (same reasoning as ansaRouter.Interrupt).
func TestAnsaKeysSafeKeyToSilentPane(t *testing.T) {
	f := newKeysFake(keysPanes())
	f.probeErr = errors.New("nats: timeout")
	out := runKeys(f, &auditLog{}, ansaKeysRequest{To: "pane-aaaa", Keys: []string{"esc"}})
	if !out.Result.OK {
		t.Fatalf("refused: %s", ansaRefusalLine(out.Result))
	}
	if out.Probed || f.wrote() != 1 {
		t.Errorf("probed=%v writes=%d", out.Probed, f.wrote())
	}
}

// No audit line, no keys.
func TestAnsaKeysAuditFailureSendsNothing(t *testing.T) {
	f := newKeysFake(keysPanes())
	a := &auditLog{failWith: errors.New("disk full")}
	out := runKeys(f, a, ansaKeysRequest{To: "pane-aaaa", Keys: []string{"esc"}})
	if out.Result.OK || out.Result.Code != msg.AnsaErrAudit {
		t.Fatalf("result = %+v, want %s", out.Result, msg.AnsaErrAudit)
	}
	if f.wrote() != 0 {
		t.Fatal("keys were sent with no audit line")
	}
}

// An unknown "key" is caller-typed text: it never reaches the audit line.
func TestAnsaKeysAuditNeverRecordsUnknownText(t *testing.T) {
	a := &auditLog{}
	runKeys(newKeysFake(keysPanes()), a, ansaKeysRequest{To: "pane-aaaa", Keys: []string{"secret-ish-words"}})
	for _, e := range a.all() {
		b, _ := json.Marshal(e)
		if strings.Contains(string(b), "secret-ish-words") {
			t.Fatalf("audit line carries the caller's text: %s", b)
		}
	}
}

// The router itself only takes ids: a name there is a caller bug.
func TestAnsaKeysRouterRefusesAName(t *testing.T) {
	f := newKeysFake(keysPanes())
	r := &ansaRouter{tr: f}
	out := r.Keys(ansaKeysAuditEntry{}, "release-prep", []string{"esc"}, false, (&auditLog{}).append)
	if out.Result.OK || out.Result.Code != msg.AnsaErrNotAnID {
		t.Fatalf("result = %+v, want %s", out.Result, msg.AnsaErrNotAnID)
	}
}

func TestAnsaKeysAuditFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sess", ansaKeysAuditFile)
	e := ansaKeysAuditEntry{Time: time.Unix(0, 0).UTC(), Door: "##", Target: "pane-aaaa", Keys: "esc", Status: "sent"}
	if err := appendAnsaKeysAudit(path, e); err != nil {
		t.Fatal(err)
	}
	if err := appendAnsaKeysAudit(path, e); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2:\n%s", len(lines), b)
	}
	var back ansaKeysAuditEntry
	if err := json.Unmarshal([]byte(lines[0]), &back); err != nil || back.Keys != "esc" || back.Target != "pane-aaaa" {
		t.Errorf("line 0 = %s (%v)", lines[0], err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 (%v)", st.Mode().Perm(), err)
	}
	if err := appendAnsaKeysAudit("", e); err == nil {
		t.Error("an empty audit path must fail, so the send is refused")
	}
}

func TestParseAnsaKeysArgs(t *testing.T) {
	cases := []struct {
		args    []string
		want    ansaKeysRequest
		wantErr string
	}{
		{args: []string{"@wkr", "esc"}, want: ansaKeysRequest{To: "@wkr", Keys: []string{"esc"}}},
		{args: []string{"pane-aaaa", "--force", "ctrl+u", "enter"}, want: ansaKeysRequest{To: "pane-aaaa", Force: true, Keys: []string{"ctrl+u", "enter"}}},
		{args: []string{"--claude-session", "sid-1", "esc*2"}, want: ansaKeysRequest{ClaudeSession: "sid-1", Keys: []string{"esc*2"}}},
		{args: []string{"--claude-session=sid-1", "esc"}, want: ansaKeysRequest{ClaudeSession: "sid-1", Keys: []string{"esc"}}},
		{args: []string{"@wkr", "--", "esc"}, want: ansaKeysRequest{To: "@wkr", Keys: []string{"esc"}}},
		{args: nil, wantErr: "needs a target"},
		{args: []string{"--claude-session"}, wantErr: "needs a session id"},
		{args: []string{"--fleet", "x", "esc"}, wantErr: "unknown flag"},
		{args: []string{"@wkr"}, wantErr: "needs at least one key"},
	}
	for _, tc := range cases {
		got, err := parseAnsaKeysArgs(tc.args)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("parseAnsaKeysArgs(%q) err = %v, want %q", tc.args, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAnsaKeysArgs(%q): %v", tc.args, err)
			continue
		}
		if got.To != tc.want.To || got.ClaudeSession != tc.want.ClaudeSession || got.Force != tc.want.Force ||
			strings.Join(got.Keys, " ") != strings.Join(tc.want.Keys, " ") {
			t.Errorf("parseAnsaKeysArgs(%q) = %+v, want %+v", tc.args, got, tc.want)
		}
	}
}

// On the wire: one MsgRawKeyInput per key press, in order, on the target's
// rawinput subject, and nothing on any other pane's.
func TestAnsaKeysOnTheWire(t *testing.T) {
	nc := newABLATestNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	target := watchRawInput(t, nc, codecs, "pane-aaaa")
	other := watchRawInput(t, nc, codecs, "pane-bbbb")

	tr := &natsAnsaTransport{pub: pub}
	if err := tr.SendKeys("pane-aaaa", [][]byte{{0x1b}, []byte("\x1b[B"), {0x15}}); err != nil {
		t.Fatal(err)
	}
	for i, want := range [][]byte{{0x1b}, []byte("\x1b[B"), {0x15}} {
		got := target.next(t, "pane-aaaa")
		if !bytes.Equal(got.Data, want) || got.PaneID != "pane-aaaa" {
			t.Errorf("press %d = %q (pane %s), want %q", i, got.Data, got.PaneID, want)
		}
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	other.requireSilent(t, "pane-bbbb")
}

// TestAnsaUsageDocumentsKeysAudit — LEAD note N2: after an unexpected approval
// the audit file is where to look, so the help names it.
func TestAnsaUsageDocumentsKeysAudit(t *testing.T) {
	var out strings.Builder
	w := &WorkspaceActor{}
	_ = w.ansaUsage(&out, "test")
	s := out.String()
	for _, want := range []string{"##ansa keys", "--claude-session", ansaKeysAuditFile} {
		if !strings.Contains(s, want) {
			t.Errorf("##ansa usage does not document %q:\n%s", want, s)
		}
	}
}
