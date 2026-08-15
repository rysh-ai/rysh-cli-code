// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/cron"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/webauto"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// ##auto v2 command surface: check, status, stop, schedule/unschedule,
// --dry-run printing, and the recipe-schedule boot sync. Shared by every
// kind — web reuses it through webKindSpec().
// ---------------------------------------------------------------------------

// webKindSpec is a minimal autoKindSpec for the web kind, used ONLY by the
// shared v2 subcommands (check/status/stop/schedule) — web's run/save keep
// their own handlers in workspace_webauto.go.
func webKindSpec() autoKindSpec {
	return autoKindSpec{
		label:   "web",
		kind:    webauto.KindWeb,
		stepDef: func(w *WorkspaceActor) *webauto.StepConfig { return w.cfg.Automation.Web.Step },
		loopDef: func(w *WorkspaceActor) *webauto.LoopConfig { return w.cfg.Automation.Web.Loop },
	}
}

// ---------------------------------------------------------------------------
// --dry-run
// ---------------------------------------------------------------------------

// printDryRun renders the fully-resolved run plan without dispatching: the
// substituted prompt, paths, budget (with seats), and loop plan. prompt is
// the final text the run WOULD dispatch.
func (w *WorkspaceActor) printDryRun(out *strings.Builder, label string, a *webauto.Automation, runtimeArgs []string, target, workdir, outputDir, prompt string, ov webAutoRunOverrides, defStep *webauto.StepConfig, defLoop *webauto.LoopConfig) {
	budget, task, fin, takeover, loop := w.resolveRecipePlan(a, runtimeArgs, outputDir, ov, defStep, defLoop)
	fmt.Fprintf(out, "\n[%s] DRY RUN — nothing dispatched, no state changed\n", label)
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	fmt.Fprintf(out, "  recipe      : %s\n", a.Name)
	if target != "" {
		fmt.Fprintf(out, "  target      : %s\n", target)
	}
	if workdir != "" {
		fmt.Fprintf(out, "  workdir     : %s\n", workdir)
	}
	fmt.Fprintf(out, "  results dir : %s (not created)\n", outputDir)
	if seats := describeSeats(budget, loop); seats != "" {
		fmt.Fprintf(out, "  models      : %s\n", seats)
	}
	reportRunBudget(out, label, budget, task, fin, takeover)
	if loop.Enabled {
		fmt.Fprintf(out, "[%s] loop: %s\n", label, describeLoop(loop, budget))
	}
	if a.ScheduleOff() {
		fmt.Fprintf(out, "[%s] schedule: %s (disabled — no cron job)\n", label, a.Schedule)
	} else if a.Schedule != "" {
		fmt.Fprintf(out, "[%s] schedule: %s (job %s)\n", label, a.Schedule, autoJobName(label, a.Name))
	}
	fmt.Fprintf(out, "[%s] prompt that would be dispatched:\n%s\n", label, indentWebPrompt(prompt, "    "))
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
}

// describeSeats renders the resolved model/effort seats, or "" when all are
// unset (session defaults everywhere).
func describeSeats(b webauto.RunBudget, loop webauto.LoopSpec) string {
	fmtSeat := func(model, effort string) string {
		switch {
		case model != "" && effort != "":
			return model + "/" + effort
		case model != "":
			return model
		case effort != "":
			return "(default)/" + effort
		}
		return ""
	}
	var parts []string
	if s := fmtSeat(b.Model, b.Effort); s != "" {
		parts = append(parts, "do="+s)
	}
	if s := fmtSeat(b.FinalizerModel, b.FinalizerEffort); s != "" {
		parts = append(parts, "finalizer="+s)
	}
	if s := fmtSeat(loop.JudgeModel, loop.JudgeEffort); s != "" {
		parts = append(parts, "judge="+s)
	}
	return strings.Join(parts, " ")
}

// ---------------------------------------------------------------------------
// ##auto <kind> check <name>
// ---------------------------------------------------------------------------

var placeholderRe = regexp.MustCompile(`\{\{(\w+)\}\}`)
var argNRe = regexp.MustCompile(`^arg\d+$`)

// unknownPlaceholders returns the {{tokens}} in text that are not in the
// known set (argN and args are always known).
func unknownPlaceholders(text string, known map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range placeholderRe.FindAllStringSubmatch(text, -1) {
		tok := m[1]
		if known[tok] || argNRe.MatchString(tok) || seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	sort.Strings(out)
	return out
}

// cmdAutoCheck lints one recipe: placeholders, loop sanity, seats, target /
// workdir existence, web_read, and the schedule expression.
func (w *WorkspaceActor) cmdAutoCheck(out *strings.Builder, spec autoKindSpec, name string) {
	store := w.autoStore(spec.kind)
	a, err := store.Load(name)
	if err != nil {
		fmt.Fprintf(out, "\n[%s] automation %q not found — ##auto %s list\n", spec.label, name, spec.label)
		w.failRysh("automation %q not found — ##auto %s list", name, spec.label)
		return
	}
	var warns, notes []string
	ok := func(format string, args ...interface{}) { notes = append(notes, fmt.Sprintf(format, args...)) }
	warn := func(format string, args ...interface{}) { warns = append(warns, fmt.Sprintf(format, args...)) }

	// Placeholders across every prompt-bearing field.
	known := map[string]bool{"args": true, "output_dir": true, "results_dir": true}
	for _, arg := range a.Args {
		known[arg] = true
	}
	if spec.workdirAware {
		known["workdir"] = true
	}
	texts := map[string]string{"prompt": a.Prompt}
	if a.Loop != nil && a.Loop.While != nil && a.Loop.While.Prompts != nil {
		texts["while.until"] = a.Loop.While.Prompts.Until
		texts["while.iterate_with"] = a.Loop.While.Prompts.IterateWith
	}
	if tp := a.TakeoverPrompt(); tp != "" {
		texts["takeover_prompt"] = tp
	}
	for where, text := range texts {
		if unknown := unknownPlaceholders(text, known); len(unknown) > 0 {
			warn("%s has unknown placeholders: {{%s}} (declared args: %s)",
				where, strings.Join(unknown, "}}, {{"), orDefault(strings.Join(a.Args, ", "), "none"))
		}
	}

	// Loop sanity.
	effDef := webauto.EffectiveStepDef(spec.stepDef(w), spec.loopDef(w))
	base := a.ResolveRunBudgetWith(effDef)
	ls, pass := a.ResolveWhileWith(spec.loopDef(w), base)
	if a.HasBothStepForms() {
		ok("both `step` and `loop.do` present — loop.do wins")
	} else if a.Step != nil {
		ok("legacy `step:` form — prefer `loop.do` (same fields, canonical layout); `step` remains a compatible alias")
	}
	if a.Loop != nil && a.Loop.While != nil {
		wl := a.Loop.While
		until := ""
		if wl.Prompts != nil {
			until = strings.TrimSpace(wl.Prompts.Until)
		}
		switch {
		case wl.Enabled != nil && !*wl.Enabled:
			ok("loop present but enabled:false — the do-step runs once")
		case until == "":
			warn("loop.while has no prompts.until — the loop cannot run (no exit condition)")
		default:
			if strings.TrimSpace(wl.MaxDuration) != "" && ls.MaxDuration == 0 {
				warn("while.max_duration %q does not exceed the per-pass duration (%s) — axis DISABLED", wl.MaxDuration, base.MaxDuration)
			}
			if strings.TrimSpace(wl.Budget) != "" && ls.MaxTokens == 0 {
				warn("while.budget %q does not exceed the per-pass token budget (%d) — axis DISABLED", wl.Budget, base.MaxContextTokens)
			}
			ok("loop: %s", describeLoop(ls, pass))
		}
	}

	// Model/effort seats.
	seatCheck := func(seat, model, effort string) {
		if !webauto.ValidEffort(effort) {
			warn("%s effort %q is not a known level (low|medium|high|xhigh|max)", seat, effort)
		}
		if effort != "" && modelRejectsEffort(model) {
			warn("%s pairs effort %q with %s, which rejects the effort parameter — the run self-heals by dropping the effort (one wasted request per leg); remove the effort or pick an effort-capable model", seat, effort, model)
		}
	}
	if es := a.EffectiveStep(); es != nil {
		seatCheck("do", es.Model, es.Effort)
		if es.Budget != nil && es.Budget.Watch != nil {
			seatCheck("finalizer", es.Budget.Watch.Model, es.Budget.Watch.Effort)
		}
	}
	if a.Loop != nil && a.Loop.While != nil {
		seatCheck("while (judge)", a.Loop.While.Model, a.Loop.While.Effort)
	}

	// Target / workdir existence.
	if spec.targeted() {
		tgt := strings.TrimSpace(spec.recipeTarget(a))
		if tgt == "" {
			warn("no `%s:` target in the frontmatter — runs need %s <name>", spec.label, spec.targetFlag())
		} else if spec.validate != nil {
			var probe strings.Builder
			if spec.validate(w, &probe, tgt) {
				ok("target %s %q is loaded", spec.label, tgt)
			} else {
				warn("target %s %q: %s", spec.label, tgt, strings.TrimSpace(probe.String()))
			}
		}
	}
	if spec.workdirAware {
		if dir := w.resolveCodeWorkdir("", a); dir == "" {
			ok("no workdir set — runs use the pane's own directory (or pass --workdir)")
		} else if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			warn("workdir %s does not exist (or is not a directory)", dir)
		} else {
			ok("workdir %s exists", dir)
		}
	}

	// web_read (web kind only).
	if spec.kind == webauto.KindWeb {
		switch strings.TrimSpace(a.WebRead) {
		case "", "text", "screenshot":
		default:
			warn("web_read %q is not text|screenshot — runs fall back to text", a.WebRead)
		}
	}

	// on_success chain target.
	if v := strings.TrimSpace(a.OnSuccess); v != "" {
		if nextLabel, nextRecipe, okc := parseChainTarget(spec.label, v); !okc {
			warn("on_success %q is not a valid target (use <name> or <kind>:<name>)", v)
		} else if nspec, _ := specForLabel(nextLabel); true {
			if _, err := w.autoStore(nspec.kind).Load(nextRecipe); err != nil {
				warn("on_success target %s:%s does not exist", nextLabel, nextRecipe)
			} else {
				ok("on_success chains to %s:%s (depth cap %d)", nextLabel, nextRecipe, maxChainDepth)
			}
		}
	}

	// notify config.
	if n := a.Notify; n != nil {
		switch {
		case strings.TrimSpace(n.Humanoid) == "" || strings.TrimSpace(n.Channel) == "":
			warn("notify needs both humanoid and channel (got humanoid=%q channel=%q)", n.Humanoid, n.Channel)
		case w.humanoidRegistryPID == nil:
			ok("notify: %s/%s (humanoid registry unavailable — existence not verified)", n.Humanoid, n.Channel)
		default:
			found := false
			if hs, err := w.queryHumanoids(); err == nil {
				for _, h := range hs {
					if h.Name == n.Humanoid {
						found = true
						break
					}
				}
			}
			if found {
				ok("notify: %s/%s (humanoid loaded)", n.Humanoid, n.Channel)
			} else {
				warn("notify humanoid %q is not loaded — ##humanoid spawn it before the run ends", n.Humanoid)
			}
		}
	}

	// Schedule expression.
	if a.ScheduleOff() {
		ok("schedule: %s — scheduling disabled (boot sync removes job %s if present)",
			a.Schedule, autoJobName(spec.label, a.Name))
	} else if strings.TrimSpace(a.Schedule) != "" {
		if _, err := cron.ParseSchedule(a.Schedule, ""); err != nil {
			warn("schedule %q does not parse: %v", a.Schedule, err)
		} else {
			ok("schedule %q parses (job name %s)", a.Schedule, autoJobName(spec.label, a.Name))
		}
	}

	fmt.Fprintf(out, "\n[%s] check %q\n", spec.label, a.Name)
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	for _, n := range notes {
		fmt.Fprintf(out, "  ✓ %s\n", n)
	}
	for _, wl := range warns {
		fmt.Fprintf(out, "  ⚠ %s\n", wl)
	}
	if len(warns) == 0 {
		fmt.Fprintf(out, "  recipe OK — no warnings\n")
	} else {
		fmt.Fprintf(out, "  %d warning(s)\n", len(warns))
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
}

// ---------------------------------------------------------------------------
// ##auto [<kind>] status / ##auto <kind> stop
// ---------------------------------------------------------------------------

// queryLoopStatuses snapshots every live loop (labelFilter "" = all kinds).
// Dead loop PIDs found along the way are pruned from the registry.
func (w *WorkspaceActor) queryLoopStatuses(labelFilter string) []LoopStatus {
	var out []LoopStatus
	for execID, pid := range w.autoLoops {
		res, err := w.actorSystem.Root.RequestFuture(pid, &loopStatusQuery{}, 2*time.Second).Result()
		if err != nil {
			delete(w.autoLoops, execID) // loop ended — self-heal the registry
			continue
		}
		st, ok := res.(LoopStatus)
		if !ok {
			continue
		}
		if labelFilter != "" && st.Label != labelFilter {
			continue
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// cmdAutoLoopStatus prints the live loops (one kind, or every kind for "").
func (w *WorkspaceActor) cmdAutoLoopStatus(out *strings.Builder, labelFilter string) {
	scope := labelFilter
	if scope == "" {
		scope = "auto"
	}
	sts := w.queryLoopStatuses(labelFilter)
	if len(sts) == 0 {
		fmt.Fprintf(out, "\n[%s] no active loops\n", scope)
		return
	}
	fmt.Fprintf(out, "\n[%s] %d active loop(s)\n", scope, len(sts))
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	for _, st := range sts {
		fmt.Fprintf(out, "  %-9s %-20s pass %d/%d  %-12s target %s\n",
			st.Label, st.Recipe, st.Pass, st.MaxPasses, st.State, st.ExecID)
		fmt.Fprintf(out, "            started %s ago", time.Since(st.StartedAt).Round(time.Second))
		if !st.Deadline.IsZero() {
			fmt.Fprintf(out, ", deadline in %s", time.Until(st.Deadline).Round(time.Second))
		}
		fmt.Fprintf(out, "\n")
		if st.LastVerdict != "" {
			fmt.Fprintf(out, "            last verdict: %s\n", truncateStr(st.LastVerdict, 70))
		}
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	fmt.Fprintf(out, "  stop one: ##auto <kind> stop [recipe]\n")
}

// cmdAutoLoopStop gracefully ends one loop's supervision: with a recipe name
// it targets that recipe's loop; without, the loop supervising the active
// pane. The in-flight pass finishes but is never judged or iterated.
func (w *WorkspaceActor) cmdAutoLoopStop(out *strings.Builder, spec autoKindSpec, name, paneID string) {
	for _, st := range w.queryLoopStatuses(spec.label) {
		if (name != "" && st.Recipe == webauto.SanitizeName(name)) || (name == "" && st.ExecID == paneID) {
			if pid, ok := w.autoLoops[st.ExecID]; ok {
				w.actorSystem.Root.Send(pid, &loopStopRequest{})
				delete(w.autoLoops, st.ExecID)
				fmt.Fprintf(out, "\n[%s] stopping loop %q (pass %d/%d) — the in-flight pass finishes but won't be judged or iterated\n",
					spec.label, st.Recipe, st.Pass, st.MaxPasses)
				fmt.Fprintf(out, "[%s] run report: %s\n", spec.label, "run-report.md in the recipe's results dir")
				return
			}
		}
	}
	if name != "" {
		fmt.Fprintf(out, "\n[%s] no active loop for recipe %q — ##auto %s status\n", spec.label, name, spec.label)
		w.failRysh("no active loop for recipe %q — ##auto %s status", name, spec.label)
	} else {
		fmt.Fprintf(out, "\n[%s] no active loop on this pane — pass a recipe name (##auto %s status lists them)\n", spec.label, spec.label)
		w.failRysh("no active loop on this pane — pass a recipe name (##auto %s status lists them)", spec.label)
	}
}

// ---------------------------------------------------------------------------
// ##auto [<kind>] runs — list the runs still executing
// ---------------------------------------------------------------------------

// autoRunEntry records one dispatched ##auto run. Registered at dispatch time
// by run/resume/continue, keyed by the executing LLM actor ID (last run wins).
type autoRunEntry struct {
	Label     string    // kind: web | task | agent | humanoid | code
	Recipe    string    // recipe name
	ExecID    string    // pane ID or agent/humanoid name
	Headless  bool      // web only: CLI-owned headless browser
	Args      string    // runtime args, for display
	StartedAt time.Time // dispatch time (refreshed on resume/continue)
}

// registerAutoRun records a dispatched run in the registry (last run wins).
func (w *WorkspaceActor) registerAutoRun(label, recipe, execID string, args []string, headless bool) {
	if w.autoRuns == nil {
		w.autoRuns = make(map[string]autoRunEntry)
	}
	w.autoRuns[execID] = autoRunEntry{
		Label: label, Recipe: recipe, ExecID: execID,
		Headless: headless, Args: strings.Join(args, " "), StartedAt: time.Now(),
	}
}

// cmdAutoRunsList prints the registered runs that are still executing
// (##auto <kind> runs [list]; labelFilter "" = every kind). Each exec's LLM
// actor is asked for its live budget accounting (MsgGetRunStatus request/
// reply); an exec that reports nothing armed and nothing in flight has
// finished — its entry is pruned, as is one that no longer answers (pane
// closed). Loop passes come from the loop supervisors (##auto status data).
func (w *WorkspaceActor) cmdAutoRunsList(out *strings.Builder, labelFilter string) {
	scope := labelFilter
	if scope == "" {
		scope = "auto"
	}
	loops := map[string]LoopStatus{}
	for _, st := range w.queryLoopStatuses(labelFilter) {
		loops[st.ExecID] = st
	}

	type runRow struct {
		e  autoRunEntry
		st *msg.MsgRunStatusReply
	}
	var rows []runRow
	for execID, e := range w.autoRuns {
		if labelFilter != "" && e.Label != labelFilter {
			continue
		}
		reply, err := w.pub.Request(msg.T("pane", execID, "llm_prompt_execution", "inbox"),
			&msg.MsgGetRunStatus{}, 1500*time.Millisecond)
		if err != nil {
			delete(w.autoRuns, execID) // exec gone (pane closed) — self-heal
			continue
		}
		st, ok := reply.(*msg.MsgRunStatusReply)
		if !ok {
			continue
		}
		if !st.Armed && !st.InFlight {
			delete(w.autoRuns, execID) // run ended — self-heal the registry
			continue
		}
		rows = append(rows, runRow{e: e, st: st})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].e.StartedAt.Before(rows[j].e.StartedAt) })

	if len(rows) == 0 {
		fmt.Fprintf(out, "\n[%s] no runs executing\n", scope)
		return
	}
	fmt.Fprintf(out, "\n[%s] %d run(s) executing\n", scope, len(rows))
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	for _, r := range rows {
		e, st := r.e, r.st
		state := "between legs"
		switch {
		case st.Finalizer:
			state = "wrap-up"
		case st.InFlight:
			state = "running"
		case st.Paused && st.PausedReason != "":
			state = "paused(" + st.PausedReason + ")"
		}
		fmt.Fprintf(out, "  %-9s %-20s %-12s target %s", e.Label, e.Recipe, state, e.ExecID)
		if e.Headless {
			out.WriteString("  headless")
		}
		out.WriteString("\n")

		// Time consumed: since the budget was armed (falls back to dispatch time).
		started := e.StartedAt
		if st.ArmedAtMs > 0 {
			started = time.UnixMilli(st.ArmedAtMs)
		}
		fmt.Fprintf(out, "            time %s", time.Since(started).Round(time.Second))
		if st.DeadlineMs > 0 {
			fmt.Fprintf(out, " (deadline in %s)", time.Until(time.UnixMilli(st.DeadlineMs)).Round(time.Second))
		}
		// Loop passes (only looped runs have a supervisor).
		if ls, ok := loops[e.ExecID]; ok && ls.MaxPasses > 0 {
			fmt.Fprintf(out, "  loop %d/%d", ls.Pass, ls.MaxPasses)
		}
		// Tokens consumed (completed legs) against the armed cumulative cap.
		fmt.Fprintf(out, "  tokens %s", fmtTokens(st.TokensUsed))
		if st.MaxContextTokens > 0 {
			fmt.Fprintf(out, "/%s", fmtTokens(st.MaxContextTokens))
		}
		// Prompt-cache hit rate: reads are ~10%-price and NOT counted against
		// the budget. A low % on a long run means the conversation prefix is
		// being re-sent at full price every round — the budget will burn fast.
		if denom := st.CacheReadTokens + st.CacheWriteTokens + st.FreshInputTokens; denom > 0 {
			fmt.Fprintf(out, "  cache %d%%", st.CacheReadTokens*100/denom)
		}
		if st.Armed {
			fmt.Fprintf(out, "  legs left %d", st.ContinuesLeft)
		}
		out.WriteString("\n")
		if e.Args != "" {
			fmt.Fprintf(out, "            args: %s\n", truncateStr(e.Args, 60))
		}
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	fmt.Fprintf(out, "  loops: ##auto <kind> status — stop one: ##auto <kind> stop [recipe]\n")
}

// fmtTokens renders a token count compactly (1234567 → "1.2m", 45200 → "45.2k").
func fmtTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fm", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// ---------------------------------------------------------------------------
// ##auto <kind> schedule / unschedule + boot sync
// ---------------------------------------------------------------------------

// autoJobName renders the cron job name for a scheduled recipe:
// auto-<kind>-<recipe>, sanitized to cron's [a-zA-Z0-9_-] name rules.
func autoJobName(label, recipe string) string {
	raw := "auto-" + label + "-" + recipe
	var sb strings.Builder
	for _, r := range raw {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('-')
		}
	}
	name := sb.String()
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// autoJobInput renders the ##cron input for a scheduled recipe run.
func autoJobInput(label, recipe string, args []string) string {
	input := "##auto " + label + " run " + recipe
	if len(args) > 0 {
		input += " " + strings.Join(args, " ")
	}
	return input
}

// parseAutoJobInput reverses autoJobInput: (label, recipe, ok). Used by the
// boot sync to map an auto-* job back to its recipe.
func parseAutoJobInput(input string) (label, recipe string, ok bool) {
	fields := strings.Fields(input)
	if len(fields) < 4 || fields[0] != "##auto" || fields[2] != "run" {
		return "", "", false
	}
	return fields[1], fields[3], true
}

// cmdAutoSchedule registers (or updates) the recipe's cron job from its
// `schedule:` frontmatter key. Extra args are baked into the scheduled run.
// A recipe without the key gets a friendly pointer instead of a job.
func (w *WorkspaceActor) cmdAutoSchedule(out *strings.Builder, spec autoKindSpec, name string, args []string) {
	store := w.autoStore(spec.kind)
	a, err := store.Load(name)
	if err != nil {
		fmt.Fprintf(out, "\n[%s] automation %q not found — ##auto %s list\n", spec.label, name, spec.label)
		w.failRysh("automation %q not found — ##auto %s list", name, spec.label)
		return
	}
	if a.ScheduleOff() {
		fmt.Fprintf(out, "\n[%s] recipe %q declares schedule: %s — scheduling is disabled; replace it with a cron expression to re-enable\n",
			spec.label, a.Name, a.Schedule)
		return
	}
	if strings.TrimSpace(a.Schedule) == "" {
		fmt.Fprintf(out, "\n[%s] recipe %q has no `schedule:` key — add one to its frontmatter (e.g. schedule: \"0 8 * * *\")\n", spec.label, a.Name)
		fmt.Fprintf(out, "[%s] (or schedule ad hoc with: ##cron add <name> \"<expr>\" ##auto %s run %s)\n", spec.label, spec.label, a.Name)
		return
	}
	jobName := autoJobName(spec.label, a.Name)
	input := autoJobInput(spec.label, a.Name, args)
	if err := cron.Validate(jobName, a.Schedule, "", input); err != nil {
		fmt.Fprintf(out, "\n[%s] schedule %q invalid: %v\n", spec.label, a.Schedule, err)
		w.failRysh("schedule %q invalid: %v", a.Schedule, err)
		return
	}
	if j := w.findCronJob(jobName); j != nil {
		j.Schedule = a.Schedule
		j.Input = input
		j.Enabled = true
		_ = j.ComputeNext(time.Now())
		w.cronPersist()
		fmt.Fprintf(out, "\n[%s] updated scheduled job %q: %s → %s\n", spec.label, jobName, a.Schedule, input)
		fmt.Fprintf(out, "[%s] next run: %s\n", spec.label, fmtCronTime(j.NextRun))
		return
	}
	if len(w.cron.jobs) >= cron.MaxJobs {
		fmt.Fprintf(out, "\n[%s] cron job limit reached (%d)\n", spec.label, cron.MaxJobs)
		return
	}
	j := &cron.Job{
		ID: uuid.New().String(), Name: jobName, Schedule: a.Schedule,
		Target: "active", Mode: "rysh", Input: input, Enabled: true,
	}
	_ = j.ComputeNext(time.Now())
	w.cron.jobs = append(w.cron.jobs, j)
	w.cronPersist()
	fmt.Fprintf(out, "\n[%s] scheduled %q: %s → %s\n", spec.label, jobName, a.Schedule, input)
	fmt.Fprintf(out, "[%s] next run: %s (manage with ##cron list|logs|rm)\n", spec.label, fmtCronTime(j.NextRun))
}

// cmdAutoUnschedule removes the recipe's auto-* cron job.
func (w *WorkspaceActor) cmdAutoUnschedule(out *strings.Builder, spec autoKindSpec, name string) {
	jobName := autoJobName(spec.label, webauto.SanitizeName(name))
	for i, j := range w.cron.jobs {
		if j.Name == jobName {
			w.cron.jobs = append(w.cron.jobs[:i], w.cron.jobs[i+1:]...)
			w.cronPersist()
			fmt.Fprintf(out, "\n[%s] unscheduled %q\n", spec.label, jobName)
			return
		}
	}
	fmt.Fprintf(out, "\n[%s] no scheduled job %q — ##cron list\n", spec.label, jobName)
}

// syncRecipeSchedules is the boot-time sync between recipe `schedule:` keys
// and auto-* cron jobs:
//
//   - recipe WITH a valid schedule key → its job is upserted (created, or
//     schedule/input refreshed)
//   - recipe WITHOUT the key → untouched: no job created, and any existing
//     auto-* job for it is LEFT ALONE (it may be manually managed) —
//     explicit removal is ##auto <kind> unschedule
//   - auto-* job whose recipe no longer EXISTS → removed
func (w *WorkspaceActor) syncRecipeSchedules() {
	specs := []autoKindSpec{webKindSpec(), taskAutoSpec(), agentAutoSpec(), humanoidAutoSpec(), codeAutoSpec()}
	exists := map[string]bool{}       // "<label>/<recipe>" → recipe file present
	scheduledOff := map[string]bool{} // recipe declares schedule: off — its auto job must go
	changed := false

	for _, spec := range specs {
		for _, a := range w.autoStore(spec.kind).List() {
			exists[spec.label+"/"+a.Name] = true
			if a.ScheduleOff() {
				scheduledOff[spec.label+"/"+a.Name] = true
				continue
			}
			if strings.TrimSpace(a.Schedule) == "" {
				continue
			}
			if _, err := cron.ParseSchedule(a.Schedule, ""); err != nil {
				continue // invalid expression — surfaced by ##auto check, never synced
			}
			jobName := autoJobName(spec.label, a.Name)
			input := autoJobInput(spec.label, a.Name, nil)
			if j := w.findCronJob(jobName); j != nil {
				if j.Schedule != a.Schedule || j.Input != input {
					j.Schedule, j.Input = a.Schedule, input
					_ = j.ComputeNext(time.Now())
					changed = true
				}
				continue
			}
			if len(w.cron.jobs) >= cron.MaxJobs {
				continue
			}
			j := &cron.Job{
				ID: uuid.New().String(), Name: jobName, Schedule: a.Schedule,
				Target: "active", Mode: "rysh", Input: input, Enabled: true,
			}
			_ = j.ComputeNext(time.Now())
			w.cron.jobs = append(w.cron.jobs, j)
			changed = true
		}
	}

	// Remove auto-* jobs whose recipe file is gone entirely, or whose recipe
	// declares schedule: off (the declarative off-switch).
	kept := w.cron.jobs[:0]
	for _, j := range w.cron.jobs {
		if strings.HasPrefix(j.Name, "auto-") {
			if label, recipe, ok := parseAutoJobInput(j.Input); ok &&
				(!exists[label+"/"+recipe] || scheduledOff[label+"/"+recipe]) {
				changed = true
				continue
			}
		}
		kept = append(kept, j)
	}
	w.cron.jobs = kept

	if changed {
		w.cronPersist()
	}
}

// webReadGuidance renders the observation-method guidance prepended to a web
// run's prompt. "" (unset) keeps today's behavior with no injection; invalid
// values return ok=false and the run falls back to text.
func webReadGuidance(mode string) (line string, ok bool) {
	switch strings.TrimSpace(mode) {
	case "":
		return "", true
	case "text":
		return "[Observation mode: text] Read pages with get_text / get_elements / get_html — " +
			"do not rely on screenshots for content.", true
	case "screenshot":
		return "[Observation mode: screenshot] Observe pages primarily with the screenshot action — " +
			"each capture is delivered to you as an image you can actually see. Use get_text / " +
			"get_elements only when you need exact strings (links, emails, handles).", true
	}
	return "", false
}

// modelRejectsEffort reports whether a model is KNOWN to reject the
// output_config.effort parameter (per the Anthropic model matrix: Haiku 4.5
// and Sonnet 4.5 error on effort). Best-effort advisory for ##auto check —
// unknown models pass, and runtime self-heals regardless.
func modelRejectsEffort(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(m, "claude-haiku-4-5") || strings.HasPrefix(m, "claude-sonnet-4-5")
}

// cmdAutoHistory renders the append-only run ledger (run-history.jsonl):
// one row per finished ##auto run across all kinds, newest last — the
// persistent answer to "what did my automation runs cost?". Written by
// AutoLoopActor.appendRunHistory; run reports are per-recipe and
// overwritten, this survives.
func (w *WorkspaceActor) cmdAutoHistory(out *strings.Builder, n int) {
	path := filepath.Join(w.browserWorkDir(), ".rysh", "automations", "run-history.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(out, "\n[auto] no run history yet (%s)\n", path)
		return
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	fmt.Fprintf(out, "\n[auto] last %d finished run(s) — %s\n", len(lines), path)
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 100))
	fmt.Fprintf(out, "  %-16s %-6s %-18s %-9s %5s  %-14s %-9s %s\n",
		"ended", "kind", "recipe", "dur", "pass", "tokens", "cache", "cause")
	for _, ln := range lines {
		var r runHistoryRecord
		if json.Unmarshal([]byte(ln), &r) != nil {
			continue
		}
		started, _ := time.Parse(time.RFC3339, r.Started)
		ended, _ := time.Parse(time.RFC3339, r.Ended)
		dur := "-"
		if !started.IsZero() && !ended.IsZero() {
			dur = ended.Sub(started).Round(time.Second).String()
		}
		tokens := "-"
		if r.AcctSamples > 0 {
			tokens = fmtTokens(r.Tokens)
			if r.TokenCap > 0 {
				tokens += "/" + fmtTokens(r.TokenCap)
			}
		}
		cache := "-"
		if denom := r.CacheRead + r.CacheWrite + r.CacheFresh; denom > 0 {
			cache = fmt.Sprintf("%d%% hit", r.CacheRead*100/denom)
		}
		fmt.Fprintf(out, "  %-16s %-6s %-18s %-9s %2d     %-14s %-9s %s\n",
			ended.Format("Jan 02 15:04"), r.Kind, truncateStr(r.Recipe, 18), dur,
			r.Passes, tokens, cache, truncateStr(r.Cause, 34))
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 100))
	fmt.Fprintf(out, "  live runs: ##auto <kind> runs · per-run detail: <results dir>/run-report.md\n")
}
