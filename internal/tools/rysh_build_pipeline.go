// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"strings"

	"github.com/nats-io/nats.go"
	"gopkg.in/yaml.v3"

	"github.com/rysh-ai/rysh-cli-code/internal/pipeline"
)

// RyshBuildPipelineTool parses .rysh/pipelines/*.rysh.yaml files and registers
// the resulting prompts into the pipeline overlay so that softdev events can
// trigger them.
type RyshBuildPipelineTool struct {
	workDir string
	plKV    nats.KeyValue
}

// ryshBuildPipelineParams holds the optional file parameter.
type ryshBuildPipelineParams struct {
	File string `json:"file,omitempty"`
}

// NewRyshBuildPipelineTool creates a RyshBuildPipelineTool rooted at workDir.
func NewRyshBuildPipelineTool(workDir string, plKV nats.KeyValue) *RyshBuildPipelineTool {
	return &RyshBuildPipelineTool{workDir: workDir, plKV: plKV}
}

// Spec returns the tool specification for the LLM.
func (t *RyshBuildPipelineTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "file": {
      "type": "string",
      "description": "Optional pipeline YAML filename inside .rysh/pipelines/. If omitted, all *.rysh.yaml files in that directory are processed."
    }
  }
}`)
	return ToolSpec{
		Name:             "rysh_build_pipeline",
		Description:      progname.Rewrite("Parse a rysh pipeline YAML file and register the phases as softdev prompts. Each phase gets a continuation instruction so the agent automatically advances through the pipeline."),
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval always returns false.
func (t *RyshBuildPipelineTool) RequiresApproval(_ json.RawMessage) bool {
	return false
}

// Execute parses the pipeline YAML(s), builds augmented prompts, and registers
// them in the pipeline overlay.
func (t *RyshBuildPipelineTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p ryshBuildPipelineParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid rysh_build_pipeline params: %w", err)
	}

	pipelinesDir := filepath.Join(t.workDir, ".rysh", "pipelines")

	var files []string
	if p.File != "" {
		// Single file specified.
		path := p.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(pipelinesDir, path)
		}
		if _, err := os.Stat(path); err != nil {
			return ErrOutputf(ErrKindMissing, "pipeline file not found: %s", path), nil
		}
		files = []string{path}
	} else {
		// Scan directory for all *.rysh.yaml files.
		entries, err := os.ReadDir(pipelinesDir)
		if err != nil {
			return ErrOutputf(ErrKindMissing, "cannot read pipelines directory: %s: %v", pipelinesDir, err), nil
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".rysh.yaml") {
				files = append(files, filepath.Join(pipelinesDir, e.Name()))
			}
		}
		if len(files) == 0 {
			return ErrOutputf(ErrKindMissing, "no *.rysh.yaml files found in %s", pipelinesDir), nil
		}
	}

	// Clear any previously registered pipeline prompts.
	pipeline.ClearPrompts()

	var summaryBuilder strings.Builder
	for _, filePath := range files {
		result, err := t.processFile(filePath)
		if err != nil {
			return ErrOutputf(ErrKindInternal, "error processing %s: %v", filePath, err), nil
		}
		summaryBuilder.WriteString(result)
	}

	// Persist pipeline prompts to KV for session restore.
	if t.plKV != nil {
		if err := pipeline.PersistToKV(t.plKV); err != nil {
			slog.Warn("rysh_build_pipeline: persist to KV", "err", err)
		}
	}

	return &ToolOutput{Content: summaryBuilder.String()}, nil
}

// pipelineYAML models the top-level structure: rysh.event.ai.softdev.<language>
type pipelineYAML struct {
	Rysh struct {
		Event struct {
			AI struct {
				Softdev map[string]yaml.Node `yaml:"softdev"`
			} `yaml:"ai"`
		} `yaml:"event"`
	} `yaml:"rysh"`
}

// phaseEntry represents one element in the phases sequence (a single-key mapping).
type phaseEntry struct {
	Name        string
	Description string
}

// processFile handles a single pipeline YAML file.
func (t *RyshBuildPipelineTool) processFile(filePath string) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("cannot read file: %w", err)
	}

	var doc pipelineYAML
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("invalid YAML: %w", err)
	}

	if len(doc.Rysh.Event.AI.Softdev) == 0 {
		return "", fmt.Errorf("no languages found under rysh.event.ai.softdev")
	}

	var sb strings.Builder

	for language, node := range doc.Rysh.Event.AI.Softdev {
		phases, err := parsePhases(&node)
		if err != nil {
			return "", fmt.Errorf("language %q: %w", language, err)
		}
		if len(phases) == 0 {
			continue
		}

		sb.WriteString(fmt.Sprintf("Pipeline: %s (%s)\n", filepath.Base(filePath), language))
		sb.WriteString(fmt.Sprintf("  Phases: %d\n", len(phases)))

		for i, phase := range phases {
			isLast := i == len(phases)-1

			// Build the augmented prompt text.
			var promptBuilder strings.Builder
			promptBuilder.WriteString(fmt.Sprintf(
				"You are working on a %s project. The %s phase has been triggered from pane '%%s'.\n",
				titleCase(language), phase.Name,
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

			sb.WriteString(fmt.Sprintf("  [%d] %s", i+1, phase.Name))
			if isLast {
				sb.WriteString(" (final)")
			}
			sb.WriteString("\n")
		}

		// Show the first event command to kick things off.
		sb.WriteString(fmt.Sprintf("\n  Start command: ##>event:ai:softdev:%s:%s\n\n", language, phases[0].Name))
	}

	return sb.String(), nil
}

// parsePhases extracts the ordered list of phase entries from a YAML sequence node.
func parsePhases(node *yaml.Node) ([]phaseEntry, error) {
	// The node might be a sequence directly, or it might be wrapped in a
	// document/alias node. We handle both cases.
	seqNode := node
	if seqNode.Kind == yaml.AliasNode {
		seqNode = seqNode.Alias
	}
	if seqNode.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("expected sequence, got kind %d", seqNode.Kind)
	}

	var phases []phaseEntry
	for _, item := range seqNode.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		// Each mapping has exactly one key-value pair.
		if len(item.Content) < 2 {
			continue
		}
		name := item.Content[0].Value
		desc := item.Content[1].Value
		phases = append(phases, phaseEntry{
			Name:        name,
			Description: desc,
		})
	}

	return phases, nil
}

// titleCase uppercases the first rune of s.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
