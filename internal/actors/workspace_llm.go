// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	sharedprovider "github.com/rysh-ai/rysh-cli-shared/provider"

	"github.com/rysh-ai/rysh-cli-code/internal/llms"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// ##llm (alias: ##model) — the session's LLM model surface, backed by the
// .rysh/llms registry (one YAML file per model at
// <rysh-dir>/llms/<provider>/<model-name>).
//
//	##llm | ##llm list             list providers/models (from .rysh/llms only)
//	##llm add <p>/<name> [id]      declare a new model file (alias: model add)
//	##llm use <provider>/<name>    set the session default model (persists the
//	                               file when it doesn't exist yet)
//	##llm enable <provider>/<name> allow the model to be activated
//	##llm disable <p>/<name>       park the model: listed, but not activatable
//	##llm info <provider>/<name>   show a model file's properties
//	##llm status                   show the model in effect (alias: default)
//	##llm <provider>/<name>        shorthand for `use`
//	##llm clear                    drop the session override (back to config)
//
// Per-pane selection lives next door in `##pane model <provider>/<name>`,
// which reads the same registry but binds only the active pane.
//
// Activation flips the SessionLLM holder on the agentic Setup, which the
// session provider decorator consults per call — every pane/agent picks the
// new default up on its next request. Explicit seats (recipe step/judge
// models, automation defaults) still win over the session default.
func (w *WorkspaceActor) handleLLMCommand(out *strings.Builder, paneID string, args []string) {
	store := llms.NewStore(w.cfg.RyshDir)
	if err := store.SeedIfEmpty(); err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "\n[llm] registry error at %s: %v\n", store.Dir(), err)
		return
	}

	if len(args) == 0 || args[0] == "list" {
		w.cmdLLMList(out, store)
		return
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("llm", "##llm add <provider>/<model-name> [api-model-id]")
			w.failRyshUsage("usage: %s", "##llm add <provider>/<model-name> [api-model-id]")
			return
		}
		modelID := ""
		if len(args) >= 3 {
			modelID = args[2]
		}
		w.cmdLLMModelAdd(out, store, args[1], modelID)
	case "use":
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("llm", "##llm use <provider>/<model-name>")
			w.failRyshUsage("usage: %s", "##llm use <provider>/<model-name>")
			return
		}
		w.cmdLLMUse(out, store, args[1])
	case "enable", "disable":
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("llm", fmt.Sprintf("##llm %s <provider>/<model-name>", args[0]))
			w.failRyshUsage("usage: %s", fmt.Sprintf("##llm %s <provider>/<model-name>", args[0]))
			return
		}
		w.cmdLLMSetEnabled(out, store, args[1], args[0] == "enable")
	case "info":
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("llm", "##llm info <provider>/<model-name>")
			w.failRyshUsage("usage: %s", "##llm info <provider>/<model-name>")
			return
		}
		w.cmdLLMInfo(out, store, args[1])
	case "select", "pick", "menu":
		w.cmdLLMSelect(out, paneID, store, args[1:])
	case "status", "default":
		w.cmdLLMStatus(out)
	case "scopes", "tree":
		w.cmdModelScopes(out, paneID)
	case "clear":
		if w.agSetup != nil && w.agSetup.SessionLLM != nil {
			w.agSetup.SessionLLM.Set("", "")
		}
		w.sessionLLMRef = ""
		fmt.Fprintf(out, "\n[llm] session override cleared — back to config default (%s)\n", w.configuredModelLabel())
	case "model":
		// Back-compat alias: ##llm model add <p>/<name> [id].
		if len(args) >= 3 && args[1] == "add" {
			modelID := ""
			if len(args) >= 4 {
				modelID = args[3]
			}
			w.cmdLLMModelAdd(out, store, args[2], modelID)
			return
		}
		ryshWriter(out).UsageLineIn("llm", "##llm add <provider>/<model-name> [api-model-id]")
		w.failRyshUsage("usage: %s", "##llm add <provider>/<model-name> [api-model-id]")
	case "help":
		w.llmUsage(out)
	default:
		// `##llm anthropic/fable5` — direct activation shorthand.
		if strings.Contains(args[0], "/") {
			w.cmdLLMUse(out, store, args[0])
			return
		}
		w.llmUsage(out)
		w.failRyshUsage("unknown ##llm subcommand: %q", args[0])
	}
}

func (w *WorkspaceActor) llmUsage(out *strings.Builder) {
	fmt.Fprintf(out, "\n[llm] usage (##model is an alias for ##llm):\n")
	fmt.Fprintf(out, "  ##llm | ##llm list                    list providers and models (.rysh/llms)\n")
	fmt.Fprintf(out, "  ##llm add <provider>/<name> [api-model-id]  declare a new model file\n")
	fmt.Fprintf(out, "  ##llm use <provider>/<name>           set the session default model (also: ##llm <provider>/<name>)\n")
	fmt.Fprintf(out, "  ##llm select [n]                      pick a model from a numbered menu (also: pick, menu)\n")
	fmt.Fprintf(out, "  ##llm enable <provider>/<name>        allow the model to be activated\n")
	fmt.Fprintf(out, "  ##llm disable <provider>/<name>       park the model: still listed, not activatable\n")
	fmt.Fprintf(out, "  ##llm info <provider>/<name>          show a model's properties\n")
	fmt.Fprintf(out, "  ##llm status                          show the model in effect (alias: default)\n")
	fmt.Fprintf(out, "  ##llm scopes                          show the whole hierarchy and which level wins\n")
	fmt.Fprintf(out, "  ##llm clear                           drop the session override\n")
	fmt.Fprintf(out, "\n[llm] the same `model` subcommand exists at every scope (narrower wins):\n")
	fmt.Fprintf(out, "  ##session model | ##workspace model | ##tab model | ##lane model | ##stack model | ##pane model\n")
}

// cmdLLMSetEnabled turns one registry entry's activatability on or off.
// Disabling the model currently in effect must not leave the session running
// on it, so the session override is dropped in the same breath.
func (w *WorkspaceActor) cmdLLMSetEnabled(out *strings.Builder, store *llms.Store, ref string, enable bool) {
	providerName, modelName, err := llms.ParseRef(ref)
	if err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "\n[llm] %v\n", err)
		return
	}
	spec, err := store.SetDisabled(providerName, modelName, !enable)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(out, "\n[llm] %s not found — declare it with: ##llm add %s [api-model-id]\n", ref, ref)
			w.failRysh("%s not found — declare it with: ##llm add %s [api-model-id]", ref, ref)
			return
		}
		fmt.Fprintf(out, "\n[llm] cannot update %s: %v\n", ref, err)
		w.failRysh("cannot update %s: %v", ref, err)
		return
	}
	full := providerName + "/" + spec.Name
	if enable {
		fmt.Fprintf(out, "\n[llm] %s enabled — activate with: ##llm use %s\n", full, full)
		return
	}
	// NOT a failure: the disable worked. The sentence merely explains what the
	// model can no longer be used for, and "refuse" in it is what made the
	// mechanical pass mark this line.
	fmt.Fprintf(out, "\n[llm] %s disabled — still listed, but ##llm use / ##pane model will refuse it\n", full)
	if w.sessionLLMRef == full {
		if w.agSetup != nil && w.agSetup.SessionLLM != nil {
			w.agSetup.SessionLLM.Set("", "")
		}
		w.sessionLLMRef = ""
		fmt.Fprintf(out, "[llm] it was the session default — reverted to %s\n", w.configuredModelLabel())
	}
}

// llmSeatSummary renders the CONFIG-EFFECTIVE automation seats (post
// applyAutomationLLMDefaults, so config overrides show through — not the raw
// built-ins). All kinds share the same seeded seats unless config diverges;
// the web kind is shown as representative.
func (w *WorkspaceActor) llmSeatSummary() string {
	doModel, judgeModel := webauto.DefaultStepModel, webauto.DefaultJudgeModel
	if s := webauto.EffectiveStepDef(w.cfg.Automation.Web.Step, w.cfg.Automation.Web.Loop); s != nil && s.Model != "" {
		doModel = s.Model
	}
	if l := w.cfg.Automation.Web.Loop; l != nil && l.While != nil && l.While.Model != "" {
		judgeModel = l.While.Model
	}
	return fmt.Sprintf("do/step %s · judge %s", doModel, judgeModel)
}

// cmdLLMStatus answers "which model is in effect right now?": the session
// override (##llm use) when set, otherwise the config/built-in default, plus
// the automation seats that outrank both.
func (w *WorkspaceActor) cmdLLMStatus(out *strings.Builder) {
	fmt.Fprintf(out, "\n[llm] status\n")
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	model, effort := "", ""
	if w.agSetup != nil && w.agSetup.SessionLLM != nil {
		model, effort = w.agSetup.SessionLLM.Get()
	}
	if model != "" || effort != "" {
		ref := w.sessionLLMRef
		if ref == "" {
			ref = "(session override)"
		}
		fmt.Fprintf(out, "  session default : %s → %s", ref, model)
		if effort != "" {
			fmt.Fprintf(out, " (effort %s)", effort)
		}
		fmt.Fprintf(out, "  [##llm clear to reset]\n")
	} else {
		fmt.Fprintf(out, "  session default : %s (no ##llm override)\n", w.configuredModelLabel())
	}
	fmt.Fprintf(out, "  loop seats      : %s\n", w.llmSeatSummary())
	fmt.Fprintf(out, "  precedence      : recipe > automation config > pane > stack > lane > tab > workspace > ##llm session > provider.model > built-in\n")
	fmt.Fprintf(out, "  [##llm scopes shows the hierarchy for the active pane]\n")
}

// cmdLLMInfo prints one model file's properties plus its runtime standing
// (path, executability, whether it is the active session default).
func (w *WorkspaceActor) cmdLLMInfo(out *strings.Builder, store *llms.Store, ref string) {
	providerName, modelName, err := llms.ParseRef(ref)
	if err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "\n[llm] %v\n", err)
		return
	}
	spec, err := store.Get(providerName, modelName)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(out, "\n[llm] %s not found — declare it with: ##llm add %s [api-model-id]\n", ref, ref)
			w.failRysh("%s not found — declare it with: ##llm add %s [api-model-id]", ref, ref)
			return
		}
		fmt.Fprintf(out, "\n[llm] cannot read %s: %v\n", ref, err)
		w.failRysh("cannot read %s: %v", ref, err)
		return
	}
	fmt.Fprintf(out, "\n[llm] %s/%s\n", providerName, spec.Name)
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
	fmt.Fprintf(out, "  file        : %s\n", store.Path(providerName, modelName))
	fmt.Fprintf(out, "  provider    : %s\n", spec.Provider)
	fmt.Fprintf(out, "  model id    : %s\n", spec.Model)
	if spec.Description != "" {
		fmt.Fprintf(out, "  description : %s\n", spec.Description)
	}
	if spec.MaxTokens > 0 {
		fmt.Fprintf(out, "  max_tokens  : %d\n", spec.MaxTokens)
	}
	if spec.Effort != "" {
		fmt.Fprintf(out, "  effort      : %s\n", spec.Effort)
	}
	if spec.Added != "" {
		fmt.Fprintf(out, "  added       : %s\n", spec.Added)
	}
	if spec.Disabled {
		fmt.Fprintf(out, "  enabled     : no (##llm enable %s/%s)\n", providerName, spec.Name)
	} else {
		fmt.Fprintf(out, "  enabled     : yes\n")
	}
	if spec.Executable() {
		fmt.Fprintf(out, "  executable  : yes\n")
	} else {
		fmt.Fprintf(out, "  executable  : no (rysh has no executor for provider %q; runnable: %s)\n",
			providerName, strings.Join(llms.ExecutableProviders, ", "))
	}
	if w.sessionLLMRef == providerName+"/"+spec.Name {
		fmt.Fprintf(out, "  active      : yes — current session default\n")
	}
}

// configuredModelLabel names the model used when no session override is set.
func (w *WorkspaceActor) configuredModelLabel() string {
	if w.cfg.DefaultModel != "" {
		return w.cfg.DefaultModel + " (rysh.config.yaml)"
	}
	// Read the built-in from the engine, so `##llm` cannot advertise a
	// default the provider stopped using.
	return sharedprovider.DefaultClaudeModel + " (built-in)"
}

// cmdLLMSelect is the picker behind `##llm select`.
//
// A bare invocation does two things: it PUSHES the picker payload to the pane
// (msg.MsgLLMPickerOpen), which a front-end with a keyboard turns into an
// arrow-key menu, and it PRINTS the numbered menu. Both, always — the daemon
// cannot tell which front-ends are attached, and the printed menu is what the
// web UI, a detached session and `rysh --llm select 3` rely on. A front-end
// that renders the picker simply covers the text it was printed over.
//
// The actor never waits for the answer. A `##` command runs on the
// WorkspaceActor's mailbox goroutine, so blocking here would stall every other
// command in the session — the agentic `ask_user` tool can block only because
// it runs inside an orchestrator, on its own goroutine. The picker resolves by
// the front-end submitting an ordinary `##<scope> model <ref>` command, which
// re-enters through the same door every other selection uses.
//
// Numbering covers only the models that can actually be activated. Anything
// disabled, or belonging to a provider with no rysh executor, is listed
// afterwards WITHOUT a number and with the reason — so every number the user
// can see is a number that works, and nothing silently disappears from the
// registry view either.
func (w *WorkspaceActor) cmdLLMSelect(out *strings.Builder, paneID string, store *llms.Store, args []string) {
	selectable, blocked, err := w.llmSelectMenu(store)
	if err != nil {
		fmt.Fprintf(out, "\n[llm] cannot read %s: %v\n", store.Dir(), err)
		w.failRysh("cannot read %s: %v", store.Dir(), err)
		return
	}
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		w.publishLLMPicker(paneID, store, selectable, blocked)
		w.renderLLMSelectMenu(out, store, selectable, blocked)
		return
	}

	choice := strings.TrimSpace(args[0])
	n, convErr := strconv.Atoi(choice)
	if convErr != nil {
		// A ref rather than a number is an easy slip and unambiguous — treat
		// `##llm select openai/sol` as the activation the user meant.
		if strings.Contains(choice, "/") {
			w.cmdLLMUse(out, store, choice)
			return
		}
		fmt.Fprintf(out, "\n[llm] %q is not a menu number — run ##llm select to see the menu\n", choice)
		w.failRysh("%q is not a menu number — run ##llm select to see the menu", choice)
		return
	}
	if len(selectable) == 0 {
		fmt.Fprintf(out, "\n[llm] no activatable models in %s — declare one with ##llm add <provider>/<name>\n", store.Dir())
		return
	}
	if n < 1 || n > len(selectable) {
		fmt.Fprintf(out, "\n[llm] %d is out of range — the menu is numbered 1-%d (run ##llm select)\n",
			n, len(selectable))
		return
	}
	// Hand off to the existing activation path so every guard applies
	// unchanged: the executable check, the cross-family provider build, the
	// missing-key warning, and the session-seat update.
	w.cmdLLMUse(out, store, selectable[n-1])
}

// llmPickerScopes is the scope list offered in step two of the picker,
// broadest first — the same hierarchy model_scope.go resolves, expressed as the
// commands that bind it. The daemon owns this list so a front-end never has to
// know the hierarchy, and adding a scope here reaches every front-end at once.
//
// The command is the binding command minus its model ref; the front-end appends
// the ref and submits it as ordinary input, so a picked model takes exactly the
// path a typed `##tab model openai/luna` takes.
var llmPickerScopes = []msg.LLMPickerScope{
	{Name: "session", Hint: "every pane and agent in this session", Command: "##session model"},
	{Name: "workspace", Hint: "every pane in this workspace", Command: "##workspace model"},
	{Name: "tab", Hint: "every pane in this tab", Command: "##tab model"},
	{Name: "lane", Hint: "every pane in this lane", Command: "##lane model"},
	{Name: "stack", Hint: "every pane in this stack", Command: "##stack model"},
	{Name: "pane", Hint: "this pane only — outranks every scope above", Command: "##pane model"},
}

// publishLLMPicker pushes the picker payload to one pane. Best-effort: a
// front-end that is not listening loses nothing, because cmdLLMSelect prints
// the numbered menu regardless.
func (w *WorkspaceActor) publishLLMPicker(paneID string, store *llms.Store, selectable, blocked []string) {
	if paneID == "" || w.pub == nil {
		return
	}
	_ = w.pub.Send(msg.T("pane", paneID, "llm", "picker"),
		w.buildLLMPickerPayload(paneID, store, selectable, blocked))
}

// buildLLMPickerPayload assembles what a front-end needs to run the picker.
// Separate from the publish so the contents — which are the entire contract
// with every front-end — can be asserted without a bus.
func (w *WorkspaceActor) buildLLMPickerPayload(paneID string, store *llms.Store, selectable, blocked []string) *msg.MsgLLMPickerOpen {
	models := make([]msg.LLMPickerModel, 0, len(selectable))
	for _, ref := range selectable {
		row := msg.LLMPickerModel{Ref: ref, Current: ref == w.sessionLLMRef}
		providerName, name, err := llms.ParseRef(ref)
		if err != nil {
			continue
		}
		row.Provider = providerFamily(providerName)
		if spec, gerr := store.Get(providerName, name); gerr == nil {
			row.Model = spec.Model
		}
		// Resolved here, not in the front-end: only the daemon can see the
		// ##secret store, and the picker's third step turns on this answer.
		row.KeyName, row.KeyMissing = w.missingProviderKey(row.Provider)
		models = append(models, row)
	}
	inEffect := w.configuredModelLabel()
	if w.sessionLLMRef != "" {
		inEffect = w.sessionLLMRef
	}
	return &msg.MsgLLMPickerOpen{
		PaneID:   paneID,
		Models:   models,
		Blocked:  blocked,
		Scopes:   llmPickerScopes,
		InEffect: inEffect,
	}
}

// llmSelectMenu splits the registry into the models a number may be assigned
// to and the ones that cannot be activated, each with its reason. Ordering
// matches ##llm list (providers sorted, models sorted within a provider), so
// the menu reads the same way the listing does.
func (w *WorkspaceActor) llmSelectMenu(store *llms.Store) (selectable []string, blocked []string, err error) {
	providers, byProvider, err := store.List()
	if err != nil {
		return nil, nil, err
	}
	for _, p := range providers {
		for _, m := range byProvider[p] {
			ref := p + "/" + m.Name
			switch {
			case m.Disabled:
				blocked = append(blocked, ref+"  [disabled — ##llm enable "+ref+"]")
			case !m.Executable():
				blocked = append(blocked, ref+"  [no rysh executor for provider "+p+"]")
			default:
				selectable = append(selectable, ref)
			}
		}
	}
	return selectable, blocked, nil
}

// renderLLMSelectMenu prints the numbered menu.
func (w *WorkspaceActor) renderLLMSelectMenu(out *strings.Builder, store *llms.Store, selectable, blocked []string) {
	o := ryshWriter(out)
	o.Tagged("llm", "select a model")
	o.Rule()
	if len(selectable) == 0 {
		o.Row("  (no activatable models — declare one with ##llm add <provider>/<name>)")
	}
	// Width the ref column to the longest entry so a long ref (gemini's
	// flash-lite spellings) does not shove its model id out of alignment.
	refWidth := 0
	for _, ref := range selectable {
		if len(ref) > refWidth {
			refWidth = len(ref)
		}
	}
	for i, ref := range selectable {
		marker := " "
		if w.sessionLLMRef == ref {
			marker = ">"
		}
		model := ""
		if p, name, perr := llms.ParseRef(ref); perr == nil {
			if spec, gerr := store.Get(p, name); gerr == nil {
				model = spec.Model
			}
		}
		o.Row("  %s %2d) %-*s  %s", marker, i+1, refWidth, ref, model)
	}
	for _, b := range blocked {
		o.Row("       %s", b)
	}
	o.Rule()
	o.Row("choose with: ##llm select <number>")
	if w.sessionLLMRef != "" {
		o.Row("in effect now: %s  (> marks it; ##llm clear to reset)", w.sessionLLMRef)
	} else {
		o.Row("in effect now: %s", w.configuredModelLabel())
	}
}

func (w *WorkspaceActor) cmdLLMList(out *strings.Builder, store *llms.Store) {
	providers, byProvider, err := store.List()
	if err != nil {
		fmt.Fprintf(out, "\n[llm] cannot read %s: %v\n", store.Dir(), err)
		w.failRysh("cannot read %s: %v", store.Dir(), err)
		return
	}
	fmt.Fprintf(out, "\n[llm] models (%s)\n", store.Dir())
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
	for _, p := range providers {
		fmt.Fprintf(out, "  %s\n", p)
		for _, m := range byProvider[p] {
			marker := " "
			if w.sessionLLMRef == p+"/"+m.Name {
				marker = ">"
			}
			flags := ""
			if m.Disabled {
				flags += "  [disabled]"
			}
			if !m.Executable() {
				flags += "  [not executable]"
			}
			fmt.Fprintf(out, "  %s %-22s %s%s\n", marker, m.Name, m.Model, flags)
		}
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
	if w.sessionLLMRef != "" {
		fmt.Fprintf(out, "session default: %s (##llm clear to reset)\n", w.sessionLLMRef)
	} else {
		fmt.Fprintf(out, "session default: %s\n", w.configuredModelLabel())
	}
	fmt.Fprintf(out, "loop seats: %s (automation.loop in rysh.config.yaml)\n", w.llmSeatSummary())
	fmt.Fprintf(out, "add a model: ##llm model add <provider>/<name> [api-model-id]\n")
	fmt.Fprintf(out, "enable/disable: ##llm enable|disable <provider>/<name>\n")
	fmt.Fprintf(out, "per-pane model: ##pane model <provider>/<name>\n")
}

// cmdLLMUse activates provider/name as the session default. A reference with
// no registry file is DECLARED first (persisted to .rysh/llms), per the
// ##llm contract: defining a default always leaves a file behind.
func (w *WorkspaceActor) cmdLLMUse(out *strings.Builder, store *llms.Store, ref string) {
	providerName, modelName, err := llms.ParseRef(ref)
	if err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "\n[llm] %v\n", err)
		return
	}
	spec, err := store.Get(providerName, modelName)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(out, "\n[llm] cannot read %s/%s: %v\n", providerName, modelName, err)
			w.failRysh("cannot read %s/%s: %v", providerName, modelName, err)
			return
		}
		// Persist the new declaration, then use it (name doubles as model id).
		spec = &llms.ModelSpec{Provider: providerName, Name: modelName}
		path, aerr := store.Add(*spec)
		if aerr != nil {
			fmt.Fprintf(out, "\n[llm] cannot persist %s: %v\n", ref, aerr)
			w.failRysh("cannot persist %s: %v", ref, aerr)
			return
		}
		spec, _ = store.Get(providerName, modelName)
		fmt.Fprintf(out, "\n[llm] declared new model %s (%s)\n", ref, path)
	}
	if spec.Disabled {
		fmt.Fprintf(out, "\n[llm] %s is disabled — re-enable it first: ##llm enable %s\n", ref, ref)
		return
	}
	if !spec.Executable() {
		fmt.Fprintf(out, "\n[llm] %s is declared but NOT activatable: rysh has no executor for provider %q (runnable: %s). Registry entry kept.\n",
			ref, providerName, strings.Join(llms.ExecutableProviders, ", "))
		w.failRysh("%s is declared but NOT activatable (no executor for that provider)", ref)
		return
	}
	if w.agSetup == nil || w.agSetup.SessionLLM == nil {
		fmt.Fprintf(out, "\n[llm] agentic setup unavailable — cannot set a session default\n")
		w.failRysh("agentic setup unavailable — cannot set a session default")
		return
	}

	// A CROSS-FAMILY selection needs a different client, not just a different
	// model id: the session decorator would otherwise send an OpenAI model down
	// the Anthropic connection and 404 on the next prompt. Build the family's
	// own provider — its own endpoint, its own key — and hand it to the session
	// seat. The configured provider's key belongs to the configured provider and
	// must never leak into another family's requests; the selected family's key
	// resolves through the ##secret store, so registering it with `##secret new`
	// activates the selection exactly like exporting it does (provider_key.go).
	family, configured := providerFamily(providerName), providerFamily(w.cfg.ProviderName)
	var sessionProv provider.AgenticProvider
	if family != configured {
		cfg := w.cfg
		cfg.ProviderName = family
		cfg.APIURL = ""
		cfg.APIKey = w.providerAPIKey(family)
		cfg.DefaultModel = spec.Model
		sessionProv = provider.NewAgenticProvider(cfg)
	}

	w.agSetup.SessionLLM.SetProvider(spec.Model, spec.Effort, sessionProv)
	w.sessionLLMRef = providerName + "/" + spec.Name
	fmt.Fprintf(out, "\n[llm] session default set: %s → %s", w.sessionLLMRef, spec.Model)
	if spec.Effort != "" {
		fmt.Fprintf(out, " (effort %s)", spec.Effort)
	}
	fmt.Fprintf(out, "\n")
	if sessionProv != nil {
		fmt.Fprintf(out, "[llm] provider switched for this session: %s → %s\n", configured, family)
		w.warnMissingKey(out, family)
	}
	fmt.Fprintf(out, "[llm] applies to every pane/agent on its next request; automation seats (do %s / judge %s) still win\n",
		webauto.DefaultStepModel, webauto.DefaultJudgeModel)
}

func (w *WorkspaceActor) cmdLLMModelAdd(out *strings.Builder, store *llms.Store, ref, modelID string) {
	providerName, modelName, err := llms.ParseRef(ref)
	if err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "\n[llm] %v\n", err)
		return
	}
	path, err := store.Add(llms.ModelSpec{Provider: providerName, Name: modelName, Model: modelID})
	if err != nil {
		fmt.Fprintf(out, "\n[llm] cannot persist %s: %v\n", ref, err)
		w.failRysh("cannot persist %s: %v", ref, err)
		return
	}
	fmt.Fprintf(out, "\n[llm] added %s (%s)\n", ref, path)
	fmt.Fprintf(out, "[llm] activate with: ##llm use %s\n", ref)
}
