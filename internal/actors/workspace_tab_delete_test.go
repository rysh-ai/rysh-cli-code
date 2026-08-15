// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// `##tab delete` (`E-44`, design 028 §6.8).
//
// THE VERB ALREADY EXISTED and was missing from `##tab`'s help, which is the
// only reason tab-per-fleet was deferred twice: the live surface was read, the
// verb was not in it, and the conclusion — "there is no teardown verb" — was
// drawn from the help rather than from the switch. It is documented now, and
// these tests pin the behaviour a fleet teardown depends on.

// TestDeletingATabKeepsTheWatcherWhereTheyWere is the defect `E-44` found.
//
// The index was fixed up only when it fell off the end, so deleting a tab to
// the LEFT of the active one left activeTabIdx pointing at whatever shifted
// into that slot — the human's view moved one tab along, silently. With a tab
// per fleet, a fleet tearing its own tab down would yank whoever was watching a
// different fleet.
func TestDeletingATabKeepsTheWatcherWhereTheyWere(t *testing.T) {
	// The human is watching C; A (index 0) is deleted. `after` is the list the
	// handler is left holding.
	after := []*tabInfo{{id: "tab-b"}, {id: "tab-c"}}

	got := reindexActiveTab(after, 0, "tab-c", "tab-a")
	if after[got].id != "tab-c" {
		t.Fatalf("the watcher landed on %q after a tab to their left closed, want tab-c",
			after[got].id)
	}

	// The old index-only rule returned 2 here — off the end, clamped to the
	// last tab — which is how the view moved without anybody touching it.
	if got == 2 {
		t.Fatal("the index-only rule is back")
	}

	// Deleting the tab the human WAS on lands them on its replacement…
	if got := reindexActiveTab(after, 0, "tab-a", "tab-a"); after[got].id != "tab-b" {
		t.Errorf("deleting the active tab landed on %q, want its replacement tab-b", after[got].id)
	}
	// …and on the last tab when it was at the end.
	if got := reindexActiveTab(after, 2, "tab-x", "tab-x"); got != 1 {
		t.Errorf("deleting the last tab landed on index %d, want the new last (1)", got)
	}
}

// TestTheLastTabCannotBeDeleted — a session with no tabs has nowhere to type
// and no way back except restarting it, so the guard refuses rather than
// leaving the human to recover.
func TestTheLastTabCannotBeDeleted(t *testing.T) {
	w := &WorkspaceActor{tabs: []*tabInfo{{id: "only", title: "only"}}}
	resp := w.handleCLIDeleteTab(nil, &msg.MsgCLIDeleteTab{TabID: "only"})
	if resp == nil || resp.OK {
		t.Fatalf("the last tab was deleted: %+v", resp)
	}
	if len(w.tabs) != 1 {
		t.Fatalf("tabs = %d after a refused delete, want 1 untouched", len(w.tabs))
	}
}

// TestDeletingAnUnknownTabIsRefusedAndChangesNothing.
func TestDeletingAnUnknownTabIsRefusedAndChangesNothing(t *testing.T) {
	w := &WorkspaceActor{
		tabs:         []*tabInfo{{id: "tab-a"}, {id: "tab-b"}},
		activeTabIdx: 1,
	}
	resp := w.handleCLIDeleteTab(nil, &msg.MsgCLIDeleteTab{TabID: "tab-zzz"})
	if resp == nil || resp.OK {
		t.Fatalf("an unknown tab id was accepted: %+v", resp)
	}
	if len(w.tabs) != 2 || w.activeTabIdx != 1 {
		t.Fatalf("a refused delete changed state: tabs=%d active=%d", len(w.tabs), w.activeTabIdx)
	}
	if resp.Error == "" {
		t.Error("the refusal carries no reason")
	}
}

// TestTabDeleteIsDocumented is the whole reason `E-44` existed.
//
// The verb worked; nothing advertised it. `##tab` listed list/list-panes/name/
// orientation/pipeline, so reading the live surface — which is what a proof run
// does — said there was no way to close a tab, and tab-per-fleet was deferred
// out of two waves on that reading. A verb nobody can find is a verb nobody has.
func TestTabDeleteIsDocumented(t *testing.T) {
	var found bool
	for _, v := range ryshCommands {
		if v.name != "tab" {
			continue
		}
		for _, line := range v.help {
			if strings.Contains(line, "##tab  delete") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("##tab delete is not in ##tab's help; it was invisible for exactly this " +
			"reason and cost two deferrals")
	}
}
