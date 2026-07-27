// Package toolpack turns an IR into a hot-loadable Rysh tool pack: a declarative
// manifest of tool definitions plus the live ToolExecutors that back them. It is
// the highest-leverage Forge target — the same manifest is exposed to agents
// either one-tool-per-operation (small specs) or via three dynamic meta-tools
// (large specs), per the context-budget policy (see exposure.go).
package toolpack

import (
	"encoding/json"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
)

// ToolPack is the generated, serializable manifest for an API.
type ToolPack struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Description string          `json:"description,omitempty"`
	BaseURL     string          `json:"base_url,omitempty"`
	Auth        []ir.AuthScheme `json:"auth,omitempty"`
	Tools       []ToolDef       `json:"tools"`
}

// ToolDef is one operation rendered as an agent-callable tool.
type ToolDef struct {
	Name        string          `json:"name"`         // sanitized, prefixed tool name
	OperationID string          `json:"operation_id"` // IR operation id (used by invoke_endpoint)
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	Summary     string          `json:"summary,omitempty"`
	Tags        []string        `json:"tags,omitempty"`
	Mutating    bool            `json:"mutating"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Build produces a ToolPack from the IR. prefix is prepended to every tool name
// (e.g. "stripe_") for namespacing; pass "" for none.
func Build(api *ir.API, prefix string) *ToolPack {
	tp := &ToolPack{
		Name:        api.Name,
		Version:     api.Version,
		Description: api.Description,
		BaseURL:     api.BaseURL(),
		Auth:        api.Auth,
	}
	for i := range api.Operations {
		op := &api.Operations[i]
		tp.Tools = append(tp.Tools, ToolDef{
			Name:        SanitizeName(prefix + op.ID),
			OperationID: op.ID,
			Method:      op.Method,
			Path:        op.Path,
			Summary:     firstLine(op.Summary, op.Description),
			Tags:        op.Tags,
			Mutating:    op.Mutating,
			InputSchema: op.ToolInputSchema(api),
		})
	}
	return tp
}

// JSON renders the manifest as indented JSON (the artifact form).
func (tp *ToolPack) JSON() ([]byte, error) {
	return json.MarshalIndent(tp, "", "  ")
}

// SanitizeName coerces a name into Anthropic's tool name charset
// (^[a-zA-Z0-9_-]{1,64}$), folding invalid runes to '_' and truncating to 64.
func SanitizeName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	for strings.Contains(s, "__") {
		s = strings.ReplaceAll(s, "__", "_")
	}
	s = strings.Trim(s, "_")
	if s == "" {
		s = "tool"
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

func firstLine(a, b string) string {
	s := strings.TrimSpace(a)
	if s == "" {
		s = strings.TrimSpace(b)
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:299] + "…"
	}
	return s
}
