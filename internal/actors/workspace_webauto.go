// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rysh-ai/rysh-cli-code/internal/browserinstance"
	"github.com/rysh-ai/rysh-cli-code/internal/cdp"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/webauto"

	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// ---------------------------------------------------------------------------
// ##auto web — reusable prompt-based web automations (recipes)
//
// A recipe binds a prompt to a browser profile (persistent auth) and start
// URL. `##auto web run <name> [args…]` (re)binds the active pane's web mode
// to that profile/URL and dispatches the substituted prompt to the pane's
// Ask Rysh AI, which drives the browser through browser_action. Because the
// enable-mode and submit-input messages travel on the SAME pane inbox
// subject, the pane processes them in order — no snapshot round-trip races.
// ---------------------------------------------------------------------------

// handleAutoCommand processes ##auto subcommands — the umbrella for reusable
// automations: web (browser recipes), task (plain prompt recipes on the
// active pane), agent and humanoid (recipes that drive a named registry
// entity), and code (task-like recipes anchored to a project workdir).
func (w *WorkspaceActor) handleAutoCommand(out *strings.Builder, paneID string, args []string) {
	if len(args) == 0 || args[0] == "help" {
		w.autoUsage(out)
		return
	}
	switch args[0] {
	case "web":
		w.handleWebAuto(out, paneID, args[1:])
	case "task":
		w.handleAutoKind(out, paneID, taskAutoSpec(), args[1:])
	case "agent":
		w.handleAutoKind(out, paneID, agentAutoSpec(), args[1:])
	case "humanoid":
		w.handleAutoKind(out, paneID, humanoidAutoSpec(), args[1:])
	case "code":
		w.handleAutoKind(out, paneID, codeAutoSpec(), args[1:])
	case "status":
		w.cmdAutoLoopStatus(out, "")
	case "history":
		n := 20
		if len(args) > 1 {
			if v, err := strconv.Atoi(args[1]); err == nil && v > 0 {
				n = v
			}
		}
		w.cmdAutoHistory(out, n)
	case "runs":
		w.cmdAutoRunsList(out, "")
	default:
		ryshWriter(out).UnknownIn("auto", args[0])
		w.failRyshUsage("unknown %s subcommand: %q", "auto", args[0])
		w.autoUsage(out)
	}
}

// autoUsage prints the ##auto umbrella usage. web's full usage is included
// inline (the original subcommand); the other kinds get one-liners plus a
// pointer to their own help.
func (w *WorkspaceActor) autoUsage(out *strings.Builder) {
	ryshWriter(out).UsageLine("##auto <subcommand>  — reusable automations")
	w.failRyshUsage("usage: %s", "##auto <subcommand>  — reusable automations")
	fmt.Fprintf(out, "  ##auto web ...       prompt-based web automations (recipes)\n")
	fmt.Fprintf(out, "  ##auto task ...      plain prompt automations on the active pane (##auto task help)\n")
	fmt.Fprintf(out, "  ##auto agent ...     automations that drive a named agent (##auto agent help)\n")
	fmt.Fprintf(out, "  ##auto humanoid ...  automations that drive a named humanoid (##auto humanoid help)\n")
	fmt.Fprintf(out, "  ##auto code ...      coding automations anchored to a project workdir (##auto code help)\n")
	fmt.Fprintf(out, "  ##auto history [n]   token/cache ledger of the last n finished runs (all kinds)\n")
	w.autoWebUsage(out)
}

// handleWebCommand processes ##web subcommands (currently: headless).
// The automation recipes moved to their own top-level command: ##auto web.
func (w *WorkspaceActor) handleWebCommand(out *strings.Builder, paneID string, args []string) {
	if len(args) == 0 || args[0] == "help" {
		w.webUsage(out)
		return
	}
	switch args[0] {
	case "auto":
		// Old location — point at the new command, preserving the tail so the
		// user can copy-paste it.
		fmt.Fprintf(out, "\n[web] `##web auto` has moved — use: ##auto web %s\n", strings.Join(args[1:], " "))
	case "headless":
		w.handleWebHeadlessCmd(out, paneID, args[1:])
	case "import-google-session":
		w.handleImportGoogleSession(out, paneID, args[1:])
	default:
		ryshWriter(out).UnknownIn("web", args[0])
		w.failRyshUsage("unknown %s subcommand: %q", "web", args[0])
		w.webUsage(out)
	}
}

// webUsage prints the ##web usage (headless browser management).
func (w *WorkspaceActor) webUsage(out *strings.Builder) {
	ryshWriter(out).Usage(
		"##web headless on [--profile P] [url]  run this pane's browser in CLI-owned headless Chromium",
		"##web headless off|status            stop / inspect the headless browser",
		"##web headless login <profile> [url] open a HEADED Chromium on the profile to log in once",
		"##web import-google-session <profile>  copy a real-Chrome Google login into the app's web profile",
		"      (so third-party \"Sign in with Google\" works in web panes; log in first via `##web headless login`)",
		"(web automations moved: ##auto web — see ##auto help)",
	)
}

func (w *WorkspaceActor) autoWebUsage(out *strings.Builder) {
	forms := []string{}
	forms = append(forms, "##auto web list                      list saved web automations")
	forms = append(forms, "##auto web show <name>               show a recipe (profile, url, prompt)")
	forms = append(forms, "##auto web save <name> <prompt...>   save active pane's web binding + prompt as a recipe")
	forms = append(forms, "##auto web run [--headless] [--step-interval N] [--max-iterations N] [--max-duration D] [--budget-size Np|Nb|Ns] [--takeover-when P] <name> [args...]")
	forms = append(forms, "      run a recipe ({{args}}/{{arg1}}/{{output_dir}} substituted); flags override the recipe budget")
	forms = append(forms, "##auto web resume [flags] <name> [args...]    fresh budget + load the latest result into context, then rerun")
	forms = append(forms, "##auto web continue [flags] <name> [args...]  resume a cancelled/stopped run from its last checkpoint (budget re-armed)")
	forms = append(forms, "##auto web results <name> [file]     list the recipe's saved results (or print one file)")
	forms = append(forms, "##auto web check <name>              lint a recipe; ##auto web status|stop [name] inspect/stop live loops")
	forms = append(forms, "##auto web runs [list]               list runs still executing (time consumed, loop pass, tokens consumed)")
	forms = append(forms, "##auto web schedule|unschedule <name>  register the recipe's schedule: key as a ##cron job")
	forms = append(forms, "##auto web delete <name>             delete a recipe")
	forms = append(forms, "--dry-run on run/resume prints the resolved plan without dispatching; web_read: text|screenshot picks the observation method")
	forms = append(forms, "loop flags: --no-loop --passes N --while-duration D --while-budget Nb; --each \"a,b,c\" fans out sequentially")
	forms = append(forms, fmt.Sprintf("record flags: --record captures a browser screenshot every %s for the whole run and encodes one video (needs ffmpeg);", webauto.DefaultRecordInterval))
	forms = append(forms, "      --recording-path <file|dir> sets the destination (default output_dir/recordings/<name>-<stamp>.mp4),")
	forms = append(forms, "      --record-interval D changes the cadence, --no-record overrides a recipe/config that records by default;")
	forms = append(forms, "      recipe block `record: {enabled, path, interval, format, quality, max_frames}`, config `automation.web.record`;")
	forms = append(forms, "      a looped run records ALL passes into one video; recordings stay on local disk and are never shared or sent upstream")
	forms = append(forms, "on_success: [<kind>:]<name> chains the next recipe on completion; notify: {humanoid, channel, to} pings a channel on run end")
	forms = append(forms, "recipes live in .rysh/automations/webs/<name>.md (top-level: web_profile, url, description, args, output_dir;")
	forms = append(forms, "      step: {interval, max_iterations, max_duration, auto_continue, auto_approve, budget: {size, watch: {takeover_when, takeover_prompt}}})")
	forms = append(forms, "step.auto_approve (default true) runs tool calls without the approval dialog; set false to be prompted")
	forms = append(forms, fmt.Sprintf("step.budget.watch.takeover_when (default %d): once that %% of every ceiling is consumed the takeover leg starts with the rest", webauto.DefaultTakeoverWhen))
	forms = append(forms, "      (a thin takeover share — <1m / <50 steps / <0.3 book — is floored at >=5m / >=100 steps / >=1 book so the wrap-up can finish)")
	forms = append(forms, "long runs auto-continue past each step.interval leg (default 50) until done or a ceiling is hit")
	forms = append(forms, fmt.Sprintf("      (defaults ~300 steps / 20m / 3 books; budget.size takes p/b/s units — page=%d tok, book=200 pages, shelf=20 books);", webauto.TokensPerPage))
	forms = append(forms, "      when a ceiling is hit, step.budget.watch.takeover_prompt runs a graceful wrap-up")
	forms = append(forms, "loop: {do, while} is the loop-engineering layout: `do` = the per-pass budget (same fields as `step`; loop.do wins over step),")
	forms = append(forms, "      `while` {enabled, max_iterations, max_duration, budget, prompts: {until, iterate_with}} repeats the pass until an LLM judge")
	forms = append(forms, fmt.Sprintf("      deems `until` fulfilled (default %d passes). while.max_duration/budget are TOTALS: bigger than the per-pass value → split", webauto.DefaultLoopIterations))
	forms = append(forms, "      evenly across passes (do.X = while.X / max_iterations); smaller → ignored. enabled:false runs the pass once, loop off")
	forms = append(forms, "config-level defaults: automation.web.step in rysh.config.yaml (same shape as the recipe step block,")
	forms = append(forms, "      incl. budget.watch.floor); precedence: run flags > recipe > config > built-ins")
	forms = append(forms, "results save to output_dir (default .rysh/automations/webs/<name>/results)")
	ryshWriter(out).Usage(forms...)
}

// handleWebHeadlessCmd processes ##web headless subcommands.
func (w *WorkspaceActor) handleWebHeadlessCmd(out *strings.Builder, paneID string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "on":
		profile, url := parseHeadlessOnArgs(args[1:])
		if profile != "" {
			if _, err := browserinstance.EnsureProfile(w.browserWorkDir(), profile); err != nil {
				fmt.Fprintf(out, "\n[web] could not prepare profile %q: %v\n", profile, err)
				w.failRysh("could not prepare profile %q: %v", profile, err)
				return
			}
		}
		_ = w.pub.Send(msg.T("pane", paneID, "inbox"),
			&msg.MsgPaneWebHeadless{PaneID: paneID, Op: "on", Profile: profile, URL: normalizeWebURL(url)})
		fmt.Fprintf(out, "\n[web] starting headless browser for this pane — status follows in pane output\n")

	case "off", "status":
		_ = w.pub.Send(msg.T("pane", paneID, "inbox"),
			&msg.MsgPaneWebHeadless{PaneID: paneID, Op: sub})

	case "login":
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("web", "##web headless login <profile> [url]")
			w.failRyshUsage("usage: %s", "##web headless login <profile> [url]")
			return
		}
		profile := browserinstance.SanitizeProfile(args[1])
		url := ""
		if len(args) > 2 {
			url = normalizeWebURL(args[2])
		}
		dir, err := browserinstance.EnsureProfile(w.browserWorkDir(), profile)
		if err != nil {
			fmt.Fprintf(out, "\n[web] could not prepare profile %q: %v\n", profile, err)
			w.failRysh("could not prepare profile %q: %v", profile, err)
			return
		}
		pid, err := cdp.LaunchInteractive(filepath.Join(dir, "headless"), url)
		if err != nil {
			fmt.Fprintf(out, "\n[web] could not open login browser: %v\n", err)
			w.failRysh("could not open login browser: %v", err)
			return
		}
		fmt.Fprintf(out, "\n[web] opened a browser window (pid %d) on headless profile %q\n", pid, profile)
		fmt.Fprintf(out, "[web] log in to your sites there, then close the window — the auth persists\n")
		fmt.Fprintf(out, "[web] subsequent `##web headless on --profile %s` / `##auto web run --headless` runs reuse it\n", profile)

	default:
		ryshWriter(out).UsageLineIn("web", "##web headless on [--profile P] [url] | off | status | login <profile> [url]")
		w.failRyshUsage("usage: %s", "##web headless on [--profile P] [url] | off | status | login <profile> [url]")
	}
}

// parseHeadlessOnArgs extracts --profile and an optional trailing URL.
func parseHeadlessOnArgs(args []string) (profile, url string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--profile" || a == "-p":
			if i+1 < len(args) {
				profile = browserinstance.SanitizeProfile(args[i+1])
				i++
			}
		case strings.HasPrefix(a, "--profile="):
			profile = browserinstance.SanitizeProfile(strings.TrimPrefix(a, "--profile="))
		default:
			if url == "" {
				url = a
			}
		}
	}
	return profile, url
}

// handleImportGoogleSession copies a Google login from a profile's real-Chrome
// jar (populated by `##web headless login`, which opens actual Chrome that
// Google permits) into the desktop app's Electron session partition for the same
// profile. Web panes on that profile then carry the Google session, so
// third-party "Sign in with Google" completes without Google's embedded-browser
// block — the credential entry already happened in real Chrome; here we only
// transfer the resulting session cookies. Read-only: we never sign in here.
func (w *WorkspaceActor) handleImportGoogleSession(out *strings.Builder, paneID string, args []string) {
	if len(args) < 1 || args[0] == "help" {
		ryshWriter(out).UsageLineIn("web", "##web import-google-session <profile>")
		w.failRyshUsage("usage: %s", "##web import-google-session <profile>")
		fmt.Fprintf(out, "  1) log in first, in real Chrome:  ##web headless login <profile> https://accounts.google.com/\n")
		fmt.Fprintf(out, "  2) close that window, then:       ##web import-google-session <profile>\n")
		fmt.Fprintf(out, "  3) use it:                        ##mode new web --profile <profile> <a \"Sign in with Google\" site>\n")
		return
	}
	profile := browserinstance.SanitizeProfile(args[0])
	dir := filepath.Join(browserinstance.ProfileDir(w.browserWorkDir(), profile), "headless")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	all, err := cdp.ExtractCookies(ctx, dir)
	if err != nil {
		fmt.Fprintf(out, "\n[web] could not read profile %q: %v\n", profile, err)
		w.failRysh("could not read profile %q: %v", profile, err)
		return
	}
	cookies := cdp.GoogleIdentityCookies(all)
	if len(cookies) == 0 {
		fmt.Fprintf(out, "\n[web] no Google cookies in profile %q — did the login finish? Re-run `##web headless login %s https://accounts.google.com/`\n", profile, profile)
		return
	}

	imp := make([]msg.ImportCookie, len(cookies))
	for i, c := range cookies {
		imp[i] = msg.ImportCookie{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Expires: c.Expires, HTTPOnly: c.HTTPOnly, Secure: c.Secure, SameSite: c.SameSite,
		}
	}
	_ = w.pub.Send(msg.T("web", "import-cookies"),
		&msg.MsgPaneImportCookies{Profile: profile, Cookies: imp})

	fmt.Fprintf(out, "\n[web] imported %d Google cookies into profile %q\n", len(cookies), profile)
	fmt.Fprintf(out, "[web] open it: ##mode new web --profile %s <a site with \"Sign in with Google\">\n", profile)
}

// autoStore opens the recipe store for one automation kind, anchored under
// the same workDir as the browser-profile store.
func (w *WorkspaceActor) autoStore(kind webauto.Kind) *webauto.Store {
	return webauto.NewStoreFor(w.browserWorkDir(), kind)
}

func (w *WorkspaceActor) webAutoStore() *webauto.Store {
	return w.autoStore(webauto.KindWeb)
}

func (w *WorkspaceActor) handleWebAuto(out *strings.Builder, paneID string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "list":
		w.cmdWebAutoList(out)
	case "show":
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("web", "##auto web show <name>")
			w.failRyshUsage("usage: %s", "##auto web show <name>")
			return
		}
		w.cmdWebAutoShow(out, args[1])
	case "save":
		if len(args) < 3 {
			ryshWriter(out).UsageLineIn("web", "##auto web save <name> <prompt...>")
			w.failRyshUsage("usage: %s", "##auto web save <name> <prompt...>")
			return
		}
		w.cmdWebAutoSave(out, paneID, args[1], strings.Join(args[2:], " "))
	case "run":
		name, runArgs, headless, ov := parseWebAutoRunFlags(args[1:])
		if name == "" {
			ryshWriter(out).UsageLineIn("web", "##auto web run [--headless] [--step-interval N] [--max-iterations N] [--max-duration D] [--budget-size Np|Nb|Ns] [--takeover-when P] <name> [args...]")
			w.failRyshUsage("usage: %s", "##auto web run [--headless] [--step-interval N] [--max-iterations N] [--max-duration D] [--budget-size Np|Nb|Ns] [--takeover-when P] <name> [args...]")
			return
		}
		w.cmdWebAutoRun(out, paneID, name, runArgs, headless, ov, "")
	case "resume":
		name, runArgs, headless, ov := parseWebAutoRunFlags(args[1:])
		if name == "" {
			ryshWriter(out).UsageLineIn("web", "##auto web resume [flags] <name> [args...]  (fresh budget + loads the latest result into context)")
			w.failRyshUsage("usage: %s", "##auto web resume [flags] <name> [args...]  (fresh budget + loads the latest result into context)")
			return
		}
		w.cmdWebAutoResume(out, paneID, name, runArgs, headless, ov)
	case "continue":
		name, runArgs, _, ov := parseWebAutoRunFlags(args[1:])
		if name == "" {
			ryshWriter(out).UsageLineIn("web", "##auto web continue [flags] <name> [args...]  (resume a cancelled/stopped run from its last checkpoint)")
			w.failRyshUsage("usage: %s", "##auto web continue [flags] <name> [args...]  (resume a cancelled/stopped run from its last checkpoint)")
			return
		}
		w.cmdWebAutoContinue(out, paneID, name, runArgs, ov)
	case "check":
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("web", "##auto web check <name>")
			w.failRyshUsage("usage: %s", "##auto web check <name>")
			return
		}
		w.cmdAutoCheck(out, webKindSpec(), args[1])
	case "status":
		w.cmdAutoLoopStatus(out, "web")
	case "runs":
		w.cmdAutoRunsList(out, "web")
	case "stop":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		w.cmdAutoLoopStop(out, webKindSpec(), name, paneID)
	case "schedule":
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("web", "##auto web schedule <name> [args...]")
			w.failRyshUsage("usage: %s", "##auto web schedule <name> [args...]")
			return
		}
		w.cmdAutoSchedule(out, webKindSpec(), args[1], args[2:])
	case "unschedule":
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("web", "##auto web unschedule <name>")
			w.failRyshUsage("usage: %s", "##auto web unschedule <name>")
			return
		}
		w.cmdAutoUnschedule(out, webKindSpec(), args[1])
	case "results":
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("web", "##auto web results <name> [file]")
			w.failRyshUsage("usage: %s", "##auto web results <name> [file]")
			return
		}
		file := ""
		if len(args) > 2 {
			file = args[2]
		}
		w.cmdWebAutoResults(out, args[1], file)
	case "delete":
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("web", "##auto web delete <name>")
			w.failRyshUsage("usage: %s", "##auto web delete <name>")
			return
		}
		if err := w.webAutoStore().Delete(args[1]); err != nil {
			fmt.Fprintf(out, "\n[web] delete failed: %v\n", err)
			w.failRysh("delete failed: %v", err)
			return
		}
		fmt.Fprintf(out, "\n[web] deleted automation %q\n", args[1])
	default:
		w.autoWebUsage(out)
		w.failRyshUsage("unknown ##auto web subcommand: %q", sub)
	}
}

func (w *WorkspaceActor) cmdWebAutoList(out *strings.Builder) {
	autos := w.webAutoStore().List()
	if len(autos) == 0 {
		fmt.Fprintf(out, "\n[web] no automations saved yet — ##auto web save <name> <prompt...>\n")
		return
	}
	fmt.Fprintf(out, "\n[web] automations (%s)\n", w.webAutoStore().Dir())
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	for _, a := range autos {
		desc := a.Description
		if desc == "" {
			desc = truncateStr(strings.ReplaceAll(a.Prompt, "\n", " "), 50)
		}
		profile := a.Profile
		if profile == "" {
			profile = "default"
		}
		fmt.Fprintf(out, "  %-20s [%s] %s\n", a.Name, profile, desc)
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
}

func (w *WorkspaceActor) cmdWebAutoShow(out *strings.Builder, name string) {
	a, err := w.webAutoStore().Load(name)
	if err != nil {
		fmt.Fprintf(out, "\n[web] automation %q not found\n", name)
		w.failRysh("automation %q not found", name)
		return
	}
	fmt.Fprintf(out, "\n[web] automation %q\n", a.Name)
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	fmt.Fprintf(out, "  description : %s\n", a.Description)
	fmt.Fprintf(out, "  profile     : %s\n", orDefault(a.Profile, "default"))
	fmt.Fprintf(out, "  url         : %s\n", orDefault(a.URL, "(none)"))
	if len(a.Args) > 0 {
		fmt.Fprintf(out, "  args        : %s\n", strings.Join(a.Args, ", "))
	}
	fmt.Fprintf(out, "  results dir : %s\n", w.webAutoStore().ResolveOutputDir(a))
	if a.HasBothStepForms() {
		fmt.Fprintf(out, "  (note: both `step` and `loop.do` present — loop.do wins)\n")
	}
	if a.WebRead != "" {
		fmt.Fprintf(out, "  web_read    : %s\n", a.WebRead)
	}
	if a.Schedule != "" {
		fmt.Fprintf(out, "  schedule    : %s (job %s)\n", a.Schedule, autoJobName("web", a.Name))
	}
	if a.OnSuccess != "" {
		fmt.Fprintf(out, "  on_success  : %s\n", a.OnSuccess)
	}
	if a.Notify != nil {
		fmt.Fprintf(out, "  notify      : %s/%s → %s\n", a.Notify.Humanoid, a.Notify.Channel, orDefault(a.Notify.To, "(default recipient)"))
	}
	effDef := webauto.EffectiveStepDef(w.cfg.Automation.Web.Step, w.cfg.Automation.Web.Loop)
	ls, sb := a.ResolveWhileWith(w.cfg.Automation.Web.Loop, a.ResolveRunBudgetWith(effDef))
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

// cmdWebAutoSave captures the ACTIVE pane's web binding (profile + URL) plus
// the given prompt as a named recipe. The pane must have web mode enabled so
// the recipe records which authenticated profile it runs against.
func (w *WorkspaceActor) cmdWebAutoSave(out *strings.Builder, paneID, name, prompt string) {
	snap, ok := w.requestPaneSnapshot(paneID)
	if !ok || snap.WebProfile == "" {
		fmt.Fprintf(out, "\n[web] the active pane has no web mode — run `##mode new web [--profile N] [url]` first,\n")
		fmt.Fprintf(out, "[web] then save: the recipe records that pane's profile and URL\n")
		return
	}
	a := &webauto.Automation{
		Name:    name,
		Profile: snap.WebProfile,
		URL:     snap.WebURL,
		Prompt:  prompt,
	}
	if err := w.webAutoStore().Save(a); err != nil {
		fmt.Fprintf(out, "\n[web] save failed: %v\n", err)
		w.failRysh("save failed: %v", err)
		return
	}
	fmt.Fprintf(out, "\n[web] saved automation %q (profile %s, url %s)\n",
		webauto.SanitizeName(name), a.Profile, orDefault(a.URL, "(none)"))
	fmt.Fprintf(out, "[web] run it with: ##auto web run %s [args...]\n", webauto.SanitizeName(name))
	fmt.Fprintf(out, "[web] edit %s/%s.md to add description/args or refine the prompt\n",
		w.webAutoStore().Dir(), webauto.SanitizeName(name))
}

// webAutoRunOverrides carries per-run budget flags from `##auto web run` that
// override the recipe frontmatter. A zero field means "not set — use the recipe".
type webAutoRunOverrides struct {
	stepInterval   int
	maxIterations  int
	maxDuration    time.Duration
	budgetSizeToks int
	takeoverWhen   int
	// dryRun prints the fully-resolved plan (prompt, budget, loop, paths)
	// without dispatching anything.
	dryRun bool
	// While-loop run flags (item 8): flags > recipe > config.
	passes         int           // --passes: outer pass cap
	whileDuration  time.Duration // --while-duration: outer wall-clock total
	whileBudgetTok int           // --while-budget: outer token total
	noLoop         bool          // --no-loop: run the do-step once, loop off
	// each is the --each fan-out queue: one run per item, sequential.
	each []string
	// Recording run flags (web kind only): flags > recipe > config. record is
	// the --record tier, folded with the recipe/config tiers by
	// webauto.ResolveRecord — see RecordFlags for why on/off are separate.
	record webauto.RecordFlags
	// fromQueue marks a queue-driven re-dispatch (never a CLI flag): the run's
	// supersede step must NOT clobber the queue that is driving it, and the
	// run is supervised so its completion advances that queue.
	fromQueue bool
}

// parseWebAutoRunFlags extracts the web-only flags (--headless, --record /
// --no-record / --recording-path / --record-interval) and the budget-override
// flags (--step-interval / --max-iterations / --max-duration / --budget-size /
// --takeover-when, each accepting "--flag value" or "--flag=value"), returning
// the recipe name, its runtime args, and the parsed overrides. Unrecognised
// tokens are positional.
func parseWebAutoRunFlags(raw []string) (name string, runArgs []string, headless bool, ov webAutoRunOverrides) {
	name, runArgs, headless, _, ov = parseAutoRunFlags(raw, "", true)
	return name, runArgs, headless, ov
}

// parseAutoRunFlags is the shared ##auto run-flag parser. targetFlag, when
// non-empty (e.g. "--agent"), captures a per-run target override. webKind
// gates the flags that only make sense with a browser attached (--headless and
// the --record family); the other kinds treat those tokens as positional.
// Budget flags accept "--flag value" or "--flag=value"; unrecognised tokens are
// positional (recipe name first, then its runtime args).
func parseAutoRunFlags(raw []string, targetFlag string, webKind bool) (name string, runArgs []string, headless bool, target string, ov webAutoRunOverrides) {
	positional := make([]string, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		a := raw[i]
		key, inlineVal, hasInline := a, "", false
		if strings.HasPrefix(a, "--") {
			if eq := strings.IndexByte(a, '='); eq >= 0 {
				key, inlineVal, hasInline = a[:eq], a[eq+1:], true
			}
		}
		next := func() string {
			if hasInline {
				return inlineVal
			}
			if i+1 < len(raw) {
				i++
				return raw[i]
			}
			return ""
		}
		switch key {
		case "--headless":
			if webKind {
				headless = true
			} else {
				positional = append(positional, a)
			}
		case "--record":
			if webKind {
				ov.record.On = true
			} else {
				positional = append(positional, a)
			}
		case "--no-record":
			if webKind {
				ov.record.Off = true
			} else {
				positional = append(positional, a)
			}
		case "--recording-path", "--record-path":
			if !webKind {
				positional = append(positional, a)
				continue
			}
			// The ##-command tokenizer keeps literal quotes around values, so
			// strip them the way --each does.
			if v := strings.Trim(strings.TrimSpace(next()), `"'`); v != "" {
				ov.record.Path = v
			}
		case "--record-interval":
			if !webKind {
				positional = append(positional, a)
				continue
			}
			if d, err := time.ParseDuration(strings.TrimSpace(next())); err == nil && d > 0 {
				ov.record.Interval = d
			}
		case "--step-interval", "--steps":
			if n, err := strconv.Atoi(strings.TrimSpace(next())); err == nil && n > 0 {
				ov.stepInterval = n
			}
		case "--max-iterations", "--max-iters":
			if n, err := strconv.Atoi(strings.TrimSpace(next())); err == nil && n > 0 {
				ov.maxIterations = n
			}
		case "--max-duration":
			if d, err := time.ParseDuration(strings.TrimSpace(next())); err == nil && d > 0 {
				ov.maxDuration = d
			}
		case "--budget-size":
			if toks, ok := webauto.ParseBudgetSize(next()); ok {
				ov.budgetSizeToks = toks
			}
		case "--takeover-when":
			if n, err := strconv.Atoi(strings.TrimSpace(next())); err == nil && n > 0 {
				ov.takeoverWhen = n
			}
		case "--dry-run":
			ov.dryRun = true
		case "--no-loop":
			ov.noLoop = true
		case "--passes":
			if n, err := strconv.Atoi(strings.TrimSpace(next())); err == nil && n > 0 {
				ov.passes = n
			}
		case "--while-duration":
			if d, err := time.ParseDuration(strings.TrimSpace(next())); err == nil && d > 0 {
				ov.whileDuration = d
			}
		case "--while-budget":
			if toks, ok := webauto.ParseBudgetSize(next()); ok {
				ov.whileBudgetTok = toks
			}
		case "--each":
			// The ##-command tokenizer keeps literal quotes (--each "a,b" arrives
			// as `"a,b"`), so strip them from the value and every item.
			val := strings.Trim(strings.TrimSpace(next()), `"'`)
			for _, item := range strings.Split(val, ",") {
				if item = strings.Trim(strings.TrimSpace(item), `"'`); item != "" {
					ov.each = append(ov.each, item)
				}
			}
		default:
			if targetFlag != "" && key == targetFlag {
				if v := strings.TrimSpace(next()); v != "" {
					target = v
				}
				continue
			}
			positional = append(positional, a)
		}
	}
	if len(positional) > 0 {
		name, runArgs = positional[0], positional[1:]
	}
	return name, runArgs, headless, target, ov
}

// armRecipeBudget computes the effective PER-PASS run budget (recipe + while
// redistribution + flag overrides + the takeover split) and sends
// MsgSetRunBudget to the target's LLM actor — execID is a pane ID, or an
// agent/humanoid name (their LLM actors listen on the same
// rysh.pane.{id}.llm_prompt_execution.inbox subject). defStep/defLoop are the
// kind's config-level defaults (automation.<kind>.step / .loop). It returns
// the armed budget, the task/takeover shares, the substituted takeover
// prompt, and the resolved while-loop spec. Shared by run, resume, continue,
// and the loop's per-pass re-arm of every kind.
func (w *WorkspaceActor) armRecipeBudget(execID string, a *webauto.Automation, runtimeArgs []string, outputDir string, ov webAutoRunOverrides, defStep *webauto.StepConfig, defLoop *webauto.LoopConfig) (budget, task, fin webauto.RunBudget, takeover string, loop webauto.LoopSpec) {
	budget, task, fin, takeover, loop = w.resolveRecipePlan(a, runtimeArgs, outputDir, ov, defStep, defLoop)
	_ = w.pub.Send(msg.T("pane", execID, "llm_prompt_execution", "inbox"),
		&msg.MsgSetRunBudget{
			AutoContinue:              budget.AutoContinue,
			AutoApprove:               budget.AutoApprove,
			MaxTotalIterations:        task.MaxIterations,
			MaxDurationMs:             task.MaxDuration.Milliseconds(),
			StepInterval:              budget.StepInterval,
			MaxContextTokens:          task.MaxContextTokens,
			FinalizerPrompt:           takeover,
			FinalizerMaxIterations:    fin.MaxIterations,
			FinalizerMaxDurationMs:    fin.MaxDuration.Milliseconds(),
			FinalizerMaxContextTokens: fin.MaxContextTokens,
			// Model/effort seats: executor (main legs) + finalizer leg.
			Model:           budget.Model,
			Effort:          budget.Effort,
			FinalizerModel:  budget.FinalizerModel,
			FinalizerEffort: budget.FinalizerEffort,
			// Restated in every synthetic continue/finalizer turn so the save
			// location survives context compaction on long runs.
			OutputDir: outputDir,
		})
	return budget, task, fin, takeover, loop
}

// resolveRecipePlan computes everything armRecipeBudget would arm — the
// per-pass budget (with while redistribution and flag overrides), the
// task/takeover split, the substituted takeover prompt, and the loop spec —
// WITHOUT sending anything. --dry-run prints from this.
func (w *WorkspaceActor) resolveRecipePlan(a *webauto.Automation, runtimeArgs []string, outputDir string, ov webAutoRunOverrides, defStep *webauto.StepConfig, defLoop *webauto.LoopConfig) (budget, task, fin webauto.RunBudget, takeover string, loop webauto.LoopSpec) {
	// Per-field precedence: run flags > while redistribution > recipe
	// frontmatter (loop.do wins over step) > config defaults > built-ins.
	def := webauto.EffectiveStepDef(defStep, defLoop)
	budget = a.ResolveRunBudgetWith(def)
	loop, budget = a.ResolveWhileWithFlags(defLoop, budget, whileFlagOverrides(ov))
	if ov.stepInterval > 0 {
		budget.StepInterval = ov.stepInterval
	}
	if ov.maxIterations > 0 {
		budget.MaxIterations = ov.maxIterations
	}
	if ov.maxDuration > 0 {
		budget.MaxDuration = ov.maxDuration
	}
	if ov.budgetSizeToks > 0 {
		budget.MaxContextTokens = ov.budgetSizeToks
	}
	if ov.takeoverWhen > 0 {
		budget.TakeoverWhen = ov.takeoverWhen
	}
	// Substitute {{args}}/{{output_dir}} into the takeover prompt so it can
	// reference the same results folder when it wraps up a partial run.
	if tp := a.TakeoverPromptWith(def); strings.TrimSpace(tp) != "" {
		takeover = webauto.SubstituteArgs(&webauto.Automation{Args: a.Args, Prompt: tp}, runtimeArgs)
		takeover = strings.ReplaceAll(takeover, "{{output_dir}}", outputDir)
		takeover = strings.ReplaceAll(takeover, "{{results_dir}}", outputDir)
	}
	// With a takeover prompt, the task runs against takeover_when% of each
	// ceiling and the takeover leg gets the remainder — raised to a comfortable
	// floor when the reserved slice is too thin to finish a graceful wrap-up.
	// Without one the task gets the full budget.
	task = budget
	if takeover != "" {
		task, fin = budget.SplitForTakeover()
		fin = fin.WithTakeoverFloor()
	}
	return budget, task, fin, takeover, loop
}

// reportRunBudget prints the budget summary footer shared by run/resume of
// every kind; label is the output prefix (web/task/agent/humanoid).
func reportRunBudget(out *strings.Builder, label string, budget, task, fin webauto.RunBudget, takeover string) {
	if budget.AutoContinue {
		fmt.Fprintf(out, "[%s] budget: ~%d steps / %s / %d tokens, %d steps per leg\n",
			label, budget.MaxIterations, budget.MaxDuration, budget.MaxContextTokens, budget.StepInterval)
		if takeover != "" {
			fmt.Fprintf(out, "[%s] takeover: at %d%% consumed — task ~%d steps / %s / %d tokens, then wrap-up gets ~%d steps / %s / %d tokens\n",
				label, budget.TakeoverWhen, task.MaxIterations, task.MaxDuration, task.MaxContextTokens, fin.MaxIterations, fin.MaxDuration, fin.MaxContextTokens)
		}
	} else {
		fmt.Fprintf(out, "[%s] auto-continue: off (pauses at the step cap — type 'continue' to resume)\n", label)
	}
}

// cmdWebAutoRun executes a recipe on the ACTIVE pane: (re)binds its web mode to
// the recipe's profile/URL, arms the auto-continue budget, then dispatches the
// substituted prompt to the pane's Ask Rysh AI. contextPrefix, when non-empty
// (##auto web resume), is prepended to the prompt to seed prior results.
func (w *WorkspaceActor) cmdWebAutoRun(out *strings.Builder, paneID, name string, runtimeArgs []string, headless bool, ov webAutoRunOverrides, contextPrefix string) {
	a, err := w.webAutoStore().Load(name)
	if err != nil {
		fmt.Fprintf(out, "\n[web] automation %q not found — ##auto web list\n", name)
		w.failRysh("automation %q not found — ##auto web list", name)
		return
	}

	profile := browserinstance.SanitizeProfile(a.Profile)
	if _, err := browserinstance.EnsureProfile(w.browserWorkDir(), profile); err != nil {
		fmt.Fprintf(out, "\n[web] could not prepare browser profile %q: %v\n", profile, err)
		w.failRysh("could not prepare browser profile %q: %v", profile, err)
		return
	}

	// --each: the first item runs now (as the run's {{args}}), the rest queue.
	if len(ov.each) > 0 {
		runtimeArgs = []string{ov.each[0]}
	}
	// Resolve and pre-create the recipe's results folder so the prompt can write
	// into it via the {{output_dir}} placeholder without a mkdir round-trip.
	outputDir := w.webAutoStore().ResolveOutputDir(a)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		fmt.Fprintf(out, "\n[web] could not prepare results dir %q: %v\n", outputDir, err)
		w.failRysh("could not prepare results dir %q: %v", outputDir, err)
		return
	}

	prompt := webauto.SubstituteArgs(a, runtimeArgs)
	prompt = strings.ReplaceAll(prompt, "{{output_dir}}", outputDir)
	prompt = strings.ReplaceAll(prompt, "{{results_dir}}", outputDir)
	// web_read steers HOW the AI observes pages: text (DOM extraction) or
	// screenshot (captures are delivered to the model as visible images).
	if guidance, ok := webReadGuidance(a.WebRead); !ok {
		fmt.Fprintf(out, "\n[web] web_read %q is not text|screenshot — using text\n", a.WebRead)
	} else if guidance != "" {
		prompt = guidance + "\n\n" + prompt
	}
	if contextPrefix != "" {
		prompt = contextPrefix + "\n\n" + prompt
	}

	// --dry-run: print the fully-resolved plan and change nothing.
	if ov.dryRun {
		w.printDryRun(out, "web", a, runtimeArgs, "", "", outputDir, prompt, ov, w.cfg.Automation.Web.Step, w.cfg.Automation.Web.Loop)
		return
	}

	// A fresh run supersedes any loop supervising this pane (last run wins;
	// startAutoLoop re-registers when the new recipe also loops). A queue-driven
	// re-dispatch must NOT clobber the very queue advancing it.
	keepQueue := w.autoQueues[paneID]
	w.stopAutoLoop(paneID)
	if ov.fromQueue && keepQueue != nil {
		if w.autoQueues == nil {
			w.autoQueues = map[string]*autoQueue{}
		}
		w.autoQueues[paneID] = keepQueue
	}

	// Arm the pane's auto-continue budget BEFORE the prompt (fresh budget each
	// run/resume), so a long browsing run resumes itself past the per-leg cap.
	budget, task, fin, finalizer, loopSpec := w.armRecipeBudget(paneID, a, runtimeArgs, outputDir, ov, w.cfg.Automation.Web.Step, w.cfg.Automation.Web.Loop)

	// (Re)bind the pane to the recipe's browser context. --headless routes to the
	// CLI-owned headless executor; otherwise the desktop app's embedded browser.
	if headless {
		_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneWebHeadless{
			PaneID: paneID, Op: "on", Profile: profile, URL: normalizeWebURL(a.URL),
		})
	} else {
		_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneEnableMode{
			PaneID: paneID, Mode: "web", WebProfile: profile, WebURL: normalizeWebURL(a.URL),
		})
		// Deterministic entry page: an EXISTING embedded pane keeps whatever
		// page the previous run left, so navigate it to the recipe's
		// frontmatter URL BEFORE the first prompt — the agent should never
		// spend its opening rounds (and tokens) getting to the start page.
		// A freshly created pane has no browser.request subscriber yet, so
		// this is dropped harmlessly — creation itself loads the URL. The
		// headless branch above restarts onto the URL already; `continue`
		// deliberately does NOT navigate (it must stay on the checkpointed
		// page, e.g. a mid-publish draft).
		if navParams, err := json.Marshal(map[string]string{"url": normalizeWebURL(a.URL)}); err == nil {
			_ = w.pub.SendBrowserActionRequest(paneID, &msg.MsgBrowserActionRequest{
				RequestID: uuid.NewString(),
				Action:    "navigate",
				Params:    navParams,
			})
		}
	}

	// Human bubble in the Ask Rysh panel, then the prompt itself.
	_ = w.pub.Send(msg.T("pane", paneID, "web", "prompt"),
		&msg.MsgWebPromptDispatched{PaneID: paneID, Prompt: prompt})
	_ = w.pub.Send(msg.T("pane", paneID, "inbox"),
		&msg.MsgPaneSubmitInput{Text: prompt, Mode: "prompt"})
	w.registerAutoRun("web", a.Name, paneID, runtimeArgs, headless)

	verb := "running"
	if contextPrefix != "" {
		verb = "resuming"
	}
	fmt.Fprintf(out, "\n[web] %s automation %q (profile %s", verb, a.Name, profile)
	if a.URL != "" {
		fmt.Fprintf(out, ", %s", a.URL)
	}
	if headless {
		fmt.Fprintf(out, ", headless")
	}
	fmt.Fprintf(out, ")\n")
	if len(runtimeArgs) > 0 {
		fmt.Fprintf(out, "[web] args: %s\n", strings.Join(runtimeArgs, " "))
	}
	fmt.Fprintf(out, "[web] results dir: %s (view: ##auto web results %s)\n", outputDir, webauto.SanitizeName(name))
	if budget.AutoApprove {
		fmt.Fprintf(out, "[web] auto-approve: on (tool calls run without the approval dialog)\n")
	}
	reportRunBudget(out, "web", budget, task, fin, finalizer)
	fmt.Fprintf(out, "[web] progress streams to this pane's Ask Rysh chat; double-Ctrl+C pauses\n")

	// Supervision: an enabled while-loop (judge → iterate), or chain-watch for
	// on_success / notify / --each completion handling on plain runs. Iterate
	// passes reuse the already-bound browser context — no re-binding.
	w.registerEachQueue(out, "web", a.Name, paneID, paneID, headless, ov)
	_, queued := w.autoQueues[paneID]
	supervised := loopSpec.Enabled || queued ||
		strings.TrimSpace(a.OnSuccess) != "" || a.Notify != nil

	// Recording (--record) covers the WHOLE run, including every pass of a
	// loop — one video per run, not per pass. A supervised run is therefore
	// stopped from handleAutoRunDone when the supervisor finishes, not when an
	// individual pass ends.
	w.startWebRecorder(out, a, paneID, outputDir, ov, supervised)

	if supervised {
		w.startAutoLoop(out, "web", a.Name, paneID, paneID, outputDir, a, runtimeArgs, loopSpec, budget,
			w.loopRearmFunc(paneID, a, runtimeArgs, outputDir, ov, w.cfg.Automation.Web.Step, w.cfg.Automation.Web.Loop),
			w.loopPaneDispatchFunc(paneID))
	}
}

// cmdWebAutoResume starts a fresh run (fresh budget) but seeds the context with
// the recipe's latest saved result, so the AI continues building on what a prior
// run produced instead of starting from scratch.
func (w *WorkspaceActor) cmdWebAutoResume(out *strings.Builder, paneID, name string, runtimeArgs []string, headless bool, ov webAutoRunOverrides) {
	a, err := w.webAutoStore().Load(name)
	if err != nil {
		fmt.Fprintf(out, "\n[web] automation %q not found — ##auto web list\n", name)
		w.failRysh("automation %q not found — ##auto web list", name)
		return
	}
	dir := w.webAutoStore().ResolveOutputDir(a)
	prefix := ""
	if fname, content, ok := latestResultFile(dir); ok {
		fmt.Fprintf(out, "\n[web] resuming from the latest result: %s (%d bytes loaded into context)\n", fname, len(content))
		// Name the FULL absolute path (not just the basename) so the save
		// location survives even if the recipe prompt is later compacted away.
		prefix = fmt.Sprintf("[Resuming a previous run] You already produced the results below (from %q). "+
			"Continue the SAME task: build on this, add only NEW items, avoid duplicates, and save the updated "+
			"list back to the same results file at this exact path: %s\n\n--- previously saved results ---\n%s\n--- end of previous results ---",
			fname, filepath.Join(dir, fname), string(content))
	} else {
		fmt.Fprintf(out, "\n[web] no prior results found in %s — resuming as a fresh run\n", dir)
	}
	w.cmdWebAutoRun(out, paneID, name, runtimeArgs, headless, ov, prefix)
}

// cmdWebAutoContinue re-arms the recipe's budget and resumes a paused run from
// its last checkpoint (after a cancel / stop). It does NOT re-bind the browser,
// so the run picks up on whatever page it was on. If nothing is paused it's a
// no-op (the LLM actor reports "nothing to continue").
func (w *WorkspaceActor) cmdWebAutoContinue(out *strings.Builder, paneID, name string, runtimeArgs []string, ov webAutoRunOverrides) {
	a, err := w.webAutoStore().Load(name)
	if err != nil {
		fmt.Fprintf(out, "\n[web] automation %q not found — ##auto web list\n", name)
		w.failRysh("automation %q not found — ##auto web list", name)
		return
	}
	// Fail-closed (design 013): resuming an automation restarts the tool loop
	// with no new prompt, so it must consult the gate here.
	if reason, blocked := policy.Blocked(); blocked {
		fmt.Fprint(out, policy.BlockedMessage(reason))
		return
	}
	w.stopAutoLoop(paneID) // manual continue supersedes any active loop
	outputDir := w.webAutoStore().ResolveOutputDir(a)
	_ = os.MkdirAll(outputDir, 0o755)
	// Re-arm the budget so the resumed run auto-continues again (a cancel disarmed
	// it). The MsgSetRunBudget arrives before the continue on the same inbox.
	budget, task, fin, finalizer, _ := w.armRecipeBudget(paneID, a, runtimeArgs, outputDir, ov, w.cfg.Automation.Web.Step, w.cfg.Automation.Web.Loop)
	_ = w.pub.Send(msg.T("pane", paneID, "llm_prompt_execution", "inbox"), &msg.MsgAgenticContinue{})
	// Keep the prior entry's headless flag: continue doesn't rebind the browser.
	w.registerAutoRun("web", a.Name, paneID, runtimeArgs, w.autoRuns[paneID].Headless)

	fmt.Fprintf(out, "\n[web] continuing %q from its last checkpoint (budget re-armed)\n", a.Name)
	reportRunBudget(out, "web", budget, task, fin, finalizer)
	fmt.Fprintf(out, "[web] if nothing was paused this is a no-op — use ##auto web resume to start fresh from the latest saved result\n")
}

// latestResultFile returns the newest (by mtime) regular file in dir, its
// contents (capped), and ok=false when the folder is missing/empty.
func latestResultFile(dir string) (name string, content []byte, ok bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, false
	}
	var newest os.DirEntry
	var newestMod time.Time
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// The loop's run report lives in the results dir but must never seed
		// resume/judge/iterate passes.
		if strings.HasPrefix(e.Name(), "run-report") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if newest == nil || info.ModTime().After(newestMod) {
			newest, newestMod = e, info.ModTime()
		}
	}
	if newest == nil {
		return "", nil, false
	}
	data, err := os.ReadFile(filepath.Join(dir, newest.Name()))
	if err != nil {
		return "", nil, false
	}
	const cap = 48 * 1024
	if len(data) > cap {
		data = data[:cap]
	}
	return newest.Name(), data, true
}

// cmdWebAutoResults lists a web recipe's saved results (see renderAutoResults).
func (w *WorkspaceActor) cmdWebAutoResults(out *strings.Builder, name, file string) {
	renderAutoResults(out, "web", w.webAutoStore(), name, file)
}

// renderAutoResults lists the files a recipe has saved to its results dir
// (newest first), or prints one file when a filename is given — shared by
// `##auto <kind> results` of every kind (label is the output prefix). It
// resolves the dir from the recipe's output_dir when the recipe still exists,
// and otherwise falls back to the default <recipe-dir>/<name>/results so
// results remain viewable after a recipe is deleted.
func renderAutoResults(out *strings.Builder, label string, store *webauto.Store, name, file string) {
	var dir string
	if a, err := store.Load(name); err == nil {
		dir = store.ResolveOutputDir(a)
	} else {
		san := webauto.SanitizeName(name)
		if san == "" {
			fmt.Fprintf(out, "\n[%s] invalid automation name %q\n", label, name)
			return
		}
		dir = filepath.Join(store.Dir(), san, "results")
	}

	// Print a single result file.
	if file != "" {
		// SanitizeName strips separators and traversal, so the join stays inside dir.
		safe := webauto.SanitizeName(file)
		if safe == "" {
			fmt.Fprintf(out, "\n[%s] invalid result file %q\n", label, file)
			return
		}
		path := filepath.Join(dir, safe)
		data, err := os.ReadFile(path)
		if err != nil {
			// renderAutoResults is a free function with no actor to report to;
			// its caller marks the failure.
			fmt.Fprintf(out, "\n[%s] cannot read result %q: %v\n", label, safe, err)
			fmt.Fprintf(out, "[%s] list results: ##auto %s results %s\n", label, label, name)
			return
		}
		const cap = 64 * 1024
		truncated := false
		if len(data) > cap {
			data = data[:cap]
			truncated = true
		}
		fmt.Fprintf(out, "\n[%s] %s\n", label, path)
		fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
		out.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			out.WriteByte('\n')
		}
		if truncated {
			fmt.Fprintf(out, "… (truncated at %d bytes)\n", cap)
		}
		fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
		return
	}

	// List the results folder, newest first.
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(out, "\n[%s] no results yet for %q\n", label, name)
		fmt.Fprintf(out, "[%s] results dir: %s\n", label, dir)
		return
	}
	type resFile struct {
		name string
		size int64
		mod  time.Time
	}
	var files []resFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, resFile{e.Name(), info.Size(), info.ModTime()})
	}
	if len(files) == 0 {
		fmt.Fprintf(out, "\n[%s] no result files yet for %q\n", label, name)
		fmt.Fprintf(out, "[%s] results dir: %s\n", label, dir)
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	fmt.Fprintf(out, "\n[%s] results for %q (%d file%s)\n", label, name, len(files), plural(len(files)))
	fmt.Fprintf(out, "  %s\n", dir)
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	for _, f := range files {
		fmt.Fprintf(out, "  %-34s %9s  %s\n", f.name, humanBytes(f.size), f.mod.Format("2006-01-02 15:04"))
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	fmt.Fprintf(out, "[%s] view one: ##auto %s results %s <file>\n", label, label, name)
}

// humanBytes formats a byte count as B/KB/MB for the results listing.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// plural returns "s" when n != 1.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// orDefault returns s, or def when s is empty.
func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// indentWebPrompt indents every non-empty line of a recipe prompt for display.
func indentWebPrompt(text, prefix string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}
