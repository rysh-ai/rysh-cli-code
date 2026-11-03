// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Design 030 W5 (E-46), founder D-23 "Never", security-lead early notes
// (root dd54b9944): the workspace half of `##board attach`. The pane makes the
// final busy check itself (pane_board_attach_unix_test.go); these pin
// everything the workspace refuses BEFORE it asks the pane, and what it records
// after.

type attachAsk struct {
	calls    int
	paneID   string
	boardID  string
	deadline time.Time
	reason   string
	err      error
}

// newAttachWorkspace: tab-A holds the caller pA and the target pT (an idle
// shell pane), tab-B holds a worker pW, an agent pane pG, a replay pane pR, and
// optionally a board pane already showing epic-a.
func newAttachWorkspace(t *testing.T, fleets []fleet.Fleet, boardOnScreen bool) (*WorkspaceActor, *attachAsk) {
	t.Helper()
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	serveBoardTab(t, nc, codecs, "tab-A",
		boardTestPane{id: "pA", title: "pA", givenName: "human-pane", meta: map[string]string{}},
		boardTestPane{id: "pT", title: "quiet-fox", givenName: "spare", meta: map[string]string{}},
		boardTestPane{id: "pC", title: "agent-caller", meta: map[string]string{metaAgentKind: "claude"}},
		// In the registry, but with no fleet meta: only the registry knows.
		boardTestPane{id: "pM", title: "member-pane", meta: map[string]string{}},
	)
	tabB := []boardTestPane{
		{id: "pW", title: "brave-otter", givenName: "wkr-01", meta: map[string]string{ansaMetaFleetRole: "worker"}},
		{id: "pG", title: "agent-pane", meta: map[string]string{metaAgentNative: "claude"}},
		{id: "pR", title: "replay-pane", paneType: domain.PaneTypeReplay},
	}
	if boardOnScreen {
		tabB = append(tabB, boardTestPane{id: "pBoard", title: "calm-heron", paneType: domain.PaneTypeAgentsBoard,
			meta: map[string]string{paneMetaBoardID: "epic-a"}})
	}
	serveBoardTab(t, nc, codecs, "tab-B", tabB...)
	if fleets != nil {
		startFakeFleetRegistry(t, nc, fleets)
	}
	w := &WorkspaceActor{
		pub:          msg.NewNATSPublisher(nc, codecs),
		nc:           nc,
		tabs:         []*tabInfo{{id: "tab-A", title: "A"}, {id: "tab-B", title: "B"}},
		activeTabIdx: 0,
		activePaneID: "pA",
		// As if typed at the keyboard; TestW5AttachRefusesProgramRoutes and
		// the "not typed" case cover the other routes.
		inputTyped: true,
	}
	ask := &attachAsk{}
	w.boardAttachAsk = func(paneID, boardID string, deadline time.Time) (string, error) {
		ask.calls++
		ask.paneID, ask.boardID, ask.deadline = paneID, boardID, deadline
		return ask.reason, ask.err
	}
	return w, ask
}

func noFleets() []fleet.Fleet { return []fleet.Fleet{} }

func attachFleets() []fleet.Fleet {
	return []fleet.Fleet{{Name: "epic-a", BoardID: "epic-a", State: fleet.StateUp, Members: []fleet.Member{
		{PaneID: "pW", Role: "worker"},
		{PaneID: "pM", Role: "manager"},
	}}}
}

// The verb is wired and documented. RED today: "unknown ##board subcommand".
func TestW5AttachIsADispatcherVerb(t *testing.T) {
	cmd, ok := lookupRyshCommand("board")
	if !ok || cmd == nil {
		t.Fatal("##board is not registered")
	}
	var help strings.Builder
	for _, h := range cmd.help {
		help.WriteString(h)
	}
	if !strings.Contains(help.String(), "##board attach") {
		t.Errorf("##board attach is undocumented; help:\n%s", help.String())
	}
	w, ask := newAttachWorkspace(t, noFleets(), false)
	var out strings.Builder
	err := w.handleBoardCommand(&out, "pA", []string{"--board", "epic-a", "attach", "quiet-fox"})
	if strings.Contains(out.String(), "unknown ##board subcommand") {
		t.Fatalf("##board attach is not a subcommand: %s", out.String())
	}
	if err != nil || ask.calls != 1 {
		t.Fatalf("attach to an idle pane: err=%v asks=%d out=%s", err, ask.calls, out.String())
	}
}

func TestW5AttachConvertsAndRecords(t *testing.T) {
	w, ask := newAttachWorkspace(t, noFleets(), false)
	var out strings.Builder
	before := time.Now()
	if err := w.handleBoardCommand(&out, "pA", []string{"--board", "Epic-A", "attach", "spare"}); err != nil {
		t.Fatalf("attach refused: %v (%s)", err, out.String())
	}
	if ask.paneID != "pT" || ask.boardID != "epic-a" {
		t.Fatalf("asked pane %q board %q, want pT epic-a", ask.paneID, ask.boardID)
	}
	if !ask.deadline.After(before) || ask.deadline.After(before.Add(3*time.Second)) {
		t.Fatalf("deadline %v is not a short future time", ask.deadline)
	}
	if got := w.boardPanes["epic-a"]; got != "pT" {
		t.Fatalf("boardPanes[epic-a] = %q, want pT", got)
	}
	for _, want := range []string{"attached to pane", "quiet-fox", "shell has ended", "hangup", "nohup, disown or setsid"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("confirmation %q does not say %q", out.String(), want)
		}
	}
}

// Self-attach is allowed (security-lead note 2): the pane's own foreground
// check is what refuses a self-attach from a child process.
func TestW5SelfAttachIsAskedLikeAnyOther(t *testing.T) {
	w, ask := newAttachWorkspace(t, noFleets(), false)
	var out strings.Builder
	if err := w.handleBoardCommand(&out, "pT", []string{"attach", "pT"}); err != nil {
		t.Fatalf("self-attach refused by the workspace: %v (%s)", err, out.String())
	}
	if ask.calls != 1 || ask.paneID != "pT" {
		t.Fatalf("self-attach did not reach the pane: %+v", ask)
	}
}

func TestW5AttachRefusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fleets    []fleet.Fleet
		caller    string
		args      []string
		scheduled string
		want      string
	}{
		{"no target", noFleets(), "pA", []string{"attach"}, "", "attach needs"},
		{"two targets", noFleets(), "pA", []string{"attach", "pT", "pW"}, "", "attach needs"},
		{"unknown pane", noFleets(), "pA", []string{"attach", "nobody"}, "", `no pane named "nobody"`},
		{"target fleet member (registry only)", attachFleets(), "pA", []string{"attach", "member-pane"}, "", "busy"},
		{"target fleet worker", attachFleets(), "pA", []string{"attach", "wkr-01"}, "", "busy"},
		{"target agent meta", noFleets(), "pA", []string{"attach", "agent-pane"}, "", "busy"},
		{"target replay", noFleets(), "pA", []string{"attach", "replay-pane"}, "", "not a shell pane"},
		{"registry silent", nil, "pA", []string{"attach", "pT"}, "", "registry did not answer"},
		{"caller is an agent", noFleets(), "pC", []string{"attach", "pT"}, "", "caller"},
		{"caller has fleet meta", attachFleets(), "pW", []string{"attach", "pT"}, "", "caller"},
		{"caller is a fleet member (registry only)", attachFleets(), "pM", []string{"attach", "pT"}, "", "caller"},
		{"no caller pane", noFleets(), "", []string{"attach", "pT"}, "", "caller is not a known pane"},
		{"caller names no pane", noFleets(), "ghost", []string{"attach", "pT"}, "", "caller is not a known pane"},
		{"cron fire", noFleets(), "pA", []string{"attach", "pT"}, "cron", "cron"},
		{"not typed by a person", noFleets(), "pA", []string{"attach", "pT"}, "untyped", "only a person typing"},
		{"trigger fire", noFleets(), "pA", []string{"attach", "pT"}, "trigger", "trigger"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, ask := newAttachWorkspace(t, tc.fleets, false)
			w.scheduledFire = tc.scheduled
			if tc.scheduled == "untyped" {
				w.scheduledFire, w.inputTyped = "", false
			}
			var out strings.Builder
			err := w.handleBoardCommand(&out, tc.caller, append([]string{"--board", "epic-a"}, tc.args...))
			if err == nil {
				t.Fatalf("accepted: %s", out.String())
			}
			if !strings.Contains(out.String()+err.Error(), tc.want) {
				t.Errorf("refusal %q / %v does not say %q", out.String(), err, tc.want)
			}
			if ask.calls != 0 {
				t.Errorf("the pane was asked to convert after a workspace refusal")
			}
			if len(w.boardPanes) != 0 {
				t.Errorf("a refused attach recorded %v", w.boardPanes)
			}
			if w.ryshFail == nil {
				t.Error("a refused attach must record the failure (exit status)")
			}
		})
	}
}

// Design §3.3: the singleton names BOTH the tab and the pane.
func TestW5AttachRefusesWhenTheBoardIsAlreadyAttached(t *testing.T) {
	w, ask := newAttachWorkspace(t, noFleets(), true)
	var out strings.Builder
	err := w.handleBoardCommand(&out, "pA", []string{"--board", "epic-a", "attach", "pT"})
	if err == nil {
		t.Fatalf("a second pane took board epic-a: %s", out.String())
	}
	if !strings.Contains(out.String(), "already attached in tab B pane calm-heron") {
		t.Errorf("refusal %q does not name the tab and pane", out.String())
	}
	if ask.calls != 0 {
		t.Error("the pane was asked anyway")
	}
}

// The pane's own refusal (busy, expired) is passed on and nothing is recorded;
// no answer at all records nothing either.
func TestW5AttachPassesOnThePanesRefusalAndTimeout(t *testing.T) {
	for _, tc := range []struct {
		reason string
		err    error
		want   string
	}{
		{"busy: a program is running in the foreground", nil, "busy: a program is running"},
		{"", errFakeNoAnswer, "did not answer"},
	} {
		w, ask := newAttachWorkspace(t, noFleets(), false)
		ask.reason, ask.err = tc.reason, tc.err
		var out strings.Builder
		if err := w.handleBoardCommand(&out, "pA", []string{"attach", "pT"}); err == nil {
			t.Fatalf("accepted although the pane said %q / %v", tc.reason, tc.err)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("output %q does not say %q", out.String(), tc.want)
		}
		if len(w.boardPanes) != 0 {
			t.Errorf("recorded %v after a pane refusal", w.boardPanes)
		}
	}
}

type fakeNoAnswer struct{}

func (fakeNoAnswer) Error() string { return "nats: timeout" }

var errFakeNoAnswer error = fakeNoAnswer{}

// The default ask goes over the wire to the pane's inbox with the new message
// and decodes the reply — the codec registration is part of the feature.
func TestW5AttachAskGoesOverTheWire(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	w := &WorkspaceActor{pub: msg.NewNATSPublisher(nc, codecs), nc: nc}
	var got msg.MsgPaneAttachBoard
	sub, err := nc.Subscribe(msg.T("pane", "pT", "inbox"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			t.Errorf("envelope: %v", err)
			return
		}
		if err := json.Unmarshal(env.Payload, &got); err != nil {
			t.Errorf("payload: %v", err)
		}
		re := &msg.RequestEnvelope{ReplyTo: env.ReplyTo, NC: nc, Codecs: codecs}
		_ = re.Reply(&msg.MsgPaneAttachBoardReply{Reason: "busy: test"})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	deadline := time.Now().Add(2 * time.Second)
	reason, err := w.defaultBoardAttachAsk("pT", "epic-a", deadline)
	if err != nil || reason != "busy: test" {
		t.Fatalf("reason %q err %v", reason, err)
	}
	if got.BoardID != "epic-a" || got.DeadlineUnixMs != deadline.UnixMilli() {
		t.Fatalf("pane received %+v", got)
	}
}

// fireScheduledInput marks the workspace for the length of its synchronous
// routing, and clears the mark afterwards.
func TestW5ScheduledFireIsMarkedOnlyWhileItRuns(t *testing.T) {
	w, _ := newAttachWorkspace(t, noFleets(), false)
	w.withScheduledFire("cron", func() {
		if w.scheduledFire != "cron" {
			t.Fatalf("scheduledFire = %q during the fire", w.scheduledFire)
		}
	})
	if w.scheduledFire != "" {
		t.Fatalf("scheduledFire = %q after the fire", w.scheduledFire)
	}
}

// The routes end to end through the real submit handler: the same line is
// asked when a person typed it and refused when a program submitted it (an
// agent's rysh tool, rysh send/prompt without --pane), whichever pane it names
// as the caller.
func TestW5AttachRefusesProgramRoutes(t *testing.T) {
	for _, programmatic := range []bool{false, true} {
		w, ask := newAttachWorkspace(t, noFleets(), false)
		w.inputTyped = false
		w.handleSubmitInput(nil, &msg.MsgSubmitInput{Text: "##board attach quiet-fox", PaneID: "pA", Programmatic: programmatic})
		if w.inputTyped {
			t.Fatal("inputTyped outlived the submit")
		}
		if programmatic && ask.calls != 0 {
			t.Fatal("a programmatic submit reached the pane conversion")
		}
		if !programmatic && ask.calls != 1 {
			t.Fatalf("a typed submit did not reach the pane (asks=%d)", ask.calls)
		}
	}
	// rysh exec's door (MsgCLIRyshCommand) never sets inputTyped.
	w, ask := newAttachWorkspace(t, noFleets(), false)
	w.inputTyped = false
	var out strings.Builder
	if err := w.handleBoardCommand(&out, "pA", []string{"attach", "pT"}); err == nil || ask.calls != 0 {
		t.Fatalf("an untyped line was accepted: %v asks=%d", err, ask.calls)
	}
}
