package tools

import (
	"context"
	"encoding/json"
)

// GroundingReportTool exposes the schema for the grounding_report tool. The
// tool itself is SPECIAL: OrchestratorActor (rysh-shared/agentic) intercepts
// tool calls named "grounding_report" before normal dispatch and routes them
// to its own handleGroundingReport method — declaring understanding opens the
// enforced grounding gate; declaring a question pauses the run until the user
// answers.
//
// As with sub_agent, the Execute method below is effectively unreachable in
// the production flow; it is kept as a defensive fallback for direct registry
// calls.
type GroundingReportTool struct{}

// NewGroundingReportTool creates a new GroundingReportTool.
func NewGroundingReportTool() *GroundingReportTool {
	return &GroundingReportTool{}
}

// Spec returns the tool specification for the LLM.
func (t *GroundingReportTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "understood": {
      "type": "boolean",
      "description": "true when the request now maps to concrete files/symbols you located and read (or plainly needs no codebase context); false when you cannot locate what the request refers to."
    },
    "relevant_files": {
      "type": "array",
      "items": {"type": "string"},
      "description": "The files your understanding is grounded on (paths you actually read)."
    },
    "evidence": {
      "type": "string",
      "description": "One short paragraph: what you found and where, in terms of the actual code."
    },
    "missing_info": {
      "type": "string",
      "description": "When understood=false: what you could not locate despite searching."
    },
    "question": {
      "type": "string",
      "description": "When understood=false: the question to show the user — typically asking WHERE the relevant code, directory, or document lives. The session pauses until they reply."
    }
  },
  "required": ["understood"]
}`)
	return ToolSpec{
		Name: "grounding_report",
		Description: "Declare the outcome of grounding yourself in the codebase. " +
			"Call with understood=true (plus relevant_files and evidence) once the request maps to concrete code you have read — " +
			"when the grounding gate is active this unlocks the mutating tools. " +
			"Call with understood=false and a question when searching cannot locate what the request refers to, " +
			"or when the request is ambiguous regardless of the code: the question is shown to the user and the session " +
			"pauses until they answer. If the prompt already contains everything you need, call understood=true immediately.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval is false: declaring understanding or asking a question is
// never destructive.
func (t *GroundingReportTool) RequiresApproval(_ json.RawMessage) bool {
	return false
}

// Execute is a defensive no-op. In normal operation OrchestratorActor
// intercepts the tool name "grounding_report" and never invokes this method.
func (t *GroundingReportTool) Execute(_ context.Context, _ json.RawMessage) (*ToolOutput, error) {
	return ErrOutput(ErrKindInternal,
		"grounding_report must be invoked through the orchestrator; direct Execute is not supported"), nil
}
