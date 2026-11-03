// SPDX-License-Identifier: Apache-2.0

package tui

// Tests for the attach-client load fixes: row-delta VT pulls, the rawDirty
// visible-pane filter, and View() skipping for messages that change nothing.

import (
	"reflect"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func vtModel(screen []string) Model {
	m := paneModel(domain.PaneSnapshot{ID: "p1", RawMode: true})
	m.paneContent = map[string]*paneContentBuf{"p1": {vtScreen: screen, haveContent: true}}
	m.paneContentHash = map[string]uint64{}
	m.paneVTHash = map[string]uint64{}
	m.dirtyRawPanes = map[string]struct{}{}
	return m
}

func TestApplyPaneVTLoadedDeltaPatchesRows(t *testing.T) {
	held := []string{"a", "b", "c"}
	m := vtModel(held)
	ok := m.applyPaneVTLoaded(paneVTLoadedMsg{
		paneID: "p1", ok: true, interactive: true, delta: true, rows: 4,
		changed:  []msgpkg.VTLineDelta{{Y: 1, S: "B"}, {Y: 3, S: "d"}},
		baseHash: msgpkg.VTRowsHash(msgpkg.VTRowHashes(held)),
	})
	if !ok {
		t.Fatal("a delta against the held screen must apply")
	}
	if got, want := m.paneContent["p1"].vtScreen, []string{"a", "B", "c", "d"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("screen = %q, want %q", got, want)
	}
}

func TestApplyPaneVTLoadedStaleDeltaIsDroppedAndRefetched(t *testing.T) {
	m := vtModel([]string{"a", "b"})
	ok := m.applyPaneVTLoaded(paneVTLoadedMsg{
		paneID: "p1", ok: true, interactive: true, delta: true, rows: 2,
		changed:  []msgpkg.VTLineDelta{{Y: 0, S: "X"}},
		baseHash: msgpkg.VTRowsHash(msgpkg.VTRowHashes([]string{"other", "screen"})),
	})
	if ok {
		t.Fatal("a delta against a different screen must not apply")
	}
	if got := m.paneContent["p1"].vtScreen; !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("screen must be untouched, got %q", got)
	}
	if _, dirty := m.dirtyRawPanes["p1"]; !dirty {
		t.Fatal("a dropped delta must mark the pane dirty so it is fetched again")
	}
}

func TestVisiblePaneSet(t *testing.T) {
	var unset *visiblePaneSet
	if !unset.has("x") {
		t.Fatal("a nil set must admit everything")
	}
	v := &visiblePaneSet{}
	if !v.has("x") {
		t.Fatal("before the first layout every pane is admitted")
	}
	if added := v.replace([]string{"a", "b"}); !reflect.DeepEqual(added, []string{"a", "b"}) {
		t.Fatalf("first replace must report every pane as new, got %v", added)
	}
	if v.has("x") || !v.has("a") {
		t.Fatal("after replace only the listed panes are admitted")
	}
	if added := v.replace([]string{"b", "c"}); !reflect.DeepEqual(added, []string{"c"}) {
		t.Fatalf("second replace must report only c as new, got %v", added)
	}
}

func TestUpdateVisiblePanesMarksNewlyVisibleDirty(t *testing.T) {
	m := vtModel(nil)
	m.visiblePanes = &visiblePaneSet{}
	m.updateVisiblePanes([]string{"p1"})
	if _, ok := m.dirtyRawPanes["p1"]; !ok {
		t.Fatal("a pane that comes into view must be marked dirty")
	}
	delete(m.dirtyRawPanes, "p1")
	m.updateVisiblePanes([]string{"p1"})
	if _, ok := m.dirtyRawPanes["p1"]; ok {
		t.Fatal("a pane that stayed visible must not be re-marked")
	}
}

func TestViewServesCacheOnlyWhenSkipped(t *testing.T) {
	m := vtModel(nil)
	m.width, m.height = 80, 24
	m.vcache = &viewCache{frame: "CACHED", width: 80, height: 24, ok: true}

	m.viewSkip = true
	if got := m.View(); got != "CACHED" {
		t.Fatalf("skipped view must serve the cache, got %q", got)
	}
	m.width = 100
	if got := m.View(); got == "CACHED" {
		t.Fatal("a size change must re-render even when skipped")
	}
	m.width = 80
	m.viewSkip = false
	if got := m.View(); got == "CACHED" {
		t.Fatal("a non-skipped view must re-render")
	}
	if !m.vcache.ok || m.vcache.frame == "CACHED" {
		t.Fatal("a fresh render must refresh the cache")
	}
}

func TestNoOpMessagesSkipView(t *testing.T) {
	m := vtModel([]string{"a"})
	m.vcache = &viewCache{}
	for name, msg := range map[string]tea.Msg{
		"layoutDirty":      layoutDirtyMsg{},
		"rawCoalesceFlush": rawCoalesceFlushMsg{},
		"unchangedFrames":  paneFramesLoadedMsg{vt: []paneVTLoadedMsg{{paneID: "p1"}}},
		"failedContent":    paneContentLoadedMsg{paneID: "p1"},
	} {
		got, _ := m.Update(msg)
		if !got.(Model).viewSkip {
			t.Errorf("%s must not trigger a re-render", name)
		}
	}
	got, _ := m.Update(tea.WindowSizeMsg{Width: 90, Height: 30})
	if got.(Model).viewSkip {
		t.Error("a resize must re-render")
	}
}
