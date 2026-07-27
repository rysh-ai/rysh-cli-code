package actors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/cron"
	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// TestBuildIteratePromptJudgeReason: the judge's unfulfilled reason is
// embedded so the next pass targets the measured gap.
func TestBuildIteratePromptJudgeReason(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "items.md"), []byte("1. one"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := buildIteratePrompt("add more items", dir, "only 1 item, need 3")
	if !strings.Contains(p, "NOT yet complete: only 1 item, need 3") ||
		!strings.Contains(p, "Address these specific gaps") ||
		!strings.Contains(p, "add more items") || !strings.Contains(p, "1. one") {
		t.Errorf("iterate prompt missing judge feedback or seed:\n%s", p)
	}
	// No reason → no feedback preamble.
	if p := buildIteratePrompt("go", t.TempDir(), ""); strings.Contains(p, "NOT yet complete") {
		t.Errorf("empty reason should not inject feedback: %s", p)
	}
}

// TestRenderRunReport: passes table, cause, plan; pipe-escaping in reasons.
func TestRenderRunReport(t *testing.T) {
	spec := webauto.LoopSpec{Enabled: true, MaxIterations: 3, Until: "3 items"}
	pass := webauto.RunBudget{MaxIterations: 300, MaxDuration: 2 * time.Minute, MaxContextTokens: 1000}
	recs := []loopPassRecord{
		{pass: 1, fulfilled: false, reason: "only 1 | item"},
		{pass: 2, fulfilled: true, reason: "all 3 present"},
	}
	started := time.Now().Add(-90 * time.Second)
	acct := runAccounting{tokens: 412_000, read: 1_200_000, write: 210_000, fresh: 95_000, samples: 2, cap_: 4_000_000}
	r := renderRunReport("task", "demo", "pane-1", "fulfilled", started, time.Now(), spec, pass, recs, acct)
	for _, want := range []string{"# Run report", "fulfilled", "| 1 | unfulfilled | only 1 \\| item |",
		"| 2 | FULFILLED | all 3 present |", "pane-1", "3 items",
		"412.0k counted / 4.0m run total", "cache    : 79% hit", "read 1.2m"} {
		if !strings.Contains(r, want) {
			t.Errorf("report missing %q:\n%s", want, r)
		}
	}
	empty := renderRunReport("task", "demo", "p", "aborted: run cancelled", started, time.Now(), spec, pass, nil, runAccounting{})
	if !strings.Contains(empty, "No passes were judged") {
		t.Errorf("empty report wrong: %s", empty)
	}
	if strings.Contains(empty, "- tokens") || strings.Contains(empty, "- cache") {
		t.Errorf("unsampled accounting must not render: %s", empty)
	}
}

// TestLatestResultFileExcludesRunReport: the report never seeds passes.
func TestLatestResultFileExcludesRunReport(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "items.md")
	if err := os.WriteFile(old, []byte("real result"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := filepath.Join(dir, "run-report.md")
	if err := os.WriteFile(rep, []byte("report"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1_000_000, 0)
	_ = os.Chtimes(old, base, base)
	_ = os.Chtimes(rep, base.Add(time.Hour), base.Add(time.Hour)) // report is newer
	name, content, ok := latestResultFile(dir)
	if !ok || name != "items.md" || string(content) != "real result" {
		t.Errorf("run-report must be excluded from seeding: (%q, %q, %v)", name, content, ok)
	}
}

// TestUnknownPlaceholders: declared args, argN, and built-ins are known.
func TestUnknownPlaceholders(t *testing.T) {
	known := map[string]bool{"args": true, "output_dir": true, "results_dir": true, "topic": true}
	got := unknownPlaceholders("{{topic}} {{arg1}} {{args}} {{output_dir}} {{oops}} {{typo}} {{oops}}", known)
	if len(got) != 2 || got[0] != "oops" || got[1] != "typo" {
		t.Errorf("unknownPlaceholders = %v, want [oops typo]", got)
	}
	if got := unknownPlaceholders("clean {{arg9}}", known); len(got) != 0 {
		t.Errorf("argN must be known: %v", got)
	}
}

// TestAutoJobNaming: sanitized job names and the input round trip.
func TestAutoJobNaming(t *testing.T) {
	if n := autoJobName("web", "guest-scout"); n != "auto-web-guest-scout" {
		t.Errorf("job name = %q", n)
	}
	if n := autoJobName("task", "my.odd name"); strings.ContainsAny(n, ". ") {
		t.Errorf("job name not sanitized: %q", n)
	}
	in := autoJobInput("task", "hello", []string{"tango", "x"})
	if in != "##auto task run hello tango x" {
		t.Errorf("input = %q", in)
	}
	label, recipe, ok := parseAutoJobInput(in)
	if !ok || label != "task" || recipe != "hello" {
		t.Errorf("parse = (%q,%q,%v)", label, recipe, ok)
	}
	if _, _, ok := parseAutoJobInput("##cron something else"); ok {
		t.Error("non-auto input must not parse")
	}
}

// TestWebReadGuidance: valid values steer, invalid fall back.
func TestWebReadGuidance(t *testing.T) {
	if g, ok := webReadGuidance(""); !ok || g != "" {
		t.Error("empty = ok, no injection")
	}
	if g, ok := webReadGuidance("screenshot"); !ok || !strings.Contains(g, "screenshot action") {
		t.Errorf("screenshot guidance wrong: %q", g)
	}
	if g, ok := webReadGuidance("text"); !ok || !strings.Contains(g, "get_text") {
		t.Errorf("text guidance wrong: %q", g)
	}
	if _, ok := webReadGuidance("pixels"); ok {
		t.Error("invalid mode must return ok=false")
	}
}

// TestDryRun: --dry-run parses, prints the full plan, and has zero side
// effects (no results dir created, safe with a nil publisher).
func TestDryRun(t *testing.T) {
	name, args, _, _, ov := parseAutoRunFlags([]string{"--dry-run", "hello", "tango"}, "", false)
	if name != "hello" || len(args) != 1 || !ov.dryRun {
		t.Fatalf("--dry-run parse failed: %q %v %+v", name, args, ov)
	}

	dir := t.TempDir()
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: dir}}
	st := webauto.NewStoreFor(dir, webauto.KindTask)
	if err := st.Save(&webauto.Automation{Name: "hello", Prompt: "write about {{arg1}} into {{output_dir}}",
		Step: &webauto.StepConfig{Model: "claude-sonnet-5", Effort: "high"},
	}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	w.handleAutoCommand(&out, "p", []string{"task", "run", "--dry-run", "hello", "tango"})
	s := out.String()
	if !strings.Contains(s, "DRY RUN") || !strings.Contains(s, "write about tango") ||
		!strings.Contains(s, "models      : do=claude-sonnet-5/high") || !strings.Contains(s, "budget:") {
		t.Errorf("dry-run output incomplete: %s", s)
	}
	if _, err := os.Stat(filepath.Join(dir, ".rysh", "automations", "tasks", "hello", "results")); !os.IsNotExist(err) {
		t.Error("dry-run must not create the results dir")
	}
}

// TestAutoCheckCmd: warnings for unknown placeholders, bad effort, bad
// schedule, undersized while totals; a clean recipe reports OK.
func TestAutoCheckCmd(t *testing.T) {
	dir := t.TempDir()
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: dir}}
	st := webauto.NewStoreFor(dir, webauto.KindTask)

	if err := st.Save(&webauto.Automation{Name: "messy", Prompt: "use {{tpoic}} here",
		Args: []string{"topic"}, Schedule: "not-a-cron",
		Step: &webauto.StepConfig{Effort: "turbo", MaxDuration: "5m"},
		Loop: &webauto.LoopConfig{While: &webauto.WhileConfig{
			MaxDuration: "1m", // undersized vs 5m per pass
			Prompts:     &webauto.LoopPromptsConfig{Until: "done"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	w.handleAutoCommand(&out, "p", []string{"task", "check", "messy"})
	s := out.String()
	for _, want := range []string{"{{tpoic}}", `do effort "turbo"`, `schedule "not-a-cron"`, "axis DISABLED", "warning(s)"} {
		if !strings.Contains(s, want) {
			t.Errorf("check should flag %q: %s", want, s)
		}
	}

	if err := st.Save(&webauto.Automation{Name: "clean", Prompt: "write about {{topic}} to {{output_dir}}",
		Args: []string{"topic"}, Schedule: "0 8 * * *"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "check", "clean"})
	if !strings.Contains(out.String(), "recipe OK — no warnings") ||
		!strings.Contains(out.String(), `schedule "0 8 * * *" parses`) {
		t.Errorf("clean recipe should be OK: %s", out.String())
	}
}

// TestScheduleCmds: schedule/unschedule against the recipe's schedule key,
// including the schedule-not-defined case.
func TestScheduleCmds(t *testing.T) {
	dir := t.TempDir()
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: dir}, cron: &cronScheduler{}}
	st := webauto.NewStoreFor(dir, webauto.KindTask)

	// No schedule key → friendly pointer, no job.
	if err := st.Save(&webauto.Automation{Name: "ondemand", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	w.handleAutoCommand(&out, "p", []string{"task", "schedule", "ondemand"})
	if !strings.Contains(out.String(), "has no `schedule:` key") || len(w.cron.jobs) != 0 {
		t.Errorf("schedule-less recipe must not create a job: %s", out.String())
	}

	// With the key → job upserted; unschedule removes it.
	if err := st.Save(&webauto.Automation{Name: "daily", Prompt: "p", Schedule: "0 8 * * *"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "schedule", "daily", "tango"})
	if len(w.cron.jobs) != 1 || w.cron.jobs[0].Name != "auto-task-daily" ||
		w.cron.jobs[0].Input != "##auto task run daily tango" || w.cron.jobs[0].Schedule != "0 8 * * *" {
		t.Fatalf("job not created correctly: %+v", w.cron.jobs)
	}
	// Re-schedule updates in place.
	a, _ := st.Load("daily")
	a.Schedule = "0 9 * * *"
	_ = st.Save(a)
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "schedule", "daily"})
	if len(w.cron.jobs) != 1 || w.cron.jobs[0].Schedule != "0 9 * * *" {
		t.Errorf("job not updated: %+v", w.cron.jobs[0])
	}
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "unschedule", "daily"})
	if len(w.cron.jobs) != 0 || !strings.Contains(out.String(), "unscheduled") {
		t.Errorf("unschedule failed: %s", out.String())
	}
}

// TestSyncRecipeSchedules: boot sync upserts keyed recipes, leaves keyless
// ones alone, and removes jobs for deleted recipes.
func TestSyncRecipeSchedules(t *testing.T) {
	dir := t.TempDir()
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: dir}, cron: &cronScheduler{}}
	st := webauto.NewStoreFor(dir, webauto.KindTask)
	if err := st.Save(&webauto.Automation{Name: "daily", Prompt: "p", Schedule: "0 8 * * *"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(&webauto.Automation{Name: "ondemand", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}

	// Pre-existing state: a manually-scheduled job for the keyless recipe
	// (must survive) and a job for a recipe that no longer exists (removed).
	w.cmdAutoSchedule(new(strings.Builder), taskAutoSpec(), "daily", nil) // will be updated, not duplicated
	mkJob := func(name, input string) *cron.Job {
		return &cron.Job{ID: name, Name: name, Schedule: "@daily", Target: "active", Mode: "rysh", Input: input, Enabled: true}
	}
	w.cron.jobs = append(w.cron.jobs,
		mkJob("auto-task-ondemand", "##auto task run ondemand"),
		mkJob("auto-task-ghost", "##auto task run ghost"),
		mkJob("mine", "echo hi"),
	)

	w.syncRecipeSchedules()

	names := map[string]bool{}
	for _, j := range w.cron.jobs {
		names[j.Name] = true
	}
	if !names["auto-task-daily"] {
		t.Error("keyed recipe should be synced")
	}
	if !names["auto-task-ondemand"] {
		t.Error("keyless recipe's existing job must be left alone")
	}
	if names["auto-task-ghost"] {
		t.Error("job for a deleted recipe must be removed")
	}
	if !names["mine"] {
		t.Error("non-auto jobs must never be touched")
	}
}

// TestStatusStopNoLoops: the no-active-loops paths are safe without NATS.
func TestStatusStopNoLoops(t *testing.T) {
	w := &WorkspaceActor{}
	var out strings.Builder
	w.handleAutoCommand(&out, "p", []string{"status"})
	if !strings.Contains(out.String(), "no active loops") {
		t.Errorf("umbrella status: %s", out.String())
	}
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "status"})
	if !strings.Contains(out.String(), "no active loops") {
		t.Errorf("kind status: %s", out.String())
	}
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "stop", "nothing"})
	if !strings.Contains(out.String(), "no active loop") {
		t.Errorf("stop: %s", out.String())
	}
}

// TestModelRejectsEffortWarning: check flags effort paired with a model that
// rejects the parameter.
func TestModelRejectsEffortWarning(t *testing.T) {
	if !modelRejectsEffort("claude-haiku-4-5") || modelRejectsEffort("claude-sonnet-5") || modelRejectsEffort("") {
		t.Fatal("modelRejectsEffort table wrong")
	}
	dir := t.TempDir()
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: dir}}
	st := webauto.NewStoreFor(dir, webauto.KindTask)
	if err := st.Save(&webauto.Automation{Name: "r", Prompt: "p", Loop: &webauto.LoopConfig{
		While: &webauto.WhileConfig{Model: "claude-haiku-4-5", Effort: "low",
			Prompts: &webauto.LoopPromptsConfig{Until: "done"}},
	}}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	w.handleAutoCommand(&out, "p", []string{"task", "check", "r"})
	if !strings.Contains(out.String(), "rejects the effort parameter") {
		t.Errorf("check should warn on the pairing: %s", out.String())
	}
}
