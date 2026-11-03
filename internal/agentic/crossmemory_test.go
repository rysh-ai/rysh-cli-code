// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/memory"
	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

func memorySetup(t *testing.T) (*Setup, *memory.Store) {
	t.Helper()
	ryshDir := filepath.Join(t.TempDir(), ".rysh")
	store := memory.Open(ryshDir)
	s := &Setup{workDir: t.TempDir()}
	s.initMemory(store, policy.Empty().MemoryRules(), "base prompt")
	return s, store
}

func TestSystemPromptCarriesRememberedEntries(t *testing.T) {
	s, store := memorySetup(t)
	e, err := store.Add(memory.KindPreference, "answer in British English", memory.SourceUser, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.RebuildMemory()
	if !strings.Contains(s.SystemPrompt, "base prompt") {
		t.Fatalf("base prompt lost")
	}
	if !strings.Contains(s.SystemPrompt, "answer in British English") || !strings.Contains(s.SystemPrompt, "["+e.ID+"]") {
		t.Fatalf("remembered entry (with its id) not in the system prompt")
	}

	// Forgetting it and rebuilding removes it.
	if err := store.Forget(e.ID); err != nil {
		t.Fatal(err)
	}
	s.RebuildMemory()
	if strings.Contains(s.SystemPrompt, "British English") {
		t.Fatalf("forgotten entry still in the system prompt")
	}
}

func TestPolicyTurnsInjectionOff(t *testing.T) {
	s, store := memorySetup(t)
	if _, err := store.Add(memory.KindFact, "the staging db is on port 5433", memory.SourceUser, nil); err != nil {
		t.Fatal(err)
	}
	no := false
	p := policy.Empty()
	p.Memory.Inject = &no
	s.SetMemoryRules(p.MemoryRules())
	if strings.Contains(s.SystemPrompt, "5433") {
		t.Fatalf("inject: false still injected")
	}
	if !s.MemoryRules().Enabled {
		t.Fatalf("inject: false must leave memory enabled")
	}

	broken := policy.Empty()
	broken.Err = errErr("parse")
	s.SetMemoryRules(broken.MemoryRules())
	if strings.Contains(s.SystemPrompt, "5433") || s.MemoryRules().Enabled {
		t.Fatalf("a broken policy must inject nothing and disable memory")
	}

	s.SetMemoryRules(policy.Empty().MemoryRules())
	if !strings.Contains(s.SystemPrompt, "5433") {
		t.Fatalf("a fixed policy must inject again")
	}
}

func TestWithheldEntryNeverReachesThePrompt(t *testing.T) {
	s, store := memorySetup(t)
	known := "kv-" + strings.Repeat("Zq9", 4)
	s.KnownSecrets = func() []string { return []string{known} }
	// Written before the value was registered as a secret (file form).
	if _, err := store.Add(memory.KindNote, "the box answers to "+known, memory.SourceUser, nil); err != nil {
		t.Fatal(err)
	}
	s.RebuildMemory()
	if strings.Contains(s.SystemPrompt, known) {
		t.Fatalf("an entry holding a registered secret value reached the prompt")
	}
}

type errErr string

func (e errErr) Error() string { return string(e) }
