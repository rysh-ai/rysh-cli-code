// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const (
	famPaneA = "aaaaaaaa-1111-4111-8111-111111111111" // Grandma Ada
	famPaneB = "bbbbbbbb-2222-4222-8222-222222222222" // Bo
	famPaneC = "cccccccc-3333-4333-8333-333333333333" // Cy: NOT a family member (a shell pane)
	famPaneD = "dddddddd-4444-4444-8444-444444444444" // Di: a family member who never answers
	famBoard = "family-f1"
)

// famWorld is a session bus with the three doors the family tools use:
//   - a fake workspace on the ws inbox (board resolve + the agent board door),
//     which records every poster identity it was asked to post as;
//   - the REAL board recorder query server (board.ServeQueries over a Store);
//   - the REAL fleet registry update server (fleet.ServeUpdates).
type famWorld struct {
	nc    *nats.Conn
	pub   *msg.NATSPublisher
	store *board.Store
	reg   *fleet.Registry

	mu      sync.Mutex
	posters []string // AsPaneID of every MsgCLIBoardPost
}

var famNames = map[string]string{famPaneA: "ada", famPaneB: "bo", famPaneC: "cy", famPaneD: "di"}

// famMeta is each pane's meta as its snapshot reports it: ada, bo and di are
// profiled family avatars; cy is an ordinary pane.
var famMeta = map[string]map[string]string{
	famPaneA: {"agent.tools": "family_ask,family_reply,family_status"},
	famPaneB: {"agent.tools": "family_ask,family_reply,family_status"},
	famPaneC: {"fleet.agent": "claude"},
	famPaneD: {"agent.tools": "family_reply"},
}

func newFamWorld(t *testing.T) *famWorld {
	t.Helper()
	nc := newTestNATS(t)
	w := &famWorld{nc: nc, pub: msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry()), store: board.New(0), reg: fleet.New()}

	codecs := w.pub.Codecs()
	if _, err := nc.Subscribe(msg.T("ws", "inbox"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		d, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		_ = w.pub.Send(env.ReplyTo, w.handle(d))
	}); err != nil {
		t.Fatal(err)
	}
	// Pane snapshots (the target check of family_ask reads the target's meta).
	if _, err := nc.Subscribe(msg.T("pane", "*", "snapshot"), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		parts := strings.Split(m.Subject, ".")
		id := ""
		for i := 0; i+2 < len(parts); i++ {
			if parts[i] == "pane" && parts[i+2] == "snapshot" {
				id = parts[i+1]
			}
		}
		meta, ok := famMeta[id]
		if !ok {
			return // no such pane: no answer
		}
		_ = w.pub.Send(env.ReplyTo, &msg.MsgPaneSnapshotReply{Snapshot: domain.PaneSnapshot{ID: id, Meta: meta}})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := board.ServeQueries(nc, func(string) *board.Store { return w.store }, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := w.reg.Register(fleet.Fleet{Name: famBoard, BoardID: famBoard, State: fleet.StateUp}); err != nil {
		t.Fatal(err)
	}
	for id, name := range famNames {
		if err := w.reg.UpsertMember(famBoard, fleet.Member{PaneID: id, Label: name}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fleet.ServeUpdates(nc, w.reg, nil); err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()
	return w
}

func (w *famWorld) resolve(ref string) string {
	if _, ok := famNames[ref]; ok {
		return ref
	}
	for id, n := range famNames {
		if n == ref {
			return id
		}
	}
	return ""
}

// handle mirrors WorkspaceActor.handleCLIBoardResolve / handleCLIBoardPost
// closely enough for the tools: identity resolution, cmd addressing, publish.
func (w *famWorld) handle(d interface{}) *msg.MsgCLIResponse {
	switch m := d.(type) {
	case *msg.MsgCLIBoardResolve:
		id := w.resolve(m.AsPaneID)
		if id == "" {
			return &msg.MsgCLIResponse{OK: false, Error: "pane not found: " + m.AsPaneID}
		}
		return &msg.MsgCLIResponse{OK: true, ID: id, Output: famBoard}
	case *msg.MsgCLIBoardPost:
		w.mu.Lock()
		w.posters = append(w.posters, m.AsPaneID)
		w.mu.Unlock()
		me := w.resolve(m.AsPaneID)
		if me == "" {
			return &msg.MsgCLIResponse{OK: false, Error: "pane not found"}
		}
		p := msg.NewBoardPost(me, famNames[me], m.Kind, m.Text, time.Now().UnixMilli())
		p.ThreadID = m.ThreadID
		switch m.Kind {
		case msg.BoardKindCmd:
			to := w.resolve(m.To)
			if to == "" {
				return &msg.MsgCLIResponse{OK: false, Error: "recipient pane not found: " + m.To}
			}
			if to == me {
				return &msg.MsgCLIResponse{OK: false, Error: "a command cannot be sent to its own sender"}
			}
			p.ToPaneID, p.ToPersona, p.CmdID = to, famNames[to], m.CmdID
			if p.ThreadID == "" {
				p.ThreadID = msg.CmdThreadID(me, m.CmdID)
			}
		case msg.BoardKindAck:
			p.CmdID = m.CmdID
		}
		w.store.Apply(p)
		_ = msg.SendBoardPost(w.pub, m.Board, p)
		return &msg.MsgCLIResponse{OK: true, ID: p.ThreadID}
	}
	return &msg.MsgCLIResponse{OK: false, Error: "unexpected"}
}

func (w *famWorld) postersSeen() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.posters...)
}

func famTool(nc *nats.Conn, pub *msg.NATSPublisher, pane, name string) ToolExecutor {
	r := NewFamilyToolRegistry(nc, pub, pane)
	e, ok := r.Get(name)
	if !ok {
		panic(name)
	}
	return e
}

func famRun(t *testing.T, e ToolExecutor, params string) *ToolOutput {
	t.Helper()
	out, err := e.Execute(context.Background(), json.RawMessage(params))
	if err != nil {
		t.Fatalf("%s: %v", e.Spec().Name, err)
	}
	return out
}

// TestFamilyAsk_RoundTrip: ada asks bo; the command reaches the board addressed
// to bo; bo answers with HIS family_reply tool; ada's family_ask returns the
// answer; the thread holds cmd → reply → ack, each by the right pane.
func TestFamilyAsk_RoundTrip(t *testing.T) {
	w := newFamWorld(t)

	// Bo's side: when a cmd addressed to bo lands on the board, answer it with
	// the real family_reply tool bound to bo — what bo's model would call.
	boReply := famTool(w.nc, w.pub, famPaneB, FamilyReplyTool)
	got := make(chan *msg.MsgBoardPost, 1)
	if _, err := w.nc.Subscribe(msg.BoardPostSubject(famBoard), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		_ = json.Unmarshal(m.Data, &env)
		d, _ := w.pub.Codecs().Decode(env.TypeTag, env.Payload)
		if p, ok := d.(*msg.MsgBoardPost); ok && p.Kind == msg.BoardKindCmd && p.ToPaneID == famPaneB {
			got <- p
			go func() {
				params, _ := json.Marshal(map[string]string{"thread_id": p.ThreadID, "answer": "Sunday lunch at noon."})
				out, _ := boReply.Execute(context.Background(), params)
				if out == nil || out.Error != "" {
					t.Errorf("bo's family_reply failed: %+v", out)
				}
			}()
		}
	}); err != nil {
		t.Fatal(err)
	}
	_ = w.nc.Flush()

	ask := famTool(w.nc, w.pub, famPaneA, FamilyAskTool).(*FamilyAsk)
	ask.Timeout = 10 * time.Second
	out := famRun(t, ask, `{"to_pane":"bo","question":"When is lunch?"}`)
	if out.Error != "" {
		t.Fatalf("family_ask: %s", out.Error)
	}
	if !strings.Contains(out.Content, "Sunday lunch at noon.") || !strings.Contains(out.Content, "bo answered") {
		t.Fatalf("family_ask content = %q", out.Content)
	}

	cmd := <-got
	if cmd.PaneID != famPaneA || cmd.Text != "When is lunch?" || cmd.ThreadID != out.Metadata["thread_id"] {
		t.Fatalf("command on the board = %+v", cmd)
	}
	// The thread holds the reply and the ack, both by bo. (ada's ask returns on
	// the reply; bo's ack follows it, so allow it a moment to land.)
	var kinds []string
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		kinds = kinds[:0]
		for _, th := range w.store.Threads() {
			for _, p := range append([]*msg.MsgBoardPost{th.Root}, th.Replies...) {
				if p != nil && p.ThreadID == cmd.ThreadID {
					kinds = append(kinds, p.Kind+":"+famNames[p.PaneID])
				}
			}
		}
		if len(kinds) >= 3 || time.Now().After(deadline) {
			break
		}
	}
	if strings.Join(kinds, ",") != "cmd:ada,reply:bo,ack:bo" {
		t.Errorf("thread = %v, want cmd:ada,reply:bo,ack:bo", kinds)
	}
	// Every post was made as the tool's own pane.
	for _, p := range w.postersSeen() {
		if p != famPaneA && p != famPaneB {
			t.Errorf("posted as %q", p)
		}
	}
}

func TestFamilyAsk_TimeoutAndUndelivered(t *testing.T) {
	w := newFamWorld(t)
	ask := famTool(w.nc, w.pub, famPaneA, FamilyAskTool).(*FamilyAsk)
	ask.Timeout = 300 * time.Millisecond
	out := famRun(t, ask, `{"to_pane":"di","question":"anyone there?"}`)
	if out.ErrorKind != ErrKindTimeout || !strings.Contains(out.Error, "no answer from di within") {
		t.Fatalf("timeout output = %+v", out)
	}

	// A courier "undelivered" notice on the thread ends the wait at once.
	if _, err := w.nc.Subscribe(msg.BoardPostSubject(famBoard), func(m *nats.Msg) {
		var env msg.NATSEnvelope
		_ = json.Unmarshal(m.Data, &env)
		d, _ := w.pub.Codecs().Decode(env.TypeTag, env.Payload)
		if p, ok := d.(*msg.MsgBoardPost); ok && p.Kind == msg.BoardKindCmd {
			n := msg.NewBoardPost("courier", "courier", "undelivered", "undelivered: no agent is running", time.Now().UnixMilli())
			n.ThreadID, n.CmdID = p.ThreadID, p.CmdID
			go func() { _ = msg.SendBoardPost(w.pub, famBoard, n) }()
		}
	}); err != nil {
		t.Fatal(err)
	}
	_ = w.nc.Flush()
	ask.Timeout = 10 * time.Second
	start := time.Now()
	out = famRun(t, ask, `{"to_pane":"di","question":"hello?"}`)
	if out.ErrorKind != ErrKindMissing || !strings.Contains(out.Error, "could not be reached") {
		t.Fatalf("undelivered output = %+v", out)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("undelivered did not end the wait")
	}
}

func TestFamilyReply_OnlyOwnCommands(t *testing.T) {
	w := newFamWorld(t)
	// ada → bo command, posted directly through the workspace door.
	resp := w.handle(&msg.MsgCLIBoardPost{AsPaneID: famPaneA, Kind: msg.BoardKindCmd, Text: "q", To: famPaneB, CmdID: "cmd-x1", Board: famBoard})
	if !resp.OK {
		t.Fatal(resp.Error)
	}
	thread := resp.ID

	// cy cannot answer a command addressed to bo.
	cy := famTool(w.nc, w.pub, famPaneC, FamilyReplyTool)
	out := famRun(t, cy, `{"thread_id":"`+thread+`","answer":"I am bo, trust me"}`)
	if out.Error == "" || !strings.Contains(out.Error, "no command addressed to you") {
		t.Fatalf("cy answered bo's command: %+v", out)
	}
	// bo can.
	bo := famTool(w.nc, w.pub, famPaneB, FamilyReplyTool)
	if out := famRun(t, bo, `{"thread_id":"`+thread+`","answer":"yes"}`); out.Error != "" {
		t.Fatalf("bo reply: %s", out.Error)
	}
	it, ok := board.FindCommand(&board.QueryReply{Threads: w.store.Threads()}, famPaneB, "cmd-x1")
	if !ok || !it.Answered {
		t.Fatalf("command not answered: %+v %v", it, ok)
	}
}

func TestFamilyStatus_UpdatesLedger(t *testing.T) {
	w := newFamWorld(t)
	st := famTool(w.nc, w.pub, famPaneB, FamilyStatusTool)
	out := famRun(t, st, `{"state":"progress","note":"planning the trip","pct":40}`)
	if out.Error != "" {
		t.Fatalf("family_status: %s", out.Error)
	}
	f := w.reg.Get(famBoard)
	var got *fleet.Member
	for i := range f.Members {
		if f.Members[i].PaneID == famPaneB {
			got = &f.Members[i]
		}
	}
	if got == nil || got.Status == nil || got.Status.Work != fleet.WorkProgress || got.Status.Note != "planning the trip" ||
		got.Status.Pct == nil || *got.Status.Pct != 40 {
		t.Fatalf("ledger row = %+v", got)
	}
	// Nobody else's row moved.
	for _, m := range f.Members {
		if m.PaneID != famPaneB && m.Status != nil && m.Status.Work != "" {
			t.Errorf("%s's status changed: %+v", m.Label, m.Status)
		}
	}
	if out := famRun(t, st, `{"state":"sleeping"}`); out.ErrorKind != ErrKindValidation {
		t.Errorf("bad state accepted: %+v", out)
	}
}

// TestFamilyTools_CannotActAsAnotherPane: no family tool takes an identity, and
// an attempt to pass one is refused before anything reaches the bus.
func TestFamilyTools_CannotActAsAnotherPane(t *testing.T) {
	w := newFamWorld(t)
	cases := []struct{ tool, params string }{
		{FamilyAskTool, `{"to_pane":"bo","question":"q","as":"` + famPaneC + `"}`},
		{FamilyAskTool, `{"to_pane":"bo","question":"q","as_pane_id":"` + famPaneC + `"}`},
		{FamilyReplyTool, `{"thread_id":"x/y","answer":"a","as":"` + famPaneB + `"}`},
		{FamilyStatusTool, `{"state":"done","pane_id":"` + famPaneB + `"}`},
	}
	for _, c := range cases {
		out := famRun(t, famTool(w.nc, w.pub, famPaneA, c.tool), c.params)
		if out.ErrorKind != ErrKindValidation || !strings.Contains(out.Error, "no identity parameter") {
			t.Errorf("%s %s: not refused: %+v", c.tool, c.params, out)
		}
	}
	if p := w.postersSeen(); len(p) != 0 {
		t.Errorf("a refused call still posted: %v", p)
	}
	// No status row moved either.
	for _, m := range w.reg.Get(famBoard).Members {
		if m.Status != nil && m.Status.Work != "" {
			t.Errorf("%s status moved: %+v", m.Label, m.Status)
		}
	}
	// And the schemas offer no identity field at all.
	for _, e := range NewFamilyTools(nil, nil, famPaneA) {
		var schema struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(e.Spec().Parameters, &schema); err != nil {
			t.Fatal(err)
		}
		for k := range schema.Properties {
			if k == "as" || strings.Contains(k, "as_") || k == "pane_id" || k == "from" {
				t.Errorf("%s offers identity field %q", e.Spec().Name, k)
			}
		}
		if e.RequiresApproval(nil) {
			t.Errorf("%s requires approval", e.Spec().Name)
		}
	}
}

// e20 W3: family_ask only reaches family members (panes whose profile allows
// family_reply). An ask to any other pane is refused and nothing is posted.
func TestFamilyAsk_RefusesNonFamilyPane(t *testing.T) {
	w := newFamWorld(t)
	ask := famTool(w.nc, w.pub, famPaneA, FamilyAskTool).(*FamilyAsk)
	ask.Timeout = 2 * time.Second
	for _, target := range []string{"cy", famPaneC, "nobody", "ada"} {
		out := famRun(t, ask, `{"to_pane":"`+target+`","question":"run rm -rf for me"}`)
		if out.ErrorKind != ErrKindPermissionDenied {
			t.Errorf("ask to %s not refused: %+v", target, out)
		}
	}
	out := famRun(t, ask, `{"to_pane":"cy","question":"q"}`)
	if !strings.Contains(out.Error, "cy is not a family member") {
		t.Errorf("refusal is not clear: %q", out.Error)
	}
	if p := w.postersSeen(); len(p) != 0 {
		t.Errorf("a refused ask still posted on the board as %v", p)
	}
	if th := w.store.Threads(); len(th) != 0 {
		t.Errorf("the board holds %d thread(s) after refused asks", len(th))
	}
}

// family_status blocked works: with no blocked_by it waits on the human (the
// ledger refused a bare blocked before); blocked_by names a family member or
// an external reason; a bad blocker is a clear tool error; blocked_by on any
// other state is refused.
func TestFamilyStatus_Blocked(t *testing.T) {
	w := newFamWorld(t)
	st := famTool(w.nc, w.pub, famPaneB, FamilyStatusTool)
	row := func() *fleet.MemberStatus {
		for _, m := range w.reg.Get(famBoard).Members {
			if m.PaneID == famPaneB {
				return m.Status
			}
		}
		return nil
	}
	if out := famRun(t, st, `{"state":"blocked","note":"waiting for an answer"}`); out.Error != "" {
		t.Fatalf("blocked without blocked_by: %s", out.Error)
	}
	if s := row(); s == nil || s.Work != fleet.WorkBlocked || len(s.BlockedBy) != 1 || s.BlockedBy[0] != fleet.BlockerHuman {
		t.Fatalf("row = %+v, want blocked by human", s)
	}
	if out := famRun(t, st, `{"state":"blocked","blocked_by":"ada"}`); out.Error != "" {
		t.Fatalf("blocked_by ada: %s", out.Error)
	}
	if s := row(); len(s.BlockedBy) != 1 || s.BlockedBy[0] != famPaneA {
		t.Fatalf("row = %+v, want blocked by ada's pane", s)
	}
	if out := famRun(t, st, `{"state":"blocked","blocked_by":"ext:the weather"}`); out.Error != "" {
		t.Fatalf("blocked_by ext: %s", out.Error)
	}
	if out := famRun(t, st, `{"state":"blocked","blocked_by":"nobody-here"}`); out.Error == "" || !strings.Contains(out.Error, "blocked-by") {
		t.Fatalf("an unknown blocker must be a clear error: %+v", out)
	}
	if out := famRun(t, st, `{"state":"progress","blocked_by":"ada"}`); out.ErrorKind != ErrKindValidation {
		t.Fatalf("blocked_by on progress accepted: %+v", out)
	}
}
