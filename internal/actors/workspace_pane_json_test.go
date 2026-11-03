// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Design 069 S0: the SDKs read panes through `rysh exec --json`, whose
// "output" field carries whatever the handler wrote. These tests pin the
// --json forms of ##pane info, ##pane list, ##new pane and ##pane new so the
// SDK parses a document rather than prose.

// paneJSONWorkspace is paneInfoWorkspace plus a pane that needs you, so the
// attention fields are exercised on a NON-active pane in a NON-active tab.
func paneJSONWorkspace(t *testing.T) *WorkspaceActor {
	t.Helper()
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	servePaneInfoTabs(t, nc, codecs,
		paneInfoTab{id: "tab-A", title: "A", panes: []domain.PaneSnapshot{
			{ID: "pane-active", Title: "active-title", GivenName: "board-ceo", Status: "running shell", Mode: "shell"},
		}},
		paneInfoTab{id: "tab-B", title: "B", panes: []domain.PaneSnapshot{
			{ID: "pane-other", Title: "other-title", GivenName: "wkr-01", Status: "", Mode: "prompt",
				Program: "claude", Meta: map[string]string{
					domain.PaneMetaAttention:       domain.AttentionNeedsYou,
					domain.PaneMetaAttentionReason: domain.AttentionReasonApproval,
					"claude.session_id":            "sid-1",
				}},
		}},
	)
	return &WorkspaceActor{
		pub:          pub,
		tabs:         []*tabInfo{{id: "tab-A", title: "A"}, {id: "tab-B", title: "B"}},
		activeTabIdx: 0,
		activePaneID: "pane-active",
		usedAliases:  map[string]struct{}{},
	}
}

func decodePaneRecord(t *testing.T, s string) PaneRecord {
	t.Helper()
	var r PaneRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &r); err != nil {
		t.Fatalf("output is not one JSON object: %v\n--- output ---\n%s", err, s)
	}
	return r
}

func TestPaneInfoJSONReportsTheNamedPane(t *testing.T) {
	w := paneJSONWorkspace(t)
	got := paneInfo(t, w, "pane-active", "wkr-01", "--json")
	r := decodePaneRecord(t, got)

	if r.ID != "pane-other" || r.GivenName != "wkr-01" || r.Title != "other-title" {
		t.Errorf("wrong pane: %+v", r)
	}
	if r.TabID != "tab-B" || r.TabTitle != "B" || r.LaneID != "lane-tab-B" || r.StackID != "grp-tab-B" {
		t.Errorf("wrong address: %+v", r)
	}
	if r.Program != "claude" || r.Mode != "prompt" {
		t.Errorf("program/mode: %+v", r)
	}
	// Empty pane status reads "idle", as agents_list does.
	if r.Status != "idle" {
		t.Errorf("status = %q, want idle", r.Status)
	}
	if !r.NeedsYou || r.Attention != domain.AttentionReasonApproval {
		t.Errorf("attention: needs_you=%v reason=%q", r.NeedsYou, r.Attention)
	}
	if r.Meta["claude.session_id"] != "sid-1" {
		t.Errorf("meta not carried: %v", r.Meta)
	}
	// The attention keys are live-only state; they are reported as fields,
	// not echoed again inside meta.
	if _, dup := r.Meta[domain.PaneMetaAttention]; dup {
		t.Errorf("attention key leaked into meta: %v", r.Meta)
	}
	if r.Active {
		t.Errorf("pane-other is not the active pane")
	}
	if w.activePaneID != "pane-active" || w.activeTabIdx != 0 {
		t.Errorf("--json moved focus")
	}
}

func TestPaneInfoJSONFlagFirstStillTakesTheRef(t *testing.T) {
	w := paneJSONWorkspace(t)
	r := decodePaneRecord(t, paneInfo(t, w, "pane-active", "--json", "pane-other"))
	if r.ID != "pane-other" {
		t.Errorf("--json before the ref lost the ref: %+v", r)
	}
}

func TestPaneInfoJSONBareIsTheAmbientPane(t *testing.T) {
	w := paneJSONWorkspace(t)
	r := decodePaneRecord(t, paneInfo(t, w, "pane-active", "--json"))
	if r.ID != "pane-active" || !r.Active || r.NeedsYou {
		t.Errorf("bare --json: %+v", r)
	}
	if r.Status != "running shell" {
		t.Errorf("status = %q", r.Status)
	}
}

func TestPaneInfoJSONUnknownRefFailsWithoutAReport(t *testing.T) {
	w := paneJSONWorkspace(t)
	got := paneInfo(t, w, "pane-active", "nope", "--json")
	if strings.Contains(got, `"id"`) {
		t.Errorf("an unknown ref produced a record:\n%s", got)
	}
	if w.ryshFail == nil {
		t.Errorf("an unknown ref must fail the command")
	}
}

func paneListOut(t *testing.T, w *WorkspaceActor, args ...string) string {
	t.Helper()
	var out strings.Builder
	w.handlePaneCommand(nil, &out, "pane-active", append([]string{"list"}, args...))
	return out.String()
}

func decodePaneList(t *testing.T, s string) []PaneRecord {
	t.Helper()
	var doc struct {
		Panes []PaneRecord `json:"panes"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &doc); err != nil {
		t.Fatalf("output is not a JSON list document: %v\n--- output ---\n%s", err, s)
	}
	return doc.Panes
}

func TestPaneListJSONActiveTab(t *testing.T) {
	w := paneJSONWorkspace(t)
	panes := decodePaneList(t, paneListOut(t, w, "--json"))
	if len(panes) != 1 || panes[0].ID != "pane-active" || !panes[0].Active {
		t.Errorf("active-tab list: %+v", panes)
	}
}

func TestPaneListJSONAllTabs(t *testing.T) {
	w := paneJSONWorkspace(t)
	panes := decodePaneList(t, paneListOut(t, w, "--all", "--json"))
	if len(panes) != 2 {
		t.Fatalf("want 2 panes across both tabs, got %+v", panes)
	}
	if panes[1].ID != "pane-other" || !panes[1].NeedsYou || panes[1].TabID != "tab-B" {
		t.Errorf("second pane: %+v", panes[1])
	}
}

func TestPaneListJSONNamedTab(t *testing.T) {
	w := paneJSONWorkspace(t)
	panes := decodePaneList(t, paneListOut(t, w, "--tab", "tab-B", "--json"))
	if len(panes) != 1 || panes[0].ID != "pane-other" {
		t.Errorf("--tab tab-B: %+v", panes)
	}
}

func TestPaneListWithoutJSONIsUnchanged(t *testing.T) {
	w := paneJSONWorkspace(t)
	got := paneListOut(t, w)
	if strings.HasPrefix(strings.TrimSpace(got), "{") || !strings.Contains(got, "id=pane-active") {
		t.Errorf("prose listing changed:\n%s", got)
	}
}

func TestNewPaneJSONReportsTheAlias(t *testing.T) {
	w := paneJSONWorkspace(t)
	var out strings.Builder
	w.handleNewInstance(nil, &out, "pane-active", []string{"pane", "--json"})
	var r NewPaneResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &r); err != nil {
		t.Fatalf("##new pane --json is not JSON: %v\n%s", err, out.String())
	}
	if r.Alias == "" || r.TabID != "tab-A" || r.LaneID != "lane-tab-A" {
		t.Errorf("result: %+v", r)
	}
	if _, ok := w.usedAliases[r.Alias]; !ok {
		t.Errorf("alias %q was not the one generated", r.Alias)
	}
}

func TestPaneNewJSONLeadingFlagOnly(t *testing.T) {
	// --json is a flag only in leading position; inside a --claude prompt it
	// is prompt text. Checked on the pure splitter so no claude launches.
	rest, asJSON := stripLeadingJSONFlag([]string{"--json", "--claude", "fix", "--json", "output"})
	if !asJSON || strings.Join(rest, " ") != "--claude fix --json output" {
		t.Errorf("leading: rest=%v json=%v", rest, asJSON)
	}
	rest, asJSON = stripLeadingJSONFlag([]string{"--claude", "--json"})
	if asJSON || len(rest) != 2 {
		t.Errorf("non-leading --json must stay: rest=%v json=%v", rest, asJSON)
	}
}

func TestPaneNewWorktreeJSONStillFailsClosedOutsideGit(t *testing.T) {
	w := &WorkspaceActor{cfg: config.Config{WorkingDirectory: t.TempDir()}}
	var out strings.Builder
	w.handlePaneNewCommand(nil, &out, "pane-1", []string{"--json", "--worktree"})
	if !strings.Contains(out.String(), "not a git repository") || w.ryshFail == nil {
		t.Fatalf("expected a loud non-git refusal, got: %s", out.String())
	}
}
