// SPDX-License-Identifier: Apache-2.0

package board

import "github.com/rysh-ai/rysh-cli-code/internal/domain"

// FlowPanesFromSnapshot lists the panes of a workspace snapshot as BuildFlow
// takes them: name is the given-name, else the auto-title (msg.BoardPersona's
// order, without its pane-<8> fallback, which BuildFlow applies itself).
//
// Only panes that can host an agent: a board pane carries `board.id` too (it
// names the board it shows), and would otherwise appear in its own flow as an
// agent that never posts. Same predicate the roster seed uses.
func FlowPanesFromSnapshot(s *domain.WorkspaceSnapshot) []FlowPane {
	if s == nil {
		return nil
	}
	var out []FlowPane
	for _, tab := range s.Tabs {
		for _, lane := range tab.Lanes {
			for _, g := range lane.PaneGroups {
				for i := range g.Panes {
					p := &g.Panes[i]
					if !domain.PaneCanHostAnAgent(p.PaneType) {
						continue
					}
					name := p.GivenName
					if name == "" || containsUnitSep(name) {
						name = p.Title
					}
					out = append(out, FlowPane{ID: p.ID, Name: name, Meta: p.Meta})
				}
			}
		}
	}
	return out
}
