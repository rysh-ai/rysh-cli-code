// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// --json forms of the pane commands (design 069 S0).
//
// The Python and TypeScript SDKs drive a session through `rysh exec --json`,
// whose "output" field carries whatever the handler wrote. Prose is for
// people; a program needs a document. So ##pane info, ##pane list, ##new pane
// and ##pane new take --json and write exactly one JSON value and nothing else.
// Errors stay prose plus failRysh: exec --json already reports ok=false, and a
// half-written document would be worse than none.

// PaneRecord is one pane as the SDKs see it. Field names are the wire
// contract with sdk/python and sdk/typescript; add, never rename.
type PaneRecord struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	GivenName string `json:"given_name,omitempty"`
	TabID     string `json:"tab_id"`
	TabTitle  string `json:"tab_title"`
	LaneID    string `json:"lane_id"`
	StackID   string `json:"stack_id"`
	// Status is the pane's own status, "idle" when it has none — the same
	// rule as agents_list. It says nothing about an agent CLI running inside
	// the pane; `rysh wait` is what observes that.
	Status  string `json:"status"`
	Mode    string `json:"mode"`
	Program string `json:"program,omitempty"`
	// NeedsYou / Attention are the #3 "needs you" state, observed by the pane.
	NeedsYou  bool              `json:"needs_you"`
	Attention string            `json:"attention,omitempty"`
	Active    bool              `json:"active"`
	Meta      map[string]string `json:"meta,omitempty"`
}

// NewPaneResult is what ##new pane --json and ##pane new --json report. The
// pane's id does not exist yet when the command returns (creation is a message
// down the Workspace → Tab → Lane → PaneGroup chain); the alias does, and
// `##pane info <alias> --json` resolves it once the pane is up.
type NewPaneResult struct {
	Alias     string `json:"alias"`
	TabID     string `json:"tab_id"`
	LaneID    string `json:"lane_id,omitempty"`
	SessionID string `json:"session_id,omitempty"` // --claude: the pinned claude session id
	Branch    string `json:"branch,omitempty"`     // --worktree
	Worktree  string `json:"worktree,omitempty"`   // --worktree
}

// stripLeadingJSONFlag removes --json only when it is the first argument.
// ##pane new --claude takes a free-text prompt, and a prompt that mentions
// "--json" must reach claude intact.
func stripLeadingJSONFlag(args []string) ([]string, bool) {
	if len(args) > 0 && args[0] == "--json" {
		return args[1:], true
	}
	return args, false
}

// writeJSONLine writes v as one line. The types above cannot fail to marshal.
func writeJSONLine(out *strings.Builder, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(out, `{"error":%q}`+"\n", err.Error())
		return
	}
	out.Write(b)
	out.WriteByte('\n')
}

// paneRecordFrom builds the record for one pane of a tab snapshot.
func paneRecordFrom(tab *tabInfo, lane domain.LaneSnapshot, group domain.PaneGroupSnapshot, ps domain.PaneSnapshot, activePaneID string) PaneRecord {
	r := PaneRecord{
		ID:        ps.ID,
		Title:     ps.Title,
		GivenName: ps.GivenName,
		TabID:     tab.id,
		TabTitle:  tab.title,
		LaneID:    lane.ID,
		StackID:   group.ID,
		Status:    ps.Status,
		Mode:      ps.Mode,
		Program:   ps.Program,
		Active:    ps.ID == activePaneID,
	}
	if r.Status == "" {
		r.Status = "idle"
	}
	if reason, ok := domain.PaneNeedsYou(ps.Meta); ok {
		r.NeedsYou = true
		r.Attention = reason
	}
	for k, v := range ps.Meta {
		if domain.IsAttentionMetaKey(k) {
			continue
		}
		if r.Meta == nil {
			r.Meta = map[string]string{}
		}
		r.Meta[k] = v
	}
	return r
}

// paneRecordsInTab lists every pane of one tab, in layout order. ok is false
// when the tab's snapshot could not be fetched.
func (w *WorkspaceActor) paneRecordsInTab(tab *tabInfo) ([]PaneRecord, bool) {
	snap := w.queryTabSnapshot(tab.id)
	if snap == nil {
		return nil, false
	}
	var recs []PaneRecord
	for _, lane := range snap.Lanes {
		for _, g := range lane.PaneGroups {
			for _, ps := range g.Panes {
				recs = append(recs, paneRecordFrom(tab, lane, g, ps, w.activePaneID))
			}
		}
	}
	return recs, true
}

// cmdPaneListJSON writes {"panes":[...]} for the given tabs.
func (w *WorkspaceActor) cmdPaneListJSON(out *strings.Builder, tabs []*tabInfo) {
	all := []PaneRecord{}
	for _, tab := range tabs {
		recs, ok := w.paneRecordsInTab(tab)
		if !ok {
			fmt.Fprintf(out, "\n[rysh] could not fetch tab snapshot for %s\n", tab.id)
			w.failRysh("could not fetch tab snapshot for %s", tab.id)
			return
		}
		all = append(all, recs...)
	}
	writeJSONLine(out, struct {
		Panes []PaneRecord `json:"panes"`
	}{all})
}
