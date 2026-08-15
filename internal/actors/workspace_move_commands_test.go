// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestParseMoveRequest(t *testing.T) {
	cases := []struct {
		name string
		line string
		want moveRequest
	}{
		{
			// The form from the original request: no subject reference, so the
			// subject is the pane the command was typed in.
			name: "pane to a lane, subject implied",
			line: "pane to-lane lane-1",
			want: moveRequest{subject: "pane", dest: moveDest{kind: "lane", ref: "lane-1"}},
		},
		{
			name: "pane named explicitly",
			line: "pane worker-3 to-lane lane-1",
			want: moveRequest{subject: "pane", subjectRef: "worker-3", dest: moveDest{kind: "lane", ref: "lane-1"}},
		},
		{
			// to-stacked-pane names a PANE inside the destination stack, because
			// stacks have ids but no names.
			name: "to-stacked-pane canonicalises to a stack destination",
			line: "pane to-stacked-pane abc123",
			want: moveRequest{subject: "pane", dest: moveDest{kind: "stack", ref: "abc123"}},
		},
		{
			name: "tab flag anywhere",
			line: "pane to-lane 2 --tab build",
			want: moveRequest{subject: "pane", tabArg: "build", dest: moveDest{kind: "lane", ref: "2"}},
		},
		{
			name: "tab flag before the subject",
			line: "--tab build pane to-lane 2",
			want: moveRequest{subject: "pane", tabArg: "build", dest: moveDest{kind: "lane", ref: "2"}},
		},
		{
			name: "stack aliases",
			line: "pg 2 to-lane 1",
			want: moveRequest{subject: "stack", subjectRef: "2", dest: moveDest{kind: "lane", ref: "1"}},
		},
		{
			name: "directional destinations take no reference",
			line: "lane build left",
			want: moveRequest{subject: "lane", subjectRef: "build", dest: moveDest{kind: "left"}},
		},
		{
			name: "unstack is an alias of out",
			line: "pane unstack",
			want: moveRequest{subject: "pane", dest: moveDest{kind: "out"}},
		},
		{
			name: "positional flag",
			line: "pane a to-stack g1 --index 2",
			want: moveRequest{
				subject: "pane", subjectRef: "a",
				dest: moveDest{kind: "stack", ref: "g1"},
				pos:  movePos{mode: "index", index: 2},
			},
		},
		{
			name: "before flag",
			line: "pane a to-stack g1 --before b",
			want: moveRequest{
				subject: "pane", subjectRef: "a",
				dest: moveDest{kind: "stack", ref: "g1"},
				pos:  movePos{mode: "before", ref: "b"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMoveRequest(strings.Fields(tc.line))
			if err != nil {
				t.Fatalf("parse %q: %v", tc.line, err)
			}
			if got != tc.want {
				t.Fatalf("parse %q =\n  %+v\nwant\n  %+v", tc.line, got, tc.want)
			}
		})
	}
}

func TestParseMoveRequestRejects(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"no subject", ""},
		{"unknown subject", "window to-lane 1"},
		{"no destination", "pane worker-3"},
		{"two subject references", "pane a b to-lane 1"},
		{"trailing junk", "pane to-lane 1 extra"},
		{"index must be 1-based", "pane to-stack g --index 0"},
		{"unknown flag", "pane to-lane 1 --sideways"},
		{"flag with no value", "pane to-lane 1 --tab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseMoveRequest(strings.Fields(tc.line)); err == nil {
				t.Fatalf("parse %q succeeded; a malformed move must refuse rather than guess", tc.line)
			}
		})
	}
}

func TestResolvePanePosition(t *testing.T) {
	g := &domain.PaneGroupSnapshot{Panes: []domain.PaneSnapshot{
		{ID: "p1", Title: "alpha"},
		{ID: "p2", Title: "beta", GivenName: "worker"},
		{ID: "p3", Title: "gamma"},
	}}
	cases := []struct {
		name string
		pos  movePos
		want int
	}{
		{"default appends", movePos{}, -1},
		{"first", movePos{mode: "first"}, 0},
		{"index is 1-based", movePos{mode: "index", index: 2}, 1},
		{"index past the end appends", movePos{mode: "index", index: 9}, -1},
		{"before by id", movePos{mode: "before", ref: "p2"}, 1},
		{"after by id", movePos{mode: "after", ref: "p2"}, 2},
		{"before by title", movePos{mode: "before", ref: "gamma"}, 2},
		{"after by given name", movePos{mode: "after", ref: "worker"}, 2},
		{"unknown reference appends", movePos{mode: "before", ref: "nope"}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvePanePosition(g, tc.pos); got != tc.want {
				t.Fatalf("resolvePanePosition = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestMoveTabByIDKeepsTheHumansTabActive(t *testing.T) {
	// A tab reorder must not change WHICH tab the human is looking at. The
	// keyboard path moves the active tab and carries the index with it; ##move
	// can name a tab nobody is on, and moving it must leave focus alone.
	w := newMoveTestWS("abc", 0) // active = a

	if !w.moveTabByID("c", msg.DirLeft) {
		t.Fatalf("moveTabByID refused")
	}
	if got, want := wsTabOrder(w), "acb"; got != want {
		t.Fatalf("order = %q, want %q", got, want)
	}
	if got := w.tabs[w.activeTabIdx].id; got != "a" {
		t.Fatalf("active tab = %q, want %q — reordering another tab moved the human", got, "a")
	}

	// Moving the active tab itself keeps it active, index and all.
	if !w.moveTabByID("a", msg.DirRight) {
		t.Fatalf("moveTabByID(active) refused")
	}
	if got := w.tabs[w.activeTabIdx].id; got != "a" {
		t.Fatalf("active tab = %q after moving it, want %q", got, "a")
	}
	if w.moveTabByID("a", msg.DirLeft) && w.moveTabByID("a", msg.DirLeft) {
		t.Fatalf("moveTabByID walked the first tab past the start of the bar")
	}
	if w.moveTabByID("missing", msg.DirLeft) {
		t.Fatalf("moveTabByID accepted an unknown tab id")
	}
}

func TestMoveUsageListsEveryDestination(t *testing.T) {
	// The usage block is the only place a user learns the vocabulary, so a
	// destination that parses but is undocumented is a destination nobody finds.
	var b strings.Builder
	(&WorkspaceActor{}).moveUsage(&b)
	usage := b.String()
	for keyword := range moveDestKinds {
		if !strings.Contains(usage, keyword) {
			t.Errorf("##move usage never mentions %q", keyword)
		}
	}
}
