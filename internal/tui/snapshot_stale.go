// SPDX-License-Identifier: Apache-2.0

package tui

import "github.com/rysh-ai/rysh-cli-code/internal/domain"

// carryStalePanes returns next with every Stale pane replaced by the pane as
// the previous snapshot had it.
//
// A Stale pane is one whose actor missed the daemon's snapshot deadline: what
// the daemon sent is its last answer or a zero-valued placeholder, never the
// present. Taking it at face value is what made a busy session flip a claude
// pane to its shell view and back — the placeholder's RawMode=false sent
// syncRawMode out of modeRaw, and the next on-time answer sent it back in. The
// pane's previous state is the best reading there is, so keep it.
//
// A group whose panes are ALL stale is the lane-level timeout (the whole group
// missed its deadline); its previous panes are carried as a set so stacked
// siblings do not vanish for a frame. A stale pane this front-end has never
// seen is kept as sent — still marked Stale, so syncRawMode will not act on it.
func carryStalePanes(prev, next domain.WorkspaceSnapshot) domain.WorkspaceSnapshot {
	if !snapshotHasStale(next) {
		return next
	}
	prevPanes := make(map[string]domain.PaneSnapshot)
	prevGroups := make(map[string][]domain.PaneSnapshot)
	for _, tab := range prev.Tabs {
		for _, lane := range tab.Lanes {
			for _, g := range lane.PaneGroups {
				prevGroups[g.ID] = g.Panes
				for _, p := range g.Panes {
					prevPanes[p.ID] = p
				}
			}
		}
	}
	// next shares its slices with the decoded message; copy each level we
	// write so the caller's value is never mutated through an alias.
	next.Tabs = append([]domain.TabSnapshot(nil), next.Tabs...)
	for ti := range next.Tabs {
		lanes := append([]domain.LaneSnapshot(nil), next.Tabs[ti].Lanes...)
		next.Tabs[ti].Lanes = lanes
		for li := range lanes {
			groups := append([]domain.PaneGroupSnapshot(nil), lanes[li].PaneGroups...)
			lanes[li].PaneGroups = groups
			for gi := range groups {
				g := &groups[gi]
				if allPanesStale(g.Panes) {
					if old, ok := prevGroups[g.ID]; ok && len(old) > 0 {
						g.Panes = append([]domain.PaneSnapshot(nil), old...)
						continue
					}
				}
				panes := append([]domain.PaneSnapshot(nil), g.Panes...)
				g.Panes = panes
				for pi := range panes {
					if !panes[pi].Stale {
						continue
					}
					if old, ok := prevPanes[panes[pi].ID]; ok {
						panes[pi] = old
					}
				}
			}
		}
	}
	return next
}

func snapshotHasStale(s domain.WorkspaceSnapshot) bool {
	for _, tab := range s.Tabs {
		for _, lane := range tab.Lanes {
			for _, g := range lane.PaneGroups {
				for _, p := range g.Panes {
					if p.Stale {
						return true
					}
				}
			}
		}
	}
	return false
}

func allPanesStale(panes []domain.PaneSnapshot) bool {
	if len(panes) == 0 {
		return false
	}
	for _, p := range panes {
		if !p.Stale {
			return false
		}
	}
	return true
}
