package actors

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The door tests. ansa_test.go covers routing POLICY over a fake transport;
// this file covers the two doors against a real in-process NATS, and in
// particular the two hazards a control channel must not have.

// serveAnsaPaneProbe answers the liveness probe for a pane, so a route can get
// past it. A pane with no responder here is a DEAD pane as far as ANSA is
// concerned, which is how the unreachable case is provoked below.
func serveAnsaPaneProbe(t *testing.T, nc *nats.Conn, codecs *msg.CodecRegistry, paneID string) {
	t.Helper()
	sub, err := nc.Subscribe(msg.T("pane", paneID, "snapshot"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			return
		}
		re := &msg.RequestEnvelope{ReplyTo: env.ReplyTo, NC: nc, Codecs: codecs}
		_ = re.Reply(&msg.MsgPaneSnapshotReply{})
	})
	if err != nil {
		t.Fatalf("subscribe probe responder for %s: %v", paneID, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// newAnsaTestWorkspace: the human sits in pA (tab-A); pB and pC live in tab-B
// and share the given-name "wkr-01". Different tabs on purpose — that is what
// makes focus theft observable.
func newAnsaTestWorkspace(t *testing.T) (*WorkspaceActor, *nats.Conn) {
	t.Helper()
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()

	serveBoardTab(t, nc, codecs, "tab-A", boardTestPane{id: "pA", title: "pA", givenName: "human-pane"})
	serveBoardTab(t, nc, codecs, "tab-B",
		boardTestPane{id: "pB", title: "brave-otter", givenName: "wkr-01"},
		boardTestPane{id: "pC", title: "calm-heron", givenName: "wkr-01"},
		boardTestPane{id: "pD", title: "eager-lynx", givenName: "mgr-01"},
	)
	for _, id := range []string{"pA", "pB", "pC", "pD"} {
		serveAnsaPaneProbe(t, nc, codecs, id)
	}

	w := &WorkspaceActor{
		pub:          msg.NewNATSPublisher(nc, codecs),
		tabs:         []*tabInfo{{id: "tab-A", title: "A"}, {id: "tab-B", title: "B"}},
		activeTabIdx: 0,
		activePaneID: "pA",
	}
	return w, nc
}

// ---------------------------------------------------------------------------
// the two hazards
// ---------------------------------------------------------------------------

// TestAnsaSendDoesNotStealFocus. The human is in pA (tab-A). An agent routes a
// message to pD in tab-B. Nothing about the human's focus may move.
//
// Reverting the fix means routing through the ## CLI path, whose pane
// resolution calls focusPaneByID; this then reports activePaneID = "pD" and
// activeTabIdx = 1.
func TestAnsaSendDoesNotStealFocus(t *testing.T) {
	w, _ := newAnsaTestWorkspace(t)

	resp := w.handleCLIAnsaSend(&msg.MsgCLIAnsaSend{AsPaneID: "pB", To: "pD", Text: "run the tests"})
	if resp == nil || !resp.OK {
		t.Fatalf("route refused: %+v", resp)
	}

	if w.activePaneID != "pA" {
		t.Errorf("activePaneID = %q, want pA — routing moved the human's focus to the target; "+
			"a control channel must not focus anything", w.activePaneID)
	}
	if w.activeTabIdx != 0 {
		t.Errorf("activeTabIdx = %d, want 0 — routing switched the active tab under the human",
			w.activeTabIdx)
	}
}

// TestAnsaSendWritesToNoPaneBufferExceptTheTargetInbox.
//
// Routing DOES legitimately publish to the target's inbox — that is delivery.
// What it must not do is write into any pane's OUTPUT or HISTORY buffers, the
// way every ## command does before it dispatches. So this asserts on the whole
// {session}.pane.> space and then requires that the only traffic in it is the
// one inbox delivery.
//
// A pattern rather than a subject list, so a buffer added later is caught
// without anyone remembering to extend this test.
func TestAnsaSendWritesToNoPaneBufferExceptTheTargetInbox(t *testing.T) {
	w, nc := newAnsaTestWorkspace(t)

	paneTraffic := recordSubject(t, nc, msg.T("pane", ">"))

	resp := w.handleCLIAnsaSend(&msg.MsgCLIAnsaSend{AsPaneID: "pB", To: "pD", Text: "run the tests"})
	if resp == nil || !resp.OK {
		t.Fatalf("route refused: %+v", resp)
	}
	settle(t, nc)

	subjects, _ := paneTraffic.seen()

	var delivery, stray []string
	for _, s := range subjects {
		switch {
		case s == msg.T("pane", "pD", "inbox"):
			delivery = append(delivery, s)
		case strings.HasSuffix(s, ".snapshot"):
			// The liveness probe. A request/reply, not a write into a buffer.
			continue
		default:
			stray = append(stray, s)
		}
	}

	if len(stray) != 0 {
		t.Errorf("routing wrote %d message(s) into pane buffers: %v\n"+
			"Delivery goes to the target's INBOX; nothing may land in any pane's output or "+
			"history — no pane asked for it and it is not typed input.", len(stray), stray)
	}
	if len(delivery) != 1 {
		t.Fatalf("target inbox saw %d deliveries, want exactly 1 — silence elsewhere only "+
			"means something if the message actually arrived", len(delivery))
	}
}

// TestAnsaDeliversTheTypeThePaneActuallyHandles.
//
// The PaneActor's Receive has cases for MsgPaneExecShell and MsgPaneExecPrompt.
// MsgSubmitInput is handled by the PaneGroupActor in the normal routing chain
// and has NO case on the pane — so publishing one to a pane inbox succeeds and
// is then dropped on the floor with a nil error. That is a silent loss, which
// is the one thing ANSA exists to prevent, and it is invisible unless something
// asserts on the type tag.
func TestAnsaDeliversTheTypeThePaneActuallyHandles(t *testing.T) {
	cases := []struct {
		mode    string
		wantTag string
	}{
		{msg.AnsaModeShell, msg.TagPaneExecShell},
		{msg.AnsaModePrompt, msg.TagPaneExecPrompt},
	}

	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			w, nc := newAnsaTestWorkspace(t)
			inbox := recordRawSubject(t, nc, msg.T("pane", "pD", "inbox"))

			resp := w.handleCLIAnsaSend(&msg.MsgCLIAnsaSend{
				AsPaneID: "pB", To: "pD", Mode: tc.mode, Text: "hello",
			})
			if !resp.OK {
				t.Fatalf("refused: %s", resp.Error)
			}
			settle(t, nc)

			tags := inbox.tags()
			if len(tags) != 1 {
				t.Fatalf("inbox saw %d messages, want 1", len(tags))
			}
			if tags[0] != tc.wantTag {
				t.Errorf("delivered %q, want %q — the PaneActor has no case for anything else, "+
					"so the wrong tag is published successfully and then dropped silently",
					tags[0], tc.wantTag)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// edge resolution through the real door
// ---------------------------------------------------------------------------

// TestAnsaSendRefusesAnAmbiguousNameEndToEnd is the safety test at the door
// rather than at the resolver: pB and pC both answer to "wkr-01", so the route
// must be refused with both ids and NOTHING delivered to either.
func TestAnsaSendRefusesAnAmbiguousNameEndToEnd(t *testing.T) {
	w, nc := newAnsaTestWorkspace(t)
	pbInbox := recordRawSubject(t, nc, msg.T("pane", "pB", "inbox"))
	pcInbox := recordRawSubject(t, nc, msg.T("pane", "pC", "inbox"))

	resp := w.handleCLIAnsaSend(&msg.MsgCLIAnsaSend{AsPaneID: "pA", To: "@wkr-01", Text: "deploy to prod"})

	if resp.OK {
		t.Fatalf("an ambiguous name was routed: %+v", resp)
	}
	if !strings.Contains(resp.Error, msg.AnsaErrAmbiguousTarget) {
		t.Errorf("error %q does not carry the ambiguous code", resp.Error)
	}
	for _, id := range []string{"pB", "pC"} {
		if !strings.Contains(resp.Error, id) {
			t.Errorf("error does not name candidate %s so the caller cannot re-address: %q", id, resp.Error)
		}
	}
	settle(t, nc)
	if n := len(pbInbox.tags()) + len(pcInbox.tags()); n != 0 {
		t.Errorf("a refused route still delivered %d message(s) — 'deploy to prod' reached a "+
			"guessed target", n)
	}
}

// TestAnsaSendResolvesAUniqueNameAtTheEdge — and the reply reports the pane ID
// it actually reached, not the name it was given.
func TestAnsaSendResolvesAUniqueNameAtTheEdge(t *testing.T) {
	w, _ := newAnsaTestWorkspace(t)

	resp := w.handleCLIAnsaSend(&msg.MsgCLIAnsaSend{AsPaneID: "pA", To: "@mgr-01", Text: "status?"})
	if !resp.OK {
		t.Fatalf("refused: %s", resp.Error)
	}
	if resp.ID != "pD" {
		t.Errorf("reported target %q, want the resolved pane id pD", resp.ID)
	}
}

// TestAnsaSendRefusesAnUnknownTarget — loudly, and with nothing sent.
func TestAnsaSendRefusesAnUnknownTarget(t *testing.T) {
	w, _ := newAnsaTestWorkspace(t)
	resp := w.handleCLIAnsaSend(&msg.MsgCLIAnsaSend{AsPaneID: "pA", To: "@nobody", Text: "hi"})
	if resp.OK {
		t.Fatalf("routed to a nonexistent target: %+v", resp)
	}
	if !strings.Contains(resp.Error, msg.AnsaErrUnknownTarget) {
		t.Errorf("error %q does not carry the unknown-target code", resp.Error)
	}
}

// TestAnsaSendRefusesADeadTarget. pE is in the layout but has no probe
// responder, i.e. its actor is gone. Publishing to its inbox would be a write
// into a subject nobody reads, so the route must be refused instead.
func TestAnsaSendRefusesADeadTarget(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	// pE is listed in the tab but deliberately gets NO probe responder.
	serveBoardTab(t, nc, codecs, "tab-A", boardTestPane{id: "pE", title: "gone", givenName: "ghost"})

	w := &WorkspaceActor{
		pub:  msg.NewNATSPublisher(nc, codecs),
		tabs: []*tabInfo{{id: "tab-A", title: "A"}},
	}

	resp := w.handleCLIAnsaSend(&msg.MsgCLIAnsaSend{To: "pE", Text: "are you there?"})
	if resp.OK {
		t.Fatalf("routed to a pane that never answered: %+v", resp)
	}
	if !strings.Contains(resp.Error, msg.AnsaErrUnreachable) {
		t.Errorf("error %q does not carry the unreachable code; the caller cannot tell a dead "+
			"target from a typo", resp.Error)
	}
}

// ---------------------------------------------------------------------------
// the ##ansa verb
// ---------------------------------------------------------------------------

func TestAnsaVerbIsReachableAndDocumented(t *testing.T) {
	spec, ok := lookupRyshCommand("ansa")
	if !ok {
		t.Fatal("##ansa is not in the dispatch table")
	}
	if len(spec.help) == 0 {
		t.Error("##ansa has no help text")
	}
	if !spec.statusAware {
		t.Error("##ansa is not statusAware, so a script cannot tell a refused route from a sent one")
	}

	var help strings.Builder
	w := newDispatchTestWorkspace(t)
	w.ryshHelp(&help)
	if !strings.Contains(help.String(), "##ansa send") {
		t.Errorf("##help does not mention ##ansa send:\n%s", help.String())
	}
}

func TestAnsaVerbReportsBadUsage(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no subcommand", nil},
		{"unknown subcommand", []string{"zzzznotasubcommand"}},
		{"send with no target", []string{"send"}},
		{"send with a target but no text", []string{"send", "@mgr-01"}},
		{"prompt with no target", []string{"prompt"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newDispatchTestWorkspace(t)
			var out strings.Builder
			if err := w.handleAnsaCommand(&out, "pA", tc.args); err == nil {
				t.Errorf("##ansa %v reported success; a script would not see the mistake.\nout: %s",
					tc.args, out.String())
			}
		})
	}
}

// TestAnsaVerbWhoFlagsDuplicateNames. The roster is where a human goes after an
// ambiguity refusal, so a name that cannot be used as an address has to be
// visible here — before somebody tries to use it, not only afterwards.
func TestAnsaVerbWhoFlagsDuplicateNames(t *testing.T) {
	w, _ := newAnsaTestWorkspace(t)
	var out strings.Builder
	if err := w.handleAnsaCommand(&out, "pA", []string{"who"}); err != nil {
		t.Fatalf("##ansa who failed: %v", err)
	}
	got := out.String()
	for _, id := range []string{"pA", "pB", "pC", "pD"} {
		if !strings.Contains(got, id) {
			t.Errorf("roster omits pane %s:\n%s", id, got)
		}
	}
	if strings.Count(got, "AMBIGUOUS") != 2 {
		t.Errorf("expected both wkr-01 panes flagged ambiguous, got:\n%s", got)
	}
}

// TestAnsaVerbSendRoutesFromTheTypingPane — the human door attributes the
// sender to the pane the command was typed in.
func TestAnsaVerbSendRoutesFromTheTypingPane(t *testing.T) {
	w, nc := newAnsaTestWorkspace(t)
	inbox := recordRawSubject(t, nc, msg.T("pane", "pD", "inbox"))

	var out strings.Builder
	if err := w.handleAnsaCommand(&out, "pA", []string{"send", "@mgr-01", "please", "review"}); err != nil {
		t.Fatalf("##ansa send failed: %v (out: %s)", err, out.String())
	}
	settle(t, nc)

	if n := len(inbox.tags()); n != 1 {
		t.Fatalf("target inbox saw %d messages, want 1", n)
	}
	if !strings.Contains(out.String(), "delivered to mgr-01") {
		t.Errorf("the human got no useful confirmation: %q", out.String())
	}
}

// TestAnsaVerbShowsCandidatesOnAmbiguity — the refusal a human reads must list
// the ids, or "re-address by id" is advice they cannot act on.
func TestAnsaVerbShowsCandidatesOnAmbiguity(t *testing.T) {
	w, _ := newAnsaTestWorkspace(t)
	var out strings.Builder
	err := w.handleAnsaCommand(&out, "pA", []string{"send", "@wkr-01", "hello"})
	if err == nil {
		t.Fatal("##ansa send to an ambiguous name reported success")
	}
	for _, id := range []string{"pB", "pC"} {
		if !strings.Contains(out.String(), id) {
			t.Errorf("the human is told to re-address by id but not shown %s:\n%s", id, out.String())
		}
	}
}

// ---------------------------------------------------------------------------
// helper
// ---------------------------------------------------------------------------

// tagRecorder captures the TYPE TAG of everything published on a subject.
// subjectRecorder (workspace_board_test.go) keeps payloads; here the tag is the
// assertion, because delivering the wrong message type to a pane inbox is a
// silent drop rather than a visible error.
type tagRecorder struct {
	mu   sync.Mutex
	seen []string
}

func recordRawSubject(t *testing.T, nc *nats.Conn, subject string) *tagRecorder {
	t.Helper()
	r := &tagRecorder{}
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		var env msg.NATSEnvelope
		_ = json.Unmarshal(m.Data, &env)
		r.mu.Lock()
		r.seen = append(r.seen, env.TypeTag)
		r.mu.Unlock()
	})
	if err != nil {
		t.Fatalf("subscribe %s: %v", subject, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return r
}

func (r *tagRecorder) tags() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}
