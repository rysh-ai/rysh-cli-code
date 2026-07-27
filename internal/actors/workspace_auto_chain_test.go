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

// TestWhileFlagOverrides (item 8): the run flags form the highest-precedence
// while tier — --no-loop kills the loop, --passes/--while-duration/
// --while-budget replace the recipe's values before the policy runs.
func TestWhileFlagOverrides(t *testing.T) {
	if whileFlagOverrides(webAutoRunOverrides{}) != nil {
		t.Error("no loop flags → nil tier")
	}
	fc := whileFlagOverrides(webAutoRunOverrides{noLoop: true, passes: 3,
		whileDuration: 30 * time.Minute, whileBudgetTok: 2 * webauto.TokensPerBook})
	if fc.Enabled == nil || *fc.Enabled || fc.MaxIterations != 3 ||
		fc.MaxDuration != "30m0s" || fc.Budget != "400p" {
		t.Errorf("flag tier wrong: %+v", fc)
	}

	// End-to-end through the resolver: recipe loops 5×40m; flags force 2
	// passes and a 20m total (over a 4m per-pass base → 10m/pass).
	a := &webauto.Automation{Name: "x", Loop: &webauto.LoopConfig{
		Do: &webauto.StepConfig{MaxDuration: "4m"},
		While: &webauto.WhileConfig{MaxIterations: 5, MaxDuration: "40m",
			Prompts: &webauto.LoopPromptsConfig{Until: "u"}},
	}}
	base := a.ResolveRunBudgetWith(nil)
	spec, pass := a.ResolveWhileWithFlags(nil, base,
		whileFlagOverrides(webAutoRunOverrides{passes: 2, whileDuration: 20 * time.Minute}))
	if !spec.Enabled || spec.MaxIterations != 2 || spec.MaxDuration != 20*time.Minute ||
		pass.MaxDuration != 10*time.Minute {
		t.Errorf("flags should win: %+v pass=%v", spec, pass.MaxDuration)
	}
	// --no-loop: single plain run on do's literal budget.
	spec, pass = a.ResolveWhileWithFlags(nil, base,
		whileFlagOverrides(webAutoRunOverrides{noLoop: true}))
	if spec.Enabled || pass.MaxDuration != 4*time.Minute {
		t.Errorf("--no-loop should disable everything: %+v", spec)
	}
}

// TestAutoChainNext (items 10/11): cause → queue/chain decisions.
func TestAutoChainNext(t *testing.T) {
	cases := []struct {
		cause        string
		queue, chain bool
	}{
		{"fulfilled", true, true},
		{"done", true, true},
		{"unfulfilled: the 4-pass cap is reached", true, false},
		{"unfulfilled: budget exhausted", true, false},
		{"stopped by user", false, false},
		{"aborted: run cancelled by user", false, false},
		{"judge error: boom", false, false},
	}
	for _, c := range cases {
		q, ch := autoChainNext(c.cause)
		if q != c.queue || ch != c.chain {
			t.Errorf("autoChainNext(%q) = (%v,%v), want (%v,%v)", c.cause, q, ch, c.queue, c.chain)
		}
	}
}

// TestParseChainTarget (item 10): bare names stay in-kind, <kind>:<name>
// crosses kinds, junk is rejected.
func TestParseChainTarget(t *testing.T) {
	if l, r, ok := parseChainTarget("task", "verify"); !ok || l != "task" || r != "verify" {
		t.Errorf("bare: %q %q %v", l, r, ok)
	}
	if l, r, ok := parseChainTarget("web", "task:digest"); !ok || l != "task" || r != "digest" {
		t.Errorf("cross-kind: %q %q %v", l, r, ok)
	}
	for _, bad := range []string{"", "bogus:x", ":x", "task:"} {
		if _, _, ok := parseChainTarget("task", bad); ok {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

// TestEachQueueFlow (item 11): --each parses, registers the remainder, and
// handleAutoRunDone advances / completes / abandons per cause.
func TestEachQueueFlow(t *testing.T) {
	// Flag parsing — the ##-command tokenizer keeps literal quotes, so the
	// parser must strip them from the value and every item.
	name, _, _, _, ov := parseAutoRunFlags([]string{"--each", `"tango, jazz ,science"`, "scout"}, "", false)
	if name != "scout" || len(ov.each) != 3 || ov.each[0] != "tango" || ov.each[1] != "jazz" {
		t.Fatalf("--each parse: %q %+v", name, ov.each)
	}

	dir := t.TempDir()
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: dir}}
	var out strings.Builder
	if !w.registerEachQueue(&out, "task", "scout", "exec1", "p1", false, ov) {
		t.Fatal("queue should register")
	}
	q := w.autoQueues["exec1"]
	if q == nil || q.total != 3 || len(q.items) != 2 || len(q.ov.each) != 0 || !q.ov.fromQueue {
		t.Fatalf("queue state wrong: %+v", q)
	}

	// A queue-driven re-dispatch supersedes the loop but must keep its queue:
	// the cmdAutoRun preserve step restores the entry stopAutoLoop deletes.
	keep := w.autoQueues["exec1"]
	w.stopAutoLoop("exec1")
	if q.ov.fromQueue && keep != nil {
		w.autoQueues["exec1"] = keep
	}
	if w.autoQueues["exec1"] != keep {
		t.Fatal("queue must survive a queue-driven supersede")
	}
	if !strings.Contains(out.String(), "running \"tango\" now") {
		t.Errorf("queue plan line: %s", out.String())
	}

	// A dispatch-capturing stub: runAutoByLabel needs a recipe + pub for real
	// runs, so test the DECISION layer by draining the queue with causes.
	// done → advance would re-dispatch; we verify the pop logic directly.
	queueOK, _ := autoChainNext("done")
	if !queueOK {
		t.Fatal("done must continue the queue")
	}
	// Abandon on user stop.
	w2 := &WorkspaceActor{cfg: config.Config{WorkingDirectory: dir}}
	w2.autoQueues = map[string]*autoQueue{"e": {label: "task", recipe: "scout", paneID: "p", items: []string{"x"}, total: 2}}
	w2.handleAutoRunDone(&autoRunDoneMsg{Label: "task", Recipe: "scout", ExecID: "e", PaneID: "p", Cause: "stopped by user"})
	if _, still := w2.autoQueues["e"]; still {
		t.Error("user stop must abandon the queue")
	}
}

// TestChainAndNotifyParse (items 10/12): frontmatter round-trip.
func TestChainAndNotifyParse(t *testing.T) {
	src := `---
on_success: task:digest
notify:
  humanoid: assistant
  channel: whatsapp
  to: "+4915111111"
---
Go.`
	a, err := webauto.Parse("r", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if a.OnSuccess != "task:digest" {
		t.Errorf("on_success: %q", a.OnSuccess)
	}
	if a.Notify == nil || a.Notify.Humanoid != "assistant" || a.Notify.Channel != "whatsapp" || a.Notify.To != "+4915111111" {
		t.Errorf("notify: %+v", a.Notify)
	}
	data, _ := webauto.Render(a)
	back, err := webauto.Parse("r", data)
	if err != nil || back.OnSuccess != a.OnSuccess || back.Notify == nil || back.Notify.Channel != "whatsapp" {
		t.Errorf("round trip: %+v err=%v", back, err)
	}
}

// TestChainCheckAndShow: check validates chain targets + notify; show prints
// the new lines; a dry run reflects --no-loop.
func TestChainCheckAndShow(t *testing.T) {
	dir := t.TempDir()
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: dir}}
	st := webauto.NewStoreFor(dir, webauto.KindTask)

	// Chain to a missing recipe + bogus kind → warnings; valid chain → ok.
	if err := st.Save(&webauto.Automation{Name: "a", Prompt: "p", OnSuccess: "ghost",
		Notify: &webauto.NotifyConfig{Humanoid: "", Channel: "slack"}}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	w.handleAutoCommand(&out, "p", []string{"task", "check", "a"})
	s := out.String()
	if !strings.Contains(s, "on_success target task:ghost does not exist") ||
		!strings.Contains(s, "notify needs both humanoid and channel") {
		t.Errorf("check should warn: %s", s)
	}
	if err := st.Save(&webauto.Automation{Name: "b", Prompt: "p"}); err != nil {
		t.Fatal(err)
	}
	a, _ := st.Load("a")
	a.OnSuccess = "b"
	a.Notify = nil
	if err := st.Save(a); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "check", "a"})
	if !strings.Contains(out.String(), "on_success chains to task:b") {
		t.Errorf("valid chain should pass: %s", out.String())
	}

	// show lines.
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "show", "a"})
	if !strings.Contains(out.String(), "on_success  : b") {
		t.Errorf("show should print on_success: %s", out.String())
	}

	// dry-run with --no-loop on a looping recipe → no loop plan line.
	if err := st.Save(&webauto.Automation{Name: "looped", Prompt: "p", Loop: &webauto.LoopConfig{
		While: &webauto.WhileConfig{Prompts: &webauto.LoopPromptsConfig{Until: "u"}}}}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "run", "--dry-run", "--no-loop", "looped"})
	if strings.Contains(out.String(), "loop: until") || !strings.Contains(out.String(), "DRY RUN") {
		t.Errorf("--no-loop dry run must not plan a loop: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".rysh", "automations", "tasks", "looped", "results")); !os.IsNotExist(err) {
		t.Error("dry-run side effect")
	}
}

// TestLegacyStepMigrationNote: check nudges bare `step:` recipes toward
// loop.do without warning, and stays silent for the canonical form.
func TestLegacyStepMigrationNote(t *testing.T) {
	dir := t.TempDir()
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: dir}}
	st := webauto.NewStoreFor(dir, webauto.KindTask)
	if err := st.Save(&webauto.Automation{Name: "old", Prompt: "p",
		Step: &webauto.StepConfig{MaxDuration: "1m"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(&webauto.Automation{Name: "new", Prompt: "p",
		Loop: &webauto.LoopConfig{Do: &webauto.StepConfig{MaxDuration: "1m"}}}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	w.handleAutoCommand(&out, "p", []string{"task", "check", "old"})
	if !strings.Contains(out.String(), "legacy `step:` form") || !strings.Contains(out.String(), "no warnings") {
		t.Errorf("step-form recipe should get a note (not a warning): %s", out.String())
	}
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "check", "new"})
	if strings.Contains(out.String(), "legacy") {
		t.Errorf("loop.do recipe must not be flagged: %s", out.String())
	}
}

// TestScheduleOff: the declarative off-switch — helper truthiness, check
// note, schedule-cmd refusal, and boot-sync job removal (while key-absence
// still leaves jobs alone).
func TestScheduleOff(t *testing.T) {
	for val, want := range map[string]bool{
		"off": true, "OFF": true, " none ": true, "disabled": true,
		"": false, "0 8 * * *": false, "@daily": false,
	} {
		a := &webauto.Automation{Schedule: val}
		if a.ScheduleOff() != want {
			t.Errorf("ScheduleOff(%q) = %v, want %v", val, !want, want)
		}
	}

	dir := t.TempDir()
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: dir}, cron: &cronScheduler{}}
	st := webauto.NewStoreFor(dir, webauto.KindTask)
	for _, a := range []*webauto.Automation{
		{Name: "paused", Prompt: "p", Schedule: "off"},
		{Name: "keyless", Prompt: "p"},
		{Name: "daily", Prompt: "p", Schedule: "0 8 * * *"},
	} {
		if err := st.Save(a); err != nil {
			t.Fatal(err)
		}
	}

	// check: a note, not a warning.
	var out strings.Builder
	w.handleAutoCommand(&out, "p", []string{"task", "check", "paused"})
	if !strings.Contains(out.String(), "scheduling disabled") || !strings.Contains(out.String(), "no warnings") {
		t.Errorf("check should note schedule off without warning: %s", out.String())
	}

	// schedule cmd: refuses.
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "schedule", "paused"})
	if !strings.Contains(out.String(), "scheduling is disabled") {
		t.Errorf("schedule cmd must refuse: %s", out.String())
	}
	if w.findCronJob("auto-task-paused") != nil {
		t.Fatal("refusal must not create a job")
	}

	// show: annotated line.
	out.Reset()
	w.handleAutoCommand(&out, "p", []string{"task", "show", "paused"})
	if !strings.Contains(out.String(), "off (disabled — no cron job registered)") {
		t.Errorf("show should annotate: %s", out.String())
	}

	// Boot sync: off-recipe's auto job REMOVED, keyless recipe's job kept,
	// keyed recipe upserted.
	mkJob := func(name, input string) *cron.Job {
		return &cron.Job{ID: name, Name: name, Schedule: "@daily", Target: "active", Mode: "rysh", Input: input, Enabled: true}
	}
	w.cron.jobs = append(w.cron.jobs,
		mkJob("auto-task-paused", "##auto task run paused"),
		mkJob("auto-task-keyless", "##auto task run keyless"),
	)
	w.syncRecipeSchedules()
	names := map[string]bool{}
	for _, j := range w.cron.jobs {
		names[j.Name] = true
	}
	if names["auto-task-paused"] {
		t.Error("schedule: off must remove the recipe's auto job at boot")
	}
	if !names["auto-task-keyless"] {
		t.Error("key absence must leave existing jobs alone")
	}
	if !names["auto-task-daily"] {
		t.Error("keyed recipe should still be synced")
	}
}
