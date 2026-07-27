package tools

import (
	"context"
	"encoding/json"
)

// SubAgentTool exposes the schema for the sub_agent tool. The tool itself is
// SPECIAL: OrchestratorActor (rysh-shared/agentic) intercepts tool calls named
// "sub_agent" before normal dispatch and routes them to its own handleSubAgent
// method, which spawns a child OrchestratorActor with its own context window
// and returns only the child's summary to the parent.
//
// As a result, the Execute method below is effectively unreachable in the
// production flow. It is kept as a defensive fallback so direct unit-testing
// of the registry (or a hypothetical caller that bypasses the orchestrator)
// receives a clear diagnostic instead of nil-dereferencing.
type SubAgentTool struct{}

// SubAgentParams documents the tool inputs. The shape mirrors
// agentic.SubAgentParams in rysh-shared so the orchestrator can parse the
// same JSON directly.
type SubAgentParams struct {
	Task         string   `json:"task"`
	Context      string   `json:"context,omitempty"`
	SystemPrompt string   `json:"system_prompt,omitempty"`
	AllowedTools []string `json:"allowed_tools,omitempty"`
}

// NewSubAgentTool creates a new SubAgentTool.
func NewSubAgentTool() *SubAgentTool {
	return &SubAgentTool{}
}

// Spec returns the tool specification for the LLM.
//
// Description guidance: the model is told this delegates to a focused child
// agent with its own context, that only the summary is returned, and that
// nested sub_agent calls are depth-limited.
func (t *SubAgentTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "task": {
      "type": "string",
      "description": "The focused sub-task to delegate. State the goal clearly; the sub-agent does not see the parent conversation."
    },
    "context": {
      "type": "string",
      "description": "Optional extra context, file paths, or constraints to pass to the sub-agent. Be concise: the sub-agent only sees what you include here."
    },
    "system_prompt": {
      "type": "string",
      "description": "Optional system prompt for the sub-agent. If omitted, a default focused-sub-agent prompt is used."
    },
    "allowed_tools": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Optional whitelist of tool names the sub-agent may use. If omitted, the sub-agent inherits the full parent tool set."
    }
  },
  "required": ["task"]
}`)
	return ToolSpec{
		Name: "sub_agent",
		Description: "Delegate a focused task to a child sub-agent that runs its own agentic loop with an isolated context window. " +
			"Only the sub-agent's final summary is returned to your conversation; its intermediate tool calls are not visible to you. " +
			"Use sub_agent when a task is self-contained and would otherwise pollute your context (e.g. multi-file refactors, deep research, complex test debugging). " +
			"Nested sub_agent calls are depth-limited.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval is false: spawning a focused child agent is not itself
// destructive; the child's own destructive tool calls go through the normal
// approval flow.
func (t *SubAgentTool) RequiresApproval(_ json.RawMessage) bool {
	return false
}

// Execute is a defensive no-op. In normal operation OrchestratorActor
// intercepts the tool name "sub_agent" and never invokes this method. If it
// IS reached (direct registry call), surface a clear diagnostic so the caller
// understands the wiring.
func (t *SubAgentTool) Execute(_ context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p SubAgentParams
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	if p.Task == "" {
		return ErrOutput(ErrKindValidation, "task is required"), nil
	}
	return ErrOutputf(ErrKindInternal, "sub_agent must be invoked through the orchestrator; direct Execute is not supported (task=%q)", p.Task), nil
}
