// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ##pane sync (web/pane_sync.go is the other half). The fixture is a fleet in
// miniature: the orchestrator the user types into and the fleet's board in one
// tab, three agents in another — one of them already paused.
func newPaneSyncTestWorkspace(t *testing.T) (*WorkspaceActor, *nats.Conn) {
	t.Helper()
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	paused := map[string]string{domain.PaneMetaWebSync: domain.WebSyncOff}

	serveBoardTab(t, nc, codecs, "tab-A",
		boardTestPane{id: "pOrch", title: "orch", givenName: "ceo"},
		boardTestPane{id: "pBoard", title: "board", givenName: "board", paneType: domain.PaneTypeAgentsBoard})
	serveBoardTab(t, nc, codecs, "tab-B",
		boardTestPane{id: "pA1", title: "a1", givenName: "wkr-1"},
		boardTestPane{id: "pA2", title: "a2", givenName: "wkr-2", meta: paused},
		boardTestPane{id: "pA3", title: "a3", givenName: "wkr-3"})

	w := &WorkspaceActor{
		pub:          msg.NewNATSPublisher(nc, codecs),
		tabs:         []*tabInfo{{id: "tab-A", title: "A"}, {id: "tab-B", title: "B"}},
		activeTabIdx: 0,
		activePaneID: "pOrch",
	}
	return w, nc
}

// syncWrites runs one ##pane sync from the orchestrator pane and returns the
// web.sync writes it published, as "paneID=value" ("" value = key deleted).
func syncWrites(t *testing.T, w *WorkspaceActor, nc *nats.Conn, args ...string) ([]string, string) {
	t.Helper()
	rec := recordSubject(t, nc, msg.T("pane", "*", "inbox"))
	var out strings.Builder
	w.handlePaneCommand(nil, &out, "pOrch", append([]string{"sync"}, args...))
	settle(t, nc)
	subjects, payloads := rec.seen()
	var writes []string
	for i, subj := range subjects {
		var m msg.MsgPaneSetMeta
		if json.Unmarshal(payloads[i], &m) != nil || m.Key != domain.PaneMetaWebSync {
			continue
		}
		writes = append(writes, paneIDFromInbox(subj)+"="+m.Value)
	}
	sort.Strings(writes)
	return writes, out.String()
}

func paneIDFromInbox(subject string) string {
	parts := strings.Split(subject, ".")
	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "pane" && parts[i+2] == "inbox" {
			return parts[i+1]
		}
	}
	return subject
}

func eqWrites(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("web.sync writes = %v, want %v", got, want)
	}
}

// The fleet case: pause everything, and the two panes the user actually
// watches — the one they typed the command in and the board — stay live.
func TestPaneSyncOffAllSparesCallerAndBoard(t *testing.T) {
	w, nc := newPaneSyncTestWorkspace(t)
	writes, out := syncWrites(t, w, nc, "off", "--all")
	// pA2 is already paused: rewriting it would fire a pointless layoutDirty.
	eqWrites(t, writes, "pA1=off", "pA3=off")
	if !strings.Contains(out, "web sync off for 2 pane(s); 2 kept live") {
		t.Errorf("summary = %q", out)
	}
}

func TestPaneSyncOffAllHonoursExcept(t *testing.T) {
	w, nc := newPaneSyncTestWorkspace(t)
	writes, _ := syncWrites(t, w, nc, "off", "--all", "--except", "wkr-3")
	eqWrites(t, writes, "pA1=off")
}

func TestPaneSyncOffTabTouchesOnlyThatTab(t *testing.T) {
	w, nc := newPaneSyncTestWorkspace(t)
	writes, _ := syncWrites(t, w, nc, "off", "--tab", "tab-B")
	eqWrites(t, writes, "pA1=off", "pA3=off")
	writes, _ = syncWrites(t, w, nc, "off", "--tab", "tab-A")
	eqWrites(t, writes) // only the caller and the board live there
}

// Resuming deletes the key: absent is the default, and `on` must never be
// written, so `##pane meta list` stays clean on panes nobody paused.
func TestPaneSyncOnDeletesTheKey(t *testing.T) {
	w, nc := newPaneSyncTestWorkspace(t)
	writes, _ := syncWrites(t, w, nc, "on", "--pane", "wkr-2")
	eqWrites(t, writes, "pA2=")
	writes, _ = syncWrites(t, w, nc, "on", "--all")
	eqWrites(t, writes, "pA2=")
}

// Naming a pane is explicit, so it overrides the bulk forms' exemptions — the
// user can pause their own pane or the board if they really mean to.
func TestPaneSyncOffNamedPane(t *testing.T) {
	w, nc := newPaneSyncTestWorkspace(t)
	writes, _ := syncWrites(t, w, nc, "off", "--pane", "board")
	eqWrites(t, writes, "pBoard=off")
	writes, _ = syncWrites(t, w, nc, "off")
	eqWrites(t, writes, "pOrch=off")
}

func TestPaneSyncStatusAndList(t *testing.T) {
	w, nc := newPaneSyncTestWorkspace(t)
	writes, out := syncWrites(t, w, nc, "--pane", "wkr-2")
	eqWrites(t, writes)
	if !strings.Contains(out, "web sync: off") {
		t.Errorf("status of a paused pane = %q", out)
	}
	_, out = syncWrites(t, w, nc)
	if !strings.Contains(out, "web sync: on") {
		t.Errorf("status of a live pane = %q", out)
	}
	_, out = syncWrites(t, w, nc, "list")
	if !strings.Contains(out, "(1)") || !strings.Contains(out, "wkr-2") || strings.Contains(out, "wkr-1") {
		t.Errorf("list = %q", out)
	}
}

// Every malformed form is refused before anything is written.
func TestPaneSyncRefusesBadForms(t *testing.T) {
	for _, args := range [][]string{
		{"sideways"},
		{"off", "extra"},
		{"--all"},
		{"off", "--except", "wkr-1"},
		{"off", "--all", "--pane", "wkr-1"},
		{"off", "--pane", "no-such-pane"},
		{"off", "--all", "--except", "no-such-pane"},
		{"off", "--tab", "no-such-tab"},
	} {
		w, nc := newPaneSyncTestWorkspace(t)
		writes, out := syncWrites(t, w, nc, args...)
		if len(writes) != 0 {
			t.Errorf("%v wrote %v, want nothing", args, writes)
		}
		if !strings.Contains(out, "usage") && !strings.Contains(out, "not found") && !strings.Contains(out, "unknown") {
			t.Errorf("%v: no refusal in %q", args, out)
		}
	}
}

func TestExtractRepeatedArg(t *testing.T) {
	rest, vals := extractRepeatedArg([]string{"off", "--except", "a", "--all", "--except=b", "--except"}, "--except")
	if strings.Join(vals, ",") != "a,b" {
		t.Errorf("values = %v", vals)
	}
	// A trailing flag with no value is left in place, for the caller to reject.
	if strings.Join(rest, ",") != "off,--all,--except" {
		t.Errorf("rest = %v", rest)
	}
}
