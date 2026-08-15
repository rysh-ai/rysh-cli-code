// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// TestAutoKindRouting covers the ##auto task|agent|humanoid subcommand split:
// the umbrella lists every kind, and each kind prints its own usage with its
// own recipe directory and (for targeted kinds) its target flag/frontmatter.
// (Only stateless paths — usage and routing — are exercised; no store or NATS
// is touched.)
func TestAutoKindRouting(t *testing.T) {
	w := &WorkspaceActor{}
	var out strings.Builder

	// ##auto (no args) → umbrella usage listing all four kinds.
	w.handleAutoCommand(&out, "p", nil)
	for _, want := range []string{"##auto web run", "##auto task", "##auto agent", "##auto humanoid", "##auto code"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("##auto usage should mention %q: %s", want, out.String())
		}
	}

	// ##auto task → task usage with the tasks recipe dir, no web flags.
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task"})
	s := out.String()
	if !strings.Contains(s, "##auto task run") || !strings.Contains(s, ".rysh/automations/tasks/") {
		t.Errorf("##auto task usage wrong: %s", s)
	}
	if strings.Contains(s, "--headless") || strings.Contains(s, "web_profile") {
		t.Errorf("##auto task usage must not mention web flags: %s", s)
	}
	if !strings.Contains(s, "automation.task.step") {
		t.Errorf("##auto task usage should name its config key: %s", s)
	}

	// ##auto agent → agent usage documenting --agent + `agent:` frontmatter.
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"agent"})
	s = out.String()
	if !strings.Contains(s, "--agent <name>") || !strings.Contains(s, "`agent: <name>`") ||
		!strings.Contains(s, ".rysh/automations/agents/") || !strings.Contains(s, "automation.agent.step") {
		t.Errorf("##auto agent usage wrong: %s", s)
	}

	// ##auto humanoid → same shape for humanoids.
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"humanoid"})
	s = out.String()
	if !strings.Contains(s, "--humanoid <name>") || !strings.Contains(s, "`humanoid: <name>`") ||
		!strings.Contains(s, ".rysh/automations/humanoids/") || !strings.Contains(s, "automation.humanoid.step") {
		t.Errorf("##auto humanoid usage wrong: %s", s)
	}

	// run with no name → one-line run usage.
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "run"})
	if !strings.Contains(out.String(), "usage: ##auto task run") {
		t.Errorf("##auto task run (no name) should print usage: %s", out.String())
	}
}

// TestAutoKindTargetErrors covers the targeted kinds' error paths: a recipe
// with no target, an unavailable registry, and a missing recipe — all
// stateless (no live registry / NATS).
func TestAutoKindTargetErrors(t *testing.T) {
	dir := t.TempDir()
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: dir}}
	var out strings.Builder

	// Agent recipe without an `agent:` key → friendly frontmatter error.
	st := webauto.NewStoreFor(dir, webauto.KindAgent)
	if err := st.Save(&webauto.Automation{Name: "review", Prompt: "Review."}); err != nil {
		t.Fatal(err)
	}
	w.handleAutoCommand(&out, "p", []string{"agent", "run", "review"})
	if !strings.Contains(out.String(), "names no target agent") || !strings.Contains(out.String(), "--agent <name>") {
		t.Errorf("target-less agent recipe should explain the fix: %s", out.String())
	}

	// Recipe naming a target, but no registry running → registry error, not a panic.
	if err := st.Save(&webauto.Automation{Name: "review2", Agent: "code-reviewer", Prompt: "Review."}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"agent", "run", "review2"})
	if !strings.Contains(out.String(), "agent registry not available") {
		t.Errorf("missing registry should be reported: %s", out.String())
	}

	// --agent flag overrides the recipe's (missing) target and reaches validation.
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"agent", "run", "--agent", "code-reviewer", "review"})
	if !strings.Contains(out.String(), "agent registry not available") {
		t.Errorf("--agent override should reach registry validation: %s", out.String())
	}

	// continue validates the target the same way run does.
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"agent", "continue", "review2"})
	if !strings.Contains(out.String(), "agent registry not available") {
		t.Errorf("continue should validate the target too: %s", out.String())
	}

	// Humanoid: same shape against the humanoid registry.
	sh := webauto.NewStoreFor(dir, webauto.KindHumanoid)
	if err := sh.Save(&webauto.Automation{Name: "greet", Humanoid: "concierge", Prompt: "Hi."}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"humanoid", "run", "greet"})
	if !strings.Contains(out.String(), "humanoid registry not available") {
		t.Errorf("missing humanoid registry should be reported: %s", out.String())
	}

	// Missing recipe → not-found with a pointer to list.
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "run", "nope"})
	if !strings.Contains(out.String(), `automation "nope" not found`) || !strings.Contains(out.String(), "##auto task list") {
		t.Errorf("missing task recipe should point at list: %s", out.String())
	}
}

// TestUnknownAutoTargetMsg covers the friendly unknown-target error body:
// available names are listed sorted; an empty registry points at spawn.
func TestUnknownAutoTargetMsg(t *testing.T) {
	got := unknownAutoTargetMsg("agent", "x", []string{"beta", "alpha"}, "##agent spawn <name>")
	if !strings.Contains(got, `agent "x" not found`) || !strings.Contains(got, "available agents: alpha, beta") {
		t.Errorf("unknown target with names: %s", got)
	}
	got = unknownAutoTargetMsg("humanoid", "x", nil, "##humanoid spawn <name>")
	if !strings.Contains(got, "no humanoids are loaded") || !strings.Contains(got, "##humanoid spawn <name>") {
		t.Errorf("unknown target with empty registry: %s", got)
	}
}

// TestAutoTaskSaveShow covers ##auto task save (which — unlike web's save —
// must NOT require a web-bound pane) and show (paths + the kind's config-level
// budget defaults resolved).
func TestAutoTaskSaveShow(t *testing.T) {
	dir := t.TempDir()
	w := &WorkspaceActor{cfg: config.Config{
		WorkingDirectory: dir,
		Automation: config.AutomationConfig{
			Task: config.AutomationKindConfig{Step: &webauto.StepConfig{Interval: 17}},
		},
	}}
	var out strings.Builder

	// save works with no pane snapshot at all (w.pub, actorSystem are nil).
	w.handleAutoCommand(&out, "p", []string{"task", "save", "hello", "Write", "about", "{{arg1}}"})
	if !strings.Contains(out.String(), `saved automation "hello"`) {
		t.Fatalf("task save failed: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".rysh", "automations", "tasks", "hello.md")); err != nil {
		t.Fatalf("task recipe not on disk: %v", err)
	}

	// show resolves the results dir under the tasks store and the task kind's
	// config-level step defaults (automation.task.step → 17 steps/leg).
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "show", "hello"})
	s := out.String()
	if !strings.Contains(s, filepath.Join(dir, ".rysh", "automations", "tasks", "hello", "results")) {
		t.Errorf("show should resolve the default results dir: %s", s)
	}
	if !strings.Contains(s, "17 steps/leg") {
		t.Errorf("show should resolve automation.task.step defaults: %s", s)
	}
	if strings.Contains(s, "profile") || strings.Contains(s, "url") {
		t.Errorf("task show must not print web fields: %s", s)
	}

	// list shows the saved recipe.
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "list"})
	if !strings.Contains(out.String(), "hello") {
		t.Errorf("task list should include the recipe: %s", out.String())
	}

	// delete removes it.
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "delete", "hello"})
	if !strings.Contains(out.String(), `deleted automation "hello"`) {
		t.Errorf("task delete failed: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".rysh", "automations", "tasks", "hello.md")); !os.IsNotExist(err) {
		t.Errorf("task recipe should be gone: %v", err)
	}
}

// TestAutoAgentSaveShow covers the targeted kinds' save/show hints: save
// points at setting the target, show prints the recipe's target (or the
// missing-target hint).
func TestAutoAgentSaveShow(t *testing.T) {
	dir := t.TempDir()
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: dir}}
	var out strings.Builder

	w.handleAutoCommand(&out, "p", []string{"agent", "save", "review", "Review", "the", "diff."})
	s := out.String()
	if !strings.Contains(s, `saved automation "review"`) || !strings.Contains(s, "`agent: <name>`") {
		t.Fatalf("agent save should hint at setting the target: %s", s)
	}

	// show on a target-less recipe prints the hint; with a target, the name.
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"agent", "show", "review"})
	if !strings.Contains(out.String(), "agent       : (none") {
		t.Errorf("agent show should flag the missing target: %s", out.String())
	}
	st := webauto.NewStoreFor(dir, webauto.KindAgent)
	if err := st.Save(&webauto.Automation{Name: "review", Agent: "code-reviewer", Prompt: "Review the diff."}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"agent", "show", "review"})
	if !strings.Contains(out.String(), "agent       : code-reviewer") {
		t.Errorf("agent show should print the target: %s", out.String())
	}

	// list shows the target in the bracket column.
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"agent", "list"})
	if !strings.Contains(out.String(), "[code-reviewer]") {
		t.Errorf("agent list should bracket the target: %s", out.String())
	}
}

// TestAutoKindConfigStepFeedsBudget proves each kind's automation.<kind>.step
// config key feeds ResolveRunBudgetWith through its spec (per-kind wiring),
// and that recipe frontmatter still wins over the kind config.
func TestAutoKindConfigStepFeedsBudget(t *testing.T) {
	w := &WorkspaceActor{cfg: config.Config{Automation: config.AutomationConfig{
		Web:      config.AutomationKindConfig{Step: &webauto.StepConfig{Interval: 21}},
		Task:     config.AutomationKindConfig{Step: &webauto.StepConfig{Interval: 22}},
		Agent:    config.AutomationKindConfig{Step: &webauto.StepConfig{Interval: 23}},
		Humanoid: config.AutomationKindConfig{Step: &webauto.StepConfig{Interval: 24}},
		Code:     config.AutomationKindConfig{Step: &webauto.StepConfig{Interval: 25}},
	}}}
	a := &webauto.Automation{Name: "x"}
	for _, c := range []struct {
		spec autoKindSpec
		want int
	}{
		{taskAutoSpec(), 22},
		{agentAutoSpec(), 23},
		{humanoidAutoSpec(), 24},
		{codeAutoSpec(), 25},
	} {
		if got := a.ResolveRunBudgetWith(c.spec.stepDef(w)).StepInterval; got != c.want {
			t.Errorf("%s: StepInterval = %d, want %d (automation.%s.step must feed ResolveRunBudgetWith)",
				c.spec.label, got, c.want, c.spec.label)
		}
	}
	// Recipe frontmatter wins over the kind config (per-field precedence).
	r := &webauto.Automation{Name: "x", Step: &webauto.StepConfig{Interval: 5}}
	if got := r.ResolveRunBudgetWith(taskAutoSpec().stepDef(w)).StepInterval; got != 5 {
		t.Errorf("recipe step should win over kind config: %d", got)
	}
	// Fields the recipe omits fall back to the kind config.
	if got := r.ResolveRunBudgetWith(agentAutoSpec().stepDef(w)); got.StepInterval != 5 {
		t.Errorf("recipe interval should override agent config: %+v", got)
	}
}

// TestParseAutoRunFlagsTarget covers the shared run-flag parser's target
// capture (--agent/--humanoid in both forms) and that --headless stays
// positional for non-web kinds.
func TestParseAutoRunFlagsTarget(t *testing.T) {
	name, args, headless, target, ov := parseAutoRunFlags(
		[]string{"--agent", "code-reviewer", "--step-interval=9", "review", "main.go"}, "--agent", false)
	if name != "review" || target != "code-reviewer" || len(args) != 1 || args[0] != "main.go" ||
		headless || ov.stepInterval != 9 {
		t.Errorf("target flag parse: name=%q target=%q args=%v headless=%v ov=%+v", name, target, args, headless, ov)
	}

	// --agent=value form.
	if _, _, _, target, _ := parseAutoRunFlags([]string{"review", "--agent=qa"}, "--agent", false); target != "qa" {
		t.Errorf("--agent=value not parsed: %q", target)
	}

	// --headless is not a flag for non-web kinds — it stays positional.
	name, _, headless, _, _ = parseAutoRunFlags([]string{"--headless", "review"}, "--agent", false)
	if headless || name != "--headless" {
		t.Errorf("--headless should stay positional off-web: name=%q headless=%v", name, headless)
	}

	// Without a target flag configured, --agent is positional (web kind).
	name, _, _, target, _ = parseAutoRunFlags([]string{"--agent", "x", "r"}, "", true)
	if target != "" || name != "--agent" {
		t.Errorf("target flag should be off when unconfigured: name=%q target=%q", name, target)
	}
}

// TestAutoCodeKind covers the workdir-aware code kind: usage documents
// --workdir + the workdir frontmatter, save/show surface the project dir,
// and the codes store is used.
func TestAutoCodeKind(t *testing.T) {
	dir := t.TempDir()
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: dir}}
	var out strings.Builder

	// ##auto code → usage with --workdir, the codes recipe dir, its config key.
	w.handleAutoCommand(&out, "p", []string{"code"})
	s := out.String()
	if !strings.Contains(s, "##auto code run [--workdir <dir>]") ||
		!strings.Contains(s, ".rysh/automations/codes/") ||
		!strings.Contains(s, "automation.code.step") ||
		!strings.Contains(s, "`workdir: <dir>`") {
		t.Errorf("##auto code usage wrong: %s", s)
	}
	if strings.Contains(s, "--headless") {
		t.Errorf("##auto code usage must not mention web flags: %s", s)
	}

	// save hints at setting the project dir; no web-bound pane required.
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"code", "save", "fix-lint", "Fix", "lint", "in", "{{workdir}}"})
	if !strings.Contains(out.String(), `saved automation "fix-lint"`) ||
		!strings.Contains(out.String(), "`workdir: <dir>`") {
		t.Fatalf("code save should hint at the workdir: %s", out.String())
	}

	// show on a workdir-less recipe flags it; with one, the resolved path.
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"code", "show", "fix-lint"})
	if !strings.Contains(out.String(), "workdir     : (none") {
		t.Errorf("code show should flag the missing workdir: %s", out.String())
	}
	st := webauto.NewStoreFor(dir, webauto.KindCode)
	if err := st.Save(&webauto.Automation{Name: "fix-lint", Workdir: "proj/api", Prompt: "Fix lint in {{workdir}}."}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"code", "show", "fix-lint"})
	if !strings.Contains(out.String(), "workdir     : "+filepath.Join(dir, "proj", "api")) {
		t.Errorf("code show should resolve the workdir: %s", out.String())
	}

	// list brackets the workdir.
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"code", "list"})
	if !strings.Contains(out.String(), "[proj/api]") {
		t.Errorf("code list should bracket the workdir: %s", out.String())
	}
}

// TestResolveCodeWorkdir covers workdir resolution: override > frontmatter,
// ~ expansion, relative anchored under the session work dir, absolute as-is,
// empty when neither source names a directory.
func TestResolveCodeWorkdir(t *testing.T) {
	anchor := t.TempDir()
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: anchor}}
	a := &webauto.Automation{Name: "x", Workdir: "proj/api"}

	// Frontmatter, relative → anchored under the session work dir.
	if got, want := w.resolveCodeWorkdir("", a), filepath.Join(anchor, "proj", "api"); got != want {
		t.Errorf("relative workdir: got %s want %s", got, want)
	}
	// Override wins over frontmatter.
	abs := t.TempDir()
	if got := w.resolveCodeWorkdir(abs, a); got != filepath.Clean(abs) {
		t.Errorf("override should win: got %s want %s", got, abs)
	}
	// ~ expands to the home directory.
	home, _ := os.UserHomeDir()
	if got, want := w.resolveCodeWorkdir("~/proj", a), filepath.Join(home, "proj"); got != want {
		t.Errorf("~ expansion: got %s want %s", got, want)
	}
	// Neither source → empty.
	if got := w.resolveCodeWorkdir("", &webauto.Automation{Name: "y"}); got != "" {
		t.Errorf("empty workdir should stay empty: %q", got)
	}
}

// TestDecorateCodePrompt covers the code prompt decoration: {{workdir}}
// substitution + the work-in-this-directory header; an empty workdir leaves
// the prompt undecorated with the placeholder removed.
func TestDecorateCodePrompt(t *testing.T) {
	got := decorateCodePrompt("Fix lint in {{workdir}}.", "/tmp/proj")
	if !strings.HasPrefix(got, "[Code automation] Project directory: /tmp/proj") ||
		!strings.Contains(got, "Fix lint in /tmp/proj.") {
		t.Errorf("decorated prompt wrong: %s", got)
	}
	if got := decorateCodePrompt("Fix lint in {{workdir}}.", ""); got != "Fix lint in ." {
		t.Errorf("empty workdir should just drop the placeholder: %q", got)
	}
}

// TestParseAutoRunFlagsWorkdir covers --workdir capture through the shared
// parser (the code kind's flagName).
func TestParseAutoRunFlagsWorkdir(t *testing.T) {
	name, args, _, wd, ov := parseAutoRunFlags(
		[]string{"--workdir", "~/proj", "--max-iterations=40", "fix-lint", "api"}, "--workdir", false)
	if name != "fix-lint" || wd != "~/proj" || len(args) != 1 || args[0] != "api" || ov.maxIterations != 40 {
		t.Errorf("--workdir parse: name=%q wd=%q args=%v ov=%+v", name, wd, args, ov)
	}
	if got := codeAutoSpec().flagName(); got != "--workdir" {
		t.Errorf("code flagName = %q, want --workdir", got)
	}
	if got := agentAutoSpec().flagName(); got != "--agent" {
		t.Errorf("agent flagName = %q, want --agent", got)
	}
	if got := taskAutoSpec().flagName(); got != "" {
		t.Errorf("task flagName = %q, want empty", got)
	}
}
