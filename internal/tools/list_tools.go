package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ListToolsTool lists all available tools and their descriptions.
type ListToolsTool struct {
	registry *ToolRegistry
}

// NewListToolsTool creates a new ListToolsTool that references the given registry.
func NewListToolsTool(registry *ToolRegistry) *ListToolsTool {
	return &ListToolsTool{registry: registry}
}

// Spec returns the tool specification for the LLM.
func (t *ListToolsTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {},
  "required": []
}`)
	return ToolSpec{
		Name:             "list_tools",
		Description:      "List all available tools and their descriptions.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval always returns false for list_tools.
func (t *ListToolsTool) RequiresApproval(_ json.RawMessage) bool {
	return false
}

// Execute returns a formatted list of all registered tools.
func (t *ListToolsTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	specs := t.registry.AllSpecs()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Available tools (%d):\n\n", len(specs)))

	for _, spec := range specs {
		sb.WriteString(fmt.Sprintf("- %s: %s\n", spec.Name, spec.Description))
	}

	return &ToolOutput{Content: sb.String()}, nil
}
