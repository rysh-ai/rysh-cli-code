// SPDX-License-Identifier: Apache-2.0

// Package llms is the file-backed LLM model registry behind the ##llm
// command. Each known model is ONE YAML file at
//
//	<rysh-dir>/llms/<provider>/<model-name>
//
// e.g. .rysh/llms/anthropic/fable5 or .rysh/llms/openai/gpt55. The ##llm
// command lists ONLY what these files declare, activates one as the session
// default, and persists new declarations (##llm model add openai/gpt55).
// A first use seeds the folder with a starter set for anthropic, openai,
// grok, and gemini.
package llms

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ModelSpec is the YAML content of one model file. Name/Provider mirror the
// file's location (path is authoritative on read; the fields are filled in
// for callers).
type ModelSpec struct {
	Provider    string `yaml:"provider"`
	Name        string `yaml:"name"`
	Model       string `yaml:"model"` // provider API model id
	Description string `yaml:"description,omitempty"`
	MaxTokens   int    `yaml:"max_tokens,omitempty"`
	// Effort is an optional default reasoning effort activated with the model
	// (low|medium|high|xhigh|max — anthropic providers only).
	Effort string `yaml:"effort,omitempty"`
	Added  string `yaml:"added,omitempty"` // YYYY-MM-DD, set on Add
	// Disabled parks a model without deleting it: ##llm use and ##pane model
	// refuse it, while ##llm list still shows it (marked) so it can be turned
	// back on. Omitempty keeps every pre-existing file valid and enabled.
	Disabled bool `yaml:"disabled,omitempty"`
}

// ExecutableProviders are the providers rysh can actually RUN a model on —
// the ones internal/provider builds a real agentic provider for. Everything
// else in the registry is a declaration for planning, selectable nowhere.
//
// This was a single provider ("anthropic") for as long as the executor spoke
// only the Anthropic Messages API. It no longer does: design 002 added the
// OpenAI-compatible dialect (Ollama, and Gemini via its compat surface), and
// OpenAI proper now speaks the Responses API. Leaving the old value in place
// meant `##llm openai/gpt-4o` refused a model that `##pane model
// openai/gpt-4o` would happily run — the pane path never consulted this.
//
// Kept as a local list rather than an import of internal/provider so this
// package stays a leaf; TestExecutableProvidersMatchProviderSelection pins the
// two against each other so they cannot drift.
var ExecutableProviders = []string{"anthropic", "claude-cli", "gemini", "ollama", "openai"}

// Executable reports whether rysh's executor can run this model.
func (m ModelSpec) Executable() bool {
	return slices.Contains(ExecutableProviders, strings.ToLower(strings.TrimSpace(m.Provider)))
}

// refPattern validates provider and model-name path segments: no separators,
// no traversal, YAML-friendly.
var refPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// ParseRef splits "provider/model-name" and validates both segments.
func ParseRef(ref string) (providerName, modelName string, err error) {
	parts := strings.SplitN(strings.TrimSpace(ref), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected <provider>/<model-name>, got %q", ref)
	}
	if !refPattern.MatchString(parts[0]) || !refPattern.MatchString(parts[1]) {
		return "", "", fmt.Errorf("provider/model-name may only contain letters, digits, '.', '_', '-': %q", ref)
	}
	return parts[0], parts[1], nil
}

// Store reads and writes the registry under <rysh-dir>/llms.
type Store struct {
	dir string
}

// NewStore roots the registry at <ryshDir>/llms.
func NewStore(ryshDir string) *Store {
	return &Store{dir: filepath.Join(ryshDir, "llms")}
}

// Dir returns the registry root.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(providerName, modelName string) string {
	return filepath.Join(s.dir, providerName, modelName)
}

// Path returns the on-disk location of one model file (whether or not it
// exists) — surfaced by `##llm info`.
func (s *Store) Path(providerName, modelName string) string {
	return s.path(providerName, modelName)
}

// Get loads one model file.
func (s *Store) Get(providerName, modelName string) (*ModelSpec, error) {
	data, err := os.ReadFile(s.path(providerName, modelName))
	if err != nil {
		return nil, err
	}
	var spec ModelSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse %s/%s: %w", providerName, modelName, err)
	}
	// The path is authoritative for identity.
	spec.Provider, spec.Name = providerName, modelName
	if spec.Model == "" {
		spec.Model = modelName
	}
	return &spec, nil
}

// Add persists a model file (creating the provider folder), returning its
// path. Overwrites an existing declaration for the same provider/name.
func (s *Store) Add(spec ModelSpec) (string, error) {
	if !refPattern.MatchString(spec.Provider) || !refPattern.MatchString(spec.Name) {
		return "", fmt.Errorf("invalid provider/model-name %q/%q", spec.Provider, spec.Name)
	}
	if spec.Model == "" {
		spec.Model = spec.Name
	}
	if spec.Added == "" {
		spec.Added = time.Now().Format("2006-01-02")
	}
	if err := os.MkdirAll(filepath.Join(s.dir, spec.Provider), 0o755); err != nil {
		return "", err
	}
	data, err := yaml.Marshal(spec)
	if err != nil {
		return "", err
	}
	p := s.path(spec.Provider, spec.Name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// SetDisabled flips one model file's disabled flag and rewrites it in place,
// returning the updated spec. A disabled model stays declared and inspectable;
// only activation is refused.
func (s *Store) SetDisabled(providerName, modelName string, disabled bool) (*ModelSpec, error) {
	spec, err := s.Get(providerName, modelName)
	if err != nil {
		return nil, err
	}
	if spec.Disabled == disabled {
		return spec, nil
	}
	spec.Disabled = disabled
	if _, err := s.Add(*spec); err != nil {
		return nil, err
	}
	return spec, nil
}

// List returns the registry content: provider names in sorted order and each
// provider's models sorted by name. Unparseable files are skipped (listed
// callers should not brick on one bad YAML file).
func (s *Store) List() (providers []string, byProvider map[string][]ModelSpec, err error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, map[string][]ModelSpec{}, nil
		}
		return nil, nil, err
	}
	byProvider = map[string][]ModelSpec{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		providerName := e.Name()
		files, err := os.ReadDir(filepath.Join(s.dir, providerName))
		if err != nil {
			continue
		}
		var specs []ModelSpec
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			spec, err := s.Get(providerName, f.Name())
			if err != nil {
				continue
			}
			specs = append(specs, *spec)
		}
		if len(specs) > 0 {
			sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
			byProvider[providerName] = specs
			providers = append(providers, providerName)
		}
	}
	sort.Strings(providers)
	return providers, byProvider, nil
}

// SeedIfEmpty populates the registry with the starter set when the llms
// folder does not exist yet (or holds no models). Existing content is never
// touched — the folder is the single source of truth once it has models.
func (s *Store) SeedIfEmpty() error {
	providers, _, err := s.List()
	if err != nil {
		return err
	}
	if len(providers) > 0 {
		return nil
	}
	for _, spec := range seedModels {
		if _, err := s.Add(spec); err != nil {
			return err
		}
	}
	return nil
}

const notExecutableNote = "no rysh executor for this provider; declared for planning"

// seedModels is the starter registry written by SeedIfEmpty. EVERY entry must
// name a model id the provider actually serves: this list is inherited by every
// new session, so a speculative name is not a harmless placeholder — it is a
// model the user can select and a 404 they have to diagnose. `gemini-3-pro` was
// exactly that (Google's 3.x line is Flash and Flash-Lite; the Pro tier is
// still 2.5, and `gemini-3-pro-image` is an image model, not a chat one).
//
// Verified against the providers' own model lists: OpenAI's /v1/models and
// Google's published model page.
var seedModels = []ModelSpec{
	{Provider: "anthropic", Name: "fable5", Model: "claude-fable-5",
		Description: "Anthropic's most intelligent generally available model (Mythos-class tier)."},
	{Provider: "anthropic", Name: "opus-4-8", Model: "claude-opus-4-8",
		Description: "Opus-tier alias; rysh's built-in session default and until-judge seat."},
	{Provider: "anthropic", Name: "sonnet-5", Model: "claude-sonnet-5",
		Description: "Sonnet-tier alias; rysh's built-in internal-loop (do legs) seat."},
	{Provider: "anthropic", Name: "haiku-4-5", Model: "claude-haiku-4-5-20251001",
		Description: "Fast/cheap tier for lightweight legs."},
	{Provider: "openai", Name: "sol", Model: "gpt-5.6-sol",
		Description: "GPT-5.6 reasoning tier. Needs the Responses API, which rysh speaks."},
	{Provider: "openai", Name: "terra", Model: "gpt-5.6-terra",
		Description: "GPT-5.6 mid tier."},
	{Provider: "openai", Name: "luna", Model: "gpt-5.6-luna",
		Description: "GPT-5.6 fast/cheap tier."},
	{Provider: "openai", Name: "gpt-5.1", Model: "gpt-5.1",
		Description: "Previous frontier generation."},
	{Provider: "openai", Name: "gpt-4o", Model: "gpt-4o",
		Description: "Older general-purpose tier; cheap and widely available."},
	{Provider: "gemini", Name: "gemini-3.6-flash", Model: "gemini-3.6-flash",
		Description: "Latest Flash tier: strongest agentic/multimodal work per token."},
	{Provider: "gemini", Name: "gemini-3.5-flash", Model: "gemini-3.5-flash",
		Description: "Near-Pro coding and parallel agentic execution at Flash cost."},
	{Provider: "gemini", Name: "gemini-3.5-flash-lite", Model: "gemini-3.5-flash-lite",
		Description: "Fastest, lowest-cost tier for high-throughput legs."},
	{Provider: "gemini", Name: "gemini-2.5-pro", Model: "gemini-2.5-pro",
		Description: "The Pro tier — the 3.x line is Flash/Flash-Lite only, so Pro work stays on 2.5."},
	{Provider: "gemini", Name: "gemini-2.5-flash", Model: "gemini-2.5-flash",
		Description: "rysh's built-in default when a session selects gemini without pinning a model."},
	// Grok has no rysh executor: no provider name selects it (see
	// internal/provider.KnownProviderNames), so these stay declarations.
	{Provider: "grok", Name: "grok-4", Model: "grok-4", Description: notExecutableNote},
	{Provider: "grok", Name: "grok-3", Model: "grok-3", Description: notExecutableNote},
}
