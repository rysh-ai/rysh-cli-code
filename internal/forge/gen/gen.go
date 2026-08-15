// SPDX-License-Identifier: Apache-2.0

// Package gen defines the Forge generator framework: a pluggable Generator
// interface, a registry, and a FileSet output collector. Each concrete target
// (docs, mcp server, Go/TS/Python SDKs, tool-pack manifest) lives in a subpackage
// that registers itself via init(); the CLI blank-imports those subpackages to
// populate the registry.
package gen

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
)

// Options parameterize a generation run.
type Options struct {
	Prefix      string // tool / symbol name prefix
	PackageName string // package/namespace name for code targets
	ModulePath  string // module path for the Go SDK (go.mod)
}

// FileSet collects generated files keyed by relative path.
type FileSet struct {
	Files map[string][]byte
}

// NewFileSet returns an empty FileSet.
func NewFileSet() *FileSet { return &FileSet{Files: map[string][]byte{}} }

// Add stores raw file content at a relative path.
func (f *FileSet) Add(path string, content []byte) { f.Files[path] = content }

// AddString stores string content at a relative path.
func (f *FileSet) AddString(path, content string) { f.Files[path] = []byte(content) }

// Paths returns the sorted relative paths in the set.
func (f *FileSet) Paths() []string {
	out := make([]string, 0, len(f.Files))
	for p := range f.Files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// WriteDir writes every file under root, creating parent directories.
func (f *FileSet) WriteDir(root string) error {
	for rel, content := range f.Files {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Merge folds another FileSet's files into this one, prefixing their paths.
func (f *FileSet) Merge(prefix string, other *FileSet) {
	for p, c := range other.Files {
		f.Files[filepath.Join(prefix, p)] = c
	}
}

// Generator produces files from an IR.API.
type Generator interface {
	// Name is the target identifier, e.g. "docs", "go-sdk", "mcp-http".
	Name() string
	// Generate renders the API into a FileSet.
	Generate(api *ir.API, opts Options) (*FileSet, error)
}

var registry = map[string]Generator{}

// Register adds a generator. Intended to be called from a subpackage init().
func Register(g Generator) { registry[g.Name()] = g }

// Get returns a generator by name.
func Get(name string) (Generator, bool) { g, ok := registry[name]; return g, ok }

// Names returns the sorted list of registered target names.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
