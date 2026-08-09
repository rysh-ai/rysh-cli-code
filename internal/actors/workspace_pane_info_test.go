package actors

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// F-16 — `##pane info <id>` used to ignore its argument and report the ACTIVE
// pane instead.
//
// WHY IT SURVIVED, and why every test here names a NON-ACTIVE pane: asking
// about the active pane passes with the bug fully present. The old code read
// w.currentTab() and the ambient paneID, so any test that happened to ask about
// the pane it was already "in" saw a correct-looking answer. The defect is only
// visible when the pane you ask about is not the pane you are.
//
// It was found live during wave 1: a query for pane 22299ff7 returned f3a6c5cd,
// a different agent's pane, in full, with nothing indicating the substitution.

// paneInfoTab describes one tab a responder should serve.
type paneInfoTab struct {
	id    string
	title string
	panes []domain.PaneSnapshot
}

// servePaneInfoTabs answers T("tab", <id>, "snapshot") for each tab, so the
// workspace's queryTabSnapshot / findPaneTab / resolvePaneID can see a real
// topology. Modelled on recordingTabResponder in workspace_focus_lookup_test.go.
func servePaneInfoTabs(t *testing.T, nc *nats.Conn, codecs *msg.CodecRegistry, tabs ...paneInfoTab) {
	t.Helper()
	for _, tb := range tabs {
		snap := domain.TabSnapshot{
			ID: tb.id,
			Lanes: []domain.LaneSnapshot{{
				ID:         "lane-" + tb.id,
				Flex:       10,
				PaneGroups: []domain.PaneGroupSnapshot{{ID: "grp-" + tb.id, Panes: tb.panes}},
			}},
		}
		sub, err := nc.Subscribe(msg.T("tab", tb.id, "snapshot"), func(m *nats.Msg) {
			var env msg.NATSEnvelope
			if err := json.Unmarshal(m.Data, &env); err != nil {
				t.Errorf("responder %s: envelope: %v", tb.id, err)
				return
			}
			re := &msg.RequestEnvelope{ReplyTo: env.ReplyTo, NC: nc, Codecs: codecs}
			if err := re.Reply(&msg.MsgTabSnapshotReply{Snapshot: snap}); err != nil {
				t.Errorf("responder %s: reply: %v", tb.id, err)
			}
		})
		if err != nil {
			t.Fatalf("subscribe responder for %s: %v", tb.id, err)
		}
		t.Cleanup(func() { _ = sub.Unsubscribe() })
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// paneInfoWorkspace builds a two-tab workspace whose ACTIVE pane is "pane-active"
// in tab-A, with "pane-other" sitting in tab-B — deliberately neither active nor
// in the active tab, which is the only arrangement that can see the defect.
func paneInfoWorkspace(t *testing.T) *WorkspaceActor {
	t.Helper()
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	servePaneInfoTabs(t, nc, codecs,
		paneInfoTab{id: "tab-A", title: "A", panes: []domain.PaneSnapshot{
			{ID: "pane-active", Title: "active-title", GivenName: "board-ceo", Status: "running shell"},
		}},
		paneInfoTab{id: "tab-B", title: "B", panes: []domain.PaneSnapshot{
			{ID: "pane-other", Title: "other-title", GivenName: "wkr-01", Status: "idle"},
		}},
	)

	return &WorkspaceActor{
		pub:          pub,
		tabs:         []*tabInfo{{id: "tab-A", title: "A"}, {id: "tab-B", title: "B"}},
		activeTabIdx: 0,
		activePaneID: "pane-active",
	}
}

func paneInfo(t *testing.T, w *WorkspaceActor, ambient string, args ...string) string {
	t.Helper()
	var out strings.Builder
	w.handlePaneCommand(nil, &out, ambient, append([]string{"info"}, args...))
	return out.String()
}

// TestPaneInfoReportsTheNamedPaneNotTheActiveOne is the regression test for
// F-16. It asks, from pane-active, about pane-other — a different pane in a
// different tab.
func TestPaneInfoReportsTheNamedPaneNotTheActiveOne(t *testing.T) {
	w := paneInfoWorkspace(t)
	got := paneInfo(t, w, "pane-active", "pane-other")

	if !strings.Contains(got, "id         : pane-other") {
		t.Errorf("##pane info pane-other did not report pane-other.\n--- output ---\n%s", got)
	}
	if strings.Contains(got, "id         : pane-active") {
		t.Errorf("##pane info pane-other reported the ACTIVE pane instead — this is F-16.\n"+
			"--- output ---\n%s", got)
	}
	if !strings.Contains(got, "given-name : wkr-01") {
		t.Errorf("wrong pane's given-name.\n--- output ---\n%s", got)
	}
	// The named pane lives in tab-B, so the report must say tab-B — proof it
	// searched the pane's OWN tab and not the active one.
	if !strings.Contains(got, "tab        : B") {
		t.Errorf("reported the active tab rather than the pane's tab.\n--- output ---\n%s", got)
	}
}

// TestPaneInfoDoesNotMoveFocus: ##pane info is a READ. focusPaneByID switches
// the active tab and moves the human's cursor, so naming a pane must not call
// it — a query that rearranges the screen is its own defect.
func TestPaneInfoDoesNotMoveFocus(t *testing.T) {
	w := paneInfoWorkspace(t)
	_ = paneInfo(t, w, "pane-active", "pane-other")

	if w.activePaneID != "pane-active" {
		t.Errorf("activePaneID moved to %q — ##pane info focused the pane it was asked about",
			w.activePaneID)
	}
	if w.activeTabIdx != 0 {
		t.Errorf("activeTabIdx moved to %d — ##pane info switched the active tab", w.activeTabIdx)
	}
}

// TestPaneInfoUnknownRefErrorsRatherThanGuessing: refuse, do not guess. An
// unresolvable ref must not degrade into a report about SOME pane.
func TestPaneInfoUnknownRefErrorsRatherThanGuessing(t *testing.T) {
	w := paneInfoWorkspace(t)
	got := paneInfo(t, w, "pane-active", "pane-does-not-exist")

	if !strings.Contains(got, "pane not found: pane-does-not-exist") {
		t.Errorf("want an error naming the ref that failed to resolve.\n--- output ---\n%s", got)
	}
	if strings.Contains(got, "pane info") {
		t.Errorf("an unknown ref produced a pane report — it fell back to a pane nobody asked "+
			"about.\n--- output ---\n%s", got)
	}
	if strings.Contains(got, "pane-active") {
		t.Errorf("an unknown ref leaked the ACTIVE pane into the answer.\n--- output ---\n%s", got)
	}
	if w.activePaneID != "pane-active" || w.activeTabIdx != 0 {
		t.Errorf("a failed lookup moved focus: pane=%q tab=%d", w.activePaneID, w.activeTabIdx)
	}
}

// TestPaneInfoWithNoArgumentStillReportsTheAmbientPane: the long-standing
// behaviour people rely on. Turning this into an error would be a second
// defect, not a fix.
func TestPaneInfoWithNoArgumentStillReportsTheAmbientPane(t *testing.T) {
	w := paneInfoWorkspace(t)
	got := paneInfo(t, w, "pane-active")

	if !strings.Contains(got, "id         : pane-active") {
		t.Errorf("bare ##pane info stopped reporting the ambient pane.\n--- output ---\n%s", got)
	}
	if !strings.Contains(got, "given-name : board-ceo") {
		t.Errorf("bare ##pane info reported the wrong pane.\n--- output ---\n%s", got)
	}
}

// TestPaneInfoResolvesByGivenName: resolvePaneID accepts an id, a title or a
// given-name, so `##pane info wkr-01` is the ergonomic form an agent will
// actually type. It must land on the same pane as the id does.
func TestPaneInfoResolvesByGivenName(t *testing.T) {
	w := paneInfoWorkspace(t)
	got := paneInfo(t, w, "pane-active", "wkr-01")

	if !strings.Contains(got, "id         : pane-other") {
		t.Errorf("##pane info wkr-01 did not resolve to pane-other.\n--- output ---\n%s", got)
	}
	if strings.Contains(got, "id         : pane-active") {
		t.Errorf("##pane info wkr-01 reported the active pane.\n--- output ---\n%s", got)
	}
}

// TestPaneInfoRef covers the "no argument keeps working" rule at the unit level,
// including the whitespace-only case a shell can easily produce.
func TestPaneInfoRef(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no args at all", nil, ""},
		{"subcommand only", []string{"info"}, ""},
		{"empty ref", []string{"info", ""}, ""},
		{"whitespace ref", []string{"info", "   "}, ""},
		{"a real ref", []string{"info", "pane-other"}, "pane-other"},
		{"ref with padding", []string{"info", "  pane-other  "}, "pane-other"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := paneInfoRef(c.args); got != c.want {
				t.Fatalf("paneInfoRef(%q) = %q, want %q", c.args, got, c.want)
			}
		})
	}
}
