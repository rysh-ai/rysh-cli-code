package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// sensitiveEnvPrefixes are prefixes that indicate potentially sensitive variables.
var sensitiveEnvPrefixes = []string{
	"SECRET", "PASSWORD", "PASSWD", "TOKEN", "PRIVATE_KEY",
	"AWS_SECRET", "CREDENTIALS",
}

// sensitiveEnvNames are exact names that should be redacted.
var sensitiveEnvNames = map[string]bool{
	"AWS_SECRET_ACCESS_KEY": true,
	"DATABASE_PASSWORD":     true,
	"DB_PASSWORD":           true,
	"PRIVATE_KEY":           true,
}

// EnvReadTool reads environment variables safely.
type EnvReadTool struct{}

// EnvReadParams holds the parameters for reading env vars.
type EnvReadParams struct {
	Name   string `json:"name,omitempty"`   // specific variable name
	Prefix string `json:"prefix,omitempty"` // filter by prefix (e.g., "RYSH_", "AWS_")
	All    bool   `json:"all,omitempty"`    // list all variables (names only, not values)
}

// NewEnvReadTool creates a new EnvReadTool.
func NewEnvReadTool() *EnvReadTool {
	return &EnvReadTool{}
}

// Spec returns the tool specification for the LLM.
func (t *EnvReadTool) Spec() ToolSpec {
	schema := json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "Specific environment variable to read"},
    "prefix": {"type": "string", "description": "Filter variables by prefix (e.g., 'RYSH_', 'PATH')"},
    "all": {"type": "boolean", "description": "List all variable names (values are hidden for potentially sensitive vars)"}
  }
}`)
	return ToolSpec{
		Name:             "env_read",
		Description:      "Read environment variables. Sensitive values (passwords, tokens, secrets) are automatically redacted for safety.",
		Parameters:       schema,
		RequiresApproval: false,
	}
}

// RequiresApproval always returns false — read-only, with redaction.
func (t *EnvReadTool) RequiresApproval(_ json.RawMessage) bool {
	return false
}

// Execute reads environment variables.
func (t *EnvReadTool) Execute(ctx context.Context, params json.RawMessage) (*ToolOutput, error) {
	var p EnvReadParams
	if params != nil {
		_ = json.Unmarshal(params, &p)
	}

	// Specific variable.
	if p.Name != "" {
		val := os.Getenv(p.Name)
		if val == "" {
			// Distinguish between unset and empty.
			if _, ok := os.LookupEnv(p.Name); !ok {
				return &ToolOutput{Content: fmt.Sprintf("%s is not set", p.Name)}, nil
			}
			return &ToolOutput{Content: fmt.Sprintf("%s=(empty)", p.Name)}, nil
		}
		if isSensitiveEnv(p.Name) {
			return &ToolOutput{Content: fmt.Sprintf("%s=<redacted> (sensitive variable)", p.Name)}, nil
		}
		return &ToolOutput{Content: fmt.Sprintf("%s=%s", p.Name, val)}, nil
	}

	// List all or filter by prefix.
	environ := os.Environ()
	sort.Strings(environ)

	var sb strings.Builder
	count := 0
	for _, entry := range environ {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			continue
		}
		name := parts[0]
		value := parts[1]

		if p.Prefix != "" && !strings.HasPrefix(name, p.Prefix) {
			continue
		}

		if p.All || p.Prefix != "" {
			if isSensitiveEnv(name) {
				sb.WriteString(fmt.Sprintf("%s=<redacted>\n", name))
			} else {
				sb.WriteString(fmt.Sprintf("%s=%s\n", name, value))
			}
			count++
		}
	}

	// If neither name, prefix, nor all was specified, just show variable names.
	if !p.All && p.Prefix == "" {
		sb.Reset()
		for _, entry := range environ {
			parts := strings.SplitN(entry, "=", 2)
			if len(parts) >= 1 {
				sb.WriteString(parts[0])
				sb.WriteByte('\n')
				count++
			}
		}
	}

	if count == 0 {
		return &ToolOutput{Content: "No matching environment variables found."}, nil
	}

	return &ToolOutput{
		Content: sb.String(),
		Metadata: map[string]string{
			"count": fmt.Sprintf("%d", count),
		},
	}, nil
}

func isSensitiveEnv(name string) bool {
	upper := strings.ToUpper(name)
	if sensitiveEnvNames[upper] {
		return true
	}
	for _, prefix := range sensitiveEnvPrefixes {
		if strings.Contains(upper, prefix) {
			return true
		}
	}
	return false
}
