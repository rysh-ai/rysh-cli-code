// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"sync"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/memory"
	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// Cross-session memory (design 010 §3a): entries kept in the workspace's
// memory store are injected into the agent's system prompt, after the RYSH.md
// block, under the resolved memory policy. The WorkspaceActor refreshes the
// rules on every policy load (SetMemoryRules) and after ##memory add/forget
// (RebuildMemory), then broadcasts the new prompt like ##agent reload-prompts.

// memoryState is the Setup's memory wiring. The rules are read by the agent's
// memory tool from pane goroutines, hence the lock.
type memoryState struct {
	mu    sync.RWMutex
	store *memory.Store
	rules policy.MemoryRules
	// fallbackBase / fallbackWorkDir rebuild the prompt when no Prompts were
	// applied (tests, hosts that never call ApplyPrompts).
	fallbackBase    string
	fallbackWorkDir string
}

// startupMemoryRules resolves the memory policy before the WorkspaceActor
// loads the full policy (which may add the central org document). Fail-closed
// like every load: a broken file turns memory off.
func startupMemoryRules(cfg config.Config) policy.MemoryRules {
	if cfg.RyshDir == "" {
		return policy.MemoryRules{Reason: "no rysh directory"}
	}
	return policy.Merge(policy.LoadOrg(cfg.Policy.OrgFile), policy.Load(cfg.RyshDir)).MemoryRules()
}

// openMemoryStore opens the workspace store, or nil without a rysh directory
// (never a store relative to the process cwd).
func openMemoryStore(cfg config.Config) *memory.Store {
	if cfg.RyshDir == "" {
		return nil
	}
	return memory.Open(cfg.RyshDir)
}

// memoryGate tells the memory tool whether writes are allowed now.
func (s *Setup) memoryGate() (bool, string) {
	r := s.MemoryRules()
	if r.Enabled {
		return true, ""
	}
	if r.Reason != "" {
		return false, r.Reason
	}
	return false, "memory is disabled by policy (memory.enabled: false)"
}

// initMemory wires the store and rules and rebuilds the prompt from base.
func (s *Setup) initMemory(store *memory.Store, rules policy.MemoryRules, base string) {
	s.mem = &memoryState{store: store, rules: rules, fallbackBase: base, fallbackWorkDir: s.workDir}
	s.sem = &semanticState{}
	s.RebuildMemory()
}

// MemoryStore is the workspace memory store (nil when none is wired).
func (s *Setup) MemoryStore() *memory.Store {
	if s == nil || s.mem == nil {
		return nil
	}
	return s.mem.store
}

// MemoryRules is the memory policy in force. With no memory wired, memory is
// off.
func (s *Setup) MemoryRules() policy.MemoryRules {
	if s == nil || s.mem == nil {
		return policy.MemoryRules{Reason: "memory is not wired in this session"}
	}
	s.mem.mu.RLock()
	defer s.mem.mu.RUnlock()
	return s.mem.rules
}

// SetMemoryRules applies a freshly loaded memory policy and rebuilds the prompt.
func (s *Setup) SetMemoryRules(r policy.MemoryRules) {
	if s == nil || s.mem == nil {
		return
	}
	s.mem.mu.Lock()
	s.mem.rules = r
	s.mem.mu.Unlock()
	s.RebuildMemory()
}

// knownSecrets returns the session's registered secret values, for the
// scrubber only. Never logged.
func (s *Setup) knownSecrets() []string {
	if s == nil || s.KnownSecrets == nil {
		return nil
	}
	return s.KnownSecrets()
}

// crossSessionMemoryBlock renders the injected block, or "" when memory is
// off, injection is off, or there is nothing to inject.
func (s *Setup) crossSessionMemoryBlock() string {
	store := s.MemoryStore()
	rules := s.MemoryRules()
	if store == nil || !rules.Enabled || !rules.Inject {
		return ""
	}
	list, err := store.List(s.knownSecrets())
	if err != nil {
		return ""
	}
	return memory.InjectionBlock(list, rules.MaxInjectBytes)
}

// withCrossSessionMemory appends the block to a prompt.
func (s *Setup) withCrossSessionMemory(prompt string) string {
	if block := s.crossSessionMemoryBlock(); block != "" {
		return prompt + "\n\n" + block
	}
	return prompt
}

// RebuildMemory re-assembles the system prompt so it reflects the store and
// rules now. The caller broadcasts the result to live panes.
func (s *Setup) RebuildMemory() {
	if s == nil || s.mem == nil {
		return
	}
	if s.Prompts != nil {
		s.ApplyPrompts(s.Prompts, s.workDir)
		return
	}
	s.SystemPrompt = s.withCrossSessionMemory(appendProjectMemory(s.mem.fallbackBase, s.mem.fallbackWorkDir))
}
