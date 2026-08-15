// SPDX-License-Identifier: Apache-2.0

package llms

// The seed registry is inherited by EVERY new session, so a wrong entry is not
// a cosmetic problem: it is a model the user can select and a 404 they have to
// diagnose. `gemini/gemini-3-pro` shipped for exactly that reason — a
// speculative name that Google never served (the 3.x line is Flash and
// Flash-Lite; the Pro tier is still 2.5).
//
// These tests cannot check a model id against a live API, so they guard the
// two things that are checkable locally: that no known-bogus name comes back,
// and that the seed's own claims about what rysh can RUN stay in sync with the
// provider selector.

import (
	"slices"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

// TestSeedModelsHaveNoRetiredNames pins the specific mistakes already made, so
// a revert or a copy-paste cannot quietly reintroduce them.
func TestSeedModelsHaveNoRetiredNames(t *testing.T) {
	// Model ids that do not exist at their provider. Keep the reason attached:
	// the next person needs to know why, not just that.
	bogus := map[string]string{
		"gemini-3-pro": "never served by Google; the 3.x line is Flash/Flash-Lite and the Pro tier is 2.5 " +
			"(gemini-3-pro-image is an IMAGE model, which is the likely source of the confusion)",
	}
	for _, m := range seedModels {
		if why, bad := bogus[m.Model]; bad {
			t.Errorf("seed declares %s/%s → model id %q: %s", m.Provider, m.Name, m.Model, why)
		}
	}
}

// TestSeedModelsAreWellFormed: every entry names a provider and a model id, and
// ref segments stay path-safe (they become file names under .rysh/llms).
func TestSeedModelsAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range seedModels {
		ref := m.Provider + "/" + m.Name
		if strings.TrimSpace(m.Provider) == "" || strings.TrimSpace(m.Name) == "" || strings.TrimSpace(m.Model) == "" {
			t.Errorf("seed entry %+v has an empty provider/name/model", m)
			continue
		}
		if _, _, err := ParseRef(ref); err != nil {
			t.Errorf("seed ref %q is not addressable: %v", ref, err)
		}
		if seen[ref] {
			t.Errorf("seed declares %q twice", ref)
		}
		seen[ref] = true
		if strings.TrimSpace(m.Description) == "" {
			t.Errorf("seed entry %q has no description — the listing shows it", ref)
		}
	}
}

// TestSeedNotExecutableNoteMatchesReality: the "declared for planning" note may
// only appear on entries rysh genuinely cannot run. It sat on the openai and
// gemini seeds long after the executor learned both dialects, telling users
// their working models were unusable.
func TestSeedNotExecutableNoteMatchesReality(t *testing.T) {
	for _, m := range seedModels {
		note := strings.Contains(m.Description, notExecutableNote)
		if note && m.Executable() {
			t.Errorf("%s/%s is runnable but described as not executable", m.Provider, m.Name)
		}
		if !note && !m.Executable() {
			t.Errorf("%s/%s is NOT runnable but its description does not say so", m.Provider, m.Name)
		}
	}
}

// TestExecutableProvidersMatchProviderSelection is the anti-drift guard.
// ExecutableProviders is duplicated here to keep this package a leaf; the list
// that decides what actually runs lives in internal/provider, and the two
// disagreeing is how `##llm openai/gpt-4o` came to refuse a model that
// `##pane model openai/gpt-4o` would run.
func TestExecutableProvidersMatchProviderSelection(t *testing.T) {
	for _, name := range ExecutableProviders {
		if !provider.IsKnownProviderName(name) {
			t.Errorf("ExecutableProviders lists %q, which selects no real provider", name)
		}
	}
	for _, name := range provider.KnownProviderNames() {
		if !slices.Contains(ExecutableProviders, name) {
			t.Errorf("provider %q is selectable but ExecutableProviders omits it — "+
				"##llm will refuse models the pane scope happily runs", name)
		}
	}
}
