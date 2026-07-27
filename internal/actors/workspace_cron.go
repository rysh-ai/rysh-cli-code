package actors

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"
	"github.com/google/uuid"

	"github.com/rysh-ai/rysh-cli-code/internal/cron"
)

// ---------------------------------------------------------------------------
// In-daemon cron service
//
// A minute-aligned ticker delivers cronTickMsg into the WorkspaceActor
// mailbox; the handler fires every due job by routing the job's saved input
// through the normal dispatch (routeInput) with an explicit target pane — so a
// job can run ANYTHING a user could type (##auto web run, @agent …, a prompt,
// a shell line). Jobs persist to the workspace KV under cronKVKey and are
// restored (next-run recomputed, no backfill) on daemon start. Cron fires
// while the daemon is alive, including when the session is detached.
// ---------------------------------------------------------------------------

// cronKVKey is the workspace-KV key under which the job list is persisted.
const cronKVKey = "cron.jobs"

// cronTickMsg is delivered to the WorkspaceActor mailbox once per minute.
type cronTickMsg struct{}

// cronScheduler owns the session's cron jobs and their persistence.
type cronScheduler struct {
	jobs []*cron.Job
}

// startCron restores persisted jobs, recomputes their next-run times (no
// backfill), and launches the minute-aligned ticker goroutine.
func (w *WorkspaceActor) startCron() {
	w.cron = &cronScheduler{}
	w.cronLoadJobs()
	// Recipe-schedule sync: recipes carrying a `schedule:` key get their
	// auto-<kind>-<name> cron job upserted; jobs whose recipe was deleted are
	// removed. Recipes without the key are untouched (purely on-demand).
	w.syncRecipeSchedules()
	now := time.Now()
	for _, j := range w.cron.jobs {
		if j.Enabled {
			if err := j.ComputeNext(now); err != nil {
				slog.Warn("cron: disabling job with bad schedule on restore", "job", j.Name, "err", err)
				j.Enabled = false
			}
		}
	}
	w.cronPersist()

	w.cronTickStop = make(chan struct{})
	self := w.selfPID
	system := w.actorSystem
	stop := w.cronTickStop
	go func() {
		// Align to the next minute boundary, then tick every minute.
		now := time.Now()
		firstDelay := now.Truncate(time.Minute).Add(time.Minute).Sub(now)
		timer := time.NewTimer(firstDelay)
		defer timer.Stop()
		for {
			select {
			case <-stop:
				return
			case <-timer.C:
				if self != nil && system != nil {
					system.Root.Send(self, &cronTickMsg{})
				}
				timer.Reset(time.Minute)
			}
		}
	}()
	if n := len(w.cron.jobs); n > 0 {
		slog.Info("cron: started", "jobs", n)
	}
}

// stopCron halts the ticker goroutine (idempotent).
func (w *WorkspaceActor) stopCron() {
	if w.cronTickStop != nil {
		close(w.cronTickStop)
		w.cronTickStop = nil
	}
}

// handleCronTick fires every due job. All cron state is mutated here (mailbox
// goroutine), so no locking is needed.
func (w *WorkspaceActor) handleCronTick(ctx actor.Context) {
	// Safety-net flush for debounced KV state. The primary trailing flush runs
	// on snapshot ticks, which only happen while a client (TUI/web) is polling.
	// A detached daemon has no such heartbeat, so pending dirty state would sit
	// unwritten until shutdown and be lost on SIGKILL. The cron ticker runs for
	// the daemon's whole lifetime including when detached, so piggy-backing here
	// bounds the exposure to ~1 minute.
	w.maybeFlushKV()

	if w.cron == nil {
		return
	}
	now := time.Now()
	changed := false
	for _, j := range w.cron.jobs {
		if !j.Due(now) {
			continue
		}
		w.fireCronJob(ctx, j)
		if err := j.ComputeNext(now); err != nil {
			j.Enabled = false
		}
		if j.FailStreak >= cron.AutoDisableAfter {
			j.Enabled = false
			_ = w.pub.SendPaneRyshOutput(w.activePaneID,
				fmt.Sprintf("[cron] job %q auto-disabled after %d consecutive failures\n", j.Name, j.FailStreak))
		}
		changed = true
	}
	if changed {
		w.cronPersist()
	}
}

// fireCronJob resolves the job's target pane, routes its input through the
// normal dispatch, and records a run entry.
func (w *WorkspaceActor) fireCronJob(ctx actor.Context, j *cron.Job) {
	paneID := w.cronResolveTarget(j.Target)
	if paneID == "" {
		j.RecordRun(cron.Run{At: time.Now(), Status: "error", Note: "target pane not found: " + j.Target})
		return
	}
	tab := w.resolveOriginTab(paneID)

	// Echo the fire to the target pane's rysh output so it's visible in-session.
	_ = w.pub.SendPaneRyshOutput(paneID, fmt.Sprintf("\n[cron] fired %q → %s\n", j.Name, j.Input))

	out := w.routeInput(ctx, paneID, tab, j.InferMode(), j.Input)

	note := "dispatched"
	if trimmed := strings.TrimSpace(out); trimmed != "" {
		note = truncateCron(firstLineCron(trimmed), 200) // synchronous ## result tail
	}
	j.RecordRun(cron.Run{At: time.Now(), Status: "ok", Note: note})
}

// cronResolveTarget maps a job's Target to a live pane id: "active"/"" → the
// current active pane; otherwise a pane id / title / given-name.
func (w *WorkspaceActor) cronResolveTarget(target string) string {
	target = strings.TrimSpace(target)
	if target == "" || target == "active" {
		return w.activePaneID
	}
	return w.resolvePaneID(target)
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

func (w *WorkspaceActor) cronLoadJobs() {
	if w.wKV == nil {
		return
	}
	entry, err := w.wKV.Get(cronKVKey)
	if err != nil {
		return // no jobs yet
	}
	var jobs []*cron.Job
	if json.Unmarshal(entry.Value(), &jobs) == nil {
		w.cron.jobs = jobs
	}
}

func (w *WorkspaceActor) cronPersist() {
	if w.wKV == nil || w.cron == nil {
		return
	}
	if data, err := json.Marshal(w.cron.jobs); err == nil {
		_, _ = w.wKV.Put(cronKVKey, data)
	}
}

// findCronJob returns the job with the given name, or nil.
func (w *WorkspaceActor) findCronJob(name string) *cron.Job {
	if w.cron == nil {
		return nil
	}
	for _, j := range w.cron.jobs {
		if j.Name == name {
			return j
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// ##cron command
// ---------------------------------------------------------------------------

// handleCronCommand processes ##cron subcommands. rawCmd is the full command
// text WITHOUT the leading "##" (e.g. `cron add ig "0 9 * * *" ...`), so the
// add subcommand can parse its quoted multi-field schedule.
func (w *WorkspaceActor) handleCronCommand(ctx actor.Context, out *strings.Builder, paneID, rawCmd string) {
	if w.cron == nil {
		fmt.Fprintf(out, "\n[cron] cron service is not available in this workspace\n")
		return
	}
	// Strip the leading "cron" token; keep the remainder verbatim for add.
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rawCmd), "cron"))
	fields := strings.Fields(rest)
	sub := ""
	if len(fields) > 0 {
		sub = fields[0]
	}

	switch sub {
	case "", "list":
		w.cronList(out)
	case "add":
		w.cronAdd(out, strings.TrimSpace(strings.TrimPrefix(rest, "add")))
	case "show":
		if len(fields) < 2 {
			fmt.Fprintf(out, "\n[cron] usage: ##cron show <name>\n")
			return
		}
		w.cronShow(out, fields[1])
	case "rm", "remove", "delete":
		if len(fields) < 2 {
			fmt.Fprintf(out, "\n[cron] usage: ##cron rm <name>\n")
			return
		}
		w.cronRemove(out, fields[1])
	case "enable", "disable":
		if len(fields) < 2 {
			fmt.Fprintf(out, "\n[cron] usage: ##cron %s <name>\n", sub)
			return
		}
		w.cronSetEnabled(out, fields[1], sub == "enable")
	case "run":
		if len(fields) < 2 {
			fmt.Fprintf(out, "\n[cron] usage: ##cron run <name>\n")
			return
		}
		w.cronRunNow(ctx, out, fields[1])
	case "next":
		w.cronNext(out)
	case "logs":
		if len(fields) < 2 {
			fmt.Fprintf(out, "\n[cron] usage: ##cron logs <name> [N]\n")
			return
		}
		n := 10
		if len(fields) > 2 {
			if v, err := parseIntCron(fields[2]); err == nil && v > 0 {
				n = v
			}
		}
		w.cronLogs(out, fields[1], n)
	case "help":
		w.cronUsage(out)
	default:
		fmt.Fprintf(out, "\n[cron] unknown subcommand: %q\n", sub)
		w.cronUsage(out)
	}
}

func (w *WorkspaceActor) cronUsage(out *strings.Builder) {
	fmt.Fprintf(out, "\n[rysh] usage:\n")
	fmt.Fprintf(out, "  ##cron add <name> \"<schedule>\" [--pane <id|name|active>] [--mode M] <input...>\n")
	fmt.Fprintf(out, "  ##cron list | show <name> | next\n")
	fmt.Fprintf(out, "  ##cron enable <name> | disable <name> | rm <name>\n")
	fmt.Fprintf(out, "  ##cron run <name>            fire now (test without waiting)\n")
	fmt.Fprintf(out, "  ##cron logs <name> [N]       last N run records (default 10)\n")
	fmt.Fprintf(out, "  schedule: 5-field cron (\"0 9 * * *\"), or @every 15m / @daily / @hourly\n")
	fmt.Fprintf(out, "  jobs fire while the session daemon is alive (running or detached)\n\n")
}

// cronAdd parses `<name> "<schedule>" [--pane P] [--mode M] <input...>`.
func (w *WorkspaceActor) cronAdd(out *strings.Builder, argstr string) {
	name, rest := nextToken(argstr)
	if name == "" {
		fmt.Fprintf(out, "\n[cron] usage: ##cron add <name> \"<schedule>\" [--pane P] [--mode M] <input...>\n")
		return
	}
	schedule, rest := extractSchedule(rest)
	if schedule == "" {
		fmt.Fprintf(out, "\n[cron] a schedule is required — quote it, e.g. \"0 9 * * *\" or \"@every 15m\"\n")
		return
	}
	target := "active"
	mode := ""
	tz := ""
	// Pull leading --pane/--mode/--tz flags off the front of rest.
	for {
		rest = strings.TrimSpace(rest)
		flag, after := nextToken(rest)
		switch flag {
		case "--pane", "-p":
			val, r := nextToken(after)
			target, rest = val, r
		case "--mode", "-m":
			val, r := nextToken(after)
			mode, rest = val, r
		case "--tz", "--timezone":
			val, r := nextToken(after)
			tz, rest = val, r
		default:
			goto flagsDone
		}
	}
flagsDone:
	input := strings.TrimSpace(rest)
	if input == "" {
		fmt.Fprintf(out, "\n[cron] an input is required — the command/prompt the job injects\n")
		return
	}
	if mode != "" && mode != "shell" && mode != "prompt" && mode != "rysh" {
		fmt.Fprintf(out, "\n[cron] invalid --mode %q (shell|prompt|rysh)\n", mode)
		return
	}
	if err := cron.Validate(name, schedule, tz, input); err != nil {
		fmt.Fprintf(out, "\n[cron] %v\n", err)
		return
	}
	if w.findCronJob(name) != nil {
		fmt.Fprintf(out, "\n[cron] a job named %q already exists — rm it first or pick another name\n", name)
		return
	}
	if len(w.cron.jobs) >= cron.MaxJobs {
		fmt.Fprintf(out, "\n[cron] job limit reached (%d)\n", cron.MaxJobs)
		return
	}

	j := &cron.Job{
		ID:       uuid.New().String(),
		Name:     name,
		Schedule: schedule,
		Timezone: tz,
		Target:   target,
		Mode:     mode,
		Input:    input,
		Enabled:  true,
	}
	_ = j.ComputeNext(time.Now())
	w.cron.jobs = append(w.cron.jobs, j)
	w.cronPersist()

	fmt.Fprintf(out, "\n[cron] added job %q\n", name)
	fmt.Fprintf(out, "  schedule : %s%s\n", schedule, tzSuffix(tz))
	fmt.Fprintf(out, "  target   : %s\n", target)
	fmt.Fprintf(out, "  input    : %s\n", input)
	fmt.Fprintf(out, "  next run : %s\n", fmtCronTime(j.NextRun))
}

func (w *WorkspaceActor) cronList(out *strings.Builder) {
	if len(w.cron.jobs) == 0 {
		fmt.Fprintf(out, "\n[cron] no jobs — ##cron add <name> \"<schedule>\" <input>\n")
		return
	}
	fmt.Fprintf(out, "\n[cron] jobs (%d)\n", len(w.cron.jobs))
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 72))
	for _, j := range w.cron.jobs {
		state := "on"
		if !j.Enabled {
			state = "off"
		}
		next := "—"
		if j.Enabled {
			next = fmtCronTime(j.NextRun)
		}
		fmt.Fprintf(out, "  %-16s [%s] %-14s next:%s  runs:%d  last:%s\n",
			j.Name, state, truncateCron(j.Schedule, 14), next, j.RunCount, orDash(j.LastStatus))
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 72))
}

func (w *WorkspaceActor) cronShow(out *strings.Builder, name string) {
	j := w.findCronJob(name)
	if j == nil {
		fmt.Fprintf(out, "\n[cron] no job named %q\n", name)
		return
	}
	fmt.Fprintf(out, "\n[cron] job %q\n", j.Name)
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	fmt.Fprintf(out, "  enabled  : %v\n", j.Enabled)
	fmt.Fprintf(out, "  schedule : %s%s\n", j.Schedule, tzSuffix(j.Timezone))
	fmt.Fprintf(out, "  target   : %s\n", j.Target)
	fmt.Fprintf(out, "  mode     : %s\n", orDash(j.InferMode()))
	fmt.Fprintf(out, "  input    : %s\n", j.Input)
	fmt.Fprintf(out, "  next run : %s\n", fmtCronTime(j.NextRun))
	fmt.Fprintf(out, "  last run : %s (%s)\n", fmtCronTime(j.LastRun), orDash(j.LastStatus))
	fmt.Fprintf(out, "  runs     : %d\n", j.RunCount)
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
}

func (w *WorkspaceActor) cronRemove(out *strings.Builder, name string) {
	for i, j := range w.cron.jobs {
		if j.Name == name {
			w.cron.jobs = append(w.cron.jobs[:i], w.cron.jobs[i+1:]...)
			w.cronPersist()
			fmt.Fprintf(out, "\n[cron] removed job %q\n", name)
			return
		}
	}
	fmt.Fprintf(out, "\n[cron] no job named %q\n", name)
}

func (w *WorkspaceActor) cronSetEnabled(out *strings.Builder, name string, enabled bool) {
	j := w.findCronJob(name)
	if j == nil {
		fmt.Fprintf(out, "\n[cron] no job named %q\n", name)
		return
	}
	j.Enabled = enabled
	if enabled {
		if err := j.ComputeNext(time.Now()); err != nil {
			fmt.Fprintf(out, "\n[cron] cannot enable %q: %v\n", name, err)
			j.Enabled = false
			return
		}
		j.FailStreak = 0
		fmt.Fprintf(out, "\n[cron] enabled %q — next run %s\n", name, fmtCronTime(j.NextRun))
	} else {
		fmt.Fprintf(out, "\n[cron] disabled %q\n", name)
	}
	w.cronPersist()
}

// cronRunNow fires a job immediately (ad-hoc), independent of its schedule —
// recorded as a run so it appears in logs, but WITHOUT advancing NextRun (a
// manual fire leaves the schedule untouched). Runs on the mailbox goroutine we
// are already on (##cron is handled synchronously in Receive), so routing
// directly with the live ctx is safe.
func (w *WorkspaceActor) cronRunNow(ctx actor.Context, out *strings.Builder, name string) {
	j := w.findCronJob(name)
	if j == nil {
		fmt.Fprintf(out, "\n[cron] no job named %q\n", name)
		return
	}
	w.fireCronJob(ctx, j)
	w.cronPersist()
	fmt.Fprintf(out, "\n[cron] fired %q now — see the target pane for output\n", name)
}

func (w *WorkspaceActor) cronNext(out *strings.Builder) {
	type up struct {
		name string
		next time.Time
	}
	var ups []up
	for _, j := range w.cron.jobs {
		if j.Enabled && !j.NextRun.IsZero() {
			ups = append(ups, up{j.Name, j.NextRun})
		}
	}
	if len(ups) == 0 {
		fmt.Fprintf(out, "\n[cron] no enabled jobs scheduled\n")
		return
	}
	// Simple insertion sort by next time (small N).
	for i := 1; i < len(ups); i++ {
		for k := i; k > 0 && ups[k].next.Before(ups[k-1].next); k-- {
			ups[k], ups[k-1] = ups[k-1], ups[k]
		}
	}
	fmt.Fprintf(out, "\n[cron] upcoming\n")
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	for _, u := range ups {
		fmt.Fprintf(out, "  %s  %s\n", fmtCronTime(u.next), u.name)
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
}

func (w *WorkspaceActor) cronLogs(out *strings.Builder, name string, n int) {
	j := w.findCronJob(name)
	if j == nil {
		fmt.Fprintf(out, "\n[cron] no job named %q\n", name)
		return
	}
	if len(j.Runs) == 0 {
		fmt.Fprintf(out, "\n[cron] job %q has not run yet\n", name)
		return
	}
	runs := j.Runs
	if len(runs) > n {
		runs = runs[len(runs)-n:]
	}
	fmt.Fprintf(out, "\n[cron] %q — last %d run(s)\n", name, len(runs))
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
	for _, r := range runs {
		fmt.Fprintf(out, "  %s  %-6s %s\n", fmtCronTime(r.At), r.Status, r.Note)
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
}
