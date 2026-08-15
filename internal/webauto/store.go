// SPDX-License-Identifier: Apache-2.0

// Package webauto stores reusable, prompt-based automations ("recipes") for
// the ##auto command family. A recipe is a Markdown file with YAML
// frontmatter (description, args, output_dir, step budget, plus per-kind
// fields) and a prompt body. Four kinds share the same store/budget engine:
//
//	web      — `##auto web run` binds a web pane to the recipe's browser
//	           profile (web_profile, url) and dispatches the prompt to the
//	           pane's Ask Rysh AI, which drives the browser through the
//	           browser_action tool.
//	task     — `##auto task run` dispatches the prompt to the ACTIVE pane's
//	           Ask Rysh AI (no web binding, no browser profile).
//	agent    — `##auto agent run` dispatches the prompt to the named
//	           autonomous agent (`agent:` frontmatter), like @name <prompt>.
//	humanoid — `##auto humanoid run` — same, for humanoids (`humanoid:`).
//	code     — `##auto code run` — like task, anchored to a project
//	           directory (`workdir:` frontmatter, {{workdir}} substituted).
//
// Layout: <workDir>/.rysh/automations/<kind-subdir>/<name>.md — plain files,
// so recipes are editable, diffable, and shareable through the repo. The
// automations directory has one child per ##auto subcommand
// (webs/tasks/agents/humanoids/codes).
package webauto

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Auto-continue budget defaults for recipes (used when the frontmatter omits a
// value). A long browsing task keeps resuming past the per-leg iteration cap
// until it finishes, ~300 iterations, or 20 minutes elapse.
const (
	DefaultMaxIterations = 300
	DefaultMaxDuration   = 20 * time.Minute
	// DefaultStepInterval is the per-leg step cap (checkpoint/auto-resume cadence).
	DefaultStepInterval = 50
	// step.budget.size units, in tokens. A page is 1000 tokens, a book is 200
	// pages, a shelf is 20 books. size takes a count with a p/b/s suffix (bare =
	// pages), e.g. "50p", "3b", "2s". The default is 3 books.
	TokensPerPage     = 1000
	TokensPerBook     = 200 * TokensPerPage // 200,000
	TokensPerShelf    = 20 * TokensPerBook  // 4,000,000
	DefaultBudgetSize = 3 * TokensPerBook   // 600,000 (3 books)
	maxStepInterval   = 500
	// DefaultTakeoverWhen is the consumption threshold at which the takeover leg
	// starts: the task runs against takeover_when% of each ceiling, and once
	// that share is spent the takeover prompt takes over with the remaining
	// (100-takeover_when)% as a fresh sub-budget.
	DefaultTakeoverWhen = 90
	minTakeoverWhen     = 10
	maxTakeoverWhen     = 99
	// Takeover floor: trust the takeover leg to finish. When the reserved share
	// is too thin on ANY axis (below a trigger threshold), every axis is raised
	// to a comfortable floor — even though the combined spend may then exceed
	// the nominal recipe budget. A clipped wrap-up loses the whole run's work;
	// a slightly overspent one saves it.
	takeoverTriggerIterations = 50                     // trigger: fewer than 50 steps
	takeoverTriggerDuration   = time.Minute            // trigger: less than 1 minute
	takeoverTriggerTokens     = 3 * TokensPerBook / 10 // trigger: less than 0.3 book (60k)
	TakeoverFloorIterations   = 100                    // floor: at least 100 steps
	TakeoverFloorDuration     = 5 * time.Minute        // floor: at least 5 minutes
	TakeoverFloorTokens       = TokensPerBook          // floor: at least 1 book (200k)
)

// Built-in model seats for automation loops, applied as the LOWEST tier of
// the model resolution (recipe > config automation.<kind> > config
// automation.loop > these). The INTERNAL loop (do/step legs, the executor
// seat) defaults to Sonnet — high-volume browsing/tool work; the until-JUDGE
// (loop.while seat) defaults to Opus — a single verdict per pass that gates
// the whole run, worth the stronger model. Config loading folds these into
// every automation kind (see config.applyAutomationLLMDefaults).
const (
	DefaultStepModel  = "claude-sonnet-5"
	DefaultJudgeModel = "claude-opus-4-8"
)

// Automation is one saved automation recipe. Profile/URL apply to the web
// kind only; Agent/Humanoid name the target of the agent/humanoid kinds;
// Workdir anchors the code kind to a project directory. Each kind ignores
// the other kinds' fields when they are present.
type Automation struct {
	Name        string `yaml:"-"`
	Description string `yaml:"description,omitempty"`
	Profile     string `yaml:"web_profile,omitempty"`
	URL         string `yaml:"url,omitempty"`
	// Agent is the registry name of the autonomous agent an `##auto agent`
	// recipe drives (the same name @agent-name prompts address).
	Agent string `yaml:"agent,omitempty"`
	// Humanoid is the registry name of the humanoid an `##auto humanoid`
	// recipe drives.
	Humanoid string `yaml:"humanoid,omitempty"`
	// Workdir is the project directory an `##auto code` recipe works in
	// (exposed to the prompt as {{workdir}}; relative values are anchored
	// under the session's work dir, ~ expands to the home directory).
	Workdir string `yaml:"workdir,omitempty"`
	// WebRead selects how a web recipe's AI observes pages: "text" (default —
	// get_text/get_elements DOM extraction) or "screenshot" (the screenshot
	// action; captures are delivered to the model as images it can see).
	// Web kind only; other kinds ignore it.
	WebRead string `yaml:"web_read,omitempty"`
	// OnSuccess names the recipe to run when this one COMPLETES — a loop
	// ending FULFILLED, or a plain run finishing cleanly. Same kind by
	// default; "<kind>:<name>" targets another kind. Cancel/error never
	// chains; chains are capped at depth 8.
	OnSuccess string `yaml:"on_success,omitempty"`
	// Notify, when set, sends the run's outcome summary through a humanoid's
	// channel adapter when the run ends (any cause). See NotifyConfig.
	Notify *NotifyConfig `yaml:"notify,omitempty"`
	// Schedule is an optional cron expression ("0 8 * * *", "@every 2h").
	// When set, the daemon's boot-time sync registers an `auto-<kind>-<name>`
	// ##cron job running the recipe; absent → the recipe is purely on-demand
	// and scheduling machinery never touches it. The explicit off-switch
	// "off" (also "none"/"disabled") REMOVES the recipe's auto job at boot —
	// absence can't do that, since an existing job may be manually managed.
	Schedule string   `yaml:"schedule,omitempty"`
	Args     []string `yaml:"args,omitempty"` // documented arg names, e.g. [query, label]
	// OutputDir is where the recipe's results are saved (exposed to the prompt
	// as {{output_dir}} and read back by `##auto web results`). Empty →
	// <recipe-dir>/<name>/results; a relative value is anchored under the
	// recipe directory; an absolute value is used as-is. See ResolveOutputDir.
	OutputDir string `yaml:"output_dir,omitempty"`
	// Step groups the run-loop / auto-continue budget (see StepConfig). Absent →
	// all defaults. When Loop.Do is also present, Loop.Do wins (Step is the
	// plain single-run spelling of the same block).
	Step *StepConfig `yaml:"step,omitempty"`
	// Loop is the loop-engineering layout: `loop: {do, while}`. `do` is the
	// per-pass budget (same shape as `step`); `while` is the outer semantic
	// loop that repeats the pass until its `until` prompt is judged
	// fulfilled. See LoopConfig.
	Loop *LoopConfig `yaml:"loop,omitempty"`
	// Record turns on run recording (web kind only): a browser screenshot
	// every Interval for the whole run, encoded to one video. See RecordConfig.
	Record *RecordConfig `yaml:"record,omitempty"`
	Prompt string        `yaml:"-"`
}

// NotifyConfig routes a run-completion notification through one of a
// humanoid's channel adapters:
//
//	notify:
//	  humanoid: assistant   # a loaded humanoid (##humanoid spawn ...)
//	  channel: whatsapp     # one of its configured channels
//	  to: "+4915..."        # channel-specific recipient/thread id
type NotifyConfig struct {
	Humanoid string `yaml:"humanoid,omitempty"`
	Channel  string `yaml:"channel,omitempty"`
	To       string `yaml:"to,omitempty"`
}

// ScheduleOff reports whether the recipe explicitly declares "do not
// schedule me": schedule set to off/none/disabled (case-insensitive).
// Distinct from an absent key — absence says nothing about intent (existing
// jobs are left alone); off carries intent, so the boot sync removes the
// recipe's auto-<kind>-<name> job and ##auto <kind> schedule refuses.
func (a *Automation) ScheduleOff() bool {
	switch strings.ToLower(strings.TrimSpace(a.Schedule)) {
	case "off", "none", "disabled":
		return true
	}
	return false
}

// LoopConfig is the top-level `loop` frontmatter block — a declarative
// do-while:
//
//	loop:
//	  do:               # per-pass budget — the same fields as `step`
//	    interval: 30
//	    max_duration: 7m
//	    budget: {size: 3b, watch: {...}}
//	  while:            # outer loop — see WhileConfig
//	    enabled: true
//	    max_iterations: 5
//	    max_duration: 40m
//	    budget: 15b
//	    prompts: {until: ..., iterate_with: ...}
type LoopConfig struct {
	Do    *StepConfig  `yaml:"do,omitempty"`
	While *WhileConfig `yaml:"while,omitempty"`
}

// WhileConfig is the outer semantic loop nested under `loop.while`. Where the
// do-step's iterations are MECHANICAL (tool-use steps toward finishing the
// prompt once), the while-loop's iterations are SEMANTIC: whole passes over
// the goal, repeated until prompts.until is judged fulfilled.
//
// Enabled is the master switch: false runs the do-step exactly ONCE with no
// while constraint applied (no redistribution either); omitted defaults to
// true when the block carries an until prompt. MaxDuration and Budget are
// TOTALS for the whole loop, governed by the per-axis policy in
// ResolveWhileWith (redistribute across passes when they exceed the per-pass
// do value; ignored when they don't). Budget takes p/b/s units
// (ParseBudgetSize).
type WhileConfig struct {
	Enabled       *bool  `yaml:"enabled,omitempty"`
	MaxIterations int    `yaml:"max_iterations,omitempty"`
	MaxDuration   string `yaml:"max_duration,omitempty"`
	Budget        string `yaml:"budget,omitempty"`
	// Model / Effort select the UNTIL-JUDGE's model and effort (seat 3 of 3;
	// empty → session defaults). A cheap fast model usually suffices for the
	// strict YES/NO evaluation.
	Model   string             `yaml:"model,omitempty"`
	Effort  string             `yaml:"effort,omitempty"`
	Prompts *LoopPromptsConfig `yaml:"prompts,omitempty"`
}

// StepConfig is the run-loop budget nested under `step` in a recipe. A long run
// auto-continues past each Interval-sized leg until it finishes or a ceiling is
// hit (MaxIterations total steps, MaxDuration wall-clock, or budget.size total
// tokens), then the budget's takeover prompt (if set) wraps up gracefully.
//
//	step:
//	  interval: 50            # steps per leg (checkpoint/auto-resume cadence)
//	  max_iterations: 300     # total steps ceiling
//	  max_duration: 20m       # wall-clock ceiling (Go duration string)
//	  auto_continue: true     # default true; resume past each leg until a ceiling
//	  auto_approve: true      # default true; run tool calls without the approval dialog
//	  budget: {size, watch: {takeover_when, takeover_prompt}}
type StepConfig struct {
	Interval      int    `yaml:"interval,omitempty"`
	MaxIterations int    `yaml:"max_iterations,omitempty"`
	MaxDuration   string `yaml:"max_duration,omitempty"`
	AutoContinue  *bool  `yaml:"auto_continue,omitempty"`
	AutoApprove   *bool  `yaml:"auto_approve,omitempty"`
	// Model / Effort override the executor's model and effort for this
	// recipe's main legs (seat 1 of 3; empty → session defaults). Effort is
	// low|medium|high|xhigh|max.
	Model  string        `yaml:"model,omitempty"`
	Effort string        `yaml:"effort,omitempty"`
	Budget *BudgetConfig `yaml:"budget,omitempty"`
}

// LoopPromptsConfig holds the outer loop's two prompts (under
// `loop.while.prompts`): Until is the LLM-judged exit condition;
// IterateWith drives passes 2..N (empty falls back to a built-in
// continue-the-task prompt). Both support {{args}}/{{argN}}/{{<name>}}/
// {{output_dir}} substitution.
type LoopPromptsConfig struct {
	Until       string `yaml:"until,omitempty"`
	IterateWith string `yaml:"iterate_with,omitempty"`
}

// BudgetConfig is the budget block nested under `step.budget`: the token
// ceiling (Size) and the Watch block that reacts when the budget runs out.
//
//	budget:
//	  size: 3b              # cumulative token ceiling (p/b/s units; see ParseBudgetSize)
//	  watch:
//	    takeover_when: 90   # takeover starts once this % of every ceiling is consumed
//	    takeover_prompt: >  # graceful wrap-up; runs once when the task share is spent
//	      ...
type BudgetConfig struct {
	Size  string       `yaml:"size,omitempty"`
	Watch *WatchConfig `yaml:"watch,omitempty"`
}

// WatchConfig is the budget watcher nested under `step.budget.watch`: it
// monitors budget consumption and hands the run over to TakeoverPrompt once
// TakeoverWhen% of the budget is consumed.
//
// TakeoverWhen is the consumption threshold (default DefaultTakeoverWhen):
// 90 → the task runs against 90% of each ceiling; when it hits that mark it
// stops and TakeoverPrompt takes over with the remaining 10% as a fresh
// sub-budget. TakeoverPrompt supports {{args}} / {{output_dir}}.
type WatchConfig struct {
	TakeoverWhen   int    `yaml:"takeover_when,omitempty"`
	TakeoverPrompt string `yaml:"takeover_prompt,omitempty"`
	// Model / Effort override the model/effort for the FINALIZER (takeover)
	// leg only (seat 2 of 3) — the mechanical wrap-up can run on a cheaper
	// model. Empty → fall back to the step's Model/Effort, then defaults.
	Model  string       `yaml:"model,omitempty"`
	Effort string       `yaml:"effort,omitempty"`
	Floor  *FloorConfig `yaml:"floor,omitempty"`
}

// FloorConfig tunes the takeover floor, nested under `step.budget.watch.floor`
// (recipe frontmatter or the config-level automation.web.step defaults in
// rysh.config.yaml). The floor fires when the reserved takeover share is below
// ANY Trigger* threshold and raises every axis to at least the floor values.
// Durations are Go duration strings; sizes take p/b/s units (ParseBudgetSize).
// Zero/absent/invalid fields keep the previous layer's value (built-ins:
// trigger <1m / <50 steps / <60p → floor 5m / 100 steps / 1b).
type FloorConfig struct {
	TriggerIterations int    `yaml:"trigger_iterations,omitempty"`
	TriggerDuration   string `yaml:"trigger_duration,omitempty"`
	TriggerSize       string `yaml:"trigger_size,omitempty"`
	Iterations        int    `yaml:"iterations,omitempty"`
	Duration          string `yaml:"duration,omitempty"`
	Size              string `yaml:"size,omitempty"`
}

// EffectiveStep returns the recipe's per-pass step block: `loop.do` wins over
// the plain `step` key when both are present (they are the same shape — step
// is the single-run spelling, loop.do the looping one).
func (a *Automation) EffectiveStep() *StepConfig {
	if a.Loop != nil && a.Loop.Do != nil {
		return a.Loop.Do
	}
	return a.Step
}

// HasBothStepForms reports whether the recipe carries BOTH `step` and
// `loop.do` — legal (loop.do wins) but worth a warning in `show`.
func (a *Automation) HasBothStepForms() bool {
	return a.Loop != nil && a.Loop.Do != nil && a.Step != nil
}

// EffectiveStepDef returns the config-level per-pass defaults with the same
// aliasing rule as the recipe: `automation.<kind>.loop.do` wins over
// `automation.<kind>.step`.
func EffectiveStepDef(step *StepConfig, loop *LoopConfig) *StepConfig {
	if loop != nil && loop.Do != nil {
		return loop.Do
	}
	return step
}

// TakeoverPrompt returns the recipe's budget takeover prompt, or "" when no
// budget.watch.takeover_prompt is configured on the effective step.
func (a *Automation) TakeoverPrompt() string {
	if w := stepWatch(a.EffectiveStep()); w != nil {
		return w.TakeoverPrompt
	}
	return ""
}

// TakeoverPromptWith returns the effective takeover prompt: the recipe's, else
// the config-level default (def = automation.web.step from rysh.config.yaml).
func (a *Automation) TakeoverPromptWith(def *StepConfig) string {
	if p := a.TakeoverPrompt(); p != "" {
		return p
	}
	if w := stepWatch(def); w != nil {
		return w.TakeoverPrompt
	}
	return ""
}

// stepWatch returns the watch block of a step config, or nil at any missing level.
func stepWatch(s *StepConfig) *WatchConfig {
	if s == nil || s.Budget == nil {
		return nil
	}
	return s.Budget.Watch
}

// RunBudget is the effective auto-continue budget for a run.
type RunBudget struct {
	AutoContinue     bool
	AutoApprove      bool          // run tool calls without the approval dialog
	MaxIterations    int           // total steps ceiling → takeover
	MaxDuration      time.Duration // wall-clock ceiling → takeover
	StepInterval     int           // steps per leg (checkpoint/auto-resume cadence)
	MaxContextTokens int           // cumulative token ceiling (budget.size) → takeover
	TakeoverWhen     int           // % of every ceiling consumed before the takeover leg starts
	// Model / Effort are the resolved executor seat (recipe > config; empty →
	// session defaults). FinalizerModel/FinalizerEffort are the finalizer
	// seat; empty falls back to Model/Effort downstream.
	Model           string
	Effort          string
	FinalizerModel  string
	FinalizerEffort string
	// Floor is the resolved takeover-floor policy (recipe > config > built-in),
	// applied by WithTakeoverFloor. Zero value → built-in floor.
	Floor TakeoverFloor
}

// TakeoverFloor is the resolved takeover floor: when the reserved takeover
// share is below ANY Trigger* threshold, every axis is raised to at least
// Iterations/Duration/Tokens.
type TakeoverFloor struct {
	TriggerIterations int
	TriggerDuration   time.Duration
	TriggerTokens     int
	Iterations        int
	Duration          time.Duration
	Tokens            int
}

// defaultTakeoverFloor returns the built-in floor policy.
func defaultTakeoverFloor() TakeoverFloor {
	return TakeoverFloor{
		TriggerIterations: takeoverTriggerIterations,
		TriggerDuration:   takeoverTriggerDuration,
		TriggerTokens:     takeoverTriggerTokens,
		Iterations:        TakeoverFloorIterations,
		Duration:          TakeoverFloorDuration,
		Tokens:            TakeoverFloorTokens,
	}
}

// resolveTakeoverFloor overlays def (config) then recipe onto the built-in
// floor — recipe wins over config, config over built-ins; zero/invalid fields
// keep the previous layer's value.
func resolveTakeoverFloor(def, recipe *FloorConfig) TakeoverFloor {
	f := defaultTakeoverFloor()
	for _, c := range []*FloorConfig{def, recipe} {
		if c == nil {
			continue
		}
		if c.TriggerIterations > 0 {
			f.TriggerIterations = c.TriggerIterations
		}
		if d, err := time.ParseDuration(strings.TrimSpace(c.TriggerDuration)); err == nil && d > 0 {
			f.TriggerDuration = d
		}
		if toks, ok := ParseBudgetSize(c.TriggerSize); ok {
			f.TriggerTokens = toks
		}
		if c.Iterations > 0 {
			f.Iterations = c.Iterations
		}
		if d, err := time.ParseDuration(strings.TrimSpace(c.Duration)); err == nil && d > 0 {
			f.Duration = d
		}
		if toks, ok := ParseBudgetSize(c.Size); ok {
			f.Tokens = toks
		}
	}
	return f
}

// SplitForTakeover partitions the budget into the task share (TakeoverWhen% of
// each ceiling) and the takeover leg's reserved remainder. All three ceilings —
// steps, duration, and cumulative tokens — split proportionally into separate
// fresh allowances that sum back to the full budget (token accounting resets
// when the takeover leg starts, so its share is a clean fresh budget, not
// headroom). StepInterval and AutoContinue are shared unchanged.
func (b RunBudget) SplitForTakeover() (task, takeover RunBudget) {
	p := b.TakeoverWhen
	if p < minTakeoverWhen {
		p = minTakeoverWhen
	}
	if p > maxTakeoverWhen {
		p = maxTakeoverWhen
	}
	task = b
	task.MaxIterations = b.MaxIterations * p / 100
	task.MaxDuration = b.MaxDuration * time.Duration(p) / 100
	task.MaxContextTokens = b.MaxContextTokens * p / 100

	takeover = b
	takeover.MaxIterations = b.MaxIterations - task.MaxIterations
	takeover.MaxDuration = b.MaxDuration - task.MaxDuration
	takeover.MaxContextTokens = b.MaxContextTokens - task.MaxContextTokens
	return task, takeover
}

// WithTakeoverFloor raises a thin takeover share to a comfortable minimum,
// using the budget's resolved Floor policy (recipe > config > built-ins:
// trigger <1m / <50 steps / <0.3 book → floor 5m / 100 steps / 1 book). If the
// share is below ANY trigger threshold, every axis is lifted to at least its
// floor — axes already above their floor are kept as-is, and a share above all
// trigger thresholds is returned unchanged. The wrap-up leg is trusted to
// finish even when that means exceeding the nominal recipe budget: a clipped
// finalizer loses the run's gathered work, which costs more than the overshoot.
func (b RunBudget) WithTakeoverFloor() RunBudget {
	f := b.Floor
	if f == (TakeoverFloor{}) {
		f = defaultTakeoverFloor() // directly-constructed budget (no Resolve pass)
	}
	if b.MaxDuration >= f.TriggerDuration &&
		b.MaxIterations >= f.TriggerIterations &&
		b.MaxContextTokens >= f.TriggerTokens {
		return b
	}
	if b.MaxDuration < f.Duration {
		b.MaxDuration = f.Duration
	}
	if b.MaxIterations < f.Iterations {
		b.MaxIterations = f.Iterations
	}
	if b.MaxContextTokens < f.Tokens {
		b.MaxContextTokens = f.Tokens
	}
	return b
}

// ParseBudgetSize parses a step.budget.size value into a token count. It
// accepts a number with an optional unit suffix (case-insensitive):
//
//	"50p" or "50" → 50 pages   (1 page  = TokensPerPage tokens)
//	"3b"          → 3 books    (1 book  = 200 pages)
//	"2s"          → 2 shelves  (1 shelf = 20 books)
//
// Returns (0, false) for an empty or malformed value (caller applies the default).
func ParseBudgetSize(s string) (int, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0, false
	}
	unit := TokensPerPage
	switch s[len(s)-1] {
	case 'p':
		unit, s = TokensPerPage, s[:len(s)-1]
	case 'b':
		unit, s = TokensPerBook, s[:len(s)-1]
	case 's':
		unit, s = TokensPerShelf, s[:len(s)-1]
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n * unit, true
}

// ResolveRunBudget resolves the recipe budget against built-in defaults only
// (no config layer). Equivalent to ResolveRunBudgetWith(nil).
func (a *Automation) ResolveRunBudget() RunBudget {
	return a.ResolveRunBudgetWith(nil)
}

// ResolveRunBudgetWith returns the effective budget with three-layer,
// per-field precedence: the recipe's `step` frontmatter wins, then def (the
// config-level `automation.web.step` defaults from rysh.config.yaml), then the
// built-ins (auto-continue on, DefaultStepInterval per leg clamped to
// maxStepInterval, DefaultMaxIterations total, DefaultMaxDuration wall-clock,
// DefaultBudgetSize of cumulative tokens, DefaultTakeoverWhen, and the
// built-in takeover floor). Zero/absent/invalid fields fall through to the
// next layer.
func (a *Automation) ResolveRunBudgetWith(def *StepConfig) RunBudget {
	s := a.EffectiveStep()
	if s == nil {
		s = &StepConfig{}
	}
	if def == nil {
		def = &StepConfig{}
	}
	bud := s.Budget
	if bud == nil {
		bud = &BudgetConfig{}
	}
	dbud := def.Budget
	if dbud == nil {
		dbud = &BudgetConfig{}
	}
	watch := bud.Watch
	if watch == nil {
		watch = &WatchConfig{}
	}
	dwatch := dbud.Watch
	if dwatch == nil {
		dwatch = &WatchConfig{}
	}

	b := RunBudget{
		AutoContinue:  resolveBool(s.AutoContinue, def.AutoContinue, true),
		AutoApprove:   resolveBool(s.AutoApprove, def.AutoApprove, true),
		MaxIterations: firstPositive(s.MaxIterations, def.MaxIterations, DefaultMaxIterations),
		StepInterval:  firstPositive(s.Interval, def.Interval, DefaultStepInterval),
	}
	if b.StepInterval > maxStepInterval {
		b.StepInterval = maxStepInterval
	}
	b.MaxDuration = firstDuration(DefaultMaxDuration, s.MaxDuration, def.MaxDuration)
	b.MaxContextTokens = firstSize(DefaultBudgetSize, bud.Size, dbud.Size)
	b.Model = firstNonEmpty(s.Model, def.Model)
	b.Effort = firstNonEmpty(s.Effort, def.Effort)
	b.FinalizerModel = firstNonEmpty(watch.Model, dwatch.Model)
	b.FinalizerEffort = firstNonEmpty(watch.Effort, dwatch.Effort)
	b.TakeoverWhen = firstPositive(watch.TakeoverWhen, dwatch.TakeoverWhen, DefaultTakeoverWhen)
	if b.TakeoverWhen < minTakeoverWhen {
		b.TakeoverWhen = minTakeoverWhen
	}
	if b.TakeoverWhen > maxTakeoverWhen {
		b.TakeoverWhen = maxTakeoverWhen
	}
	b.Floor = resolveTakeoverFloor(dwatch.Floor, watch.Floor)
	return b
}

// firstPositive returns the first value > 0 (callers put the built-in last).
func firstPositive(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// firstNonEmpty returns the first non-blank string (callers put the recipe
// value first, config default second).
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ValidEffort reports whether s is a known effort level (or empty = unset).
func ValidEffort(s string) bool {
	switch strings.TrimSpace(s) {
	case "", "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

// resolveBool returns the recipe's explicit value, else the config default,
// else the built-in.
func resolveBool(recipe, def *bool, builtin bool) bool {
	if recipe != nil {
		return *recipe
	}
	if def != nil {
		return *def
	}
	return builtin
}

// firstDuration returns the first candidate (recipe, then config) that parses
// to a positive Go duration, else the built-in.
func firstDuration(builtin time.Duration, candidates ...string) time.Duration {
	for _, c := range candidates {
		if d, err := time.ParseDuration(strings.TrimSpace(c)); err == nil && d > 0 {
			return d
		}
	}
	return builtin
}

// firstSize returns the first candidate (recipe, then config) that parses as a
// budget size (ParseBudgetSize), else the built-in.
func firstSize(builtin int, candidates ...string) int {
	for _, c := range candidates {
		if toks, ok := ParseBudgetSize(c); ok {
			return toks
		}
	}
	return builtin
}

// Kind selects which automation family a Store manages — one child directory
// per ##auto subcommand under .rysh/automations.
type Kind string

const (
	KindWeb      Kind = "web"
	KindTask     Kind = "task"
	KindAgent    Kind = "agent"
	KindHumanoid Kind = "humanoid"
	KindCode     Kind = "code"
)

// Subdir returns the store child directory for the kind. Unknown kinds fall
// back to webs (the original store) so a zero-value Kind stays compatible.
func (k Kind) Subdir() string {
	switch k {
	case KindTask:
		return "tasks"
	case KindAgent:
		return "agents"
	case KindHumanoid:
		return "humanoids"
	case KindCode:
		return "codes"
	default:
		return "webs"
	}
}

// Store manages the on-disk recipe directory for one automation kind.
type Store struct {
	root string
}

// NewStoreFor anchors the recipe directory for one automation kind under
// workDir (the same anchor the browser-profile store uses):
// .rysh/automations/<kind-subdir>, one child per ##auto subcommand
// (webs/tasks/agents/humanoids/codes). For the web kind, a legacy
// .rysh/web-automations directory is migrated (renamed) in place the first
// time a store is opened on that workDir.
func NewStoreFor(workDir string, kind Kind) *Store {
	root := filepath.Join(workDir, ".rysh", "automations", kind.Subdir())
	if kind.Subdir() == "webs" {
		migrateLegacyDir(filepath.Join(workDir, ".rysh", "web-automations"), root)
	}
	return &Store{root: root}
}

// NewStore opens the web-kind store — the original constructor, kept so
// existing callers/tests are untouched. Equivalent to
// NewStoreFor(workDir, KindWeb).
func NewStore(workDir string) *Store {
	return NewStoreFor(workDir, KindWeb)
}

// migrateLegacyDir renames the pre-rename recipe directory to its new
// location when the old one exists and the new one doesn't. Errors are
// ignored: worst case the store simply starts empty at the new path and the
// legacy folder stays untouched for a manual move.
func migrateLegacyDir(oldDir, newDir string) {
	if _, err := os.Stat(newDir); err == nil {
		return
	}
	if _, err := os.Stat(oldDir); err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(newDir), 0o755); err != nil {
		return
	}
	_ = os.Rename(oldDir, newDir)
}

// Dir returns the recipe directory path.
func (s *Store) Dir() string { return s.root }

// ResolveOutputDir returns the absolute directory where a recipe's results are
// saved. Precedence:
//
//	output_dir empty     → <recipe-dir>/<sanitized-name>/results   (default)
//	output_dir relative  → anchored under the recipe directory
//	output_dir absolute  → used as-is
//
// The result is exposed to the prompt as {{output_dir}} (created before the run)
// and is the folder `##auto web results <name>` lists.
func (s *Store) ResolveOutputDir(a *Automation) string {
	dir := strings.TrimSpace(a.OutputDir)
	if dir == "" {
		name := SanitizeName(a.Name)
		if name == "" {
			name = "unnamed"
		}
		return filepath.Join(s.root, name, "results")
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	return filepath.Join(s.root, filepath.Clean(dir))
}

// SanitizeName hardens a recipe name to a single safe path segment (mirrors
// browserinstance.SanitizeProfile). Returns "" when nothing safe remains.
func SanitizeName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, string(os.PathSeparator), "-")
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "..", "")
	name = strings.Trim(name, ". ")
	return name
}

// Save writes (or overwrites) a recipe file.
func (s *Store) Save(a *Automation) error {
	name := SanitizeName(a.Name)
	if name == "" {
		return fmt.Errorf("invalid automation name %q", a.Name)
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return fmt.Errorf("automation %q has an empty prompt", name)
	}
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return err
	}
	data, err := Render(a)
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.root, "."+name+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.root, name+".md"))
}

// Load reads one recipe by name.
func (s *Store) Load(name string) (*Automation, error) {
	name = SanitizeName(name)
	if name == "" {
		return nil, fmt.Errorf("invalid automation name")
	}
	data, err := os.ReadFile(filepath.Join(s.root, name+".md"))
	if err != nil {
		return nil, err
	}
	return Parse(name, data)
}

// List returns all recipes sorted by name. Unparseable files are skipped.
func (s *Store) List() []*Automation {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil
	}
	var out []*Automation
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		if a, err := s.Load(name); err == nil {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Delete removes a recipe file.
func (s *Store) Delete(name string) error {
	name = SanitizeName(name)
	if name == "" {
		return fmt.Errorf("invalid automation name")
	}
	return os.Remove(filepath.Join(s.root, name+".md"))
}

// Parse decodes a recipe file: optional `---`-delimited YAML frontmatter
// followed by the prompt body. A file with no frontmatter is all prompt.
func Parse(name string, data []byte) (*Automation, error) {
	a := &Automation{Name: name}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if strings.HasPrefix(text, "---\n") {
		rest := text[4:]
		if idx := strings.Index(rest, "\n---"); idx >= 0 {
			front := rest[:idx]
			body := rest[idx+4:]
			if err := yaml.Unmarshal([]byte(front), a); err != nil {
				return nil, fmt.Errorf("automation %q: invalid frontmatter: %w", name, err)
			}
			a.Prompt = strings.TrimSpace(body)
		}
	}
	if a.Prompt == "" {
		a.Prompt = strings.TrimSpace(text)
	}
	if a.Prompt == "" {
		return nil, fmt.Errorf("automation %q has an empty prompt", name)
	}
	return a, nil
}

// Render encodes a recipe as frontmatter + prompt body.
func Render(a *Automation) ([]byte, error) {
	front, err := yaml.Marshal(a)
	if err != nil {
		return nil, err
	}
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.Write(front)
	sb.WriteString("---\n\n")
	sb.WriteString(strings.TrimSpace(a.Prompt))
	sb.WriteString("\n")
	return []byte(sb.String()), nil
}

// SubstituteArgs expands placeholders in the prompt:
//
//	{{args}}          → all runtime args joined with spaces
//	{{arg1}}..{{argN}} → positional args (missing → "")
//	{{<name>}}        → named per Automation.Args ordering (args[i] ↔ Args[i])
func SubstituteArgs(a *Automation, runtimeArgs []string) string {
	prompt := a.Prompt
	prompt = strings.ReplaceAll(prompt, "{{args}}", strings.Join(runtimeArgs, " "))
	// Positional placeholders — support a few beyond what was supplied so
	// unfilled ones vanish instead of leaking template syntax.
	max := len(runtimeArgs)
	if max < 9 {
		max = 9
	}
	for i := 1; i <= max; i++ {
		val := ""
		if i-1 < len(runtimeArgs) {
			val = runtimeArgs[i-1]
		}
		prompt = strings.ReplaceAll(prompt, "{{arg"+strconv.Itoa(i)+"}}", val)
	}
	// Named placeholders from the declared arg names.
	for i, argName := range a.Args {
		val := ""
		if i < len(runtimeArgs) {
			val = runtimeArgs[i]
		}
		prompt = strings.ReplaceAll(prompt, "{{"+argName+"}}", val)
	}
	return prompt
}

// ---------------------------------------------------------------------------
// Loop engineering — the resolved outer loop (loop.while)
// ---------------------------------------------------------------------------

// DefaultLoopIterations is the outer-pass cap applied when a while block
// omits max_iterations. It is also the redistribution divisor.
const DefaultLoopIterations = 5

// DefaultLoopIteratePrompt drives passes 2..N when prompts.iterate_with is
// omitted. The pass is additionally seeded with the latest saved result (the
// same mechanism `##auto <kind> resume` uses), so "continue the same task"
// has the concrete prior state in front of it.
const DefaultLoopIteratePrompt = "Continue the SAME task as before: build on the previous results, " +
	"add only NEW work, avoid duplicates, and save the updated results to the same location."

// LoopSpec is the resolved while-loop for a run. Zero value = no loop.
type LoopSpec struct {
	// Enabled is true when a while block is configured, not switched off
	// (`enabled: false`), and has an until prompt (without an exit condition
	// there is nothing to loop for).
	Enabled bool
	// MaxIterations caps the outer passes while Until stays unfulfilled, and
	// divides the while totals during redistribution.
	MaxIterations int
	// MaxDuration is the while-loop's wall-clock TOTAL, kept as a hard outer
	// deadline. Non-zero only when the redistribute branch fired for the
	// duration axis (while.max_duration > per-pass do.max_duration); 0 means
	// no outer time constraint (axis omitted, unparseable, or disabled by the
	// policy).
	MaxDuration time.Duration
	// MaxTokens is the while-loop's token TOTAL, mirrored for reporting.
	// Non-zero only when the redistribute branch fired for the token axis;
	// its enforcement is the even division itself — each pass is armed with
	// MaxTokens/MaxIterations, so N passes spend ≈ the total (a takeover
	// floor may raise a wrap-up leg past its share; a clipped wrap-up loses
	// the pass's work, which costs more than the overshoot).
	MaxTokens int
	// Until is the LLM-judged exit condition, evaluated against the latest
	// saved results after each pass.
	Until string
	// IterateWith is the prompt for passes 2..N (never empty when Enabled —
	// falls back to DefaultLoopIteratePrompt).
	IterateWith string
	// JudgeModel / JudgeEffort are the resolved judge seat (while.model /
	// while.effort, recipe > config; empty → session defaults).
	JudgeModel  string
	JudgeEffort string
}

// ResolveWhileWith resolves the recipe's while-loop against the
// ALREADY-RESOLVED per-pass budget (ResolveRunBudgetWith output) and applies
// the per-axis budget policy, returning the loop spec and the possibly
// REDISTRIBUTED per-pass budget. defLoop is the config-level
// automation.<kind>.loop defaults; per-field precedence is recipe > config >
// built-ins, as everywhere else.
//
// The policy, per axis with a do-counterpart in the same unit
// (while.max_duration ↔ do.max_duration, while.budget ↔ do.budget.size):
//
//	redistribute — while.X > do.X: the while TOTAL is authoritative and is
//	               split evenly across passes: do.X := while.X / max_iterations
//	               (replacing do's own value); the total is kept on the spec
//	               as the outer ceiling.
//	disable      — while.X <= do.X (or omitted/unparseable): the axis cannot
//	               cover even one pass, so it is ignored — do keeps its own
//	               value and there is no outer constraint on that axis.
//
// `enabled: false` short-circuits everything: the do-step runs once with its
// own literal budget — no judge, no iterate, no redistribution. Omitted
// `enabled` defaults to true when the merged block carries an until prompt.
func (a *Automation) ResolveWhileWith(defLoop *LoopConfig, base RunBudget) (LoopSpec, RunBudget) {
	return a.ResolveWhileWithFlags(defLoop, base, nil)
}

// ResolveWhileWithFlags is ResolveWhileWith with a third, highest-precedence
// override tier: run flags (--passes / --while-duration / --while-budget /
// --no-loop), expressed as a synthetic WhileConfig merged last.
func (a *Automation) ResolveWhileWithFlags(defLoop *LoopConfig, base RunBudget, flags *WhileConfig) (LoopSpec, RunBudget) {
	var recipe, fallback *WhileConfig
	if a.Loop != nil {
		recipe = a.Loop.While
	}
	if defLoop != nil {
		fallback = defLoop.While
	}
	if recipe == nil && fallback == nil && flags == nil {
		return LoopSpec{}, base
	}

	// Per-field merge — config, then recipe, then flags, so flags win.
	var enabled *bool
	spec := LoopSpec{MaxIterations: DefaultLoopIterations}
	var durStr, budStr string
	for _, c := range []*WhileConfig{fallback, recipe, flags} {
		if c == nil {
			continue
		}
		if c.Enabled != nil {
			enabled = c.Enabled
		}
		if c.MaxIterations > 0 {
			spec.MaxIterations = c.MaxIterations
		}
		if strings.TrimSpace(c.MaxDuration) != "" {
			durStr = c.MaxDuration
		}
		if strings.TrimSpace(c.Budget) != "" {
			budStr = c.Budget
		}
		if strings.TrimSpace(c.Model) != "" {
			spec.JudgeModel = strings.TrimSpace(c.Model)
		}
		if strings.TrimSpace(c.Effort) != "" {
			spec.JudgeEffort = strings.TrimSpace(c.Effort)
		}
		if c.Prompts != nil {
			if strings.TrimSpace(c.Prompts.Until) != "" {
				spec.Until = strings.TrimSpace(c.Prompts.Until)
			}
			if strings.TrimSpace(c.Prompts.IterateWith) != "" {
				spec.IterateWith = strings.TrimSpace(c.Prompts.IterateWith)
			}
		}
	}

	// Master switch and exit-condition gate: explicit `enabled: false` (or a
	// block with no until prompt) means a single plain run on do's literal
	// budget — the zero LoopSpec, base untouched.
	if (enabled != nil && !*enabled) || spec.Until == "" {
		return LoopSpec{}, base
	}
	spec.Enabled = true
	if spec.IterateWith == "" {
		spec.IterateWith = DefaultLoopIteratePrompt
	}

	// Duration axis: redistribute or disable (strictly-bigger fires; equal or
	// smaller totals cannot cover a multi-pass loop and are ignored).
	if d, err := time.ParseDuration(strings.TrimSpace(durStr)); err == nil && d > base.MaxDuration {
		spec.MaxDuration = d
		base.MaxDuration = d / time.Duration(spec.MaxIterations)
	}
	// Token axis: same policy (integer division floors).
	if toks, ok := ParseBudgetSize(budStr); ok && toks > base.MaxContextTokens {
		spec.MaxTokens = toks
		base.MaxContextTokens = toks / spec.MaxIterations
	}
	return spec, base
}
