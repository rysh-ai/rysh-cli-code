// SPDX-License-Identifier: Apache-2.0

package fleet

import "testing"

// Design 030 §9.3, founder ruling D-24 (wave W3): only a fleet's head may post
// to the reserved fleet board. A head is a registered member holding a top
// role: roadmap-manager, ceo or portfolio-lead.

func TestW3TopRoles(t *testing.T) {
	for _, r := range []string{"roadmap-manager", "ceo", "portfolio-lead", "CEO", " Roadmap-Manager "} {
		if !IsTopRole(r) {
			t.Errorf("IsTopRole(%q) = false", r)
		}
	}
	for _, r := range []string{"", "worker", "manager", "fleet-manager", "misc", "lead", "head"} {
		if IsTopRole(r) {
			t.Errorf("IsTopRole(%q) = true", r)
		}
	}
}

func TestW3FleetBoardPoster(t *testing.T) {
	fleets := []Fleet{
		{Name: "epic-a", Members: []Member{
			{PaneID: "p-rm", Role: "roadmap-manager"},
			{PaneID: "p-ceo", Role: "ceo", Parent: "p-rm"},
			{PaneID: "p-wkr", Role: "worker", Parent: "p-ceo"},
		}},
		{Name: "port", Members: []Member{{PaneID: "p-pl", Role: "portfolio-lead"}}},
	}
	for pane, want := range map[string]string{"p-rm": "epic-a", "p-ceo": "epic-a", "p-pl": "port"} {
		if got, ok := FleetBoardPoster(fleets, pane); !ok || got != want {
			t.Errorf("FleetBoardPoster(%s) = %q,%v; want %q,true", pane, got, ok, want)
		}
	}
	for _, pane := range []string{"p-wkr", "p-nobody", ""} {
		if got, ok := FleetBoardPoster(fleets, pane); ok {
			t.Errorf("FleetBoardPoster(%q) = %q,true; want refused", pane, got)
		}
	}
}
