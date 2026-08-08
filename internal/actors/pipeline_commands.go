package actors

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/pipeline"
)

// Pipeline YAML structure (reused from rysh_build_pipeline tool).
type pipelineYAMLFile struct {
	Rysh struct {
		Name  string `yaml:"name"`
		Event struct {
			AI struct {
				Softdev map[string]yaml.Node `yaml:"softdev"`
			} `yaml:"ai"`
		} `yaml:"event"`
	} `yaml:"rysh"`
}

// cmdPipelineHelp writes help text for pipeline commands.
func cmdPipelineHelp(out *strings.Builder) {
	out.WriteString("\n[pipeline] commands:\n")
	out.WriteString("  list              -- list available files and loaded pipelines\n")
	out.WriteString("  list files        -- list pipeline files in .rysh/pipelines/\n")
	out.WriteString("  list loaded       -- list loaded pipelines\n")
	out.WriteString("  load <file>       -- load a pipeline from .rysh/pipelines/<file>\n")
	out.WriteString("  unload <name>     -- remove a loaded pipeline\n")
	out.WriteString("  show <name>       -- show pipeline phases and descriptions\n")
	out.WriteString("  build <name>      -- build augmented prompts and save to .build.yaml\n")
	out.WriteString("  run [name]        -- run a pipeline (default: first loaded)\n")
	out.WriteString("  status            -- show pipeline status\n")
	out.WriteString("  clear             -- clear pipeline output\n")
	out.WriteString("  name <name>       -- set the pipeline name label\n")
	out.WriteString("  help              -- show this help\n")
}

// cmdPipelineShow displays the phases and descriptions of a loaded pipeline.
func cmdPipelineShow(out *strings.Builder, t *TabActor, name string) {
	if name == "" {
		ryshWriter(out).UsageLineIn("pipeline", "show <name>")
		return
	}
	lp, exists := t.pipelines[name]
	if !exists {
		out.WriteString(fmt.Sprintf("\n[pipeline] pipeline '%s' not found\n", name))
		return
	}

	out.WriteString(fmt.Sprintf("\n[pipeline] '%s' (%s, %d phases) [%s]\n", lp.Name, lp.Language, len(lp.Phases), lp.Status))
	out.WriteString(fmt.Sprintf("  source: .rysh/pipelines/%s\n", lp.File))
	out.WriteString(strings.Repeat("─", 60) + "\n")
	for i, phase := range lp.Phases {
		marker := "  "
		if lp.CurrentPhase == i {
			marker = "▶ "
		}
		out.WriteString(fmt.Sprintf("  %s[%d] %s\n", marker, i+1, phase.Name))
		// Indent the description.
		desc := strings.TrimSpace(phase.Description)
		for _, line := range strings.Split(desc, "\n") {
			out.WriteString(fmt.Sprintf("       %s\n", line))
		}
	}
	out.WriteString(strings.Repeat("─", 60) + "\n")
}

// cmdPipelineBuild builds augmented prompts for a loaded pipeline and writes
// them to .rysh/pipelines/<source-file>.build.yaml.
func cmdPipelineBuild(out *strings.Builder, t *TabActor, name string) {
	if name == "" {
		ryshWriter(out).UsageLineIn("pipeline", "build <name>")
		return
	}
	lp, exists := t.pipelines[name]
	if !exists {
		out.WriteString(fmt.Sprintf("\n[pipeline] pipeline '%s' not found\n", name))
		return
	}

	if len(lp.Phases) == 0 {
		out.WriteString(fmt.Sprintf("\n[pipeline] pipeline '%s' has no phases\n", lp.Name))
		return
	}

	// Build augmented prompts for each phase (same logic as load/rysh_build_pipeline).
	type buildPhase struct {
		Name   string `yaml:"name"`
		Prompt string `yaml:"prompt"`
	}
	type buildDoc struct {
		Rysh struct {
			Name     string       `yaml:"name"`
			Source   string       `yaml:"source"`
			Language string       `yaml:"language"`
			Phases   []buildPhase `yaml:"phases"`
		} `yaml:"rysh"`
	}

	var doc buildDoc
	doc.Rysh.Name = lp.Name
	doc.Rysh.Source = lp.File
	doc.Rysh.Language = lp.Language

	for i, phase := range lp.Phases {
		key := lp.Language + ":" + phase.Name
		prompt, found := pipeline.LookupPrompt(key)
		if !found {
			// Prompt not registered yet — regenerate it inline.
			var pb strings.Builder
			pb.WriteString(fmt.Sprintf(
				"You are working on a %s project. The %s phase has been triggered from pane '%%s'.\n",
				strings.ToUpper(lp.Language[:1])+lp.Language[1:], phase.Name,
			))
			pb.WriteString(strings.TrimSpace(phase.Description))
			pb.WriteString("\n\n")

			isLast := i == len(lp.Phases)-1
			if isLast {
				pb.WriteString("This is the final step in the pipeline. When complete, summarize what was accomplished across all phases.\n")
			} else {
				nextPhase := lp.Phases[i+1].Name
				pb.WriteString(fmt.Sprintf(
					"IMPORTANT: When you have fully completed this task, signal the next pipeline step by outputting exactly this line on its own:\n##>event:ai:softdev:%s:%s\n",
					lp.Language, nextPhase,
				))
			}
			pb.WriteString("\nContext from source pane:\n%s")
			prompt = pb.String()
		}
		doc.Rysh.Phases = append(doc.Rysh.Phases, buildPhase{
			Name:   phase.Name,
			Prompt: prompt,
		})
	}

	data, err := yaml.Marshal(&doc)
	if err != nil {
		out.WriteString(fmt.Sprintf("\n[pipeline] marshal error: %s\n", err))
		return
	}

	workDir, _ := os.Getwd()
	buildPath := filepath.Join(workDir, ".rysh", "pipelines", lp.File+".build.yaml")
	if err := os.WriteFile(buildPath, data, 0644); err != nil {
		out.WriteString(fmt.Sprintf("\n[pipeline] write error: %s\n", err))
		return
	}

	out.WriteString(fmt.Sprintf("\n[pipeline] built '%s' → .rysh/pipelines/%s.build.yaml (%d phases)\n",
		lp.Name, lp.File, len(doc.Rysh.Phases)))
}

// cmdPipelineList dispatches list subcommands: "files", "loaded", or both.
func cmdPipelineList(out *strings.Builder, t *TabActor, sub string) {
	switch sub {
	case "files":
		cmdPipelineListFiles(out)
	case "loaded":
		cmdPipelineListLoaded(out, t)
	default:
		// Bare "list" shows both.
		cmdPipelineListFiles(out)
		cmdPipelineListLoaded(out, t)
	}
}

// cmdPipelineListFiles lists pipeline YAML files in .rysh/pipelines/.
func cmdPipelineListFiles(out *strings.Builder) {
	workDir, _ := os.Getwd()
	dir := filepath.Join(workDir, ".rysh", "pipelines")
	entries, err := os.ReadDir(dir)
	if err != nil {
		out.WriteString(fmt.Sprintf("\n[pipeline] cannot read .rysh/pipelines/: %s\n", err))
		return
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".rysh.yaml") || strings.HasSuffix(name, ".rysh.yml") {
			files = append(files, name)
		}
	}
	if len(files) == 0 {
		out.WriteString("\n[pipeline] no pipeline files found in .rysh/pipelines/\n")
		return
	}
	out.WriteString(fmt.Sprintf("\n[pipeline] %d file(s) in .rysh/pipelines/:\n", len(files)))
	for _, f := range files {
		out.WriteString(fmt.Sprintf("  %s\n", f))
	}
}

// cmdPipelineListLoaded lists all loaded pipelines for this tab.
func cmdPipelineListLoaded(out *strings.Builder, t *TabActor) {
	if len(t.pipelines) == 0 {
		out.WriteString("\n[pipeline] no pipelines loaded. Use 'load <file>' to load one.\n")
		return
	}
	out.WriteString(fmt.Sprintf("\n[pipeline] %d pipeline(s) loaded:\n", len(t.pipelines)))
	for name, p := range t.pipelines {
		phases := make([]string, len(p.Phases))
		for i, ph := range p.Phases {
			phases[i] = ph.Name
		}
		out.WriteString(fmt.Sprintf("  %s (%s, %d phases: %s) [%s]\n",
			name, p.Language, len(p.Phases), strings.Join(phases, " -> "), p.Status))
	}
}

// cmdPipelineLoad loads a pipeline YAML file and registers it under its name.
func cmdPipelineLoad(out *strings.Builder, t *TabActor, filename string) {
	if filename == "" {
		ryshWriter(out).UsageLineIn("pipeline", "load <filename>")
		return
	}

	workDir, _ := os.Getwd()
	path := filepath.Join(workDir, ".rysh", "pipelines", filename)
	data, err := os.ReadFile(path)
	if err != nil {
		out.WriteString(fmt.Sprintf("\n[pipeline] cannot read file: %s\n", err))
		return
	}

	var doc pipelineYAMLFile
	if err := yaml.Unmarshal(data, &doc); err != nil {
		out.WriteString(fmt.Sprintf("\n[pipeline] invalid YAML: %s\n", err))
		return
	}

	pipelineName := doc.Rysh.Name
	if pipelineName == "" {
		out.WriteString("\n[pipeline] YAML missing 'rysh.name' field\n")
		return
	}

	// Check uniqueness.
	if _, exists := t.pipelines[pipelineName]; exists {
		out.WriteString(fmt.Sprintf("\n[pipeline] pipeline '%s' already loaded. Use 'unload %s' first.\n", pipelineName, pipelineName))
		return
	}

	if len(doc.Rysh.Event.AI.Softdev) == 0 {
		out.WriteString("\n[pipeline] no languages found under rysh.event.ai.softdev\n")
		return
	}

	for language, node := range doc.Rysh.Event.AI.Softdev {
		phases, err := parsePipelinePhases(&node)
		if err != nil {
			out.WriteString(fmt.Sprintf("\n[pipeline] error parsing phases for %s: %s\n", language, err))
			return
		}
		if len(phases) == 0 {
			continue
		}

		lp := &LoadedPipeline{
			Name:         pipelineName,
			File:         filename,
			Language:     language,
			Phases:       phases,
			Status:       "idle",
			CurrentPhase: -1,
		}
		t.pipelines[pipelineName] = lp

		// Register augmented prompts into the pipeline registry.
		for i, phase := range phases {
			isLast := i == len(phases)-1
			var promptBuilder strings.Builder
			promptBuilder.WriteString(fmt.Sprintf(
				"You are working on a %s project. The %s phase has been triggered from pane '%%s'.\n",
				strings.ToUpper(language[:1])+language[1:], phase.Name,
			))
			promptBuilder.WriteString(strings.TrimSpace(phase.Description))
			promptBuilder.WriteString("\n\n")

			if isLast {
				promptBuilder.WriteString("This is the final step in the pipeline. When complete, summarize what was accomplished across all phases.\n")
			} else {
				nextPhase := phases[i+1].Name
				promptBuilder.WriteString(fmt.Sprintf(
					"IMPORTANT: When you have fully completed this task, signal the next pipeline step by outputting exactly this line on its own:\n##>event:ai:softdev:%s:%s\n",
					language, nextPhase,
				))
			}
			promptBuilder.WriteString("\nContext from source pane:\n%s")

			key := language + ":" + phase.Name
			pipeline.RegisterPrompt(key, promptBuilder.String())
		}

		phaseNames := make([]string, len(phases))
		for i, ph := range phases {
			phaseNames[i] = ph.Name
		}
		out.WriteString(fmt.Sprintf("\n[pipeline] loaded '%s' (%s, %d phases: %s)\n",
			pipelineName, language, len(phases), strings.Join(phaseNames, " -> ")))
	}
}

// cmdPipelineUnload removes a loaded pipeline by name.
func cmdPipelineUnload(out *strings.Builder, t *TabActor, name string) {
	if name == "" {
		ryshWriter(out).UsageLineIn("pipeline", "unload <name>")
		return
	}
	if _, exists := t.pipelines[name]; !exists {
		out.WriteString(fmt.Sprintf("\n[pipeline] pipeline '%s' not found\n", name))
		return
	}
	delete(t.pipelines, name)
	out.WriteString(fmt.Sprintf("\n[pipeline] unloaded '%s'\n", name))
}

// cmdPipelineRun starts a pipeline by sending the first phase event to the pipeline LLMPromptExecutionActor.
func cmdPipelineRun(out *strings.Builder, t *TabActor, name string) {
	if len(t.pipelines) == 0 {
		out.WriteString("\n[pipeline] no pipelines loaded\n")
		return
	}

	var lp *LoadedPipeline
	if name == "" {
		// Use first loaded pipeline.
		for _, p := range t.pipelines {
			lp = p
			break
		}
	} else {
		var ok bool
		lp, ok = t.pipelines[name]
		if !ok {
			out.WriteString(fmt.Sprintf("\n[pipeline] pipeline '%s' not found\n", name))
			return
		}
	}

	if len(lp.Phases) == 0 {
		out.WriteString(fmt.Sprintf("\n[pipeline] pipeline '%s' has no phases\n", lp.Name))
		return
	}

	lp.Status = "running"
	lp.CurrentPhase = 0
	firstPhase := lp.Phases[0].Name

	// Build the prompt for the first phase.
	prompt, found := pipeline.LookupPrompt(lp.Language + ":" + firstPhase)
	if !found {
		out.WriteString(fmt.Sprintf("\n[pipeline] no prompt registered for %s:%s\n", lp.Language, firstPhase))
		return
	}

	// Format the prompt with placeholder values for source alias and context.
	formattedPrompt := fmt.Sprintf(prompt, "pipeline", "(pipeline initiated by user)")

	// Send prompt to pipeline LLMPromptExecutionActor.
	pipelinePaneID := "pipeline-" + t.id
	_ = t.pub.Send(
		msg.T("pane", pipelinePaneID, "llm_prompt_execution", "inbox"),
		&msg.MsgAgenticPrompt{Prompt: formattedPrompt},
	)

	// Also emit to pipeline output.
	pipelineOutputSubject := msg.T("tab", t.id, "pipelineOutput")
	_ = t.pub.Send(pipelineOutputSubject, &msg.MsgPipelineOutputAppend{
		Text: fmt.Sprintf("\n--- Pipeline '%s' started -- phase: %s\n", lp.Name, firstPhase),
	})

	out.WriteString(fmt.Sprintf("\n[pipeline] running '%s' -- starting phase: %s\n", lp.Name, firstPhase))
}

// cmdPipelineStatus shows pipeline execution status.
func cmdPipelineStatus(out *strings.Builder, t *TabActor) {
	if len(t.pipelines) == 0 {
		out.WriteString("\n[pipeline] no pipelines loaded\n")
		return
	}
	out.WriteString("\n[pipeline] status:\n")
	for name, p := range t.pipelines {
		phase := "(none)"
		if p.CurrentPhase >= 0 && p.CurrentPhase < len(p.Phases) {
			phase = p.Phases[p.CurrentPhase].Name
		}
		out.WriteString(fmt.Sprintf("  %s: %s (current phase: %s)\n", name, p.Status, phase))
	}
}

// cmdPipelineClear clears the pipeline output buffer.
func cmdPipelineClear(out *strings.Builder, t *TabActor) {
	t.pipelineBuffer.Reset()
	out.WriteString("\n[pipeline] output cleared\n")
}

// parsePipelinePhases extracts ordered phases from a YAML sequence node.
// Reuses the same logic as the rysh_build_pipeline tool.
func parsePipelinePhases(node *yaml.Node) ([]PipelinePhase, error) {
	seqNode := node
	if seqNode.Kind == yaml.AliasNode {
		seqNode = seqNode.Alias
	}
	if seqNode.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("expected sequence, got kind %d", seqNode.Kind)
	}

	var phases []PipelinePhase
	for _, item := range seqNode.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		if len(item.Content) < 2 {
			continue
		}
		name := item.Content[0].Value
		desc := item.Content[1].Value
		phases = append(phases, PipelinePhase{
			Name:        name,
			Description: desc,
		})
	}
	return phases, nil
}
