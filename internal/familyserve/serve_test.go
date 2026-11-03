// SPDX-License-Identifier: Apache-2.0

package familyserve

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-shared/provider"

	"github.com/rysh-ai/rysh-cli-code/internal/actors"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/tools"
)

// ready lists exactly the panes with an agent profile; a prompt streams its
// text as chunks and ends with exactly one done carrying the full text.
func TestServe_PromptStreamsChunksAndOneDone(t *testing.T) {
	w := newWorld(t)
	w.setScript(paneAda, func(context.Context, []provider.ConversationTurn) (*provider.AgenticResponse, error) {
		return textResp("Hello dear,", "lunch is at noon."), nil
	})
	s := w.serve(nil)
	ready, skipped := s.next(5*time.Second, ev(EvReady))
	if len(skipped) != 0 {
		t.Fatalf("ready must be the first line; before it: %v", skipped)
	}
	if ready["session"] != "test" || ready["version"] != "v-test" {
		t.Fatalf("ready = %v", ready)
	}
	panes, _ := json.Marshal(ready["panes"])
	if string(panes) != fmt.Sprintf(`[%q,%q]`, paneAda, paneBo) {
		t.Fatalf("ready panes = %s, want ada and bo only (cy has no profile)", panes)
	}

	s.sendJSON(map[string]string{"op": "prompt", "id": "t-1", "pane": paneAda, "text": "When is lunch?"})
	done, before := s.next(10*time.Second, doneFor("t-1"))
	var chunks []string
	for _, m := range before {
		if m["ev"] == EvChunk {
			if m["id"] != "t-1" || m["pane"] != paneAda {
				t.Errorf("chunk for the wrong turn: %v", m)
			}
			chunks = append(chunks, m["text"].(string))
		}
	}
	if len(chunks) != 2 {
		t.Fatalf("chunks = %q, want two (one per text block)", chunks)
	}
	if done["status"] != StatusDone || done["pane"] != paneAda || done["error"] != "" {
		t.Fatalf("done = %v", done)
	}
	if done["text"] != strings.Join(chunks, "") || !strings.Contains(done["text"].(string), "lunch is at noon.") {
		t.Fatalf("done text %q != chunks %q", done["text"], chunks)
	}
	s.quiet(500*time.Millisecond, doneFor("t-1")) // exactly one done
	if got := w.provs[paneAda].userTurns(); len(got) != 1 || got[0] != "When is lunch?" {
		t.Fatalf("the model saw user turns %q", got)
	}
}

// A text starting with "##" (or the "##>" pipeline prefix) is delivered to the
// LLM as an agent prompt, verbatim — never interpreted as a command.
func TestServe_HashPrefixedTextIsAPrompt(t *testing.T) {
	w := newWorld(t)
	w.setScript(paneBo, func(context.Context, []provider.ConversationTurn) (*provider.AgenticResponse, error) {
		return textResp("I am just an avatar."), nil
	})
	s := w.serve(nil)
	s.next(5*time.Second, ev(EvReady))
	texts := []string{"##pane profile clear bo", "##> pipeline-looking text", "@@bo stop", "#cost"}
	for i, txt := range texts {
		id := fmt.Sprintf("h-%d", i)
		s.sendJSON(map[string]string{"op": "prompt", "id": id, "pane": paneBo, "text": txt})
		done, _ := s.next(10*time.Second, doneFor(id))
		if done["status"] != StatusDone || !strings.Contains(done["text"].(string), "I am just an avatar.") {
			t.Fatalf("%q: done = %v", txt, done)
		}
	}
	if got := w.provs[paneBo].userTurns(); strings.Join(got, "|") != strings.Join(texts, "|") {
		t.Fatalf("the model saw %q, want %q", got, texts)
	}
	execs, _ := w.execsSeen()
	for _, e := range execs {
		if !e.Literal {
			t.Errorf("serve sent a non-literal prompt: %+v", e)
		}
	}
	// The profile is untouched: the "##pane profile clear" text was chat.
	if w.metaOf(paneBo)["agent.tools"] != familyTools {
		t.Fatalf("meta changed: %v", w.metaOf(paneBo))
	}
}

// gate is a model that blocks every call until released (or its run is
// cancelled), recording the order of the prompts it was asked.
type gate struct {
	mu      sync.Mutex
	started chan string
	release chan struct{}
}

func newGate() *gate { return &gate{started: make(chan string, 64), release: make(chan struct{}, 64)} }

func (g *gate) script(ctx context.Context, conv []provider.ConversationTurn) (*provider.AgenticResponse, error) {
	u := lastUser(conv)
	g.started <- u
	select {
	case <-g.release:
		return textResp("answer to " + u), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (g *gate) waitStarted(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-g.started:
		if got != want {
			t.Fatalf("model started %q, want %q", got, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("model never started %q", want)
	}
}

// One turn at a time per pane, in FIFO order; at most 8 wait; the 9th
// waiting prompt is answered done/busy at once.
func TestServe_FIFOAndBusyOverflow(t *testing.T) {
	w := newWorld(t)
	g := newGate()
	w.setScript(paneAda, g.script)
	s := w.serve(nil)
	s.next(5*time.Second, ev(EvReady))

	s.sendJSON(map[string]string{"op": "prompt", "id": "r", "pane": paneAda, "text": "running"})
	g.waitStarted(t, "running")
	for i := 1; i <= DefaultQueueMax; i++ {
		s.sendJSON(map[string]string{"op": "prompt", "id": fmt.Sprintf("q%d", i), "pane": paneAda, "text": fmt.Sprintf("queued %d", i)})
	}
	s.sendJSON(map[string]string{"op": "prompt", "id": "over", "pane": paneAda, "text": "one too many"})
	busy, skipped := s.next(3*time.Second, doneFor("over"))
	if busy["status"] != StatusBusy {
		t.Fatalf("overflow done = %v", busy)
	}
	for _, m := range skipped {
		if m["ev"] == EvDone {
			t.Fatalf("a queued turn finished early: %v", m)
		}
	}
	// A prompt for ANOTHER pane is not held up by ada's queue.
	w.setScript(paneBo, func(context.Context, []provider.ConversationTurn) (*provider.AgenticResponse, error) {
		return textResp("bo here"), nil
	})
	s.sendJSON(map[string]string{"op": "prompt", "id": "b1", "pane": paneBo, "text": "hi bo"})
	if d, _ := s.next(10*time.Second, doneFor("b1")); d["status"] != StatusDone {
		t.Fatalf("bo's turn = %v", d)
	}

	order := []string{"r"}
	g.release <- struct{}{}
	s.next(10*time.Second, doneFor("r"))
	for i := 1; i <= DefaultQueueMax; i++ {
		g.waitStarted(t, fmt.Sprintf("queued %d", i))
		g.release <- struct{}{}
		id := fmt.Sprintf("q%d", i)
		d, _ := s.next(10*time.Second, doneFor(id))
		if d["status"] != StatusDone || !strings.Contains(d["text"].(string), fmt.Sprintf("answer to queued %d", i)) {
			t.Fatalf("%s done = %v", id, d)
		}
		order = append(order, id)
	}
	if len(order) != 1+DefaultQueueMax {
		t.Fatalf("order = %v", order)
	}
}

// cancel ends a queued turn at once and a running one through the pane's
// agent; both report done/cancelled; a cancel for an unknown id is an error.
func TestServe_Cancel(t *testing.T) {
	w := newWorld(t)
	g := newGate()
	w.setScript(paneAda, g.script)
	s := w.serve(nil)
	s.next(5*time.Second, ev(EvReady))

	s.sendJSON(map[string]string{"op": "prompt", "id": "run", "pane": paneAda, "text": "long one"})
	g.waitStarted(t, "long one")
	s.sendJSON(map[string]string{"op": "prompt", "id": "wait", "pane": paneAda, "text": "never runs"})
	s.sendJSON(map[string]string{"op": "cancel", "id": "wait"})
	if d, _ := s.next(3*time.Second, doneFor("wait")); d["status"] != StatusCancelled {
		t.Fatalf("queued cancel: %v", d)
	}
	s.sendJSON(map[string]string{"op": "cancel", "id": "run"})
	if d, _ := s.next(10*time.Second, doneFor("run")); d["status"] != StatusCancelled {
		t.Fatalf("running cancel: %v", d)
	}
	s.sendJSON(map[string]string{"op": "cancel", "id": "nobody"})
	if e, _ := s.next(3*time.Second, ev(EvError)); !strings.Contains(e["message"].(string), "nobody") {
		t.Fatalf("unknown cancel: %v", e)
	}
	// The cancelled queued prompt never reached the model.
	for _, u := range w.provs[paneAda].userTurns() {
		if u == "never runs" {
			t.Fatal("a cancelled queued prompt reached the model")
		}
	}
	// The pane takes a new turn normally afterwards.
	w.setScript(paneAda, func(context.Context, []provider.ConversationTurn) (*provider.AgenticResponse, error) {
		return textResp("fresh"), nil
	})
	if d := func() map[string]any {
		s.sendJSON(map[string]string{"op": "prompt", "id": "after", "pane": paneAda, "text": "again"})
		d, _ := s.next(10*time.Second, doneFor("after"))
		return d
	}(); d["status"] != StatusDone {
		t.Fatalf("turn after cancel: %v", d)
	}
}

var threadRE = regexp.MustCompile(`\| thread ([^\]\s]+)\]`)

// boAnswers makes bo's model answer every delivered board command with its
// family_reply tool, then say done.
func boAnswers(answer string) script {
	return func(_ context.Context, conv []provider.ConversationTurn) (*provider.AgenticResponse, error) {
		if tr, ok := lastTool(conv); ok {
			return textResp("replied: " + tr.Content), nil
		}
		if m := threadRE.FindStringSubmatch(lastUser(conv)); m != nil {
			return toolResp("r1", "family_reply", map[string]string{"thread_id": m[1], "answer": answer}), nil
		}
		return textResp("nothing to do"), nil
	}
}

// adaAsks makes ada's model ask `to` with family_ask, then repeat the tool
// result as its reply.
func adaAsks(to, question string) script {
	return func(_ context.Context, conv []provider.ConversationTurn) (*provider.AgenticResponse, error) {
		if tr, ok := lastTool(conv); ok {
			return textResp("Result: " + tr.Content), nil
		}
		return toolResp("a1", "family_ask", map[string]string{"to_pane": to, "question": question}), nil
	}
}

// A family_ask round trip through the real courier: ada's turn asks bo; the
// courier delivers the command into bo's pane; bo answers with family_reply.
// serve emits the cmd, reply and ack as board events, and ada's done carries
// bo's answer.
func TestServe_BoardEventsForFamilyAskRoundTrip(t *testing.T) {
	w := newWorld(t)
	w.startCourier(true)
	w.setScript(paneAda, adaAsks("bo", "Are you coming on Sunday?"))
	w.setScript(paneBo, boAnswers("Yes, with a cake."))
	s := w.serve(nil)
	s.next(5*time.Second, ev(EvReady))

	s.sendJSON(map[string]string{"op": "prompt", "id": "ask", "pane": paneAda, "text": "Ask Bo about Sunday"})
	done, before := s.next(20*time.Second, doneFor("ask"))
	if done["status"] != StatusDone || !strings.Contains(done["text"].(string), "Yes, with a cake.") {
		t.Fatalf("done = %v", done)
	}
	boardEvs := map[string]map[string]any{}
	collect := func(ms []map[string]any) {
		for _, m := range ms {
			if m["ev"] == EvBoard {
				boardEvs[m["kind"].(string)] = m
			}
		}
	}
	collect(before)
	if _, ok := boardEvs["ack"]; !ok { // bo's ack lands just after the reply
		m, more := s.next(5*time.Second, func(m map[string]any) bool { return m["ev"] == EvBoard && m["kind"] == "ack" })
		collect(append(more, m))
	}
	cmd, reply, ack := boardEvs["cmd"], boardEvs["reply"], boardEvs["ack"]
	if cmd == nil || reply == nil || ack == nil {
		t.Fatalf("board events = %v", boardEvs)
	}
	if cmd["from"] != paneAda || cmd["to"] != paneBo || cmd["text"] != "Are you coming on Sunday?" || cmd["thread"] == "" {
		t.Errorf("cmd event = %v", cmd)
	}
	if reply["from"] != paneBo || reply["text"] != "Yes, with a cake." || reply["thread"] != cmd["thread"] {
		t.Errorf("reply event = %v", reply)
	}
	if ack["from"] != paneBo || ack["thread"] != cmd["thread"] {
		t.Errorf("ack event = %v", ack)
	}
	if at, _ := cmd["at"].(string); at == "" {
		t.Errorf("cmd event has no time: %v", cmd)
	} else if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Errorf("at %q is not RFC 3339: %v", at, err)
	}
	// bo's courier-delivered turn is not a serve turn: no chunk/done for it.
	for _, m := range before {
		if (m["ev"] == EvChunk || m["ev"] == EvDone) && m["pane"] == paneBo {
			t.Errorf("serve streamed bo's courier turn: %v", m)
		}
	}
}

// A `rysh fleet status` declaration reaches serve as a status event through
// the fleet actor's new ledger change feed; the family_status tool too.
func TestServe_StatusEventOnFleetStatus(t *testing.T) {
	w := newWorld(t)
	afa := actors.NewAgentFleetActor(w.nc, w.pub.Codecs(), nil)
	afa.Start()
	t.Cleanup(afa.Stop)
	if _, err := fleet.Send(w.nc, fleet.Update{Op: fleet.OpRegister, Fleet: fleet.Fleet{Name: famBrd, BoardID: famBrd, State: fleet.StateUp}}, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{paneAda, paneBo} {
		if _, err := fleet.Send(w.nc, fleet.Update{Op: fleet.OpMemberUpsert, Name: famBrd, Member: fleet.Member{PaneID: id, Label: paneNames[id]}}, 3*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	s := w.serve(nil)
	s.next(5*time.Second, ev(EvReady))

	pct := 40
	// What `rysh fleet status progress --note … --pct 40 --as <pane>` sends.
	if _, err := fleet.Send(w.nc, fleet.Update{Op: fleet.OpStatus, PaneID: paneAda,
		Status: fleet.StatusUpdate{Work: fleet.WorkProgress, Task: "lunch", Note: "baking bread", Pct: &pct}}, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	st, _ := s.next(5*time.Second, func(m map[string]any) bool { return m["ev"] == EvStatus && m["pane"] == paneAda })
	if st["state"] != "progress" || st["note"] != "baking bread" || st["pct"] != float64(40) {
		t.Fatalf("status = %v", st)
	}

	// bo declares through his own family_status tool, in a serve turn.
	w.setScript(paneBo, func(_ context.Context, conv []provider.ConversationTurn) (*provider.AgenticResponse, error) {
		if tr, ok := lastTool(conv); ok {
			return textResp("noted: " + tr.Content), nil
		}
		return toolResp("s1", "family_status", map[string]any{"state": "done", "note": "bread is baked"}), nil
	})
	s.sendJSON(map[string]string{"op": "prompt", "id": "st", "pane": paneBo, "text": "how are you?"})
	st2, _ := s.next(10*time.Second, func(m map[string]any) bool { return m["ev"] == EvStatus && m["pane"] == paneBo && m["state"] == "done" })
	if st2["note"] != "bread is baked" {
		t.Fatalf("bo status = %v", st2)
	}
	if _, has := st2["pct"]; !has || st2["pct"] != nil {
		t.Fatalf("pct must be present and null when not declared: %v", st2)
	}
	s.next(10*time.Second, doneFor("st"))
}

// Malformed and invalid lines get an error (or, for a valid id, a done/error)
// and serve keeps going; ping answers pong.
func TestServe_MalformedLinesAndValidation(t *testing.T) {
	w := newWorld(t)
	s := w.serve(nil)
	s.next(5*time.Second, ev(EvReady))

	for _, line := range []string{
		`this is not json`,
		`{"op":"launch"}`,
		`{"id":"x"}`,
		`[1,2,3]`,
		`{"op":"prompt","id":"bad id!","pane":"` + paneAda + `","text":"hi"}`,
		`{"op":"prompt","id":"` + strings.Repeat("a", 65) + `","pane":"` + paneAda + `","text":"hi"}`,
		`{"op":"prompt","id":"p","pane":"` + paneAda + `","text":` + strings.Repeat(" ", MaxLineBytes) + `"hi"}`, // > 64 KiB
	} {
		s.send(line)
		e, _ := s.next(3*time.Second, func(m map[string]any) bool { return m["ev"] == EvError || m["ev"] == EvDone })
		if e["ev"] != EvError || e["message"] == "" {
			t.Fatalf("%.60q: got %v, want an error event", line, e)
		}
	}
	for _, c := range []struct{ id, pane, text, want string }{
		{"v1", "not-a-uuid", "hi", "pane must be a pane uuid"},
		{"v2", paneAda, "   ", "text is empty"},
		{"v3", paneAda, strings.Repeat("x", MaxPromptBytes+1), "exceeds"},
		{"v4", paneCy, "hi", "no agent profile"},
		{"v5", "dddddddd-4444-4444-8444-444444444444", "hi", "did not answer"},
	} {
		s.sendJSON(map[string]string{"op": "prompt", "id": c.id, "pane": c.pane, "text": c.text})
		d, _ := s.next(5*time.Second, doneFor(c.id))
		if d["status"] != StatusError || !strings.Contains(d["error"].(string), c.want) {
			t.Fatalf("%s: done = %v, want error containing %q", c.id, d, c.want)
		}
	}
	s.send(`{"op":"ping","extra":"ignored"}`)
	s.next(3*time.Second, ev(EvPong))
	// Still serving: a real turn works after all of that.
	s.sendJSON(map[string]string{"op": "prompt", "id": "ok", "pane": paneAda, "text": "hi"})
	if d, _ := s.next(10*time.Second, doneFor("ok")); d["status"] != StatusDone {
		t.Fatalf("turn after bad lines: %v", d)
	}
	// A duplicate id while the first is still queued/running is refused with
	// an error event, not a second done.
	g := newGate()
	w.setScript(paneAda, g.script)
	s.sendJSON(map[string]string{"op": "prompt", "id": "dup", "pane": paneAda, "text": "first"})
	g.waitStarted(t, "first")
	s.sendJSON(map[string]string{"op": "prompt", "id": "dup", "pane": paneAda, "text": "second"})
	if e, _ := s.next(3*time.Second, func(m map[string]any) bool { return m["ev"] == EvError || m["ev"] == EvDone }); e["ev"] != EvError {
		t.Fatalf("duplicate id: %v", e)
	}
	g.release <- struct{}{}
	s.next(10*time.Second, doneFor("dup"))
}

// stdin EOF ends serve with a nil error (exit 0); stdout carried only JSON.
func TestServe_EOFExitsZero(t *testing.T) {
	w := newWorld(t)
	s := w.serve(nil)
	s.next(5*time.Second, ev(EvReady))
	s.send(`{"op":"ping"}`)
	s.next(3*time.Second, ev(EvPong))
	_ = s.in.Close()
	select {
	case err := <-s.result:
		if err != nil {
			t.Fatalf("Run after EOF = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not exit on stdin EOF")
	}
}

// The courier no longer cancels a busy native pane's turn: ada and bo ask each
// other at the same moment, each turn blocked in family_ask. Before H3 the
// courier's delivery cancelled both waits. Now each delivery waits for the
// recipient's turn to end, so both serve turns finish normally (their asks
// time out, nothing is interrupted) and then both questions are answered on
// the board.
func TestServe_CourierDoesNotCancelBusyPane(t *testing.T) {
	w := newWorld(t)
	w.startCourier(true)
	for _, id := range []string{paneAda, paneBo} {
		w.asks[id].Timeout = 2 * time.Second
	}
	both := func(other, question, answer string) script {
		return func(_ context.Context, conv []provider.ConversationTurn) (*provider.AgenticResponse, error) {
			if tr, ok := lastTool(conv); ok {
				return textResp("Result: " + tr.Content), nil
			}
			if m := threadRE.FindStringSubmatch(lastUser(conv)); m != nil {
				return toolResp("r1", "family_reply", map[string]string{"thread_id": m[1], "answer": answer}), nil
			}
			return toolResp("a1", "family_ask", map[string]string{"to_pane": other, "question": question}), nil
		}
	}
	w.setScript(paneAda, both("bo", "Bo, lunch?", "ada: yes"))
	w.setScript(paneBo, both("ada", "Ada, dinner?", "bo: yes"))
	s := w.serve(nil)
	s.next(5*time.Second, ev(EvReady))
	s.sendJSON(map[string]string{"op": "prompt", "id": "A", "pane": paneAda, "text": "ask bo"})
	s.sendJSON(map[string]string{"op": "prompt", "id": "B", "pane": paneBo, "text": "ask ada"})

	got := map[string]map[string]any{}
	var replies []map[string]any
	deadline := time.After(30 * time.Second)
	for len(got) < 2 || len(replies) < 2 {
		select {
		case m := <-s.lines:
			switch {
			case m["ev"] == EvDone:
				got[m["id"].(string)] = m
			case m["ev"] == EvBoard && m["kind"] == "reply":
				replies = append(replies, m)
			}
		case <-deadline:
			t.Fatalf("dones %v replies %v", got, replies)
		}
	}
	for _, id := range []string{"A", "B"} {
		d := got[id]
		txt, _ := d["text"].(string)
		if d["status"] != StatusDone || strings.Contains(txt, "Interrupted") || !strings.Contains(txt, "Result:") {
			t.Fatalf("turn %s was cut short: %v", id, d)
		}
	}
	answers := map[string]string{}
	for _, r := range replies {
		answers[r["from"].(string)] = r["text"].(string)
	}
	if answers[paneAda] != "ada: yes" || answers[paneBo] != "bo: yes" {
		t.Fatalf("board replies = %v", replies)
	}
}

// gatedTool always asks for an approval.
type gatedTool struct{}

func (gatedTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{Name: "order_flowers", Description: "order flowers",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`), RequiresApproval: true}
}
func (gatedTool) RequiresApproval(json.RawMessage) bool { return true }
func (gatedTool) Execute(context.Context, json.RawMessage) (*tools.ToolOutput, error) {
	return &tools.ToolOutput{Content: "ordered"}, nil
}

// A turn that stops on an approval gate ends with a normal done (status done,
// never a gate_blocked status) whose text says why; nothing is approved.
func TestServe_ApprovalGateEndsTheTurnWithAnExplanation(t *testing.T) {
	w := newWorldWith(t, []tools.ToolExecutor{gatedTool{}})
	w.setScript(paneAda, func(_ context.Context, conv []provider.ConversationTurn) (*provider.AgenticResponse, error) {
		if tr, ok := lastTool(conv); ok {
			return textResp("tool said: " + tr.Content), nil
		}
		return &provider.AgenticResponse{StopReason: provider.StopReasonToolUse,
			TextBlocks: []provider.TextBlock{{Text: "Let me order some."}},
			ToolCalls:  []provider.ToolCallRequest{{ID: "g1", Name: "order_flowers", Input: json.RawMessage(`{}`)}}}, nil
	})
	s := w.serve(nil)
	s.next(5*time.Second, ev(EvReady))
	s.sendJSON(map[string]string{"op": "prompt", "id": "gate", "pane": paneAda, "text": "send flowers"})
	d, _ := s.next(15*time.Second, doneFor("gate"))
	txt, _ := d["text"].(string)
	if d["status"] != StatusDone || !strings.Contains(txt, "needed an approval") || !strings.Contains(txt, "Let me order some.") {
		t.Fatalf("done = %v", d)
	}
	if strings.Contains(txt, "ordered") {
		t.Fatalf("the gated tool ran: %v", d)
	}
}

// e20 N1: serve drives a pane only if the pane's meta, read when the prompt
// arrives (and again before the send), allows family tools and nothing else.
// A profile widened to bash after serve started is refused; a narrower family
// list is fine; nothing reaches the model of a refused pane.
func TestServe_DrivesOnlyFamilyOnlyPanes(t *testing.T) {
	w := newWorld(t)
	w.setScript(paneBo, func(context.Context, []provider.ConversationTurn) (*provider.AgenticResponse, error) {
		return textResp("bo here"), nil
	})
	s := w.serve(nil)
	ready, _ := s.next(5*time.Second, ev(EvReady))
	if b, _ := json.Marshal(ready["panes"]); !strings.Contains(string(b), paneBo) {
		t.Fatalf("ready = %v", ready)
	}

	for i, c := range []struct {
		tools string
		ok    bool
	}{
		{familyTools + ",bash", false},
		{"family_ask,web_search", false},
		{"bash", false},
		{"", false},
		{"family_reply", true},
		{familyTools, true},
	} {
		w.setMeta(paneBo, "agent.tools", c.tools)
		id := fmt.Sprintf("n%d", i)
		s.sendJSON(map[string]string{"op": "prompt", "id": id, "pane": paneBo, "text": "hello"})
		d, _ := s.next(10*time.Second, doneFor(id))
		if c.ok && d["status"] != StatusDone {
			t.Fatalf("tools %q: done = %v, want done", c.tools, d)
		}
		if !c.ok && (d["status"] != StatusError || d["error"] == "") {
			t.Fatalf("tools %q: done = %v, want a refusal", c.tools, d)
		}
	}
	if got := w.provs[paneBo].userTurns(); len(got) != 2 {
		t.Fatalf("model saw %d turns, want 2 (only the family-only cases): %q", len(got), got)
	}

	// Widened while the turn waits in the queue: refused at send time.
	g := newGate()
	w.setScript(paneBo, g.script)
	s.sendJSON(map[string]string{"op": "prompt", "id": "run", "pane": paneBo, "text": "first"})
	g.waitStarted(t, "first")
	s.sendJSON(map[string]string{"op": "prompt", "id": "queued", "pane": paneBo, "text": "second"})
	time.Sleep(200 * time.Millisecond)
	w.setMeta(paneBo, "agent.tools", familyTools+",bash")
	g.release <- struct{}{}
	s.next(10*time.Second, doneFor("run"))
	d, _ := s.next(10*time.Second, doneFor("queued"))
	if d["status"] != StatusError || !strings.Contains(d["error"].(string), "family-only") {
		t.Fatalf("queued turn after the profile widened: %v", d)
	}
	select {
	case u := <-g.started:
		t.Fatalf("the widened pane's model was prompted: %q", u)
	case <-time.After(300 * time.Millisecond):
	}
}
