package actors

import (
	"fmt"
	"os"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/llms"
	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// ##llm — the session's LLM model surface, backed by the .rysh/llms registry
// (one YAML file per model at <rysh-dir>/llms/<provider>/<model-name>).
//
//	##llm | ##llm list             list providers/models (from .rysh/llms only)
//	##llm add <p>/<name> [id]      declare a new model file (alias: model add)
//	##llm use <provider>/<name>    set the session default model (persists the
//	                               file when it doesn't exist yet)
//	##llm info <provider>/<name>   show a model file's properties
//	##llm status                   show the model in effect (alias: default)
//	##llm <provider>/<name>        shorthand for `use`
//	##llm clear                    drop the session override (back to config)
//
// Activation flips the SessionLLM holder on the agentic Setup, which the
// session provider decorator consults per call — every pane/agent picks the
// new default up on its next request. Explicit seats (recipe step/judge
// models, automation defaults) still win over the session default.
func (w *WorkspaceActor) handleLLMCommand(out *strings.Builder, args []string) {
	store := llms.NewStore(w.cfg.RyshDir)
	if err := store.SeedIfEmpty(); err != nil {
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
			fmt.Fprintf(out, "\n[llm] usage: ##llm add <provider>/<model-name> [api-model-id]\n")
			return
		}
		modelID := ""
		if len(args) >= 3 {
			modelID = args[2]
		}
		w.cmdLLMModelAdd(out, store, args[1], modelID)
	case "use":
		if len(args) < 2 {
			fmt.Fprintf(out, "\n[llm] usage: ##llm use <provider>/<model-name>\n")
			return
		}
		w.cmdLLMUse(out, store, args[1])
	case "info":
		if len(args) < 2 {
			fmt.Fprintf(out, "\n[llm] usage: ##llm info <provider>/<model-name>\n")
			return
		}
		w.cmdLLMInfo(out, store, args[1])
	case "status", "default":
		w.cmdLLMStatus(out)
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
		fmt.Fprintf(out, "\n[llm] usage: ##llm add <provider>/<model-name> [api-model-id]\n")
	case "help":
		w.llmUsage(out)
	default:
		// `##llm anthropic/fable5` — direct activation shorthand.
		if strings.Contains(args[0], "/") {
			w.cmdLLMUse(out, store, args[0])
			return
		}
		w.llmUsage(out)
	}
}

func (w *WorkspaceActor) llmUsage(out *strings.Builder) {
	fmt.Fprintf(out, "\n[llm] usage:\n")
	fmt.Fprintf(out, "  ##llm | ##llm list                    list providers and models (.rysh/llms)\n")
	fmt.Fprintf(out, "  ##llm add <provider>/<name> [api-model-id]  declare a new model file\n")
	fmt.Fprintf(out, "  ##llm use <provider>/<name>           set the session default model (also: ##llm <provider>/<name>)\n")
	fmt.Fprintf(out, "  ##llm info <provider>/<name>          show a model's properties\n")
	fmt.Fprintf(out, "  ##llm status                          show the model in effect (alias: default)\n")
	fmt.Fprintf(out, "  ##llm clear                           drop the session override\n")
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
	fmt.Fprintf(out, "  precedence      : recipe > automation config > ##llm > provider.model > built-in\n")
}

// cmdLLMInfo prints one model file's properties plus its runtime standing
// (path, executability, whether it is the active session default).
func (w *WorkspaceActor) cmdLLMInfo(out *strings.Builder, store *llms.Store, ref string) {
	providerName, modelName, err := llms.ParseRef(ref)
	if err != nil {
		fmt.Fprintf(out, "\n[llm] %v\n", err)
		return
	}
	spec, err := store.Get(providerName, modelName)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(out, "\n[llm] %s not found — declare it with: ##llm add %s [api-model-id]\n", ref, ref)
			return
		}
		fmt.Fprintf(out, "\n[llm] cannot read %s: %v\n", ref, err)
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
	if spec.Executable() {
		fmt.Fprintf(out, "  executable  : yes\n")
	} else {
		fmt.Fprintf(out, "  executable  : no (rysh's executor speaks the Anthropic API only)\n")
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
	return "claude-opus-4-8 (built-in)"
}

func (w *WorkspaceActor) cmdLLMList(out *strings.Builder, store *llms.Store) {
	providers, byProvider, err := store.List()
	if err != nil {
		fmt.Fprintf(out, "\n[llm] cannot read %s: %v\n", store.Dir(), err)
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
			exec := ""
			if !m.Executable() {
				exec = "  [not executable]"
			}
			fmt.Fprintf(out, "  %s %-22s %s%s\n", marker, m.Name, m.Model, exec)
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
}

// cmdLLMUse activates provider/name as the session default. A reference with
// no registry file is DECLARED first (persisted to .rysh/llms), per the
// ##llm contract: defining a default always leaves a file behind.
func (w *WorkspaceActor) cmdLLMUse(out *strings.Builder, store *llms.Store, ref string) {
	providerName, modelName, err := llms.ParseRef(ref)
	if err != nil {
		fmt.Fprintf(out, "\n[llm] %v\n", err)
		return
	}
	spec, err := store.Get(providerName, modelName)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(out, "\n[llm] cannot read %s/%s: %v\n", providerName, modelName, err)
			return
		}
		// Persist the new declaration, then use it (name doubles as model id).
		spec = &llms.ModelSpec{Provider: providerName, Name: modelName}
		path, aerr := store.Add(*spec)
		if aerr != nil {
			fmt.Fprintf(out, "\n[llm] cannot persist %s: %v\n", ref, aerr)
			return
		}
		spec, _ = store.Get(providerName, modelName)
		fmt.Fprintf(out, "\n[llm] declared new model %s (%s)\n", ref, path)
	}
	if !spec.Executable() {
		fmt.Fprintf(out, "\n[llm] %s is declared but NOT activatable: rysh's executor speaks the Anthropic API only (provider %q). Registry entry kept.\n",
			ref, providerName)
		return
	}
	if w.agSetup == nil || w.agSetup.SessionLLM == nil {
		fmt.Fprintf(out, "\n[llm] agentic setup unavailable — cannot set a session default\n")
		return
	}
	w.agSetup.SessionLLM.Set(spec.Model, spec.Effort)
	w.sessionLLMRef = providerName + "/" + spec.Name
	fmt.Fprintf(out, "\n[llm] session default set: %s → %s", w.sessionLLMRef, spec.Model)
	if spec.Effort != "" {
		fmt.Fprintf(out, " (effort %s)", spec.Effort)
	}
	fmt.Fprintf(out, "\n[llm] applies to every pane/agent on its next request; automation seats (do %s / judge %s) still win\n",
		webauto.DefaultStepModel, webauto.DefaultJudgeModel)
}

func (w *WorkspaceActor) cmdLLMModelAdd(out *strings.Builder, store *llms.Store, ref, modelID string) {
	providerName, modelName, err := llms.ParseRef(ref)
	if err != nil {
		fmt.Fprintf(out, "\n[llm] %v\n", err)
		return
	}
	path, err := store.Add(llms.ModelSpec{Provider: providerName, Name: modelName, Model: modelID})
	if err != nil {
		fmt.Fprintf(out, "\n[llm] cannot persist %s: %v\n", ref, err)
		return
	}
	fmt.Fprintf(out, "\n[llm] added %s (%s)\n", ref, path)
	fmt.Fprintf(out, "[llm] activate with: ##llm use %s\n", ref)
}
