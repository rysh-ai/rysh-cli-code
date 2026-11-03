// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Design 093 H1: `##pane profile` and the pane's forwarding of its profile to
// its LLM actor.

func newProfileTestWorkspace(t *testing.T) (*WorkspaceActor, *nats.Conn, string) {
	t.Helper()
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	base := t.TempDir()
	stored := filepath.Join(base, ".rysh", domain.PaneProfileDirName, "pP2.md")
	if err := os.MkdirAll(filepath.Dir(stored), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stored, []byte("You are Bo.\nBe brief."), 0o600); err != nil {
		t.Fatal(err)
	}
	serveBoardTab(t, nc, codecs, "tab-A",
		boardTestPane{id: "pOrch", title: "orch", givenName: "ceo"},
		boardTestPane{id: "pP1", title: "p1", givenName: "ada"},
		boardTestPane{id: "pP2", title: "p2", givenName: "bo", meta: map[string]string{
			domain.PaneMetaAgentTools:      "family_ask,family_reply",
			domain.PaneMetaAgentPromptFile: stored,
		}})
	w := &WorkspaceActor{
		pub:          msg.NewNATSPublisher(nc, codecs),
		tabs:         []*tabInfo{{id: "tab-A", title: "A"}},
		activeTabIdx: 0,
		activePaneID: "pOrch",
		cfg:          config.Config{WorkingDirectory: base},
	}
	return w, nc, base
}

// profileWrites runs one ##pane command and returns the profile meta writes
// it published, in order, as "pane:key=value".
func profileWrites(t *testing.T, w *WorkspaceActor, nc *nats.Conn, args ...string) ([]string, string) {
	t.Helper()
	rec := recordSubject(t, nc, msg.T("pane", "*", "inbox"))
	var out strings.Builder
	w.handlePaneCommand(nil, &out, "pOrch", args)
	settle(t, nc)
	subjects, payloads := rec.seen()
	var writes []string
	for i, subj := range subjects {
		var m msg.MsgPaneSetMeta
		if json.Unmarshal(payloads[i], &m) != nil || !domain.IsPaneAgentProfileKey(m.Key) {
			continue
		}
		writes = append(writes, paneIDFromInbox(subj)+":"+m.Key+"="+m.Value)
	}
	return writes, out.String()
}

func TestPaneProfileSet_CopiesPromptAndWritesMeta(t *testing.T) {
	w, nc, base := newProfileTestWorkspace(t)
	if err := os.WriteFile(filepath.Join(base, "ada.md"), []byte("You are Grandma Ada."), 0o644); err != nil {
		t.Fatal(err)
	}
	writes, out := profileWrites(t, w, nc, "profile", "set", "ada", "--tools", "family_ask, family_reply,family_status", "--prompt-file", "ada.md")
	stored := filepath.Join(base, ".rysh", domain.PaneProfileDirName, "pP1.md")
	want := []string{
		"pP1:" + domain.PaneMetaAgentPromptFile + "=" + stored, // prompt FIRST
		"pP1:" + domain.PaneMetaAgentTools + "=family_ask,family_reply,family_status",
	}
	if !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes = %v\nwant %v\nout: %s", writes, want, out)
	}
	b, err := os.ReadFile(stored)
	if err != nil || string(b) != "You are Grandma Ada." {
		t.Fatalf("stored prompt = %q, %v", b, err)
	}
	if fi, _ := os.Stat(stored); fi.Mode().Perm() != 0o600 {
		t.Errorf("stored prompt mode = %v, want 0600", fi.Mode().Perm())
	}
	if !strings.Contains(out, "agent profile set") || !strings.Contains(out, "family_ask, family_reply, family_status") {
		t.Errorf("out = %q", out)
	}
	if strings.Contains(out, base) {
		t.Errorf("output names an absolute path: %q", out)
	}

	// set with tools only REPLACES: the prompt meta is cleared and its copy removed.
	writes, _ = profileWrites(t, w, nc, "profile", "set", "pP1", "--tools=family_status")
	want = []string{"pP1:" + domain.PaneMetaAgentPromptFile + "=", "pP1:" + domain.PaneMetaAgentTools + "=family_status"}
	if !reflect.DeepEqual(writes, want) {
		t.Fatalf("tools-only set writes = %v, want %v", writes, want)
	}
	if _, err := os.Stat(stored); !os.IsNotExist(err) {
		t.Errorf("stale prompt copy kept: %v", err)
	}
}

func TestPaneProfileClear(t *testing.T) {
	w, nc, base := newProfileTestWorkspace(t)
	writes, out := profileWrites(t, w, nc, "profile", "clear", "bo")
	want := []string{"pP2:" + domain.PaneMetaAgentTools + "=", "pP2:" + domain.PaneMetaAgentPromptFile + "="}
	if !reflect.DeepEqual(writes, want) {
		t.Fatalf("clear writes = %v, want %v", writes, want)
	}
	if _, err := os.Stat(filepath.Join(base, ".rysh", domain.PaneProfileDirName, "pP2.md")); !os.IsNotExist(err) {
		t.Errorf("prompt copy not removed: %v", err)
	}
	if !strings.Contains(out, "agent profile cleared") {
		t.Errorf("out = %q", out)
	}
}

func TestPaneProfileShow(t *testing.T) {
	w, nc, _ := newProfileTestWorkspace(t)
	_, out := profileWrites(t, w, nc, "profile", "show", "bo")
	for _, want := range []string{"tools  : family_ask, family_reply", "prompt : 21 bytes", "first  : You are Bo."} {
		if !strings.Contains(out, want) {
			t.Errorf("show missing %q:\n%s", want, out)
		}
	}
	_, out = profileWrites(t, w, nc, "profile", "show", "ada")
	if !strings.Contains(out, "(none — the pane uses the session defaults)") {
		t.Errorf("show for an unprofiled pane:\n%s", out)
	}
}

func TestPaneProfileSet_Refusals(t *testing.T) {
	w, nc, _ := newProfileTestWorkspace(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"profile"}, "##pane profile set <pane>"},
		{[]string{"profile", "set", "nobody", "--tools", "x"}, "pane not found: nobody"},
		{[]string{"profile", "set", "ada"}, "needs --tools and/or --prompt-file"},
		{[]string{"profile", "set", "ada", "--tools", "bash;rm"}, `invalid tool name "bash;rm"`},
		{[]string{"profile", "set", "ada", "--prompt-file", "missing.md"}, "--prompt-file missing.md"},
		{[]string{"profile", "set", "ada", "--tools", "x", "stray"}, `unexpected argument "stray"`},
		{[]string{"profile", "wibble", "ada"}, "##pane profile set"},
	}
	for _, c := range cases {
		writes, out := profileWrites(t, w, nc, c.args...)
		if !strings.Contains(out, c.want) {
			t.Errorf("%v: output %q lacks %q", c.args, out, c.want)
		}
		if len(writes) != 0 {
			t.Errorf("%v: refused command still wrote %v", c.args, writes)
		}
	}
}

// The profile keys are reserved: `##pane meta set` cannot point a pane's system
// prompt at an arbitrary file or widen its tools behind ##pane profile's back.
func TestPaneMetaRefusesProfileKeys(t *testing.T) {
	for _, k := range []string{domain.PaneMetaAgentTools, domain.PaneMetaAgentPromptFile} {
		if why := paneMetaReserved(k); !strings.Contains(why, "##pane profile") {
			t.Errorf("%s not reserved: %q", k, why)
		}
	}
}

func TestPaneAgentProfileFromMeta(t *testing.T) {
	base := t.TempDir()
	dir := paneProfileDir(nil, base)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := paneProfilePromptPath(dir, "pane-1")
	if want != filepath.Join(base, ".rysh", domain.PaneProfileDirName, "pane-1.md") {
		t.Fatalf("expected path = %s", want)
	}
	_ = os.WriteFile(want, []byte("persona"), 0o600)

	m, err := paneAgentProfileFromMeta(map[string]string{
		domain.PaneMetaAgentTools: " family_ask ,family_ask,, family_status", domain.PaneMetaAgentPromptFile: want}, want)
	if err != nil || m.SystemPrompt != "persona" || !reflect.DeepEqual(m.AllowTools, []string{"family_ask", "family_status"}) {
		t.Fatalf("got %+v, %v", m, err)
	}
	if m, _ := paneAgentProfileFromMeta(nil, want); len(m.AllowTools) != 0 || m.SystemPrompt != "" {
		t.Fatalf("empty meta must give an empty (clearing) profile: %+v", m)
	}
}

// e20 W1: only the exact <workspace>/.rysh/pane-profiles/<pane-id>.md is read.
// Another pane's file, a file in some other pane-profiles directory, any other
// path, and a symlink (to a file or as the directory) are all refused — and the
// allow-list still applies (a broken prompt never widens the tool set).
func TestReadProfilePrompt_ExactPathNoSymlinks(t *testing.T) {
	base := t.TempDir()
	dir := paneProfileDir(nil, base)
	_ = os.MkdirAll(dir, 0o700)
	want := paneProfilePromptPath(dir, "pane-1")
	_ = os.WriteFile(want, []byte("persona"), 0o600)
	if got, err := readProfilePrompt(want, want); err != nil || got != "persona" {
		t.Fatalf("exact path refused: %q %v", got, err)
	}

	secret := filepath.Join(t.TempDir(), "secret.txt")
	_ = os.WriteFile(secret, []byte("DO-NOT-READ"), 0o600)
	otherDir := filepath.Join(t.TempDir(), ".rysh", domain.PaneProfileDirName)
	_ = os.MkdirAll(otherDir, 0o700)
	elsewhere := filepath.Join(otherDir, "pane-1.md") // right name, wrong directory
	_ = os.WriteFile(elsewhere, []byte("DO-NOT-READ"), 0o600)
	otherPane := paneProfilePromptPath(dir, "pane-2")
	_ = os.WriteFile(otherPane, []byte("DO-NOT-READ"), 0o600)

	for name, path := range map[string]string{
		"another file":             secret,
		"wrong pane-profiles dir":  elsewhere,
		"another pane's file":      otherPane,
		"dot-dot back to the file": filepath.Join(dir, "..", domain.PaneProfileDirName, "x", "..", "pane-2.md"),
	} {
		got, err := readProfilePrompt(path, want)
		if err == nil || strings.Contains(got, "DO-NOT-READ") {
			t.Errorf("%s: read (%q, %v)", name, got, err)
		}
	}

	// A symlink at the exact path is refused, even though it names the right file.
	if err := os.Remove(want); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, want); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	got, err := readProfilePrompt(want, want)
	if err == nil || strings.Contains(got, "DO-NOT-READ") || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlinked prompt file read: %q %v", got, err)
	}
	m, err := paneAgentProfileFromMeta(map[string]string{
		domain.PaneMetaAgentTools: "family_ask", domain.PaneMetaAgentPromptFile: want}, want)
	if err == nil || m.SystemPrompt != "" || !reflect.DeepEqual(m.AllowTools, []string{"family_ask"}) {
		t.Errorf("symlink via meta: %+v %v", m, err)
	}

	// A symlinked pane-profiles directory is refused too.
	base2 := t.TempDir()
	real := filepath.Join(t.TempDir(), "real")
	_ = os.MkdirAll(real, 0o700)
	_ = os.WriteFile(filepath.Join(real, "pane-1.md"), []byte("DO-NOT-READ"), 0o600)
	_ = os.MkdirAll(filepath.Join(base2, ".rysh"), 0o700)
	if err := os.Symlink(real, filepath.Join(base2, ".rysh", domain.PaneProfileDirName)); err != nil {
		t.Fatal(err)
	}
	want2 := paneProfilePromptPath(paneProfileDir(nil, base2), "pane-1")
	if got, err := readProfilePrompt(want2, want2); err == nil || strings.Contains(got, "DO-NOT-READ") {
		t.Errorf("symlinked directory read: %q %v", got, err)
	}
}

// profileProbe stands in for the pane's LLM actor and records the profiles it
// is sent.
type profileProbe struct {
	mu  sync.Mutex
	got []*msg.MsgSetPaneAgentProfile
}

func (p *profileProbe) Receive(ctx actor.Context) {
	if m, ok := ctx.Message().(*msg.MsgSetPaneAgentProfile); ok {
		p.mu.Lock()
		p.got = append(p.got, m)
		p.mu.Unlock()
	}
}

func (p *profileProbe) wait(t *testing.T, n int) []*msg.MsgSetPaneAgentProfile {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		p.mu.Lock()
		if len(p.got) >= n {
			out := append([]*msg.MsgSetPaneAgentProfile(nil), p.got...)
			p.mu.Unlock()
			return out
		}
		p.mu.Unlock()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	t.Fatalf("probe got %d profile message(s), want %d", len(p.got), n)
	return nil
}

// paneHost runs a PaneActor's real handlers inside an actor context: on
// Started it does what PaneActor.Started does once the LLM actor is spawned
// (reapplyAgentProfileOnSpawn); MsgPaneSetMeta goes through PaneActor.Receive.
type paneHost struct {
	p     *PaneActor
	probe *actor.PID
}

func (h *paneHost) Receive(ctx actor.Context) {
	switch ctx.Message().(type) {
	case *actor.Started:
		h.p.llmPromptExecPID = h.probe
		h.p.reapplyAgentProfileOnSpawn(ctx)
	case *msg.MsgPaneSetMeta:
		h.p.Receive(ctx)
	}
}

// TestPaneProfileSurvivesRestart: a profile set on a pane persists with its
// meta; a pane restored from KV (a daemon restart) re-sends it to its new LLM
// actor before anything else, and a clear sends the empty (default) profile.
func TestPaneProfileSurvivesRestart(t *testing.T) {
	nc, _ := startJetStreamNATS(t, t.TempDir())
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: "rysh-panes-h1"})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir := paneProfileDir(nil, root)
	_ = os.MkdirAll(dir, 0o700)
	prompt := paneProfilePromptPath(dir, "pane-1")
	_ = os.WriteFile(prompt, []byte("You are Ada."), 0o600)
	paneCfg := config.Config{WorkingDirectory: root}

	sys := actor.NewActorSystem()

	// Before the restart: meta changes reach the LLM actor as they happen.
	probe1 := &profileProbe{}
	probe1PID := sys.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return probe1 }))
	before := &PaneActor{id: "pane-1", nc: nc, kvStore: kv, cfg: paneCfg}
	host1 := sys.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return &paneHost{p: before, probe: probe1PID} }))
	sys.Root.Send(host1, &msg.MsgPaneSetMeta{Key: domain.PaneMetaAgentPromptFile, Value: prompt})
	sys.Root.Send(host1, &msg.MsgPaneSetMeta{Key: domain.PaneMetaAgentTools, Value: "family_reply,family_status"})
	sys.Root.Send(host1, &msg.MsgPaneSetMeta{Key: "unrelated", Value: "x"}) // not a profile key: no send
	got := probe1.wait(t, 2)
	if last := got[len(got)-1]; last.SystemPrompt != "You are Ada." || !reflect.DeepEqual(last.AllowTools, []string{"family_reply", "family_status"}) {
		t.Fatalf("live profile = %+v", last)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(probe1.wait(t, 2)); n != 2 {
		t.Errorf("a non-profile meta key re-sent the profile (%d sends)", n)
	}
	sys.Root.Stop(host1)

	// The restart: a fresh PaneActor restored from the durable meta record.
	after := &PaneActor{id: "pane-1", nc: nc, kvStore: kv, cfg: paneCfg}
	after.restoreDurableMeta()
	probe2 := &profileProbe{}
	probe2PID := sys.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return probe2 }))
	host2 := sys.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return &paneHost{p: after, probe: probe2PID} }))
	re := probe2.wait(t, 1)[0]
	if re.SystemPrompt != "You are Ada." || !reflect.DeepEqual(re.AllowTools, []string{"family_reply", "family_status"}) {
		t.Fatalf("profile after restart = %+v", re)
	}

	// clear → the empty profile (session defaults) is sent.
	sys.Root.Send(host2, &msg.MsgPaneSetMeta{Key: domain.PaneMetaAgentTools})
	sys.Root.Send(host2, &msg.MsgPaneSetMeta{Key: domain.PaneMetaAgentPromptFile})
	got = probe2.wait(t, 3)
	if last := got[2]; len(last.AllowTools) != 0 || last.SystemPrompt != "" {
		t.Fatalf("after clear = %+v", last)
	}

	// A pane with no profile sends nothing when its LLM actor is spawned.
	probe3 := &profileProbe{}
	probe3PID := sys.Root.Spawn(actor.PropsFromProducer(func() actor.Actor { return probe3 }))
	sys.Root.Spawn(actor.PropsFromProducer(func() actor.Actor {
		return &paneHost{p: &PaneActor{id: "pane-plain", nc: nc}, probe: probe3PID}
	}))
	time.Sleep(150 * time.Millisecond)
	probe3.mu.Lock()
	n := len(probe3.got)
	probe3.mu.Unlock()
	if n != 0 {
		t.Errorf("an unprofiled pane sent %d profile message(s) on spawn", n)
	}
}
