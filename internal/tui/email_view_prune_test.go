// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// TestEmailViewsPruneOnPaneDisappearance pins E-13: emailViews is keyed by pane
// ID (email_view.go emailViewFor) and grew for the whole life of a session
// because nothing ever deleted from it — every pane that once opened an email
// view leaked its inbox/reader/answer state. The per-pane maps pruned beside it
// in syncPaneContentSet (paneContent, paneContentHash, paneVTHash,
// dirtyRawPanes) are the idiom emailViews has to join.
func TestEmailViewsPruneOnPaneDisappearance(t *testing.T) {
	stay := domain.PaneSnapshot{ID: "p-stay"}
	gone := domain.PaneSnapshot{ID: "p-gone"}

	m := paneModel(stay)
	m.emailViews = map[string]*emailViewState{
		stay.ID: {humanoid: "h-stay"},
		gone.ID: {humanoid: "h-gone"},
	}
	// paneContent is pruned by the same sweep; carry an entry for the departed
	// pane so the test fails loudly if the sweep itself stops running.
	m.paneContent = map[string]*paneContentBuf{
		stay.ID: {haveContent: true},
		gone.ID: {haveContent: true},
	}
	m.paneContentHash = map[string]uint64{}
	m.paneVTHash = map[string]uint64{}
	m.dirtyRawPanes = map[string]struct{}{}

	// The snapshot from paneModel contains only p-stay: p-gone has vanished.
	m.syncPaneContentSet()

	if _, ok := m.paneContent[gone.ID]; ok {
		t.Fatalf("paneContent[%q] survived the prune sweep — the sweep did not run", gone.ID)
	}
	if _, ok := m.emailViews[gone.ID]; ok {
		t.Errorf("emailViews[%q] survived a snapshot without that pane: per-pane email state leaks for the life of the session", gone.ID)
	}
	if _, ok := m.emailViews[stay.ID]; !ok {
		t.Errorf("emailViews[%q] was pruned while its pane is still present in the snapshot", stay.ID)
	}
}
