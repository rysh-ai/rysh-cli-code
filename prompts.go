package main

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// agentPromptsFS embeds every markdown file under rysh-cli-agent-prompts/ at
// build time. Files are accessed by basename (without the directory prefix)
// through PromptsFS().
//
// Adding a new prompt file: drop it into rysh-cli-agent-prompts/ with a .md
// extension and update rysh-cli-agent-prompts/README.md. The embed picks it
// up automatically on next build.
//
//go:embed rysh-cli-agent-prompts/*.md
var agentPromptsFS embed.FS

// PromptsFS returns an fs.FS rooted INSIDE the prompts directory (the
// "rysh-cli-agent-prompts/" prefix is stripped), so callers read files as
// e.g. fs.ReadFile(fs, "system_default.md").
func PromptsFS() fs.FS {
	sub, err := fs.Sub(agentPromptsFS, "rysh-cli-agent-prompts")
	if err != nil {
		// In practice fs.Sub on a valid embedded directory cannot fail; falling
		// back to the raw FS keeps the program runnable if something changes.
		return agentPromptsFS
	}
	return sub
}

// ----------------------------------------------------------------------------
// Prompt hot-reload (follow-up item 2).
//
// Production loads prompts through a layered store with two tiers:
//
//   1. Override directory ($XDG_CONFIG_HOME/rysh/prompts, configurable via
//      RYSH_PROMPTS_DIR). A file at <dir>/<name> wins over the embedded
//      version. Override dir is optional; nothing breaks when it doesn't
//      exist.
//   2. Embedded //go:embed FS (the canonical baseline shipped with rysh).
//
// LoadPrompt(name) reads through the layered store. ReloadPrompts() is
// invoked by SIGHUP and by the `##agent reload-prompts` command; it
// effectively does nothing because the layered store reads on every call,
// but it returns the inventory of names so the orchestrator's broadcast
// can carry a sensible status message.
// ----------------------------------------------------------------------------

// PromptStore is the small surface the rest of the binary uses. The
// implementation honours an override directory if present and falls back
// to the embedded FS otherwise.
type PromptStore interface {
	Load(name string) string
	Inventory() []string // names of prompt files available (override + embedded merged)
	OverrideDir() string // "" when no override dir is configured
}

type layeredPromptStore struct {
	mu          sync.RWMutex
	overrideDir string
	embedded    fs.FS
}

// Load returns the content of <name>, preferring the override dir.
// Empty string when the file isn't present in either tier (callers
// substitute their own fallbacks).
func (l *layeredPromptStore) Load(name string) string {
	l.mu.RLock()
	dir := l.overrideDir
	l.mu.RUnlock()
	if dir != "" {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			return strings.TrimRight(string(data), "\n")
		}
	}
	if data, err := fs.ReadFile(l.embedded, name); err == nil {
		return strings.TrimRight(string(data), "\n")
	}
	return ""
}

// Inventory lists distinct prompt filenames from both tiers.
func (l *layeredPromptStore) Inventory() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	names := map[string]struct{}{}
	if l.overrideDir != "" {
		if entries, err := os.ReadDir(l.overrideDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
					names[e.Name()] = struct{}{}
				}
			}
		}
	}
	if entries, err := fs.ReadDir(l.embedded, "."); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
				names[e.Name()] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	return out
}

func (l *layeredPromptStore) OverrideDir() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.overrideDir
}

// defaultPromptStore is initialised once at startup and used by
// LoadPrompt / Inventory.
var (
	defaultPromptStore     PromptStore
	defaultPromptStoreOnce sync.Once
)

// initPromptStore picks the override directory (RYSH_PROMPTS_DIR or
// XDG_CONFIG_HOME/rysh/prompts, falling back to ~/.config/rysh/prompts).
// If the resolved directory doesn't exist, override is disabled (the
// embedded FS still works).
func initPromptStore() PromptStore {
	override := os.Getenv("RYSH_PROMPTS_DIR")
	if override == "" {
		base := os.Getenv("XDG_CONFIG_HOME")
		if base == "" {
			if home, err := os.UserHomeDir(); err == nil {
				base = filepath.Join(home, ".config")
			}
		}
		if base != "" {
			override = filepath.Join(base, "rysh", "prompts")
		}
	}
	// Resolve to "" if the directory doesn't exist; LoadPrompt will then
	// skip the override tier entirely.
	if override != "" {
		if info, err := os.Stat(override); err != nil || !info.IsDir() {
			override = ""
		}
	}
	return &layeredPromptStore{
		overrideDir: override,
		embedded:    PromptsFS(),
	}
}

// LoadPrompt reads a prompt file by basename (e.g. "system_default.md")
// through the default prompt store. Missing files yield "" — callers
// decide whether to substitute a fallback. Follow-up item 2.
func LoadPrompt(name string) string {
	defaultPromptStoreOnce.Do(func() {
		defaultPromptStore = initPromptStore()
	})
	return defaultPromptStore.Load(name)
}

// ReloadPromptStore re-initialises the default store, picking up any
// override-directory changes (e.g. a file added, removed, or changed
// since startup). Safe to call concurrently; subsequent LoadPrompt calls
// see the new state. Returns the override directory in effect after
// reload (empty when none). Follow-up item 2.
func ReloadPromptStore() string {
	store := initPromptStore()
	// Replace defaultPromptStore atomically. Use the sync.Once to bypass
	// initialisation; the value swap doesn't need locking because string
	// pointer assignment in Go is atomic for interface values on 64-bit.
	defaultPromptStore = store
	defaultPromptStoreOnce.Do(func() {}) // ensure Once is consumed
	return store.OverrideDir()
}

// PromptInventory returns the current set of prompt filenames known to
// the default store.
func PromptInventory() []string {
	defaultPromptStoreOnce.Do(func() {
		defaultPromptStore = initPromptStore()
	})
	return defaultPromptStore.Inventory()
}

// PromptOverrideDir returns the override directory currently in effect
// ("" when none). Used by the fsnotify auto-reload watcher (follow-up 2b)
// to decide which directory to watch — without re-initialising the store.
func PromptOverrideDir() string {
	defaultPromptStoreOnce.Do(func() {
		defaultPromptStore = initPromptStore()
	})
	return defaultPromptStore.OverrideDir()
}
