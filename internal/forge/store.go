// Package forge ties the Forge pipeline to the live rysh runtime: it stores
// per-project integration definitions, ingests their specs into the IR, builds
// tool-packs, and registers the resulting tools into the agent registry under
// the §5.6 exposure policy. It is the daemon-side counterpart to the `rysh forge`
// CLI (which generates artifacts to disk).
package forge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/runtime"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/toolpack"
)

// Integration is a persisted, per-project integration definition. Secrets are
// never stored here; CredentialEnv names an environment variable that holds the
// credential at runtime.
type Integration struct {
	Name          string `json:"name"`
	Source        string `json:"source"`             // "openapi" | "graphql"
	SpecFile      string `json:"spec_file"`          // path under .rysh/forge/<name>/, relative to workDir
	BaseURL       string `json:"base_url,omitempty"` // override the spec's server URL
	CredentialEnv string `json:"credential_env,omitempty"`
	BasicUserEnv  string `json:"basic_user_env,omitempty"`
	BasicPassEnv  string `json:"basic_pass_env,omitempty"`
	// Auth declares a JWT/OAuth2 token-acquisition + refresh flow (forged-API auth
	// plan, Phase A). When set, it supersedes the static CredentialEnv bearer.
	// It holds only env-var names + URLs — never secret values.
	Auth    *runtime.AuthConfig `json:"auth,omitempty"`
	Policy  toolpack.Policy     `json:"policy"`
	Enabled bool                `json:"enabled"`
	Scope   string              `json:"scope,omitempty"` // scope key it was enabled at ("" / "global" ⇒ global)
}

const (
	SourceOpenAPI = "openapi"
	SourceGraphQL = "graphql"
	// SourceGRPC ingests a compiled protobuf FileDescriptorSet
	// (protoc --descriptor_set_out / buf build -o). Methods are modeled as
	// unary POSTs to the gRPC wire path, so calling them needs a server that
	// speaks JSON over that path (grpc-gateway transcoding or Connect) — see
	// ingest.GRPC.
	SourceGRPC = "grpc"
)

// dir returns <workDir>/.rysh/forge.
func dir(workDir string) string { return filepath.Join(workDir, ".rysh", "forge") }

// storePath returns the integrations index file.
func storePath(workDir string) string { return filepath.Join(dir(workDir), "integrations.json") }

// SpecDir returns the per-integration directory holding the stored spec.
func SpecDir(workDir, name string) string { return filepath.Join(dir(workDir), name) }

// Validate checks a definition is usable.
func (d Integration) Validate() error {
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("integration name is required")
	}
	switch d.Source {
	case SourceOpenAPI, SourceGraphQL, SourceGRPC, "":
	default:
		return fmt.Errorf("unknown source %q (want openapi, graphql or grpc)", d.Source)
	}
	if strings.TrimSpace(d.SpecFile) == "" {
		return fmt.Errorf("spec file is required")
	}
	return nil
}

func (d Integration) source() string {
	if d.Source == "" {
		return SourceOpenAPI
	}
	return d.Source
}

// LoadStore reads the integrations index. A missing file yields an empty slice.
func LoadStore(workDir string) ([]Integration, error) {
	data, err := os.ReadFile(storePath(workDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 {
		return nil, nil
	}
	var defs []Integration
	if err := json.Unmarshal(data, &defs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", storePath(workDir), err)
	}
	return defs, nil
}

// SaveStore writes the integrations index atomically.
func SaveStore(workDir string, defs []Integration) error {
	d := dir(workDir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	if defs == nil {
		defs = []Integration{}
	}
	data, err := json.MarshalIndent(defs, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(d, "integrations-*.json.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, storePath(workDir))
}

// StoreSpec writes spec bytes under the integration's directory and returns the
// workDir-relative path saved into the definition.
func StoreSpec(workDir, name string, spec []byte, ext string) (string, error) {
	d := SpecDir(workDir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	if ext == "" {
		ext = "json"
	}
	full := filepath.Join(d, "spec."+ext)
	if err := os.WriteFile(full, spec, 0o644); err != nil {
		return "", err
	}
	rel, err := filepath.Rel(workDir, full)
	if err != nil {
		return full, nil
	}
	return rel, nil
}

func upsert(defs []Integration, d Integration) []Integration {
	for i := range defs {
		if defs[i].Name == d.Name {
			defs[i] = d
			return defs
		}
	}
	return append(defs, d)
}

func remove(defs []Integration, name string) ([]Integration, bool) {
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
