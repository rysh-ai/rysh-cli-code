package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ServerDef is a persisted MCP server definition. It is stored as JSON in the
// project-local .rysh/mcp.json file (alongside .rysh/agents/), so MCP servers
// are a per-project asset that can be checked into git.
type ServerDef struct {
	Name      string `json:"name"`
	Transport string `json:"transport"` // "stdio" | "http"

	// stdio transport
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Env     []string `json:"env,omitempty"` // "KEY=VALUE" entries added to the child env

	// http (Streamable HTTP) transport
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"` // e.g. {"Authorization": "Bearer …"}

	// Cross-cutting policy
	Prefix           *string `json:"prefix,omitempty"`            // tool-name prefix; nil ⇒ "<name>_"; "" ⇒ none
	RequiresApproval bool    `json:"requires_approval,omitempty"` // gate every call from this server
	MaxTools         int     `json:"max_tools,omitempty"`         // cap registered tools (0 ⇒ defaultMaxTools)
	Scope            string  `json:"scope,omitempty"`             // scope key the server's tools register at ("" ⇒ global)
}

// TransportStdio / TransportHTTP are the supported transport identifiers.
const (
	TransportStdio = "stdio"
	TransportHTTP  = "http"
)

// Validate checks a definition is internally consistent before connecting.
func (d ServerDef) Validate() error {
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("server name is required")
	}
	switch d.Transport {
	case TransportStdio:
		if strings.TrimSpace(d.Command) == "" {
			return fmt.Errorf("stdio transport requires a command")
		}
	case TransportHTTP:
		if strings.TrimSpace(d.URL) == "" {
			return fmt.Errorf("http transport requires a url")
		}
		if !strings.HasPrefix(d.URL, "http://") && !strings.HasPrefix(d.URL, "https://") {
			return fmt.Errorf("http url must start with http:// or https://")
		}
	case "":
		return fmt.Errorf("transport is required (stdio or http)")
	default:
		return fmt.Errorf("unknown transport %q (want stdio or http)", d.Transport)
	}
	return nil
}

// prefix resolves the tool-name prefix for this server: the explicit Prefix when
// set (including an empty string for no prefix), otherwise "<name>_".
func (d ServerDef) prefix() string {
	if d.Prefix != nil {
		return *d.Prefix
	}
	return d.Name + "_"
}

// StorePath returns the location of the MCP server store for a project rooted at
// workDir: <workDir>/.rysh/mcp.json.
func StorePath(workDir string) string {
	return filepath.Join(workDir, ".rysh", "mcp.json")
}

// LoadStore reads persisted server definitions. A missing file yields an empty
// slice with no error (a project simply has no MCP servers configured yet).
func LoadStore(workDir string) ([]ServerDef, error) {
	data, err := os.ReadFile(StorePath(workDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	data = trimSpaceBytes(data)
	if len(data) == 0 {
		return nil, nil
	}
	var defs []ServerDef
	if err := json.Unmarshal(data, &defs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", StorePath(workDir), err)
	}
	return defs, nil
}

// SaveStore writes server definitions, creating the .rysh directory if needed.
// The write is atomic (temp file + rename) so a crash never leaves a partial
// store.
func SaveStore(workDir string, defs []ServerDef) error {
	dir := filepath.Join(workDir, ".rysh")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if defs == nil {
		defs = []ServerDef{}
	}
	data, err := json.MarshalIndent(defs, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	path := StorePath(workDir)
	tmp, err := os.CreateTemp(dir, "mcp-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// upsertDef inserts or replaces (by name) a definition in a slice.
func upsertDef(defs []ServerDef, d ServerDef) []ServerDef {
	for i := range defs {
		if defs[i].Name == d.Name {
			defs[i] = d
			return defs
		}
	}
	return append(defs, d)
}

// removeDef drops a definition by name, reporting whether anything was removed.
func removeDef(defs []ServerDef, name string) ([]ServerDef, bool) {
	out := defs[:0]
	removed := false
	for _, d := range defs {
		if d.Name == name {
			removed = true
			continue
		}
		out = append(out, d)
	}
	return out, removed
}

func trimSpaceBytes(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}
