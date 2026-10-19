// SPDX-License-Identifier: Apache-2.0

package board

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// flowJSON round-trips v through JSON into a generic value, so two models
// compare key-order-insensitively — the way the app's port is compared too.
func flowJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestBuildFlowGolden is the design-052 contract: the golden fixture shared
// with rysh-cli-app's flow.ts.
func TestBuildFlowGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/flow-input.json")
	if err != nil {
		t.Fatal(err)
	}
	var in struct {
		Board   string     `json:"board"`
		Threads []Thread   `json:"threads"`
		Panes   []FlowPane `json:"panes"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	wantRaw, err := os.ReadFile("testdata/flow-expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal(wantRaw, &want); err != nil {
		t.Fatal(err)
	}
	got := BuildFlow(in.Board, in.Threads, in.Panes)
	if g := flowJSON(t, got); !reflect.DeepEqual(g, want) {
		gb, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("BuildFlow differs from flow-expected.json; got:\n%s", gb)
	}
}

func flowCmd(from, to, id string, ts int64) *msg.MsgBoardPost {
	c := msg.NewBoardPost(from, from, msg.BoardKindCmd, "do "+id, ts)
	c.ToPaneID, c.CmdID, c.ThreadID = to, id, msg.CmdThreadID(from, id)
	return c
}

func onThread(p *msg.MsgBoardPost, thread, cmdID string) *msg.MsgBoardPost {
	p.ThreadID, p.CmdID = thread, cmdID
	return p
}

func findEdge(f Flow, typ, from, to string) *FlowEdge {
	for i := range f.Edges {
		if e := f.Edges[i]; e.Type == typ && e.From == from && e.To == to {
			return &f.Edges[i]
		}
	}
	return nil
}

func TestBuildFlowProvisionalPromptThread(t *testing.T) {
	// The ack arrived before its cmd: no root, so no resolvable cmd — the
	// ack is classified as status (§3.2).
	s := New(0)
	ack := msg.NewBoardPost("b", "b", msg.BoardKindAck, "ok", 3)
	s.Apply(onThread(ack, msg.CmdThreadID("a", "c1"), "c1"))
	f := BuildFlow("session", s.Threads(), nil)
	if len(f.Events) != 1 || f.Events[0].Type != FlowStatus || f.Events[0].To != "board:session" || f.Events[0].CmdID != "" {
		t.Fatalf("events = %+v, want one status event without cmd_id", f.Events)
	}
	if f.Stats.Status != 1 || f.Stats.Prompts != 0 {
		t.Errorf("stats = %+v", f.Stats)
	}

	// The cmd lands: now it is a prompt thread, the ack answers it.
	s.Apply(flowCmd("a", "b", "c1", 1))
	f = BuildFlow("session", s.Threads(), nil)
	if f.Stats.Prompts != 1 || f.Stats.Answered != 1 || f.Stats.Status != 0 {
		t.Fatalf("stats after root = %+v", f.Stats)
	}
	e := findEdge(f, FlowPrompt, "a", "b")
	if e == nil || e.LatencyMS == nil || *e.LatencyMS != 2 {
		t.Fatalf("prompt edge = %+v", e)
	}
}

func TestBuildFlowAckFromNonRecipient(t *testing.T) {
	// c acks a's cmd to b: the event is an ack from c to the cmd's sender,
	// but it does not answer the prompt — only the recipient's posts do
	// (the board inbox rule).
	s := New(0)
	s.Apply(flowCmd("a", "b", "c1", 1))
	s.Apply(onThread(msg.NewBoardPost("c", "c", msg.BoardKindAck, "ok", 2), msg.CmdThreadID("a", "c1"), "c1"))
	f := BuildFlow("session", s.Threads(), nil)
	ev := f.Events[1]
	if ev.Type != FlowAck || ev.From != "c" || ev.To != "a" || ev.CmdID != "c1" {
		t.Fatalf("ack event = %+v", ev)
	}
	if f.Stats.Answered != 0 || f.Stats.Open != 1 {
		t.Errorf("stats = %+v, want the prompt still open", f.Stats)
	}
	if e := findEdge(f, FlowPrompt, "a", "b"); e == nil || e.LatencyMS != nil || *e.Open != 1 {
		t.Errorf("prompt edge = %+v", e)
	}
	// A third party's reply goes to the recipient; the sender's to the
	// recipient too.
	s.Apply(onThread(msg.NewBoardPost("c", "c", msg.BoardKindReply, "fyi", 3), msg.CmdThreadID("a", "c1"), ""))
	s.Apply(onThread(msg.NewBoardPost("a", "a", msg.BoardKindReply, "well?", 4), msg.CmdThreadID("a", "c1"), ""))
	f = BuildFlow("session", s.Threads(), nil)
	if ev := f.Events[2]; ev.Type != FlowReply || ev.To != "b" || ev.CmdID != "c1" {
		t.Errorf("third-party reply = %+v", ev)
	}
	if ev := f.Events[3]; ev.Type != FlowReply || ev.To != "b" {
		t.Errorf("sender reply = %+v", ev)
	}
}

func TestBuildFlowLatencyMedianEvenCount(t *testing.T) {
	s := New(0)
	// latencies 1, 2, 4, 8 → median (2+4)/2 = 3; then 1, 2 → 1 (1.5 floored)
	for i, lat := range []int64{1, 8, 2, 4} {
		id := fmt.Sprintf("c%d", i)
		at := int64(100 * (i + 1))
		s.Apply(flowCmd("a", "b", id, at))
		s.Apply(onThread(msg.NewBoardPost("b", "b", msg.BoardKindAck, "ok", at+lat), msg.CmdThreadID("a", id), id))
	}
	for i, lat := range []int64{1, 2} {
		id := fmt.Sprintf("d%d", i)
		at := int64(1000 * (i + 1))
		s.Apply(flowCmd("b", "a", id, at))
		s.Apply(onThread(msg.NewBoardPost("a", "a", msg.BoardKindAck, "ok", at+lat), msg.CmdThreadID("b", id), id))
	}
	f := BuildFlow("session", s.Threads(), nil)
	if e := findEdge(f, FlowPrompt, "a", "b"); e == nil || e.LatencyMS == nil || *e.LatencyMS != 3 {
		t.Errorf("a→b = %+v, want latency 3", e)
	}
	if e := findEdge(f, FlowPrompt, "b", "a"); e == nil || e.LatencyMS == nil || *e.LatencyMS != 1 {
		t.Errorf("b→a = %+v, want latency 1 (floored)", e)
	}
}

func TestBuildFlowCommNormalised(t *testing.T) {
	panes := []FlowPane{
		{ID: "p1", Name: "one", Meta: map[string]string{"board.id": "fx", "fleet.comm": "polling-stream"}},
		{ID: "p2", Name: "two", Meta: map[string]string{"board.id": "fx", "fleet.comm": "input-stream"}},
		{ID: "p3", Name: "three", Meta: map[string]string{"board.id": "other"}},
	}
	f := BuildFlow("fx", nil, panes)
	if len(f.Nodes) != 3 {
		t.Fatalf("nodes = %+v, want board + p1 + p2", f.Nodes)
	}
	if f.Nodes[1].Comm != "grid" || f.Nodes[2].Comm != "input-stream" {
		t.Errorf("comm = %q, %q", f.Nodes[1].Comm, f.Nodes[2].Comm)
	}
	// Empty board: arrays are present, not null.
	b, _ := json.Marshal(f)
	if !strings.Contains(string(b), `"edges":[]`) || !strings.Contains(string(b), `"events":[]`) {
		t.Errorf("empty slices must marshal as []: %s", b)
	}
}

func TestBuildFlowEventCap(t *testing.T) {
	s := New(0)
	for i := 0; i < 250; i++ {
		s.Apply(msg.NewBoardPost("a", "a", msg.BoardKindMilestone, fmt.Sprintf("m%d", i), int64(i+1)))
	}
	f := BuildFlow("session", s.Threads(), nil)
	if len(f.Events) != FlowEventCap {
		t.Fatalf("events = %d, want %d", len(f.Events), FlowEventCap)
	}
	if f.Events[0].Summary != "m50" || f.Events[FlowEventCap-1].Summary != "m249" {
		t.Errorf("kept %q..%q, want the newest 200", f.Events[0].Summary, f.Events[FlowEventCap-1].Summary)
	}
	// Edges and stats still count every post.
	if f.Stats.Status != 250 || findEdge(f, FlowStatus, "a", "board:session").Count != 250 {
		t.Errorf("stats = %+v", f.Stats)
	}
}

func TestFlowSummary(t *testing.T) {
	long := strings.Repeat("é", 81)
	for _, c := range []struct{ in, want string }{
		{"\n\n  hello  \nworld", "hello"},
		{"\x1b[31mred\x1b[0m", "red"},
		{strings.Repeat("x", 80), strings.Repeat("x", 80)},
		{long, strings.Repeat("é", 79) + "…"},
		{"", ""},
		{"\r\n\t\r\nok", "ok"},
	} {
		if got := flowSummary(c.in); got != c.want {
			t.Errorf("flowSummary(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildFlowRetriedSendIsOnePrompt(t *testing.T) {
	// §3.3/§3.8: the same (sender, cmd_id) twice is one prompt; the retry is
	// still a prompt event.
	s := New(0)
	s.Apply(flowCmd("a", "b", "c1", 1))
	s.Apply(flowCmd("a", "b", "c1", 5))
	f := BuildFlow("session", s.Threads(), nil)
	if len(f.Events) != 2 || f.Events[1].Type != FlowPrompt {
		t.Fatalf("events = %+v, want two prompt events", f.Events)
	}
	e := findEdge(f, FlowPrompt, "a", "b")
	if f.Stats.Prompts != 1 || e == nil || e.Count != 1 || e.LastTS != 1 || *e.Open != 1 {
		t.Errorf("stats = %+v, edge = %+v; want one open prompt", f.Stats, e)
	}
}

func TestBuildFlowUnaddressedCmdIsStatus(t *testing.T) {
	s := New(0)
	s.Apply(flowCmd("a", "", "c1", 1))
	f := BuildFlow("session", s.Threads(), nil)
	if ev := f.Events[0]; ev.Type != FlowStatus || ev.To != "board:session" || ev.CmdID != "" {
		t.Fatalf("event = %+v, want status to the board", ev)
	}
	if f.Stats.Prompts != 0 || f.Stats.Status != 1 {
		t.Errorf("stats = %+v", f.Stats)
	}
}
