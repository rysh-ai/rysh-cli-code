package actors

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The two headline tests in this file are the exit criterion for the agent-side
// posting path (design 025 §4.1). They exist because the OBVIOUS
// implementation — route a board post through the existing ## CLI machinery —
// is wrong in three ways that only show up on a busy session:
//
//	1. with no --pane-id, handleCLIRyshCommand runs as the workspace's ACTIVE
//	   pane, so a post is credited to a bystander;
//	2. naming a pane calls focusPaneByID, which switches the active tab and moves
//	   the human's cursor;
//	3. runRyshCommand echoes the command line into the pane's output and history
//	   buffers before dispatch.
//
// None of those is visible with one agent posting once. All three make the TUI
// unusable with dozens of agents posting milestones, which is the situation
// agents-board was built for. So they are pinned here rather than left to
// review.

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

// boardTestPane describes a pane the fake tab responder should serve. Unlike
// recordingTabResponder in workspace_focus_lookup_test.go this carries
// GivenName and Meta, because persona resolution and the fleet-field guard both
// read them.
type boardTestPane struct {
	id        string
	title     string
	givenName string
	paneType  string
	meta      map[string]string
}

// serveBoardTab answers tab-snapshot requests for one tab, so resolvePaneID and
// paneSnapshotByID can find panes without a live TabActor.
func serveBoardTab(t *testing.T, nc *nats.Conn, codecs *msg.CodecRegistry, tabID string, panes ...boardTestPane) {
	t.Helper()

	snaps := make([]domain.PaneSnapshot, 0, len(panes))
	for _, p := range panes {
		snaps = append(snaps, domain.PaneSnapshot{
			ID:        p.id,
			Title:     p.title,
			GivenName: p.givenName,
			PaneType:  p.paneType,
			Meta:      p.meta,
		})
	}
	snap := domain.TabSnapshot{
		ID: tabID,
		Lanes: []domain.LaneSnapshot{{
			ID:         "lane-" + tabID,
			PaneGroups: []domain.PaneGroupSnapshot{{ID: "grp-" + tabID, Panes: snaps}},
		}},
	}

	sub, err := nc.Subscribe(msg.T("tab", tabID, "snapshot"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			t.Errorf("board responder %s: envelope: %v", tabID, err)
			return
		}
		re := &msg.RequestEnvelope{ReplyTo: env.ReplyTo, NC: nc, Codecs: codecs}
		if err := re.Reply(&msg.MsgTabSnapshotReply{Snapshot: snap}); err != nil {
			t.Errorf("board responder %s: reply: %v", tabID, err)
		}
	})
	if err != nil {
		t.Fatalf("subscribe board responder for %s: %v", tabID, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// subjectRecorder captures every message published on a subject pattern.
type subjectRecorder struct {
	mu       sync.Mutex
	subjects []string
	payloads []json.RawMessage
}

func recordSubject(t *testing.T, nc *nats.Conn, pattern string) *subjectRecorder {
	t.Helper()
	r := &subjectRecorder{}
	sub, err := nc.Subscribe(pattern, func(m *nats.Msg) {
		var env msg.NATSEnvelope
		_ = json.Unmarshal(m.Data, &env)
		r.mu.Lock()
		r.subjects = append(r.subjects, m.Subject)
		r.payloads = append(r.payloads, env.Payload)
		r.mu.Unlock()
	})
	if err != nil {
		t.Fatalf("subscribe %s: %v", pattern, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return r
}

func (r *subjectRecorder) seen() ([]string, []json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.subjects...), append([]json.RawMessage(nil), r.payloads...)
}

// settle gives NATS a moment to deliver anything already published, so an
// assertion that "nothing was published" cannot pass merely by being early.
func settle(t *testing.T, nc *nats.Conn) {
	t.Helper()
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	time.Sleep(75 * time.Millisecond)
}

// newBoardTestWorkspace wires a workspace over two tabs: tab-A holds the
// focused pane, tab-B holds the poster. They are in DIFFERENT tabs on purpose —
// that is what makes focus theft observable, since focusPaneByID switches the
// active tab as well as the active pane.
func newBoardTestWorkspace(t *testing.T) (*WorkspaceActor, *nats.Conn) {
	t.Helper()
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()

	serveBoardTab(t, nc, codecs, "tab-A", boardTestPane{id: "pA", title: "pA", givenName: "human-pane"})
	serveBoardTab(t, nc, codecs, "tab-B", boardTestPane{id: "pB", title: "brave-otter", givenName: "wkr-01"})

	w := &WorkspaceActor{
		pub:          msg.NewNATSPublisher(nc, codecs),
		tabs:         []*tabInfo{{id: "tab-A", title: "A"}, {id: "tab-B", title: "B"}},
		activeTabIdx: 0,
		activePaneID: "pA",
	}
	return w, nc
}

// ---------------------------------------------------------------------------
// headline 1 — a post must not move the human's focus
// ---------------------------------------------------------------------------

// TestBoardPostDoesNotStealFocus pins hazard 2.
//
// The human is working in pane pA in tab-A. An agent in pane pB, in a DIFFERENT
// tab, posts a milestone. Nothing about the human's focus may change — not the
// active pane, not the active tab.
//
// Reverting the fix means routing the post through the ## CLI path, whose pane
// resolution calls w.focusPaneByID(paneID); this test then reports
// activePaneID = "pB" and activeTabIdx = 1, i.e. the human's cursor jumped to
// the agent that posted. With 44 agents posting, that is every few seconds.
func TestBoardPostDoesNotStealFocus(t *testing.T) {
	w, _ := newBoardTestWorkspace(t)

	resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{
		AsPaneID: "pB",
		Kind:     msg.BoardKindMilestone,
		Text:     "finished the codec wiring",
	})
	if resp == nil || !resp.OK {
		t.Fatalf("post refused: %+v", resp)
	}

	if w.activePaneID != "pA" {
		t.Errorf("activePaneID = %q, want pA — posting moved the human's focus to the poster; "+
			"a board post must not focus anything (design 025 §4.1 hazard 2)", w.activePaneID)
	}
	if w.activeTabIdx != 0 {
		t.Errorf("activeTabIdx = %d, want 0 — posting switched the active tab out from under the human",
			w.activeTabIdx)
	}
}

// ---------------------------------------------------------------------------
// headline 2 — a post must not write into ANY pane's buffers
// ---------------------------------------------------------------------------

// TestBoardPostWritesToNoPaneBuffer pins hazards 1 and 3 together.
//
// It subscribes to the whole `{session}.pane.>` space rather than to a list of
// named subjects. That is deliberate on two counts: every pane output, per-mode
// output and history subject already lives under it
// (NATSPublisher.SendConversation / SendConversationHistory), and a buffer added
// later is caught without anyone remembering to extend this test.
//
// Asserting the post DID reach the board subject is half the test: a handler
// that quietly does nothing would otherwise pass the silence check perfectly.
//
// Reverting the fix — routing through runRyshCommand — makes this report the
// four writes it performs before dispatch (rysh history, mode history,
// SendPaneOutput, SendPaneRyshOutput), plus the result echo afterwards. Under
// hazard 1 those land in a bystander's scrollback, not the poster's.
func TestBoardPostWritesToNoPaneBuffer(t *testing.T) {
	w, nc := newBoardTestWorkspace(t)

	paneTraffic := recordSubject(t, nc, msg.T("pane", ">"))
	boardTraffic := recordSubject(t, nc, msg.BoardPostSubject())

	resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{
		AsPaneID: "pB",
		Text:     "milestone: parser lands",
	})
	if resp == nil || !resp.OK {
		t.Fatalf("post refused: %+v", resp)
	}
	settle(t, nc)

	if subjects, _ := paneTraffic.seen(); len(subjects) != 0 {
		t.Errorf("a board post wrote %d message(s) into pane buffers: %v\n"+
			"A post must not touch any pane's output or history — it is not typed input and no "+
			"pane asked for it (design 025 §4.1 hazards 1+3).", len(subjects), subjects)
	}

	subjects, payloads := boardTraffic.seen()
	if len(subjects) != 1 {
		t.Fatalf("board subject saw %d posts, want exactly 1 — silence on the pane subjects is "+
			"only meaningful if the post actually happened", len(subjects))
	}
	var got msg.MsgBoardPost
	if err := json.Unmarshal(payloads[0], &got); err != nil {
		t.Fatalf("decode board post: %v", err)
	}
	if got.PaneID != "pB" {
		t.Errorf("post attributed to pane %q, want pB — the poster is declared, never ambient", got.PaneID)
	}
	if got.Persona != "wkr-01" {
		t.Errorf("persona = %q, want wkr-01 (the pane's given-name)", got.Persona)
	}
	if got.V != msg.BoardSchemaVersion {
		t.Errorf("schema version = %d, want %d", got.V, msg.BoardSchemaVersion)
	}
}

// ---------------------------------------------------------------------------
// attribution is required, not inferred
// ---------------------------------------------------------------------------

// TestBoardPostRequiresAnExplicitPoster is hazard 1 at its source. An empty
// AsPaneID must be an ERROR — not a fall back to w.activePaneID, which is
// sitting right there and is what the ## path would use. And nothing may be
// published: a post credited to the wrong agent is worse than no post.
func TestBoardPostRequiresAnExplicitPoster(t *testing.T) {
	w, nc := newBoardTestWorkspace(t)
	boardTraffic := recordSubject(t, nc, msg.BoardPostSubject())

	resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{Text: "who am I?"})
	if resp == nil || resp.OK {
		t.Fatalf("empty AsPaneID was accepted (%+v); it must be refused, because the only "+
			"alternative is crediting the post to whichever pane happened to be focused", resp)
	}
	if !strings.Contains(resp.Error, "--as") {
		t.Errorf("error %q does not name the flag the caller has to fix", resp.Error)
	}
	settle(t, nc)
	if subjects, _ := boardTraffic.seen(); len(subjects) != 0 {
		t.Errorf("a refused post still published %d message(s): %v", len(subjects), subjects)
	}
}

// TestBoardPostRefusesAnUnknownPane keeps the required-poster rule from being
// satisfied by any old string.
func TestBoardPostRefusesAnUnknownPane(t *testing.T) {
	w, nc := newBoardTestWorkspace(t)
	boardTraffic := recordSubject(t, nc, msg.BoardPostSubject())

	resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{AsPaneID: "no-such-pane", Text: "hello"})
	if resp == nil || resp.OK {
		t.Fatalf("post from a nonexistent pane was accepted: %+v", resp)
	}
	settle(t, nc)
	if subjects, _ := boardTraffic.seen(); len(subjects) != 0 {
		t.Errorf("a refused post still published %d message(s): %v", len(subjects), subjects)
	}
}

// TestBoardPostRefusesEmptyText — an empty post is noise on a view whose whole
// job is being readable.
func TestBoardPostRefusesEmptyText(t *testing.T) {
	w, _ := newBoardTestWorkspace(t)
	if resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{AsPaneID: "pB", Text: "   "}); resp.OK {
		t.Fatalf("empty post text was accepted: %+v", resp)
	}
}

// ---------------------------------------------------------------------------
// persona resolution
// ---------------------------------------------------------------------------

// TestBoardPostPersonaGoesThroughBoardPersona covers the fallback chain and the
// trap that motivates it: approval panes OVERLOAD GivenName to carry
// "requestID\x1FresponseSubject" for the TUI (approval_pane.go). A handler that
// published the raw given-name would one day print a NATS subject as somebody's
// name on a shared board.
func TestBoardPostPersonaGoesThroughBoardPersona(t *testing.T) {
	cases := []struct {
		name string
		pane boardTestPane
		want string
	}{
		{
			name: "given-name wins",
			pane: boardTestPane{id: "p1", title: "brave-otter", givenName: "mgr-01"},
			want: "mgr-01",
		},
		{
			name: "unnamed pane falls back to its auto-title",
			pane: boardTestPane{id: "p1", title: "brave-otter"},
			want: "brave-otter",
		},
		{
			name: "nameless and titleless falls back to the pane id",
			pane: boardTestPane{id: "abcdef0123456789"},
			want: "pane-abcdef01",
		},
		{
			// The approval-pane overload. \x1f in a given-name is never a name.
			name: "a given-name carrying the unit separator is rejected",
			pane: boardTestPane{
				id:        "abcdef0123456789",
				givenName: "req-42\x1frysh.approval.reply.42",
				paneType:  "approval",
			},
			want: "pane-abcdef01",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := startInProcessNATS(t)
			codecs := msg.DefaultCodecRegistry()
			serveBoardTab(t, nc, codecs, "tab-A", tc.pane)
			w := &WorkspaceActor{
				pub:  msg.NewNATSPublisher(nc, codecs),
				tabs: []*tabInfo{{id: "tab-A", title: "A"}},
			}
			boardTraffic := recordSubject(t, nc, msg.BoardPostSubject())

			resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{AsPaneID: tc.pane.id, Text: "hi"})
			if !resp.OK {
				t.Fatalf("post refused: %+v", resp)
			}
			settle(t, nc)

			_, payloads := boardTraffic.seen()
			if len(payloads) != 1 {
				t.Fatalf("want 1 post, got %d", len(payloads))
			}
			var got msg.MsgBoardPost
			if err := json.Unmarshal(payloads[0], &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Persona != tc.want {
				t.Errorf("persona = %q, want %q", got.Persona, tc.want)
			}
			if strings.ContainsRune(got.Persona, 0x1f) {
				t.Errorf("persona %q carries the unit separator — an approval pane's overloaded "+
					"GivenName reached the board as a name", got.Persona)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// founder gates 3 and 4 — no fleet anything on the wire
// ---------------------------------------------------------------------------

// TestBoardPostCarriesNoFleetFields pins gate 4, and through it gate 3.
//
// A post is chat: who spoke, under which thread. Fleet routing is a separate
// concern and does not enter this schema — so even when the poster's pane is
// covered in fleet.* meta, none of it may appear on the wire. With no fleet
// field in the schema there is no field that can be missing for a non-fleet
// claude, which is what makes "every claude may post" (gate 3) true by
// construction rather than by care.
//
// This asserts on the raw JSON rather than the struct on purpose: a
// reintroduced field would still decode into a struct that had it, and the
// point is that it must not be on the wire at all.
func TestBoardPostCarriesNoFleetFields(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	serveBoardTab(t, nc, codecs, "tab-A", boardTestPane{
		id:        "pF",
		title:     "brave-otter",
		givenName: "wkr-01",
		meta: map[string]string{
			"fleet.name": "board", "fleet.role": "worker",
			"fleet.unit": "01", "fleet.label": "wkr-01-agents-board-slack-2",
		},
	})
	w := &WorkspaceActor{
		pub:  msg.NewNATSPublisher(nc, codecs),
		tabs: []*tabInfo{{id: "tab-A", title: "A"}},
	}
	boardTraffic := recordSubject(t, nc, msg.BoardPostSubject())

	resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{AsPaneID: "pF", Text: "done"})
	if !resp.OK {
		t.Fatalf("post refused: %+v", resp)
	}
	settle(t, nc)

	_, payloads := boardTraffic.seen()
	if len(payloads) != 1 {
		t.Fatalf("want 1 post, got %d", len(payloads))
	}
	var raw map[string]any
	if err := json.Unmarshal(payloads[0], &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// to_persona / to_pane_id are NOT in this list: gate 4 was reopened in
	// a89523f so a DIRECTED post may carry a recipient. What stays banned is
	// fleet context — the routing envelope and the fleet/role/unit triple.
	for _, banned := range []string{"fleet", "role", "unit", "envelope"} {
		if _, present := raw[banned]; present {
			t.Errorf("board post carries %q = %v — founder gate 4 removed fleet context from the "+
				"board schema; a post is chat, not fleet routing", banned, raw[banned])
		}
	}
	// And the poster is still fully identified without any of it.
	if raw["pane_id"] != "pF" || raw["persona"] != "wkr-01" {
		t.Errorf("poster identity lost: pane_id=%v persona=%v", raw["pane_id"], raw["persona"])
	}
}

// ---------------------------------------------------------------------------
// threading
// ---------------------------------------------------------------------------

// TestBoardPostCarriesTheThreadItWasGiven — thread ids are minted by the poster
// (design 025 §4.3), so the handler's whole job here is to carry the id through
// unchanged and hand it back. It must not invent one, because an id an agent
// only sometimes gets is worse than one it never needs.
func TestBoardPostCarriesTheThreadItWasGiven(t *testing.T) {
	w, nc := newBoardTestWorkspace(t)
	boardTraffic := recordSubject(t, nc, msg.BoardPostSubject())

	thread := msg.MintThreadID("pB", 3)
	resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{
		AsPaneID: "pB",
		Kind:     msg.BoardKindReply,
		ThreadID: thread,
		Text:     "and the tests are green",
	})
	if !resp.OK {
		t.Fatalf("post refused: %+v", resp)
	}
	if resp.ID != thread {
		t.Errorf("response ID = %q, want the thread id %q it was given", resp.ID, thread)
	}
	settle(t, nc)

	_, payloads := boardTraffic.seen()
	if len(payloads) != 1 {
		t.Fatalf("want 1 post, got %d", len(payloads))
	}
	var got msg.MsgBoardPost
	if err := json.Unmarshal(payloads[0], &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ThreadID != thread {
		t.Errorf("thread id = %q, want %q", got.ThreadID, thread)
	}
	if got.Kind != msg.BoardKindReply {
		t.Errorf("kind = %q, want %q", got.Kind, msg.BoardKindReply)
	}
}

// TestBoardPostDefaultsKindToMilestone — Kind is free-form (fleetctl's --kind
// already is), but an omitted one still needs a value the view can group by.
func TestBoardPostDefaultsKindToMilestone(t *testing.T) {
	w, nc := newBoardTestWorkspace(t)
	boardTraffic := recordSubject(t, nc, msg.BoardPostSubject())

	if resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{AsPaneID: "pB", Text: "no kind given"}); !resp.OK {
		t.Fatalf("post refused: %+v", resp)
	}
	settle(t, nc)

	_, payloads := boardTraffic.seen()
	if len(payloads) != 1 {
		t.Fatalf("want 1 post, got %d", len(payloads))
	}
	var got msg.MsgBoardPost
	if err := json.Unmarshal(payloads[0], &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != msg.BoardKindMilestone {
		t.Errorf("kind = %q, want %q", got.Kind, msg.BoardKindMilestone)
	}
}

// ---------------------------------------------------------------------------
// the ##board verb — the human front door
// ---------------------------------------------------------------------------

// TestBoardVerbIsReachableAndDocumented. A verb that dispatches but is not in
// the help, or vice versa, is the drift the command table was built to stop.
// The generic table tests cover every command; this one names ##board so its
// absence fails here rather than as a puzzling gap somewhere else.
func TestBoardVerbIsReachableAndDocumented(t *testing.T) {
	spec, ok := lookupRyshCommand("board")
	if !ok {
		t.Fatal("##board is not in the dispatch table — the human front door does not exist")
	}
	if len(spec.help) == 0 {
		t.Error("##board has no help text; a verb cannot ship undocumented")
	}
	if !spec.statusAware {
		t.Error("##board is not statusAware, so a script cannot tell a refused post from a posted one")
	}

	var help strings.Builder
	w := newDispatchTestWorkspace(t)
	w.ryshHelp(&help)
	if !strings.Contains(help.String(), "##board post") {
		t.Errorf("##help does not mention ##board post:\n%s", help.String())
	}
}

// TestBoardVerbReportsBadUsage earns the statusAware claim: every wrong
// invocation reports an error, so `set -e` in a .rysh script stops instead of
// sailing past a typo.
func TestBoardVerbReportsBadUsage(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no subcommand", nil},
		{"unknown subcommand", []string{"zzzznotasubcommand"}},
		{"post with no text", []string{"post"}},
		{"reply with no thread", []string{"reply"}},
		{"reply with a thread but no text", []string{"reply", "pB/1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newDispatchTestWorkspace(t)
			var out strings.Builder
			if err := w.handleBoardCommand(&out, "pB", tc.args); err == nil {
				t.Errorf("##board %v reported success; a script would not see the mistake.\nout: %s",
					tc.args, out.String())
			}
		})
	}
}

// TestBoardVerbPostsAsTheTypingPane — the human door attributes to the pane the
// command was typed in. That is the ONLY place ambient attribution is correct,
// and it is correct because the human is looking at that pane.
func TestBoardVerbPostsAsTheTypingPane(t *testing.T) {
	w, nc := newBoardTestWorkspace(t)
	boardTraffic := recordSubject(t, nc, msg.BoardPostSubject())

	var out strings.Builder
	if err := w.handleBoardCommand(&out, "pA", []string{"post", "shipping", "the", "board"}); err != nil {
		t.Fatalf("##board post failed: %v (out: %s)", err, out.String())
	}
	settle(t, nc)

	_, payloads := boardTraffic.seen()
	if len(payloads) != 1 {
		t.Fatalf("want 1 post, got %d", len(payloads))
	}
	var got msg.MsgBoardPost
	if err := json.Unmarshal(payloads[0], &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.PaneID != "pA" {
		t.Errorf("pane id = %q, want pA (the pane the human typed in)", got.PaneID)
	}
	if got.Text != "shipping the board" {
		t.Errorf("text = %q, want the joined words", got.Text)
	}
	if !strings.Contains(out.String(), "posted to the board") {
		t.Errorf("the human got no confirmation: %q", out.String())
	}
}

// TestBoardVerbReplyCarriesTheThread — `##board reply <thread> <text>` marks the
// post as a reply and keeps the thread key the human named.
func TestBoardVerbReplyCarriesTheThread(t *testing.T) {
	w, nc := newBoardTestWorkspace(t)
	boardTraffic := recordSubject(t, nc, msg.BoardPostSubject())

	var out strings.Builder
	if err := w.handleBoardCommand(&out, "pA", []string{"reply", "pB/2", "looks", "good"}); err != nil {
		t.Fatalf("##board reply failed: %v (out: %s)", err, out.String())
	}
	settle(t, nc)

	_, payloads := boardTraffic.seen()
	if len(payloads) != 1 {
		t.Fatalf("want 1 post, got %d", len(payloads))
	}
	var got msg.MsgBoardPost
	if err := json.Unmarshal(payloads[0], &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ThreadID != "pB/2" {
		t.Errorf("thread id = %q, want pB/2", got.ThreadID)
	}
	if got.Kind != msg.BoardKindReply {
		t.Errorf("kind = %q, want %q", got.Kind, msg.BoardKindReply)
	}
	if got.Text != "looks good" {
		t.Errorf("text = %q", got.Text)
	}
}
