// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/memory"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

func memoryWorkspace(t *testing.T) *WorkspaceActor {
	t.Helper()
	t.Chdir(t.TempDir()) // no project-local .rysh/secrets leaks in
	return &WorkspaceActor{
		cfg:     config.Config{RyshDir: filepath.Join(t.TempDir(), ".rysh")},
		policy:  policy.Empty(),
		secrets: newSecretStore(nil, nil),
	}
}

func memCmd(w *WorkspaceActor, args ...string) string {
	var out strings.Builder
	w.handleMemoryCommand(&out, "", args)
	return out.String()
}

func TestMemoryCommandAddListShowForget(t *testing.T) {
	w := memoryWorkspace(t)
	w.inputTyped = true // as handleSubmitInput sets it for typed input
	out := memCmd(w, "add", "--kind", "preference", "keep", "answers", "short")
	if !strings.Contains(out, "remembered") {
		t.Fatalf("add output: %q", out)
	}
	list, _ := memory.Open(w.cfg.RyshDir).List(nil)
	if len(list) != 1 || list[0].Kind != memory.KindPreference || list[0].Text != "keep answers short" || list[0].Source != memory.SourceUser {
		t.Fatalf("stored = %+v", list)
	}
	id := list[0].ID

	if out := memCmd(w, "list"); !strings.Contains(out, id) || !strings.Contains(out, "keep answers short") {
		t.Fatalf("list output: %q", out)
	}
	if out := memCmd(w); !strings.Contains(out, id) {
		t.Fatalf("bare ##memory must list: %q", out)
	}
	if out := memCmd(w, "show", id); !strings.Contains(out, "keep answers short") || !strings.Contains(out, "preference") {
		t.Fatalf("show output: %q", out)
	}
	if out := memCmd(w, "path"); !strings.Contains(out, filepath.Join(".rysh", "memory", "entries")) {
		t.Fatalf("path output: %q", out)
	}
	if out := memCmd(w, "forget", id); !strings.Contains(out, "forgot") {
		t.Fatalf("forget output: %q", out)
	}
	if list, _ := memory.Open(w.cfg.RyshDir).List(nil); len(list) != 0 {
		t.Fatalf("forget left %d entries", len(list))
	}
	if out := memCmd(w, "forget", id); !strings.Contains(out, "no such memory") {
		t.Fatalf("second forget: %q", out)
	}
}

func TestMemoryCommandForgetAll(t *testing.T) {
	w := memoryWorkspace(t)
	memCmd(w, "add", "one")
	memCmd(w, "add", "two")
	if out := memCmd(w, "forget", "--all"); !strings.Contains(out, "2") {
		t.Fatalf("forget --all output: %q", out)
	}
	if list, _ := memory.Open(w.cfg.RyshDir).List(nil); len(list) != 0 {
		t.Fatalf("forget --all left %d entries", len(list))
	}
}

func TestMemoryCommandRefusesSecretsWithoutEchoingThem(t *testing.T) {
	w := memoryWorkspace(t)
	value := "Vx" + strings.Repeat("q7R", 5)
	w.secrets = newSecretStore(nil, map[string]map[string]string{"default": {"DEPLOY_KEY": value}})
	out := memCmd(w, "add", "the deploy key is "+value)
	if !strings.Contains(out, memory.RuleKnownSecret) || strings.Contains(out, value) {
		t.Fatalf("known secret: refusal must name the rule and not echo the value")
	}
}

func TestMemoryCommandRefusesCredentialShapes(t *testing.T) {
	w := memoryWorkspace(t)
	out := memCmd(w, "add", "pass"+"word="+"hunter2hunter2")
	if !strings.Contains(out, "refused") || strings.Contains(out, "hunter2") {
		t.Fatalf("credential-shaped add: %q", out)
	}
	if list, _ := memory.Open(w.cfg.RyshDir).List(nil); len(list) != 0 {
		t.Fatalf("refused add stored %d entries", len(list))
	}

	// A hand-edited credential entry is listed withheld and not shown.
	s := memory.Open(w.cfg.RyshDir)
	if _, err := s.Add(memory.KindNote, "placeholder", memory.SourceUser, nil); err != nil {
		t.Fatal(err)
	}
	bad := "---\nkind: fact\n---\ntoken" + ": " + "xo" + "xb-" + strings.Repeat("a1B2", 6) + "\n"
	if err := os.WriteFile(filepath.Join(s.Dir(), "pasted.md"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := memCmd(w, "list"); !strings.Contains(out, "withheld") || strings.Contains(out, "a1B2") {
		t.Fatalf("list of a withheld entry: %q", out)
	}
	if out := memCmd(w, "show", "pasted"); !strings.Contains(out, "withheld") || strings.Contains(out, "a1B2") {
		t.Fatalf("show of a withheld entry: %q", out)
	}
	if out := memCmd(w, "forget", "pasted"); !strings.Contains(out, "forgot") {
		t.Fatalf("a withheld entry must still be forgettable: %q", out)
	}
}

func TestMemoryCommandFailsClosedOnPolicy(t *testing.T) {
	w := memoryWorkspace(t)
	w.policy = &policy.Policy{Path: ".rysh/policy.yaml", Err: errors.New("parse")}
	out := memCmd(w, "add", "something harmless")
	if !strings.Contains(out, "refused") || !strings.Contains(out, "policy") {
		t.Fatalf("add under a broken policy: %q", out)
	}
	if _, err := os.Stat(memory.Open(w.cfg.RyshDir).Dir()); err == nil {
		t.Fatalf("a refused add created the store")
	}
	if out := memCmd(w, "list"); !strings.Contains(out, "policy") {
		t.Fatalf("list must say memory is off and why: %q", out)
	}

	no := false
	w.policy = policy.Empty()
	w.policy.Memory.Enabled = &no
	if out := memCmd(w, "add", "x y"); !strings.Contains(out, "refused") {
		t.Fatalf("add with memory.enabled false: %q", out)
	}
}

func TestMemoryCommandUsage(t *testing.T) {
	w := memoryWorkspace(t)
	for _, args := range [][]string{{"bogus"}, {"show"}, {"forget"}, {"add"}, {"add", "--kind", "secret", "x"}, {"show", "../policy"}} {
		if out := memCmd(w, args...); !strings.Contains(strings.ToLower(out), "usage") && !strings.Contains(out, "invalid") && !strings.Contains(out, "unknown kind") {
			t.Errorf("%v: no usage/validation message: %q", args, out)
		}
	}
	if _, ok := lookupRyshCommand("memory"); !ok {
		t.Fatalf("##memory is not registered")
	}
}

func TestSyncMemoryPolicyWiresTheScrubberLookup(t *testing.T) {
	w := memoryWorkspace(t)
	value := "Wy" + strings.Repeat("k4P", 5)
	w.secrets = newSecretStore(nil, map[string]map[string]string{"default": {"SOME_TOKEN": value}})
	w.agSetup = &agentic.Setup{}
	w.syncMemoryPolicy() // no panes, no publisher: must not panic
	if w.agSetup.KnownSecrets == nil {
		t.Fatalf("KnownSecrets not installed")
	}
	found := false
	for _, v := range w.agSetup.KnownSecrets() {
		if v == value {
			found = true
		}
	}
	if !found {
		t.Fatalf("the workspace's registered value is not visible to the scrubber")
	}
}

// An entry added through input nobody typed (an agent's rysh_command, a
// script, `rysh exec`) is recorded as source: agent; only input a person typed
// into a pane is source: user (e20 round 3, item 2).
func TestMemoryAddRecordsWhoIssuedIt(t *testing.T) {
	nc := startInProcessNATS(t)
	w := memoryWorkspace(t)
	w.pub = msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	w.activePaneID = "p1"
	w.tabs = []*tabInfo{{id: "tab-1", title: "tab"}}

	w.handleSubmitInput(nil, &msg.MsgSubmitInput{Text: "##memory add typed by a person", Mode: "shell", PaneID: "p1"})
	w.handleSubmitInput(nil, &msg.MsgSubmitInput{Text: "##memory add sent by an agent", Mode: "shell", PaneID: "p1", Programmatic: true})
	memCmd(w, "add", "no", "input", "context") // e.g. the CLI path: not typed

	list, err := memory.Open(w.cfg.RyshDir).List(nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range list {
		got[e.Text] = e.Source
	}
	want := map[string]string{
		"typed by a person": memory.SourceUser,
		"sent by an agent":  memory.SourceAgent,
		"no input context":  memory.SourceAgent,
	}
	if len(got) != len(want) {
		t.Fatalf("stored %d entries, want %d: %v", len(got), len(want), got)
	}
	for text, src := range want {
		if got[text] != src {
			t.Errorf("%q: source = %q, want %q", text, got[text], src)
		}
	}
}

// The scrubber's registered-secret lookup is wired when the workspace is
// built, before any pane exists, so the first prompt a pane gets is already
// scrubbed against the registered values (e20 round 3, minor). That a wired
// lookup withholds an entry is agentic's TestWithheldEntryNeverReachesThePrompt.
func TestNewWorkspaceActorWiresTheScrubberBeforeAnyPrompt(t *testing.T) {
	t.Chdir(t.TempDir())
	value := "Zt" + strings.Repeat("m5Q", 5)
	cfg := config.Config{
		RyshDir: filepath.Join(t.TempDir(), ".rysh"),
		Secrets: map[string]map[string]string{"default": {"SOME_TOKEN": value}},
	}
	setup := &agentic.Setup{}
	NewWorkspaceActor(cfg, nil, setup, nil, nil, nil, nil, nil, nil, "", "", 0, nil, true, false)
	if setup.KnownSecrets == nil {
		t.Fatalf("KnownSecrets is not wired when the workspace is built")
	}
	found := false
	for _, v := range setup.KnownSecrets() {
		found = found || v == value
	}
	if !found {
		t.Fatalf("the registered value is not visible to the scrubber")
	}
}
