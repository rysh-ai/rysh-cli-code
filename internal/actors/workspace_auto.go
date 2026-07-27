package actors

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/webauto"

	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// ---------------------------------------------------------------------------
// ##auto task / ##auto agent / ##auto humanoid — the shared automation engine
//
// The three non-web automation kinds ride the same recipe store, run-flag
// parser, budget machinery (MsgSetRunBudget / MsgAgenticContinue), and
// results plumbing as ##auto web, differing only in where the prompt goes:
//
//	task     — the ACTIVE pane's Ask Rysh AI (no web binding, no browser
//	           profile, no --headless).
//	agent    — a named autonomous agent (##agent spawn): the budget is armed
//	           on the agent's own LLM inbox and the prompt takes the same
//	           path as `@agent-name <prompt>`.
//	humanoid — same as agent, for humanoids.
//	code     — like task, anchored to a project directory: `workdir:`
//	           frontmatter (or --workdir) resolves to an absolute path,
//	           substitutes {{workdir}}, and prepends a work-in-this-directory
//	           header to the prompt.
//
// One autoKindSpec per kind parameterizes the shared handlers below; web
// keeps its original handlers in workspace_webauto.go (its behaviour is
// frozen) but shares all of the underlying machinery.
// ---------------------------------------------------------------------------

// autoKindSpec parameterizes the shared ##auto engine for one automation kind.
type autoKindSpec struct {
	label string       // subcommand + output prefix: task | agent | humanoid
	kind  webauto.Kind // recipe store child (tasks/agents/humanoids)
	// stepDef / loopDef return the kind's config-level defaults
	// (automation.<kind>.step / automation.<kind>.loop in rysh.config.yaml).
	stepDef func(w *WorkspaceActor) *webauto.StepConfig
	loopDef func(w *WorkspaceActor) *webauto.LoopConfig
	// recipeTarget reads the recipe's target name (agent/humanoid frontmatter);
	// nil marks an untargeted kind (task) that runs on the active pane.
	recipeTarget func(a *webauto.Automation) string
	// validate checks the target exists in its registry, printing a friendly
	// error (with the available names) when it doesn't. Only for targeted kinds.
	validate func(w *WorkspaceActor, out *strings.Builder, target string) bool
	// dispatch sends the substituted prompt on its way (pane submit for task,
	// registry prompt for agent/humanoid).
	dispatch func(w *WorkspaceActor, paneID, target, prompt string)
	// progressHint is the footer line printed after a run is dispatched.
	progressHint string
	// workdirAware kinds (code) accept a `workdir:` frontmatter key and a
	// --workdir run flag anchoring the task to a project directory.
	workdirAware bool
}

// targeted reports whether the kind drives a named registry entity
// (agent/humanoid) rather than the active pane.
func (s autoKindSpec) targeted() bool { return s.recipeTarget != nil }

// targetFlag is the per-run override flag for targeted kinds ("--agent" /
// "--humanoid"), or "" for untargeted kinds.
func (s autoKindSpec) targetFlag() string {
	if !s.targeted() {
		return ""
	}
	return "--" + s.label
}

// flagName is the kind's captured string run-flag for the shared parser:
// the target override for targeted kinds, --workdir for workdir-aware kinds,
// "" otherwise.
func (s autoKindSpec) flagName() string {
	switch {
	case s.targeted():
		return s.targetFlag()
	case s.workdirAware:
		return "--workdir"
	}
	return ""
}

// taskAutoSpec parameterizes ##auto task: plain prompt automations on the
// ACTIVE pane — no web dependency (never binds web mode, never touches
// browser profiles) and no agent dependency.
func taskAutoSpec() autoKindSpec {
	return autoKindSpec{
		label:        "task",
		kind:         webauto.KindTask,
		stepDef:      func(w *WorkspaceActor) *webauto.StepConfig { return w.cfg.Automation.Task.Step },
		loopDef:      func(w *WorkspaceActor) *webauto.LoopConfig { return w.cfg.Automation.Task.Loop },
		dispatch:     dispatchPanePrompt,
		progressHint: "progress streams to this pane's Ask Rysh chat; double-Ctrl+C pauses",
	}
}

// codeAutoSpec parameterizes ##auto code: coding automations on the ACTIVE
// pane, anchored to a project directory. Like task (no web binding, no
// registry target), plus the optional `workdir:` frontmatter / --workdir
// run flag: the resolved absolute path substitutes {{workdir}} and a
// work-in-this-directory header is prepended to the prompt.
func codeAutoSpec() autoKindSpec {
	return autoKindSpec{
		label:        "code",
		kind:         webauto.KindCode,
		workdirAware: true,
		stepDef:      func(w *WorkspaceActor) *webauto.StepConfig { return w.cfg.Automation.Code.Step },
		loopDef:      func(w *WorkspaceActor) *webauto.LoopConfig { return w.cfg.Automation.Code.Loop },
		dispatch:     dispatchPanePrompt,
		progressHint: "progress streams to this pane's Ask Rysh chat; double-Ctrl+C pauses",
	}
}

// dispatchPanePrompt sends a substituted recipe prompt to the ACTIVE pane's
// Ask Rysh AI — the shared dispatch of the task and code kinds: a human
// bubble in the Ask Rysh panel, then the prompt itself (the same dispatch
// cmdWebAutoRun uses, minus the web-mode binding).
func dispatchPanePrompt(w *WorkspaceActor, paneID, _, prompt string) {
	_ = w.pub.Send(msg.T("pane", paneID, "web", "prompt"),
		&msg.MsgWebPromptDispatched{PaneID: paneID, Prompt: prompt})
	_ = w.pub.Send(msg.T("pane", paneID, "inbox"),
		&msg.MsgPaneSubmitInput{Text: prompt, Mode: "prompt"})
}

// resolveCodeWorkdir resolves the effective project directory for a
// workdir-aware run: the --workdir override wins over the recipe's
// `workdir:`; "~" expands to the home directory; a relative path is anchored
// under the session's work dir (the same anchor the recipe store uses).
// Returns "" when neither source names a directory.
func (w *WorkspaceActor) resolveCodeWorkdir(override string, a *webauto.Automation) string {
	dir := strings.TrimSpace(override)
	if dir == "" {
		dir = strings.TrimSpace(a.Workdir)
	}
	if dir == "" {
		return ""
	}
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(dir, "~"), "/"))
		}
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(w.browserWorkDir(), dir)
	}
	return filepath.Clean(dir)
}

// decorateCodePrompt applies the code kind's workdir anchoring to a
// substituted prompt: {{workdir}} placeholders become the resolved path, and
// a work-in-this-directory header is prepended when a workdir is set. Pure,
// so tests can exercise it directly.
func decorateCodePrompt(prompt, workdir string) string {
	prompt = strings.ReplaceAll(prompt, "{{workdir}}", workdir)
	if workdir == "" {
		return prompt
	}
	return "[Code automation] Project directory: " + workdir +
		" — run every shell command and file operation inside this directory.\n\n" + prompt
}

// agentAutoSpec parameterizes ##auto agent: recipes that drive a named
// autonomous agent (one spawned via ##agent spawn). The budget is armed on
// the agent's own LLM inbox (rysh.pane.{name}.llm_prompt_execution.inbox)
// and the prompt takes the same path as `@agent-name <prompt>`; output
// appears wherever the agent's output is registered (##agent register-output).
func agentAutoSpec() autoKindSpec {
	return autoKindSpec{
		label:        "agent",
		kind:         webauto.KindAgent,
		stepDef:      func(w *WorkspaceActor) *webauto.StepConfig { return w.cfg.Automation.Agent.Step },
		loopDef:      func(w *WorkspaceActor) *webauto.LoopConfig { return w.cfg.Automation.Agent.Loop },
		recipeTarget: func(a *webauto.Automation) string { return a.Agent },
		validate: func(w *WorkspaceActor, out *strings.Builder, target string) bool {
			if w.agentRegistryPID == nil {
				fmt.Fprintf(out, "\n[agent] agent registry not available (agentic mode disabled?)\n")
				return false
			}
			agents, err := w.queryAgents()
			if err != nil {
				fmt.Fprintf(out, "\n[agent] failed to list agents: %v\n", err)
				return false
			}
			names := make([]string, 0, len(agents))
			for _, a := range agents {
				if a.Name == target {
					return true
				}
				names = append(names, a.Name)
			}
			out.WriteString(unknownAutoTargetMsg("agent", target, names, "##agent spawn <name>"))
			return false
		},
		dispatch: func(w *WorkspaceActor, paneID, target, prompt string) {
			// Same path @agent-name prompts take: the registry re-points the
			// agent at the invoking pane's scope and forwards to its LLM actor.
			w.actorSystem.Root.Send(w.agentRegistryPID, &msg.MsgAgentPrompt{
				AgentName:    target,
				Prompt:       prompt,
				SourcePaneID: paneID,
				ScopeHint:    w.resolveScopeIDs(paneID).Hint(),
			})
		},
		progressHint: "output goes to the agent's registered output panes (##agent register-output); @@<agent> stop pauses",
	}
}

// humanoidAutoSpec parameterizes ##auto humanoid — same as agent, for
// humanoids (`humanoid:` frontmatter, humanoid registry validation, the
// `@humanoid-name <prompt>` dispatch path).
func humanoidAutoSpec() autoKindSpec {
	return autoKindSpec{
		label:        "humanoid",
		kind:         webauto.KindHumanoid,
		stepDef:      func(w *WorkspaceActor) *webauto.StepConfig { return w.cfg.Automation.Humanoid.Step },
		loopDef:      func(w *WorkspaceActor) *webauto.LoopConfig { return w.cfg.Automation.Humanoid.Loop },
		recipeTarget: func(a *webauto.Automation) string { return a.Humanoid },
		validate: func(w *WorkspaceActor, out *strings.Builder, target string) bool {
			if w.humanoidRegistryPID == nil {
				fmt.Fprintf(out, "\n[humanoid] humanoid registry not available (agentic mode disabled?)\n")
				return false
			}
			humanoids, err := w.queryHumanoids()
			if err != nil {
				fmt.Fprintf(out, "\n[humanoid] failed to list humanoids: %v\n", err)
				return false
			}
			names := make([]string, 0, len(humanoids))
			for _, h := range humanoids {
				if h.Name == target {
					return true
				}
				names = append(names, h.Name)
			}
			out.WriteString(unknownAutoTargetMsg("humanoid", target, names, "##humanoid spawn <name>"))
			return false
		},
		dispatch: func(w *WorkspaceActor, paneID, target, prompt string) {
			w.actorSystem.Root.Send(w.humanoidRegistryPID, &msg.MsgHumanoidPrompt{
				HumanoidName: target,
				Prompt:       prompt,
				SourcePaneID: paneID,
				ScopeHint:    w.resolveScopeIDs(paneID).Hint(),
			})
		},
		progressHint: "output goes to the humanoid's registered output panes (##humanoid register-output); @@<humanoid> stop pauses",
	}
}

// unknownAutoTargetMsg renders the friendly unknown-target error for the
// targeted kinds, listing the names that ARE loaded. Pure (no registry/actor
// dependency) so tests can exercise it directly.
func unknownAutoTargetMsg(label, target string, available []string, spawnHint string) string {
	sort.Strings(available)
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n[%s] %s %q not found\n", label, label, target)
	if len(available) == 0 {
		fmt.Fprintf(&sb, "[%s] no %ss are loaded — spawn one with %s\n", label, label, spawnHint)
	} else {
		fmt.Fprintf(&sb, "[%s] available %ss: %s\n", label, label, strings.Join(available, ", "))
	}
	return sb.String()
}

// handleAutoKind processes the ##auto <kind> subcommands for the non-web
// kinds — the same subcommand set as ##auto web (run/resume/continue/list/
// show/save/delete/results), parameterized by spec.
func (w *WorkspaceActor) handleAutoKind(out *strings.Builder, paneID string, spec autoKindSpec, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list":
		w.cmdAutoList(out, spec)
	case "show":
		if len(args) < 2 {
			fmt.Fprintf(out, "\n[%s] usage: ##auto %s show <name>\n", spec.label, spec.label)
			return
		}
		w.cmdAutoShow(out, spec, args[1])
	case "save":
		if len(args) < 3 {
			fmt.Fprintf(out, "\n[%s] usage: ##auto %s save <name> <prompt...>\n", spec.label, spec.label)
			return
		}
		w.cmdAutoSave(out, spec, args[1], strings.Join(args[2:], " "))
	case "run":
		name, runArgs, _, target, ov := parseAutoRunFlags(args[1:], spec.flagName(), false)
		if name == "" {
			w.autoKindRunUsage(out, spec, "run")
			return
		}
		w.cmdAutoRun(out, paneID, spec, name, runArgs, target, ov, "")
	case "resume":
		name, runArgs, _, target, ov := parseAutoRunFlags(args[1:], spec.flagName(), false)
		if name == "" {
			fmt.Fprintf(out, "\n[%s] usage: ##auto %s resume [flags] <name> [args...]  (fresh budget + loads the latest result into context)\n", spec.label, spec.label)
			return
		}
		w.cmdAutoResume(out, paneID, spec, name, runArgs, target, ov)
	case "continue":
		name, runArgs, _, target, ov := parseAutoRunFlags(args[1:], spec.flagName(), false)
		if name == "" {
			fmt.Fprintf(out, "\n[%s] usage: ##auto %s continue [flags] <name> [args...]  (resume a cancelled/stopped run from its last checkpoint)\n", spec.label, spec.label)
			return
		}
		w.cmdAutoContinue(out, paneID, spec, name, runArgs, target, ov)
	case "check":
		if len(args) < 2 {
			fmt.Fprintf(out, "\n[%s] usage: ##auto %s check <name>\n", spec.label, spec.label)
			return
		}
		w.cmdAutoCheck(out, spec, args[1])
	case "status":
		w.cmdAutoLoopStatus(out, spec.label)
	case "runs":
		w.cmdAutoRunsList(out, spec.label)
	case "stop":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		w.cmdAutoLoopStop(out, spec, name, paneID)
	case "schedule":
		if len(args) < 2 {
			fmt.Fprintf(out, "\n[%s] usage: ##auto %s schedule <name> [args...]  (uses the recipe's schedule: key)\n", spec.label, spec.label)
			return
		}
		w.cmdAutoSchedule(out, spec, args[1], args[2:])
	case "unschedule":
		if len(args) < 2 {
			fmt.Fprintf(out, "\n[%s] usage: ##auto %s unschedule <name>\n", spec.label, spec.label)
			return
		}
		w.cmdAutoUnschedule(out, spec, args[1])
	case "results":
		if len(args) < 2 {
			fmt.Fprintf(out, "\n[%s] usage: ##auto %s results <name> [file]\n", spec.label, spec.label)
			return
		}
		file := ""
		if len(args) > 2 {
			file = args[2]
		}
		renderAutoResults(out, spec.label, w.autoStore(spec.kind), args[1], file)
	case "delete":
		if len(args) < 2 {
			fmt.Fprintf(out, "\n[%s] usage: ##auto %s delete <name>\n", spec.label, spec.label)
			return
		}
		if err := w.autoStore(spec.kind).Delete(args[1]); err != nil {
			fmt.Fprintf(out, "\n[%s] delete failed: %v\n", spec.label, err)
			return
		}
		fmt.Fprintf(out, "\n[%s] deleted automation %q\n", spec.label, args[1])
	default:
		w.autoKindUsage(out, spec)
	}
}

// autoKindRunUsage prints the one-line run/resume/continue flag usage.
func (w *WorkspaceActor) autoKindRunUsage(out *strings.Builder, spec autoKindSpec, verb string) {
	targetPart := ""
	switch {
	case spec.targeted():
		targetPart = fmt.Sprintf("[%s <name>] ", spec.targetFlag())
	case spec.workdirAware:
		targetPart = "[--workdir <dir>] "
	}
	fmt.Fprintf(out, "\n[%s] usage: ##auto %s %s %s[--step-interval N] [--max-iterations N] [--max-duration D] [--budget-size Np|Nb|Ns] [--takeover-when P] <name> [args...]\n",
		spec.label, spec.label, verb, targetPart)
}

// autoKindUsage prints the full ##auto <kind> usage, mirroring autoWebUsage.
func (w *WorkspaceActor) autoKindUsage(out *strings.Builder, spec autoKindSpec) {
	l := spec.label
	fmt.Fprintf(out, "\n[rysh] usage:\n")
	fmt.Fprintf(out, "  ##auto %s list                      list saved %s automations\n", l, l)
	fmt.Fprintf(out, "  ##auto %s show <name>               show a recipe\n", l)
	fmt.Fprintf(out, "  ##auto %s save <name> <prompt...>   save a prompt as a recipe\n", l)
	switch {
	case spec.targeted():
		fmt.Fprintf(out, "  ##auto %s run [%s <name>] [--step-interval N] [--max-iterations N] [--max-duration D] [--budget-size Np|Nb|Ns] [--takeover-when P] <name> [args...]\n", l, spec.targetFlag())
	case spec.workdirAware:
		fmt.Fprintf(out, "  ##auto %s run [--workdir <dir>] [--step-interval N] [--max-iterations N] [--max-duration D] [--budget-size Np|Nb|Ns] [--takeover-when P] <name> [args...]\n", l)
	default:
		fmt.Fprintf(out, "  ##auto %s run [--step-interval N] [--max-iterations N] [--max-duration D] [--budget-size Np|Nb|Ns] [--takeover-when P] <name> [args...]\n", l)
	}
	fmt.Fprintf(out, "        run a recipe ({{args}}/{{arg1}}/{{output_dir}} substituted); flags override the recipe budget\n")
	fmt.Fprintf(out, "  ##auto %s resume [flags] <name> [args...]    fresh budget + load the latest result into context, then rerun\n", l)
	fmt.Fprintf(out, "  ##auto %s continue [flags] <name> [args...]  resume a cancelled/stopped run from its last checkpoint (budget re-armed)\n", l)
	fmt.Fprintf(out, "  ##auto %s results <name> [file]     list the recipe's saved results (or print one file)\n", l)
	fmt.Fprintf(out, "  ##auto %s check <name>              lint a recipe (placeholders, loop, targets, schedule)\n", l)
	fmt.Fprintf(out, "  ##auto %s status | stop [name]      inspect / gracefully stop this kind's active loops\n", l)
	fmt.Fprintf(out, "  ##auto %s runs [list]               list runs still executing (time consumed, loop pass, tokens consumed)\n", l)
	fmt.Fprintf(out, "  ##auto %s schedule|unschedule <name> [args...]  register the recipe's schedule: key as a ##cron job\n", l)
	fmt.Fprintf(out, "  ##auto %s delete <name>             delete a recipe\n", l)
	fmt.Fprintf(out, "  --dry-run on run/resume prints the resolved plan (prompt, budget, loop) without dispatching\n")
	fmt.Fprintf(out, "  loop flags on run/resume: --no-loop (run once) --passes N --while-duration D --while-budget Np|Nb|Ns (flags > recipe > config)\n")
	fmt.Fprintf(out, "  --each \"a,b,c\" fans the recipe out over the items, one sequential run each ({{args}} = the item)\n")
	fmt.Fprintf(out, "  on_success: [<kind>:]<name> chains the next recipe when a run completes; notify: {humanoid, channel, to} pings a channel on run end\n")
	fmt.Fprintf(out, "  model/effort seats: do.model/effort (executor), do.budget.watch.model/effort (finalizer leg), while.model/effort (judge)\n")
	fmt.Fprintf(out, "  recipes live in .rysh/automations/%s/<name>.md (top-level: description, args, output_dir;\n", spec.kind.Subdir())
	fmt.Fprintf(out, "        step: {interval, max_iterations, max_duration, auto_continue, auto_approve, budget: {size, watch: {takeover_when, takeover_prompt}}})\n")
	if spec.targeted() {
		fmt.Fprintf(out, "  `%s: <name>` frontmatter names the target %s (required unless %s <name> is passed at run time)\n", l, l, spec.targetFlag())
	}
	if spec.workdirAware {
		fmt.Fprintf(out, "  `workdir: <dir>` frontmatter anchors the task to a project directory ({{workdir}} substituted, work-in-dir header prepended; --workdir <dir> overrides)\n")
	}
	fmt.Fprintf(out, "  loop: {do, while} is the loop-engineering layout: `do` = the per-pass budget (same fields as `step`; loop.do wins over step),\n")
	fmt.Fprintf(out, "        `while` {enabled, max_iterations, max_duration, budget, prompts: {until, iterate_with}} repeats the pass until an LLM judge\n")
	fmt.Fprintf(out, "        deems `until` fulfilled (default %d passes). while.max_duration/budget are TOTALS: bigger than the per-pass value → split\n", webauto.DefaultLoopIterations)
	fmt.Fprintf(out, "        evenly across passes (do.X = while.X / max_iterations); smaller → ignored. enabled:false runs the pass once, loop off\n")
	fmt.Fprintf(out, "  config-level defaults: automation.%s.step in rysh.config.yaml (same shape as the recipe step block);\n", l)
	fmt.Fprintf(out, "        precedence: run flags > recipe > config > built-ins\n")
	fmt.Fprintf(out, "  results save to output_dir (default .rysh/automations/%s/<name>/results)\n\n", spec.kind.Subdir())
}

// cmdAutoList lists the kind's saved recipes (mirrors cmdWebAutoList; the
// bracket column shows the target for agent/humanoid kinds).
func (w *WorkspaceActor) cmdAutoList(out *strings.Builder, spec autoKindSpec) {
	store := w.autoStore(spec.kind)
	autos := store.List()
	if len(autos) == 0 {
		fmt.Fprintf(out, "\n[%s] no automations saved yet — ##auto %s save <name> <prompt...>\n", spec.label, spec.label)
		return
	}
	fmt.Fprintf(out, "\n[%s] automations (%s)\n", spec.label, store.Dir())
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	for _, a := range autos {
		desc := a.Description
		if desc == "" {
			desc = truncateStr(strings.ReplaceAll(a.Prompt, "\n", " "), 50)
		}
		switch {
		case spec.targeted():
			fmt.Fprintf(out, "  %-20s [%s] %s\n", a.Name, orDefault(spec.recipeTarget(a), "no "+spec.label), desc)
		case spec.workdirAware && strings.TrimSpace(a.Workdir) != "":
			fmt.Fprintf(out, "  %-20s [%s] %s\n", a.Name, strings.TrimSpace(a.Workdir), desc)
		default:
			fmt.Fprintf(out, "  %-20s %s\n", a.Name, desc)
		}
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
}

// cmdAutoShow prints one recipe with its resolved budget and paths (mirrors
// cmdWebAutoShow, with the kind's target line instead of profile/url).
func (w *WorkspaceActor) cmdAutoShow(out *strings.Builder, spec autoKindSpec, name string) {
	store := w.autoStore(spec.kind)
	a, err := store.Load(name)
	if err != nil {
		fmt.Fprintf(out, "\n[%s] automation %q not found\n", spec.label, name)
		return
	}
	fmt.Fprintf(out, "\n[%s] automation %q\n", spec.label, a.Name)
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	fmt.Fprintf(out, "  description : %s\n", a.Description)
	if spec.targeted() {
		fmt.Fprintf(out, "  %-11s : %s\n", spec.label,
			orDefault(spec.recipeTarget(a), "(none — set `"+spec.label+":` or pass "+spec.targetFlag()+" at run time)"))
	}
	if spec.workdirAware {
		fmt.Fprintf(out, "  workdir     : %s\n",
			orDefault(w.resolveCodeWorkdir("", a), "(none — set `workdir:` or pass --workdir at run time)"))
	}
	if a.ScheduleOff() {
		fmt.Fprintf(out, "  schedule    : %s (disabled — no cron job registered)\n", a.Schedule)
	} else if a.Schedule != "" {
		fmt.Fprintf(out, "  schedule    : %s (job %s — ##auto %s schedule %s to register)\n",
			a.Schedule, autoJobName(spec.label, a.Name), spec.label, a.Name)
	}
	if a.OnSuccess != "" {
		fmt.Fprintf(out, "  on_success  : %s\n", a.OnSuccess)
	}
	if a.Notify != nil {
		fmt.Fprintf(out, "  notify      : %s/%s → %s\n", a.Notify.Humanoid, a.Notify.Channel, orDefault(a.Notify.To, "(default recipient)"))
	}
	if len(a.Args) > 0 {
		fmt.Fprintf(out, "  args        : %s\n", strings.Join(a.Args, ", "))
	}
	fmt.Fprintf(out, "  results dir : %s\n", store.ResolveOutputDir(a))
	if a.HasBothStepForms() {
		fmt.Fprintf(out, "  (note: both `step` and `loop.do` present — loop.do wins)\n")
	}
	effDef := webauto.EffectiveStepDef(spec.stepDef(w), spec.loopDef(w))
	ls, sb := a.ResolveWhileWith(spec.loopDef(w), a.ResolveRunBudgetWith(effDef))
	if ls.Enabled {
		fmt.Fprintf(out, "  loop        : %s\n", describeLoop(ls, sb))
	}
	if seats := describeSeats(sb, ls); seats != "" {
		fmt.Fprintf(out, "  models      : %s\n", seats)
	}
	if sb.AutoContinue {
		fmt.Fprintf(out, "  auto-cont.  : on — %d steps/leg, ceilings ~%d steps / %s / %d ctx-tokens\n",
			sb.StepInterval, sb.MaxIterations, sb.MaxDuration, sb.MaxContextTokens)
	} else {
		fmt.Fprintf(out, "  auto-cont.  : off — pauses at the step cap (manual continue)\n")
	}
	fmt.Fprintf(out, "  auto-approve: %v\n", sb.AutoApprove)
	if tp := a.TakeoverPromptWith(effDef); strings.TrimSpace(tp) != "" {
		fmt.Fprintf(out, "  takeover    : at %d%% consumed — %s\n",
			sb.TakeoverWhen, truncateStr(strings.ReplaceAll(tp, "\n", " "), 50))
	}
	fmt.Fprintf(out, "  prompt      :\n%s\n", indentWebPrompt(a.Prompt, "    "))
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
}

// cmdAutoSave saves a prompt as a named recipe. Unlike web's save it needs no
// web-bound pane — the prompt is the whole recipe; targeted kinds set their
// target by editing the file (`agent:`/`humanoid:`) or with the run-time flag.
func (w *WorkspaceActor) cmdAutoSave(out *strings.Builder, spec autoKindSpec, name, prompt string) {
	store := w.autoStore(spec.kind)
	a := &webauto.Automation{Name: name, Prompt: prompt}
	if err := store.Save(a); err != nil {
		fmt.Fprintf(out, "\n[%s] save failed: %v\n", spec.label, err)
		return
	}
	san := webauto.SanitizeName(name)
	fmt.Fprintf(out, "\n[%s] saved automation %q\n", spec.label, san)
	fmt.Fprintf(out, "[%s] run it with: ##auto %s run %s [args...]\n", spec.label, spec.label, san)
	if spec.targeted() {
		fmt.Fprintf(out, "[%s] set its target: add `%s: <name>` to %s/%s.md, or pass %s <name> when running\n",
			spec.label, spec.label, store.Dir(), san, spec.targetFlag())
	}
	if spec.workdirAware {
		fmt.Fprintf(out, "[%s] set its project dir: add `workdir: <dir>` to %s/%s.md, or pass --workdir <dir> when running\n",
			spec.label, store.Dir(), san)
	}
	fmt.Fprintf(out, "[%s] edit %s/%s.md to add description/args or refine the prompt\n",
		spec.label, store.Dir(), san)
}

// resolveAutoTarget picks the effective target for a targeted kind (run flag
// wins over the recipe frontmatter) and validates it against the registry.
// It returns ok=false (after printing the error) when the kind is targeted
// and no valid target could be resolved; untargeted kinds return ("", true).
func (w *WorkspaceActor) resolveAutoTarget(out *strings.Builder, spec autoKindSpec, a *webauto.Automation, override string) (string, bool) {
	if !spec.targeted() {
		return "", true
	}
	target := strings.TrimSpace(override)
	if target == "" {
		target = strings.TrimSpace(spec.recipeTarget(a))
	}
	if target == "" {
		fmt.Fprintf(out, "\n[%s] recipe %q names no target %s — add `%s: <name>` to its frontmatter or pass %s <name>\n",
			spec.label, a.Name, spec.label, spec.label, spec.targetFlag())
		return "", false
	}
	if !spec.validate(w, out, target) {
		return "", false
	}
	return target, true
}

// cmdAutoRun executes a recipe: resolves the target (agent/humanoid kinds),
// pre-creates the results folder, substitutes {{args}}/{{output_dir}}, arms
// the auto-continue budget on the executing LLM actor, and dispatches the
// prompt. contextPrefix, when non-empty (##auto <kind> resume), is prepended
// to seed prior results. Mirrors cmdWebAutoRun minus the web-binding block.
func (w *WorkspaceActor) cmdAutoRun(out *strings.Builder, paneID string, spec autoKindSpec, name string, runtimeArgs []string, targetOverride string, ov webAutoRunOverrides, contextPrefix string) {
	store := w.autoStore(spec.kind)
	a, err := store.Load(name)
	if err != nil {
		fmt.Fprintf(out, "\n[%s] automation %q not found — ##auto %s list\n", spec.label, name, spec.label)
		return
	}

	target, ok := w.resolveAutoTarget(out, spec, a, targetOverride)
	if !ok {
		return
	}
	// Workdir-aware kinds anchor the prompt to a project directory
	// (--workdir override wins over the recipe's `workdir:`; optional).
	workdir := ""
	if spec.workdirAware {
		workdir = w.resolveCodeWorkdir(targetOverride, a)
	}
	// The budget is armed on the executing LLM actor: the pane for task, the
	// named entity for agent/humanoid (their LLMPromptExecutionActors listen
	// on rysh.pane.{name}.llm_prompt_execution.inbox).
	execID := paneID
	if target != "" {
		execID = target
	}
	// --each: the first item runs now (as the run's {{args}}), the rest queue.
	if len(ov.each) > 0 {
		runtimeArgs = []string{ov.each[0]}
	}
	outputDir := store.ResolveOutputDir(a)

	prompt := webauto.SubstituteArgs(a, runtimeArgs)
	prompt = strings.ReplaceAll(prompt, "{{output_dir}}", outputDir)
	prompt = strings.ReplaceAll(prompt, "{{results_dir}}", outputDir)
	if spec.workdirAware {
		prompt = decorateCodePrompt(prompt, workdir)
	}
	if contextPrefix != "" {
		prompt = contextPrefix + "\n\n" + prompt
	}

	// --dry-run: print the fully-resolved plan and change nothing (no loop
	// superseded, no dir created, no budget armed, nothing dispatched).
	if ov.dryRun {
		w.printDryRun(out, spec.label, a, runtimeArgs, target, workdir, outputDir, prompt, ov, spec.stepDef(w), spec.loopDef(w))
		return
	}

	// A fresh run supersedes any loop supervising this target (last run wins;
	// startAutoLoop re-registers when the new recipe also loops). A queue-driven
	// re-dispatch must NOT clobber the very queue advancing it.
	keepQueue := w.autoQueues[execID]
	w.stopAutoLoop(execID)
	if ov.fromQueue && keepQueue != nil {
		if w.autoQueues == nil {
			w.autoQueues = map[string]*autoQueue{}
		}
		w.autoQueues[execID] = keepQueue
	}

	// Pre-create the recipe's results folder so the prompt can write into it
	// via the {{output_dir}} placeholder without a mkdir round-trip.
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		fmt.Fprintf(out, "\n[%s] could not prepare results dir %q: %v\n", spec.label, outputDir, err)
		return
	}

	// Arm the auto-continue budget BEFORE the prompt (fresh budget each
	// run/resume), so a long run resumes itself past the per-leg cap.
	budget, task, fin, finalizer, loopSpec := w.armRecipeBudget(execID, a, runtimeArgs, outputDir, ov, spec.stepDef(w), spec.loopDef(w))

	spec.dispatch(w, paneID, target, prompt)
	w.registerAutoRun(spec.label, a.Name, execID, runtimeArgs, false)

	verb := "running"
	if contextPrefix != "" {
		verb = "resuming"
	}
	fmt.Fprintf(out, "\n[%s] %s automation %q", spec.label, verb, a.Name)
	if target != "" {
		fmt.Fprintf(out, " (%s %s)", spec.label, target)
	}
	fmt.Fprintf(out, "\n")
	if len(runtimeArgs) > 0 {
		fmt.Fprintf(out, "[%s] args: %s\n", spec.label, strings.Join(runtimeArgs, " "))
	}
	if workdir != "" {
		fmt.Fprintf(out, "[%s] workdir: %s\n", spec.label, workdir)
	}
	fmt.Fprintf(out, "[%s] results dir: %s (view: ##auto %s results %s)\n",
		spec.label, outputDir, spec.label, webauto.SanitizeName(name))
	if budget.AutoApprove {
		fmt.Fprintf(out, "[%s] auto-approve: on (tool calls run without the approval dialog)\n", spec.label)
	}
	reportRunBudget(out, spec.label, budget, task, fin, finalizer)
	if spec.progressHint != "" {
		fmt.Fprintf(out, "[%s] %s\n", spec.label, spec.progressHint)
	}

	// Supervision: an enabled while-loop (judge → iterate), or chain-watch for
	// on_success / notify / --each completion handling on plain runs.
	w.registerEachQueue(out, spec.label, a.Name, execID, paneID, false, ov)
	if _, queued := w.autoQueues[execID]; loopSpec.Enabled || queued ||
		strings.TrimSpace(a.OnSuccess) != "" || a.Notify != nil {
		w.startAutoLoop(out, spec.label, a.Name, execID, paneID, outputDir, a, runtimeArgs, loopSpec, budget,
			w.loopRearmFunc(execID, a, runtimeArgs, outputDir, ov, spec.stepDef(w), spec.loopDef(w)),
			w.loopDispatchFor(spec, paneID, target))
	}
}

// cmdAutoResume starts a fresh run (fresh budget) seeded with the recipe's
// latest saved result, so the run continues building on what a prior run
// produced instead of starting from scratch. Mirrors cmdWebAutoResume.
func (w *WorkspaceActor) cmdAutoResume(out *strings.Builder, paneID string, spec autoKindSpec, name string, runtimeArgs []string, target string, ov webAutoRunOverrides) {
	store := w.autoStore(spec.kind)
	a, err := store.Load(name)
	if err != nil {
		fmt.Fprintf(out, "\n[%s] automation %q not found — ##auto %s list\n", spec.label, name, spec.label)
		return
	}
	dir := store.ResolveOutputDir(a)
	prefix := ""
	if fname, content, ok := latestResultFile(dir); ok {
		fmt.Fprintf(out, "\n[%s] resuming from the latest result: %s (%d bytes loaded into context)\n", spec.label, fname, len(content))
		// Name the FULL absolute path (not just the basename) so the save
		// location survives even if the recipe prompt is later compacted away.
		prefix = fmt.Sprintf("[Resuming a previous run] You already produced the results below (from %q). "+
			"Continue the SAME task: build on this, add only NEW items, avoid duplicates, and save the updated "+
			"list back to the same results file at this exact path: %s\n\n--- previously saved results ---\n%s\n--- end of previous results ---",
			fname, filepath.Join(dir, fname), string(content))
	} else {
		fmt.Fprintf(out, "\n[%s] no prior results found in %s — resuming as a fresh run\n", spec.label, dir)
	}
	w.cmdAutoRun(out, paneID, spec, name, runtimeArgs, target, ov, prefix)
}

// cmdAutoContinue re-arms the recipe's budget on the executing LLM actor and
// resumes a paused run from its last checkpoint (after a cancel / stop). If
// nothing is paused it's a no-op (the LLM actor reports "nothing to
// continue"). Mirrors cmdWebAutoContinue.
func (w *WorkspaceActor) cmdAutoContinue(out *strings.Builder, paneID string, spec autoKindSpec, name string, runtimeArgs []string, targetOverride string, ov webAutoRunOverrides) {
	store := w.autoStore(spec.kind)
	a, err := store.Load(name)
	if err != nil {
		fmt.Fprintf(out, "\n[%s] automation %q not found — ##auto %s list\n", spec.label, name, spec.label)
		return
	}
	target, ok := w.resolveAutoTarget(out, spec, a, targetOverride)
	if !ok {
		return
	}
	execID := paneID
	if target != "" {
		execID = target
	}
	// Fail-closed (design 013): resuming an automation restarts the tool loop
	// with no new prompt, so it must consult the gate here.
	if reason, blocked := policy.Blocked(); blocked {
		fmt.Fprint(out, policy.BlockedMessage(reason))
		return
	}
	w.stopAutoLoop(execID) // manual continue supersedes any active loop
	outputDir := store.ResolveOutputDir(a)
	_ = os.MkdirAll(outputDir, 0o755)
	// Re-arm the budget so the resumed run auto-continues again (a cancel disarmed
	// it). The MsgSetRunBudget arrives before the continue on the same inbox.
	budget, task, fin, finalizer, _ := w.armRecipeBudget(execID, a, runtimeArgs, outputDir, ov, spec.stepDef(w), spec.loopDef(w))
	_ = w.pub.Send(msg.T("pane", execID, "llm_prompt_execution", "inbox"), &msg.MsgAgenticContinue{})
	w.registerAutoRun(spec.label, a.Name, execID, runtimeArgs, w.autoRuns[execID].Headless)

	fmt.Fprintf(out, "\n[%s] continuing %q from its last checkpoint (budget re-armed)\n", spec.label, a.Name)
	reportRunBudget(out, spec.label, budget, task, fin, finalizer)
	fmt.Fprintf(out, "[%s] if nothing was paused this is a no-op — use ##auto %s resume to start fresh from the latest saved result\n", spec.label, spec.label)
}
