// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// paneModel builds a minimal Model whose active pane is the given snapshot.
func paneModel(active domain.PaneSnapshot) Model {
	return Model{
		snapshot: domain.WorkspaceSnapshot{
			ActivePaneID: active.ID,
			Tabs: []domain.TabSnapshot{{
				ID: "t1",
				Lanes: []domain.LaneSnapshot{{
					ID: "l1",
					PaneGroups: []domain.PaneGroupSnapshot{{
						ID:           "g1",
						ActivePaneID: active.ID,
						Panes:        []domain.PaneSnapshot{active},
					}},
				}},
			}},
		},
	}
}

func TestNextEnabledModeFiltersByEnabledModes(t *testing.T) {
	cases := []struct {
		name    string
		enabled []string
		current string
		want    string
	}{
		{"default cycle shell->prompt", nil, "shell", "prompt"},
		{"default wraps chat->shell", nil, "chat", "shell"},
		{"chat disabled rysh->shell", []string{"shell", "prompt", "rysh"}, "rysh", "shell"},
		{"only shell stays shell", []string{"shell"}, "shell", "shell"},
		{"web enabled chat->web", []string{"shell", "prompt", "rysh", "chat", "web"}, "chat", "web"},
		{"web enabled wraps web->shell", []string{"shell", "prompt", "rysh", "chat", "web"}, "web", "shell"},
		{"prompt disabled shell->rysh", []string{"shell", "rysh", "chat"}, "shell", "rysh"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := paneModel(domain.PaneSnapshot{ID: "p1", EnabledModes: tc.enabled})
			if got := m.nextEnabledMode(tc.current); got != tc.want {
				t.Errorf("nextEnabledMode(%q) enabled=%v = %q, want %q", tc.current, tc.enabled, got, tc.want)
			}
		})
	}
}

func TestNextEnabledModeExternalHiddenWithoutHumanoid(t *testing.T) {
	// external enabled but no ExternalOutput → skipped (stays shell).
	m := paneModel(domain.PaneSnapshot{ID: "p1", EnabledModes: []string{"shell", "external"}})
	if got := m.nextEnabledMode("shell"); got != "shell" {
		t.Errorf("external hidden without humanoid output: got %q, want shell", got)
	}
	// With humanoid output, external becomes reachable.
	m = paneModel(domain.PaneSnapshot{ID: "p1", EnabledModes: []string{"shell", "external"}, ExternalOutput: "hi"})
	if got := m.nextEnabledMode("shell"); got != "external" {
		t.Errorf("external reachable with humanoid output: got %q, want external", got)
	}
}

func TestNextEnabledModeShareDisabledAppliesOnTop(t *testing.T) {
	// chat enabled per-pane but share-disabled → skipped, wraps to shell.
	m := paneModel(domain.PaneSnapshot{
		ID:                "p1",
		EnabledModes:      []string{"shell", "prompt", "chat"},
		ShareRestrictions: &domain.ShareRestrictions{DisabledModes: []string{"chat"}},
	})
	if got := m.nextEnabledMode("prompt"); got != "shell" {
		t.Errorf("share-disabled chat should be skipped: got %q, want shell", got)
	}
}

func TestPaneModeEnabled(t *testing.T) {
	def := domain.PaneSnapshot{} // pre-field snapshot → default set
	for _, m := range []string{"shell", "prompt", "rysh", "chat"} {
		if !paneModeEnabled(def, m) {
			t.Errorf("default set should include %q", m)
		}
	}
	if paneModeEnabled(def, "web") {
		t.Errorf("default set should not include web")
	}
	p := domain.PaneSnapshot{EnabledModes: []string{"shell", "web"}}
	if !paneModeEnabled(p, "web") || !paneModeEnabled(p, "shell") {
		t.Errorf("explicit set should include shell+web")
	}
	if paneModeEnabled(p, "chat") {
		t.Errorf("chat should not be enabled in explicit set")
	}
}
