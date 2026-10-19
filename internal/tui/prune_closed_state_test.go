// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// F-71: attention, mirror-VT and pipeline state was never pruned for closed
// panes and tabs, so a long session accumulated an entry per pane and tab it
// ever had.

func pruneTestModel(tabs ...domain.TabSnapshot) Model {
	return Model{
		snapshot:          domain.WorkspaceSnapshot{Tabs: tabs},
		attentionState:    map[string]*attentionInfo{"live": {Count: 1}, "closed": {Count: 2}},
		attentionLastBell: map[string]time.Time{"live": time.Now(), "closed": time.Now()},
		mirrorVTSeq:       map[string]uint64{"closed": 7},
		mirrorVTResync:    map[string]bool{"closed": true},
		pipelineOutputs:   map[string]string{"tab-1": "out", "tab-gone": "old"},
	}
}

func liveTab() domain.TabSnapshot {
	return domain.TabSnapshot{ID: "tab-1", Lanes: []domain.LaneSnapshot{{
		ID: "l", PaneGroups: []domain.PaneGroupSnapshot{{ID: "g", Panes: []domain.PaneSnapshot{{ID: "live"}}}},
	}}}
}

func TestClosedPaneAndTabStateIsPruned(t *testing.T) {
	m := pruneTestModel(liveTab())
	m.syncPaneContentSet()

	if _, ok := m.attentionState["closed"]; ok {
		t.Error("attentionState kept a closed pane")
	}
	if _, ok := m.attentionLastBell["closed"]; ok {
		t.Error("attentionLastBell kept a closed pane")
	}
	if len(m.mirrorVTSeq) != 0 || len(m.mirrorVTResync) != 0 {
		t.Error("mirror VT state kept a closed pane")
	}
	if _, ok := m.pipelineOutputs["tab-gone"]; ok {
		t.Error("pipelineOutputs kept a closed tab")
	}
	if m.attentionState["live"] == nil || m.pipelineOutputs["tab-1"] == "" {
		t.Error("state of a live pane/tab was pruned")
	}
}

func TestAttentionSurvivesASnapshotWithATimedOutTab(t *testing.T) {
	// A Partial tab lists no panes, so "closed" is only absent, not gone: its
	// attention count must survive — it cannot be fetched again.
	m := pruneTestModel(liveTab(), domain.TabSnapshot{ID: "tab-2", Partial: true})
	m.syncPaneContentSet()
	if m.attentionState["closed"] == nil {
		t.Fatal("an attention count was pruned on the strength of a snapshot with a timed-out tab")
	}
}
