package webauto

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAndRenderRoundTrip(t *testing.T) {
	src := `---
description: Summarize unread emails
web_profile: work-gmail
url: https://mail.google.com
args: [label]
---

Open the inbox and summarize unread emails under label {{label}}.
Report the count first.`
	a, err := Parse("gmail-triage", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if a.Profile != "work-gmail" || a.URL != "https://mail.google.com" || a.Description == "" {
		t.Errorf("frontmatter lost: %+v", a)
	}
	if len(a.Args) != 1 || a.Args[0] != "label" {
		t.Errorf("args lost: %+v", a.Args)
	}
	if !strings.HasPrefix(a.Prompt, "Open the inbox") {
		t.Errorf("prompt lost: %q", a.Prompt)
	}

	// Render → Parse round trip.
	data, err := Render(a)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse("gmail-triage", data)
	if err != nil {
		t.Fatal(err)
	}
	if back.Profile != a.Profile || back.URL != a.URL || back.Prompt != a.Prompt || len(back.Args) != 1 {
		t.Errorf("round trip lost data: %+v", back)
	}
}

func TestParse_NoFrontmatter(t *testing.T) {
	a, err := Parse("simple", []byte("Just do the thing on the current page."))
	if err != nil {
		t.Fatal(err)
	}
	if a.Profile != "" || a.Prompt != "Just do the thing on the current page." {
		t.Errorf("plain prompt parse: %+v", a)
	}
	if _, err := Parse("empty", []byte("  \n")); err == nil {
		t.Error("empty recipe should error")
	}
}

func TestResolveOutputDir(t *testing.T) {
	s := NewStore(t.TempDir())
	root := s.Dir()

	// Default: <recipe-dir>/<sanitized-name>/results.
	if got, want := s.ResolveOutputDir(&Automation{Name: "guest-scout"}), filepath.Join(root, "guest-scout", "results"); got != want {
		t.Errorf("default output dir:\n got: %s\nwant: %s", got, want)
	}
	// Name is sanitized into the default path (no traversal / separators leak).
	if got := s.ResolveOutputDir(&Automation{Name: "../evil"}); strings.Contains(got, "..") || !strings.HasPrefix(got, root) {
		t.Errorf("unsafe default dir for bad name: %s", got)
	}
	// Relative output_dir is anchored under the recipe directory.
	if got, want := s.ResolveOutputDir(&Automation{Name: "x", OutputDir: "leads/tango"}), filepath.Join(root, "leads", "tango"); got != want {
		t.Errorf("relative output dir:\n got: %s\nwant: %s", got, want)
	}
	// Absolute output_dir is used as-is.
	abs := filepath.Join(t.TempDir(), "custom-out")
	if got := s.ResolveOutputDir(&Automation{Name: "x", OutputDir: abs}); got != filepath.Clean(abs) {
		t.Errorf("absolute output dir: got %s want %s", got, abs)
	}
}

func TestOutputDirRoundTrip(t *testing.T) {
	a := &Automation{Name: "r", Profile: "p", OutputDir: "r/results", Prompt: "save to {{output_dir}}"}
	data, err := Render(a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "output_dir: r/results") {
		t.Errorf("output_dir not serialized to frontmatter: %s", data)
	}
	back, err := Parse("r", data)
	if err != nil {
		t.Fatal(err)
	}
	if back.OutputDir != "r/results" {
		t.Errorf("output_dir lost in round trip: %q", back.OutputDir)
	}
}

func TestResolveRunBudget(t *testing.T) {
	// Defaults when the recipe declares nothing.
	b := (&Automation{Name: "x"}).ResolveRunBudget()
	if !b.AutoContinue || b.MaxIterations != DefaultMaxIterations || b.MaxDuration != DefaultMaxDuration {
		t.Errorf("defaults: %+v", b)
	}
	if b.StepInterval != DefaultStepInterval || b.MaxContextTokens != DefaultBudgetSize {
		t.Errorf("default step/token: %+v", b)
	}

	// Explicit values under `step` are honoured (budget.size with a book unit).
	f := false
	b = (&Automation{Name: "x", Step: &StepConfig{MaxIterations: 120, MaxDuration: "5m", AutoContinue: &f, Interval: 25, Budget: &BudgetConfig{Size: "2b"}}}).ResolveRunBudget()
	if b.AutoContinue || b.MaxIterations != 120 || b.MaxDuration != 5*time.Minute || b.StepInterval != 25 || b.MaxContextTokens != 2*TokensPerBook {
		t.Errorf("explicit: %+v", b)
	}

	// auto_continue: true explicit.
	tr := true
	if b := (&Automation{Name: "x", Step: &StepConfig{AutoContinue: &tr}}).ResolveRunBudget(); !b.AutoContinue {
		t.Error("explicit auto_continue:true should be on")
	}

	// auto_approve defaults to true; explicit false is honoured.
	if b := (&Automation{Name: "x"}).ResolveRunBudget(); !b.AutoApprove {
		t.Error("auto_approve should default to true")
	}
	if b := (&Automation{Name: "x", Step: &StepConfig{AutoApprove: &f}}).ResolveRunBudget(); b.AutoApprove {
		t.Error("explicit auto_approve:false should be off")
	}

	// Fallbacks: bad duration, non-positive iterations/step, malformed budget.size.
	b = (&Automation{Name: "x", Step: &StepConfig{MaxIterations: -5, MaxDuration: "not-a-duration", Interval: -1, Budget: &BudgetConfig{Size: "nonsense"}}}).ResolveRunBudget()
	if b.MaxIterations != DefaultMaxIterations || b.MaxDuration != DefaultMaxDuration ||
		b.StepInterval != DefaultStepInterval || b.MaxContextTokens != DefaultBudgetSize {
		t.Errorf("fallbacks: %+v", b)
	}
	if got := (&Automation{Name: "x", Step: &StepConfig{Interval: 9999}}).ResolveRunBudget().StepInterval; got != maxStepInterval {
		t.Errorf("step interval not clamped: got %d want %d", got, maxStepInterval)
	}

	// takeover_when (nested under step.budget.watch) defaults, and clamps to [min, max].
	if got := (&Automation{Name: "x"}).ResolveRunBudget().TakeoverWhen; got != DefaultTakeoverWhen {
		t.Errorf("default takeover_when = %d, want %d", got, DefaultTakeoverWhen)
	}
	if got := (&Automation{Name: "x", Step: &StepConfig{Budget: &BudgetConfig{Watch: &WatchConfig{TakeoverWhen: 200}}}}).ResolveRunBudget().TakeoverWhen; got != maxTakeoverWhen {
		t.Errorf("takeover_when not clamped above: got %d want %d", got, maxTakeoverWhen)
	}
	if got := (&Automation{Name: "x", Step: &StepConfig{Budget: &BudgetConfig{Watch: &WatchConfig{TakeoverWhen: 5}}}}).ResolveRunBudget().TakeoverWhen; got != minTakeoverWhen {
		t.Errorf("takeover_when not clamped below: got %d want %d", got, minTakeoverWhen)
	}
}

func TestParseBudgetSize(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"50p", 50 * TokensPerPage, true},
		{"50", 50 * TokensPerPage, true}, // bare number = pages
		{"3b", 3 * TokensPerBook, true},
		{"2s", 2 * TokensPerShelf, true},
		{"3B", 3 * TokensPerBook, true},   // case-insensitive
		{" 3b ", 3 * TokensPerBook, true}, // trimmed
		{"", 0, false},
		{"b", 0, false},   // no number
		{"abc", 0, false}, // malformed
		{"0b", 0, false},  // non-positive
		{"-2b", 0, false}, // negative
	}
	for _, c := range cases {
		got, ok := ParseBudgetSize(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseBudgetSize(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestSplitForTakeover(t *testing.T) {
	// Takeover at 90% consumed: the task gets 90% of steps/time/tokens; the
	// takeover leg gets the remaining 10% as a separate fresh allowance.
	b := RunBudget{MaxIterations: 300, MaxDuration: 20 * time.Minute, MaxContextTokens: 50000, StepInterval: 50, TakeoverWhen: 90}
	task, fin := b.SplitForTakeover()
	if task.MaxIterations != 270 || fin.MaxIterations != 30 {
		t.Errorf("iterations split: task=%d fin=%d, want 270/30", task.MaxIterations, fin.MaxIterations)
	}
	if task.MaxDuration != 18*time.Minute || fin.MaxDuration != 2*time.Minute {
		t.Errorf("duration split: task=%s fin=%s, want 18m/2m", task.MaxDuration, fin.MaxDuration)
	}
	if task.MaxContextTokens != 45000 || fin.MaxContextTokens != 5000 {
		t.Errorf("token split: task=%d fin=%d, want 45000/5000", task.MaxContextTokens, fin.MaxContextTokens)
	}
	// All three shares (steps, duration, tokens) sum back to the full budget.
	if task.MaxIterations+fin.MaxIterations != b.MaxIterations ||
		task.MaxDuration+fin.MaxDuration != b.MaxDuration ||
		task.MaxContextTokens+fin.MaxContextTokens != b.MaxContextTokens {
		t.Errorf("shares don't sum to the full budget")
	}
	// Out-of-range percent is clamped inside the split (500 → 99: fin gets 1%).
	if _, f := (RunBudget{MaxIterations: 300, TakeoverWhen: 500}).SplitForTakeover(); f.MaxIterations != 300-300*maxTakeoverWhen/100 {
		t.Errorf("percent not clamped above in split: fin iters=%d", f.MaxIterations)
	}
	// Below-min percent is clamped up (0 → 10: task gets 10%).
	if tk, _ := (RunBudget{MaxIterations: 300, TakeoverWhen: 0}).SplitForTakeover(); tk.MaxIterations != 300*minTakeoverWhen/100 {
		t.Errorf("percent not clamped below in split: task iters=%d", tk.MaxIterations)
	}
}

// TestWithTakeoverFloor covers the takeover-floor policy: a share thinner than
// any trigger threshold (<1m / <50 steps / <0.3 book) is raised to the floors
// (≥5m / ≥100 steps / ≥1 book); a share above all thresholds is untouched.
func TestWithTakeoverFloor(t *testing.T) {
	// guest-scout shape: 300 steps / 7m / 3b at takeover_when 90 → the reserved
	// 30 steps / 42s / 60k trips the duration+steps triggers → all axes floored.
	_, fin := (RunBudget{MaxIterations: 300, MaxDuration: 7 * time.Minute, MaxContextTokens: 3 * TokensPerBook, TakeoverWhen: 90}).SplitForTakeover()
	got := fin.WithTakeoverFloor()
	if got.MaxIterations != TakeoverFloorIterations || got.MaxDuration != TakeoverFloorDuration || got.MaxContextTokens != TakeoverFloorTokens {
		t.Errorf("thin share not floored: %+v", got)
	}

	// A comfortable share (100 steps / 6m / 2 books) is above every trigger →
	// returned unchanged, even though 100 steps == the floor.
	roomy := RunBudget{MaxIterations: 100, MaxDuration: 6 * time.Minute, MaxContextTokens: 2 * TokensPerBook}
	if got := roomy.WithTakeoverFloor(); got != roomy {
		t.Errorf("roomy share should be unchanged: %+v", got)
	}

	// Exactly at the trigger thresholds (50 steps / 1m / 0.3 book) → no trigger
	// (strictly "less than"), unchanged.
	edge := RunBudget{MaxIterations: takeoverTriggerIterations, MaxDuration: takeoverTriggerDuration, MaxContextTokens: takeoverTriggerTokens}
	if got := edge.WithTakeoverFloor(); got != edge {
		t.Errorf("at-threshold share should be unchanged: %+v", got)
	}

	// Mixed: only one axis thin (40 steps) — it triggers the floor, but axes
	// already above their floor (10m, 500k tokens) are kept, not lowered.
	mixed := RunBudget{MaxIterations: 40, MaxDuration: 10 * time.Minute, MaxContextTokens: 500_000}
	got = mixed.WithTakeoverFloor()
	if got.MaxIterations != TakeoverFloorIterations || got.MaxDuration != 10*time.Minute || got.MaxContextTokens != 500_000 {
		t.Errorf("mixed share floored wrong: %+v", got)
	}
}

func TestBudgetTakeoverRoundTrip(t *testing.T) {
	src := `---
web_profile: p
step:
  interval: 40
  budget:
    size: 2b
    watch:
      takeover_when: 85
      takeover_prompt: save what you have to {{output_dir}} and summarize gaps
---
Do the long thing.`
	a, err := Parse("r", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if a.Step == nil || a.Step.Budget == nil || a.Step.Budget.Watch == nil {
		t.Fatal("step.budget.watch not parsed")
	}
	if a.Step.Interval != 40 || a.Step.Budget.Size != "2b" {
		t.Errorf("step fields not parsed: %+v", a.Step)
	}
	if a.TakeoverPrompt() != "save what you have to {{output_dir}} and summarize gaps" {
		t.Errorf("takeover prompt not parsed: %q", a.TakeoverPrompt())
	}
	if a.Step.Budget.Watch.TakeoverWhen != 85 {
		t.Errorf("budget watch takeover_when not parsed: %d", a.Step.Budget.Watch.TakeoverWhen)
	}
	b := a.ResolveRunBudget()
	if b.TakeoverWhen != 85 || b.StepInterval != 40 || b.MaxContextTokens != 2*TokensPerBook {
		t.Errorf("step budget not resolved: %+v", b)
	}
	// Render → Parse round trip keeps the nested structure.
	data, err := Render(a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "step:") || !strings.Contains(string(data), "budget:") ||
		!strings.Contains(string(data), "watch:") || !strings.Contains(string(data), "takeover_when: 85") {
		t.Errorf("step/budget/watch not serialized nested: %s", data)
	}
	back, _ := Parse("r", data)
	if back.TakeoverPrompt() != a.TakeoverPrompt() || back.Step.Budget.Watch.TakeoverWhen != 85 || back.Step.Interval != 40 {
		t.Errorf("step lost in round trip: %+v", back.Step)
	}

	// No step block → no takeover prompt, default takeover_when.
	plain, _ := Parse("p", []byte("Just do it."))
	if plain.TakeoverPrompt() != "" || plain.ResolveRunBudget().TakeoverWhen != DefaultTakeoverWhen {
		t.Errorf("absent step: prompt=%q pct=%d", plain.TakeoverPrompt(), plain.ResolveRunBudget().TakeoverWhen)
	}
}

func TestLegacyBudgetKeysIgnored(t *testing.T) {
	// Clean break: the retired keys (step.token_budget, step.finalizer, and the
	// flat step.budget.takeover_* from before the watch block) no longer parse —
	// a legacy recipe falls back to the budget defaults.
	src := `---
web_profile: p
step:
  interval: 40
  token_budget: 2b
  finalizer:
    prompt: old-style wrap-up
    budget_percent: 15
  budget:
    takeover_when: 20
    takeover_prompt: flat old-style wrap-up
---
Do the thing.`
	a, err := Parse("legacy", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if a.Step == nil || a.Step.Interval != 40 {
		t.Fatalf("surviving step fields not parsed: %+v", a.Step)
	}
	if (a.Step.Budget != nil && a.Step.Budget.Watch != nil) || a.TakeoverPrompt() != "" {
		t.Errorf("legacy keys should not populate step.budget.watch: %+v", a.Step.Budget)
	}
	b := a.ResolveRunBudget()
	if b.MaxContextTokens != DefaultBudgetSize || b.TakeoverWhen != DefaultTakeoverWhen {
		t.Errorf("legacy keys should fall back to defaults: %+v", b)
	}
}

func TestSubstituteArgs(t *testing.T) {
	a := &Automation{
		Args:   []string{"query", "label"},
		Prompt: "Search for {{query}} in {{label}}; also {{arg1}} + {{arg2}}; all: {{args}}; unused: {{arg5}}.",
	}
	got := SubstituteArgs(a, []string{"invoices", "work"})
	want := "Search for invoices in work; also invoices + work; all: invoices work; unused: ."
	if got != want {
		t.Errorf("substitute:\n got: %s\nwant: %s", got, want)
	}
	// No runtime args → placeholders vanish.
	got2 := SubstituteArgs(a, nil)
	if strings.Contains(got2, "{{") {
		t.Errorf("template residue: %s", got2)
	}
}

func TestStoreCRUD(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)

	a := &Automation{Name: "orders", Profile: "shop", URL: "https://shop.example", Prompt: "Check my orders."}
	if err := s.Save(a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".rysh", "automations", "webs", "orders.md")); err != nil {
		t.Fatalf("recipe file missing: %v", err)
	}

	loaded, err := s.Load("orders")
	if err != nil || loaded.Profile != "shop" || loaded.Prompt != "Check my orders." {
		t.Fatalf("load: %+v err=%v", loaded, err)
	}

	if list := s.List(); len(list) != 1 || list[0].Name != "orders" {
		t.Fatalf("list: %+v", list)
	}

	if err := s.Delete("orders"); err != nil {
		t.Fatal(err)
	}
	if list := s.List(); len(list) != 0 {
		t.Fatalf("delete didn't remove: %+v", list)
	}

	// Path-escape hardening: the sanitized name must be a single safe path
	// segment (no separators, no traversal), and the file must land inside
	// the store dir.
	san := SanitizeName("../evil")
	if san == "" || strings.ContainsAny(san, "/\\") || strings.Contains(san, "..") {
		t.Fatalf("unsafe sanitized name: %q", san)
	}
	if err := s.Save(&Automation{Name: "../evil", Prompt: "x"}); err != nil {
		t.Fatalf("sanitized save should succeed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".rysh", "automations", "webs", san+".md")); err != nil {
		t.Fatalf("sanitized name %q not used: %v", san, err)
	}
	if SanitizeName("..") != "" {
		t.Error("pure traversal should sanitize to empty")
	}
}

// TestLegacyDirMigration verifies NewStore renames a pre-rename
// .rysh/web-automations directory to .rysh/automations/webs, keeping recipes
// and their result folders intact — and leaves an already-migrated (or
// coexisting) layout alone.
func TestLegacyDirMigration(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, ".rysh", "web-automations")
	if err := os.MkdirAll(filepath.Join(old, "scout", "results"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "scout.md"), []byte("do the thing"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "scout", "results", "r1.md"), []byte("result"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := NewStore(dir)
	if want := filepath.Join(dir, ".rysh", "automations", "webs"); s.Dir() != want {
		t.Fatalf("store dir = %s, want %s", s.Dir(), want)
	}
	a, err := s.Load("scout")
	if err != nil || a.Prompt != "do the thing" {
		t.Fatalf("migrated recipe not loadable: %+v err=%v", a, err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir(), "scout", "results", "r1.md")); err != nil {
		t.Errorf("migrated results missing: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("legacy dir should be gone after migration: %v", err)
	}

	// New layout already present → a (re)created legacy dir is left alone.
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = NewStore(dir)
	if _, err := os.Stat(old); err != nil {
		t.Errorf("coexisting legacy dir must not be touched when new layout exists: %v", err)
	}
}

// TestResolveRunBudgetWithConfigDefaults covers the three-layer precedence:
// recipe frontmatter > config-level automation.web.step defaults > built-ins,
// including the takeover floor and the config-default takeover prompt.
func TestResolveRunBudgetWithConfigDefaults(t *testing.T) {
	tr := true
	f := false
	def := &StepConfig{
		Interval:      40,
		MaxIterations: 500,
		MaxDuration:   "9m",
		AutoApprove:   &f,
		AutoContinue:  &tr,
		Budget: &BudgetConfig{
			Size: "2b",
			Watch: &WatchConfig{
				TakeoverWhen:   80,
				TakeoverPrompt: "config wrap-up",
				Floor: &FloorConfig{
					TriggerIterations: 60, TriggerDuration: "2m", TriggerSize: "100p",
					Iterations: 150, Duration: "6m", Size: "2b",
				},
			},
		},
	}

	// Recipe with no step block → config defaults win over built-ins.
	empty := &Automation{Name: "x"}
	b := empty.ResolveRunBudgetWith(def)
	if b.StepInterval != 40 || b.MaxIterations != 500 || b.MaxDuration != 9*time.Minute ||
		b.MaxContextTokens != 2*TokensPerBook || b.TakeoverWhen != 80 || b.AutoApprove || !b.AutoContinue {
		t.Errorf("config defaults not applied: %+v", b)
	}
	if b.Floor.TriggerIterations != 60 || b.Floor.TriggerDuration != 2*time.Minute ||
		b.Floor.TriggerTokens != 100*TokensPerPage || b.Floor.Iterations != 150 ||
		b.Floor.Duration != 6*time.Minute || b.Floor.Tokens != 2*TokensPerBook {
		t.Errorf("config floor not applied: %+v", b.Floor)
	}
	if got := empty.TakeoverPromptWith(def); got != "config wrap-up" {
		t.Errorf("config takeover prompt not applied: %q", got)
	}

	// Recipe values win over config defaults.
	recipe := &Automation{Name: "x", Step: &StepConfig{
		Interval: 25, MaxDuration: "5m", AutoApprove: &tr,
		Budget: &BudgetConfig{Size: "1b", Watch: &WatchConfig{TakeoverWhen: 85, TakeoverPrompt: "recipe wrap-up"}},
	}}
	b = recipe.ResolveRunBudgetWith(def)
	if b.StepInterval != 25 || b.MaxDuration != 5*time.Minute || b.MaxContextTokens != TokensPerBook ||
		b.TakeoverWhen != 85 || !b.AutoApprove {
		t.Errorf("recipe should override config: %+v", b)
	}
	// Fields the recipe omits still come from config.
	if b.MaxIterations != 500 || b.Floor.Iterations != 150 {
		t.Errorf("omitted fields should fall back to config: %+v", b)
	}
	if got := recipe.TakeoverPromptWith(def); got != "recipe wrap-up" {
		t.Errorf("recipe takeover prompt should win: %q", got)
	}

	// Nil defaults → identical to ResolveRunBudget (built-ins).
	if got, want := empty.ResolveRunBudgetWith(nil), empty.ResolveRunBudget(); got != want {
		t.Errorf("nil defaults mismatch:\n got %+v\nwant %+v", got, want)
	}

	// A resolved custom floor drives WithTakeoverFloor.
	_, fin := empty.ResolveRunBudgetWith(def).SplitForTakeover()
	// share = 20% of 500 steps / 9m / 2b = 100 steps / 108s / 80k tokens →
	// below the config triggers (60 steps? no: 100>=60; 108s<2m yes) → floored.
	fin = fin.WithTakeoverFloor()
	if fin.MaxIterations != 150 || fin.MaxDuration != 6*time.Minute || fin.MaxContextTokens != 2*TokensPerBook {
		t.Errorf("config floor not enforced: %+v", fin)
	}
}

// TestStoreKinds covers the per-kind directory layout: each ##auto kind gets
// its own child under .rysh/automations, kinds are isolated from each other,
// and NewStore stays the web store for compatibility.
func TestStoreKinds(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		kind Kind
		sub  string
	}{
		{KindWeb, "webs"},
		{KindTask, "tasks"},
		{KindAgent, "agents"},
		{KindHumanoid, "humanoids"},
		{KindCode, "codes"},
	} {
		s := NewStoreFor(dir, tc.kind)
		want := filepath.Join(dir, ".rysh", "automations", tc.sub)
		if s.Dir() != want {
			t.Errorf("NewStoreFor(%q) dir = %s, want %s", tc.kind, s.Dir(), want)
		}
		if got := tc.kind.Subdir(); got != tc.sub {
			t.Errorf("Kind(%q).Subdir() = %q, want %q", tc.kind, got, tc.sub)
		}
		if err := s.Save(&Automation{Name: "r-" + tc.sub, Prompt: "p"}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(want, "r-"+tc.sub+".md")); err != nil {
			t.Errorf("recipe not under %s: %v", tc.sub, err)
		}
	}
	// Kinds are isolated: each store lists only its own recipe.
	for _, k := range []Kind{KindWeb, KindTask, KindAgent, KindHumanoid, KindCode} {
		if list := NewStoreFor(dir, k).List(); len(list) != 1 {
			t.Errorf("store %q sees %d recipes, want 1", k, len(list))
		}
	}
	// NewStore == the web store.
	if NewStore(dir).Dir() != NewStoreFor(dir, KindWeb).Dir() {
		t.Error("NewStore should equal NewStoreFor(KindWeb)")
	}
	// An unknown/zero kind falls back to webs (compatibility).
	if NewStoreFor(dir, Kind("")).Dir() != NewStoreFor(dir, KindWeb).Dir() {
		t.Error("zero-value kind should fall back to the web store")
	}
}

// TestLegacyDirMigrationOnlyWeb verifies only the web store migrates the
// pre-rename .rysh/web-automations directory — opening another kind's store
// must leave it untouched.
func TestLegacyDirMigrationOnlyWeb(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, ".rysh", "web-automations")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "scout.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = NewStoreFor(dir, KindTask)
	if _, err := os.Stat(old); err != nil {
		t.Errorf("non-web store must not migrate the legacy web dir: %v", err)
	}
}

// TestParseKindTargets covers the per-kind frontmatter: a task recipe needs
// no web fields, agent/humanoid recipes parse their target name, and the
// targets survive a Render→Parse round trip.
func TestParseKindTargets(t *testing.T) {
	// Task recipe: description/args/prompt only — no web_profile, no url.
	task, err := Parse("hello", []byte("---\ndescription: d\nargs: [topic]\n---\nWrite about {{topic}}, save to {{output_dir}}."))
	if err != nil {
		t.Fatal(err)
	}
	if task.Profile != "" || task.URL != "" || task.Agent != "" || task.Humanoid != "" {
		t.Errorf("task recipe should have no web/target fields: %+v", task)
	}
	if len(task.Args) != 1 || task.Args[0] != "topic" {
		t.Errorf("task args lost: %+v", task.Args)
	}

	// Agent recipe parses its target name.
	ag, err := Parse("review", []byte("---\nagent: code-reviewer\n---\nReview the diff."))
	if err != nil {
		t.Fatal(err)
	}
	if ag.Agent != "code-reviewer" {
		t.Errorf("agent target not parsed: %q", ag.Agent)
	}

	// Humanoid recipe parses its target name.
	hu, err := Parse("greet", []byte("---\nhumanoid: concierge\n---\nGreet the guests."))
	if err != nil {
		t.Fatal(err)
	}
	if hu.Humanoid != "concierge" {
		t.Errorf("humanoid target not parsed: %q", hu.Humanoid)
	}

	// Round trip keeps the targets.
	data, err := Render(ag)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "agent: code-reviewer") {
		t.Errorf("agent not serialized to frontmatter: %s", data)
	}
	back, err := Parse("review", data)
	if err != nil || back.Agent != "code-reviewer" {
		t.Errorf("agent lost in round trip: %+v err=%v", back, err)
	}
	hdata, err := Render(hu)
	if err != nil {
		t.Fatal(err)
	}
	hback, err := Parse("greet", hdata)
	if err != nil || hback.Humanoid != "concierge" {
		t.Errorf("humanoid lost in round trip: %+v err=%v", hback, err)
	}

	// Code recipe parses its project directory and keeps it round-trip.
	co, err := Parse("fix-lint", []byte("---\nworkdir: ~/proj/api\n---\nFix the lint errors in {{workdir}}."))
	if err != nil {
		t.Fatal(err)
	}
	if co.Workdir != "~/proj/api" {
		t.Errorf("code workdir not parsed: %q", co.Workdir)
	}
	cdata, err := Render(co)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cdata), "workdir: ~/proj/api") {
		t.Errorf("workdir not serialized to frontmatter: %s", cdata)
	}
	cback, err := Parse("fix-lint", cdata)
	if err != nil || cback.Workdir != "~/proj/api" {
		t.Errorf("workdir lost in round trip: %+v err=%v", cback, err)
	}
}

// TestLoopParse covers the loop:{do,while} frontmatter: parsing, the
// Render→Parse round trip, the step/loop.do aliasing, and the clean break
// for the retired step.loop_over_step key.
func TestLoopParse(t *testing.T) {
	src := `---
loop:
  do:
    interval: 30
    max_duration: 7m
    budget:
      size: 3b
  while:
    enabled: true
    max_iterations: 5
    max_duration: 40m
    budget: 15b
    prompts:
      until: list has 12 candidates
      iterate_with: find more, avoid duplicates
---
Do the thing.`
	a, err := Parse("r", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if a.Loop == nil || a.Loop.Do == nil || a.Loop.While == nil {
		t.Fatalf("loop block not parsed: %+v", a.Loop)
	}
	if a.Loop.Do.Interval != 30 || a.Loop.Do.MaxDuration != "7m" || a.Loop.Do.Budget.Size != "3b" {
		t.Errorf("loop.do not parsed: %+v", a.Loop.Do)
	}
	wl := a.Loop.While
	if wl.Enabled == nil || !*wl.Enabled || wl.MaxIterations != 5 || wl.MaxDuration != "40m" || wl.Budget != "15b" {
		t.Errorf("loop.while not parsed: %+v", wl)
	}
	if wl.Prompts == nil || wl.Prompts.Until != "list has 12 candidates" || wl.Prompts.IterateWith != "find more, avoid duplicates" {
		t.Errorf("while prompts not parsed: %+v", wl.Prompts)
	}

	// loop.do is the effective step; with a plain step alongside, loop.do wins.
	if a.EffectiveStep() != a.Loop.Do {
		t.Error("EffectiveStep should return loop.do")
	}
	a.Step = &StepConfig{Interval: 99}
	if a.EffectiveStep() != a.Loop.Do || !a.HasBothStepForms() {
		t.Error("loop.do should win over step, and HasBothStepForms should report it")
	}
	if b := a.ResolveRunBudgetWith(nil); b.StepInterval != 30 || b.MaxDuration != 7*time.Minute {
		t.Errorf("run budget should resolve from loop.do: %+v", b)
	}
	a.Step = nil

	// Round trip keeps the whole loop block.
	data, err := Render(a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "loop:") || !strings.Contains(string(data), "while:") ||
		!strings.Contains(string(data), "budget: 15b") {
		t.Errorf("loop not serialized: %s", data)
	}
	back, err := Parse("r", data)
	if err != nil || back.Loop == nil || back.Loop.While == nil || back.Loop.While.Budget != "15b" {
		t.Errorf("loop lost in round trip: %+v err=%v", back.Loop, err)
	}

	// Clean break: the retired step.loop_over_step key no longer parses — a
	// legacy recipe falls back to a plain (loop-less) run.
	legacy, err := Parse("l", []byte("---\nstep:\n  interval: 40\n  loop_over_step:\n    max_iterations: 3\n    prompts:\n      until: done\n---\nDo it."))
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Step == nil || legacy.Step.Interval != 40 {
		t.Fatalf("surviving step fields not parsed: %+v", legacy.Step)
	}
	if spec, _ := legacy.ResolveWhileWith(nil, legacy.ResolveRunBudget()); spec.Enabled {
		t.Errorf("retired loop_over_step should not enable a loop: %+v", spec)
	}
}

// TestResolveWhilePolicy covers the while budget policy: the enabled gate,
// per-axis redistribute (while total > per-pass do value → do.X = while.X/N)
// vs disable (while total <= per-pass value → ignored), the strict-comparison
// edge, omitted axes, defaults, and config-level merging.
func TestResolveWhilePolicy(t *testing.T) {
	tr, f := true, false
	mkLoop := func(w *WhileConfig) *Automation {
		return &Automation{Name: "x", Loop: &LoopConfig{
			Do:    &StepConfig{MaxDuration: "7m", Budget: &BudgetConfig{Size: "3b"}},
			While: w,
		}}
	}

	// No while block anywhere → disabled zero value, base untouched.
	plain := &Automation{Name: "x"}
	base := plain.ResolveRunBudget()
	if spec, got := plain.ResolveWhileWith(nil, base); spec.Enabled || got != base {
		t.Errorf("no while block should be a no-op: %+v", spec)
	}

	// The user's worked example: while {5, 200m, 20b} over do {7m, 3b} →
	// redistribute both axes: 200m/5 = 40m/pass, 20b/5 = 4b/pass; totals kept.
	a := mkLoop(&WhileConfig{MaxIterations: 5, MaxDuration: "200m", Budget: "20b",
		Prompts: &LoopPromptsConfig{Until: "done enough"}})
	spec, got := a.ResolveWhileWith(nil, a.ResolveRunBudget())
	if !spec.Enabled || spec.MaxIterations != 5 {
		t.Fatalf("loop should enable: %+v", spec)
	}
	if spec.MaxDuration != 200*time.Minute || got.MaxDuration != 40*time.Minute {
		t.Errorf("duration redistribution: total=%v pass=%v, want 200m/40m", spec.MaxDuration, got.MaxDuration)
	}
	if spec.MaxTokens != 20*TokensPerBook || got.MaxContextTokens != 4*TokensPerBook {
		t.Errorf("token redistribution: total=%d pass=%d, want 20b/4b", spec.MaxTokens, got.MaxContextTokens)
	}

	// Guest-scout shape: while {5, 40m, 15b} over do {7m, 3b} → 8m/pass, 3b/pass.
	a = mkLoop(&WhileConfig{MaxIterations: 5, MaxDuration: "40m", Budget: "15b",
		Prompts: &LoopPromptsConfig{Until: "u"}})
	spec, got = a.ResolveWhileWith(nil, a.ResolveRunBudget())
	if got.MaxDuration != 8*time.Minute || got.MaxContextTokens != 3*TokensPerBook {
		t.Errorf("guest-scout shape: pass=%v/%d, want 8m/3b", got.MaxDuration, got.MaxContextTokens)
	}

	// Disable: while totals smaller than one pass's budget are ignored.
	a = mkLoop(&WhileConfig{MaxIterations: 5, MaxDuration: "5m", Budget: "2b",
		Prompts: &LoopPromptsConfig{Until: "u"}})
	spec, got = a.ResolveWhileWith(nil, a.ResolveRunBudget())
	if spec.MaxDuration != 0 || spec.MaxTokens != 0 ||
		got.MaxDuration != 7*time.Minute || got.MaxContextTokens != 3*TokensPerBook {
		t.Errorf("undersized while totals should be disabled: spec=%+v pass=%v/%d", spec, got.MaxDuration, got.MaxContextTokens)
	}

	// Equal is NOT bigger → disable branch.
	a = mkLoop(&WhileConfig{MaxIterations: 5, MaxDuration: "7m", Budget: "3b",
		Prompts: &LoopPromptsConfig{Until: "u"}})
	if spec, got := a.ResolveWhileWith(nil, a.ResolveRunBudget()); spec.MaxDuration != 0 || spec.MaxTokens != 0 ||
		got.MaxDuration != 7*time.Minute {
		t.Errorf("equal totals should be disabled: %+v", spec)
	}

	// Omitted axes → no outer caps, no redistribution; defaults fill in
	// (5 passes, built-in iterate prompt).
	a = mkLoop(&WhileConfig{Prompts: &LoopPromptsConfig{Until: "u"}})
	spec, got = a.ResolveWhileWith(nil, a.ResolveRunBudget())
	if !spec.Enabled || spec.MaxIterations != DefaultLoopIterations ||
		spec.MaxDuration != 0 || spec.MaxTokens != 0 ||
		spec.IterateWith != DefaultLoopIteratePrompt || got.MaxDuration != 7*time.Minute {
		t.Errorf("omitted axes/defaults: %+v", spec)
	}

	// enabled:false → single run, NOTHING applies (no redistribution even
	// with oversized totals).
	a = mkLoop(&WhileConfig{Enabled: &f, MaxIterations: 5, MaxDuration: "200m", Budget: "20b",
		Prompts: &LoopPromptsConfig{Until: "u"}})
	if spec, got := a.ResolveWhileWith(nil, a.ResolveRunBudget()); spec.Enabled ||
		got.MaxDuration != 7*time.Minute || got.MaxContextTokens != 3*TokensPerBook {
		t.Errorf("enabled:false must be a plain single run: spec=%+v pass=%v", spec, got.MaxDuration)
	}

	// No until prompt → loop cannot run (even with enabled:true).
	a = mkLoop(&WhileConfig{Enabled: &tr, MaxIterations: 3})
	if spec, _ := a.ResolveWhileWith(nil, a.ResolveRunBudget()); spec.Enabled {
		t.Errorf("while without until should be disabled: %+v", spec)
	}

	// Config-level while defaults apply when the recipe has none; recipe
	// fields win per-field.
	def := &LoopConfig{While: &WhileConfig{MaxIterations: 7,
		Prompts: &LoopPromptsConfig{Until: "config until", IterateWith: "config iterate"}}}
	spec, _ = plain.ResolveWhileWith(def, plain.ResolveRunBudget())
	if !spec.Enabled || spec.MaxIterations != 7 || spec.Until != "config until" || spec.IterateWith != "config iterate" {
		t.Errorf("config while not applied: %+v", spec)
	}
	recipe := mkLoop(&WhileConfig{MaxIterations: 2, Prompts: &LoopPromptsConfig{Until: "recipe until"}})
	spec, _ = recipe.ResolveWhileWith(def, recipe.ResolveRunBudget())
	if spec.MaxIterations != 2 || spec.Until != "recipe until" || spec.IterateWith != "config iterate" {
		t.Errorf("recipe should win per-field over config: %+v", spec)
	}
	// Config-level enabled:false kills the loop unless the recipe re-enables.
	defOff := &LoopConfig{While: &WhileConfig{Enabled: &f, Prompts: &LoopPromptsConfig{Until: "config until"}}}
	if spec, _ := plain.ResolveWhileWith(defOff, plain.ResolveRunBudget()); spec.Enabled {
		t.Error("config enabled:false should disable the loop")
	}
	on := mkLoop(&WhileConfig{Enabled: &tr, Prompts: &LoopPromptsConfig{Until: "u"}})
	if spec, _ := on.ResolveWhileWith(defOff, on.ResolveRunBudget()); !spec.Enabled {
		t.Error("recipe enabled:true should override config enabled:false")
	}
}

// TestModelEffortSeats covers the three model/effort seats: executor
// (step/do), finalizer (budget.watch), judge (while) — recipe > config
// precedence and finalizer fallback semantics (empty = fall back downstream).
func TestModelEffortSeats(t *testing.T) {
	def := &StepConfig{Model: "cfg-model", Effort: "low",
		Budget: &BudgetConfig{Watch: &WatchConfig{Model: "cfg-fin", Effort: "medium"}}}

	// Recipe values win over config on every seat.
	a := &Automation{Name: "x", Step: &StepConfig{Model: "claude-sonnet-5", Effort: "high",
		Budget: &BudgetConfig{Watch: &WatchConfig{Model: "claude-haiku-4-5", Effort: "low"}}}}
	b := a.ResolveRunBudgetWith(def)
	if b.Model != "claude-sonnet-5" || b.Effort != "high" ||
		b.FinalizerModel != "claude-haiku-4-5" || b.FinalizerEffort != "low" {
		t.Errorf("recipe seats should win: %+v", b)
	}

	// Recipe omits → config fills.
	plain := &Automation{Name: "x"}
	b = plain.ResolveRunBudgetWith(def)
	if b.Model != "cfg-model" || b.Effort != "low" || b.FinalizerModel != "cfg-fin" || b.FinalizerEffort != "medium" {
		t.Errorf("config seats should fill: %+v", b)
	}

	// Nothing anywhere → empty (session defaults downstream).
	b = plain.ResolveRunBudget()
	if b.Model != "" || b.Effort != "" || b.FinalizerModel != "" || b.FinalizerEffort != "" {
		t.Errorf("unset seats must stay empty: %+v", b)
	}

	// Judge seat rides the loop spec (recipe > config).
	loopDef := &LoopConfig{While: &WhileConfig{Model: "cfg-judge", Effort: "low",
		Prompts: &LoopPromptsConfig{Until: "u"}}}
	spec, _ := plain.ResolveWhileWith(loopDef, plain.ResolveRunBudget())
	if spec.JudgeModel != "cfg-judge" || spec.JudgeEffort != "low" {
		t.Errorf("config judge seat: %+v", spec)
	}
	r := &Automation{Name: "x", Loop: &LoopConfig{While: &WhileConfig{Model: "claude-haiku-4-5",
		Prompts: &LoopPromptsConfig{Until: "u"}}}}
	spec, _ = r.ResolveWhileWith(loopDef, r.ResolveRunBudget())
	if spec.JudgeModel != "claude-haiku-4-5" || spec.JudgeEffort != "low" {
		t.Errorf("recipe judge model + config effort: %+v", spec)
	}

	// ValidEffort table.
	for _, ok := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		if !ValidEffort(ok) {
			t.Errorf("ValidEffort(%q) should be true", ok)
		}
	}
	for _, bad := range []string{"turbo", "HIGH2", "maximal"} {
		if ValidEffort(bad) {
			t.Errorf("ValidEffort(%q) should be false", bad)
		}
	}
}

// TestWebReadScheduleParse: the new top-level keys round-trip.
func TestWebReadScheduleParse(t *testing.T) {
	src := "---\nweb_read: screenshot\nschedule: \"0 8 * * *\"\n---\nGo."
	a, err := Parse("r", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if a.WebRead != "screenshot" || a.Schedule != "0 8 * * *" {
		t.Errorf("keys not parsed: web_read=%q schedule=%q", a.WebRead, a.Schedule)
	}
	data, err := Render(a)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse("r", data)
	if err != nil || back.WebRead != "screenshot" || back.Schedule != "0 8 * * *" {
		t.Errorf("round trip lost keys: %+v err=%v", back, err)
	}
}
