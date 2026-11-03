// SPDX-License-Identifier: Apache-2.0

package tools

import "github.com/rysh-ai/rysh-cli-code/internal/domain"

// paneListStatus is the Status column of agents_list: the pane's own status
// (idle when it has none), or "needs you (<reason>)" when the pane is waiting
// on a human (root-order-044 #3).
//
// A remote pane (##remote, design 070) shows the working/idle state its own
// rysh observed and is tagged host:<alias>.
func paneListStatus(p domain.PaneSnapshot) string {
	s := localPaneListStatus(p)
	if host := p.Meta[domain.PaneMetaRemoteHost]; host != "" {
		if _, needs := domain.PaneNeedsYou(p.Meta); !needs {
			if a := p.Meta[domain.PaneMetaActivity]; a != "" {
				s = a
			}
		}
		s += " host:" + host
	}
	return s
}

func localPaneListStatus(p domain.PaneSnapshot) string {
	if reason, ok := domain.PaneNeedsYou(p.Meta); ok {
		if reason == "" {
			return "needs you"
		}
		return "needs you (" + reason + ")"
	}
	if p.Status == "" {
		return "idle"
	}
	return p.Status
}
