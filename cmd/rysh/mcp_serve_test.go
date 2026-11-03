// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// fakeSession is the session as `rysh mcp serve` sees it, with every write
// recorded so a test can prove what did NOT happen.
type fakeSession struct {
	mu       sync.Mutex
	snap     *domain.WorkspaceSnapshot
	screens  map[string]*msg.MsgPaneVTReply
	outputs  map[string]*domain.PaneSnapshot
	board    *board.QueryReply
	humanoid []msg.HumanoidInfo
	procs    chan msg.MsgPaneProcess

	gateResp *msg.MsgCLIResponse
	gateErr  error
	gates    []msg.MsgCLIMCPGate
	sends    []string // "<pane>|<mode>|<text>"
	posts    []string
	channel  []msg.MsgHumanoidMCPSend
	chReply  *msg.MsgHumanoidMCPSendReply
	commands []string // "<pane>|<command>"
}

func (f *fakeSession) Snapshot(context.Context) (*domain.WorkspaceSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snap == nil {
		return nil, errors.New("no session")
	}
	b, _ := json.Marshal(f.snap)
	var cp domain.WorkspaceSnapshot
	_ = json.Unmarshal(b, &cp)
	return &cp, nil
}
func (f *fakeSession) setSnap(s *domain.WorkspaceSnapshot) { f.mu.Lock(); f.snap = s; f.mu.Unlock() }
func (f *fakeSession) PaneOutput(_ context.Context, id string) (*domain.PaneSnapshot, error) {
	if p, ok := f.outputs[id]; ok {
		return p, nil
	}
	return nil, errors.New("no pane")
}
func (f *fakeSession) PaneScreen(_ context.Context, id string) (*msg.MsgPaneVTReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.screens[id]; ok {
		return s, nil
	}
	return &msg.MsgPaneVTReply{PaneID: id}, nil
}
func (f *fakeSession) Board(context.Context, string, int) (*board.QueryReply, error) {
	if f.board == nil {
		return nil, board.ErrNoRecorder
	}
	return f.board, nil
}
func (f *fakeSession) Humanoids(context.Context) ([]msg.HumanoidInfo, error) { return f.humanoid, nil }
func (f *fakeSession) WatchProcess(context.Context, string) (<-chan msg.MsgPaneProcess, func(), error) {
	if f.procs == nil {
		f.procs = make(chan msg.MsgPaneProcess, 4)
	}
	return f.procs, func() {}, nil
}
func (f *fakeSession) Gate(_ context.Context, req *msg.MsgCLIMCPGate) (*msg.MsgCLIResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gates = append(f.gates, *req)
	if f.gateErr != nil {
		return nil, f.gateErr
	}
	if f.gateResp != nil {
		return f.gateResp, nil
	}
	return &msg.MsgCLIResponse{OK: true}, nil
}
func (f *fakeSession) SendInput(_ context.Context, pane, text, mode string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, pane+"|"+mode+"|"+text)
	return nil
}
func (f *fakeSession) BoardPost(_ context.Context, as, text, thread, _ string) (*msg.MsgCLIResponse, error) {
	f.posts = append(f.posts, as+"|"+thread+"|"+text)
	return &msg.MsgCLIResponse{OK: true, ID: "t1", Output: "posted"}, nil
}
func (f *fakeSession) ChannelSend(_ context.Context, _ string, req *msg.MsgHumanoidMCPSend) (*msg.MsgHumanoidMCPSendReply, error) {
	f.channel = append(f.channel, *req)
	if f.chReply != nil {
		return f.chReply, nil
	}
	return &msg.MsgHumanoidMCPSendReply{Outcome: "sent", Governance: "ai"}, nil
}
func (f *fakeSession) RyshCommand(_ context.Context, pane, command string) (*msg.MsgCLIResponse, error) {
	f.commands = append(f.commands, pane+"|"+command)
	return &msg.MsgCLIResponse{OK: true, ID: "new-pane", Output: "created"}, nil
}

func fixtureSession() *fakeSession {
	return &fakeSession{
		snap: &domain.WorkspaceSnapshot{
			ActivePaneID: "p-shell",
			Tabs: []domain.TabSnapshot{{
				ID: "t1", Title: "work",
				Lanes: []domain.LaneSnapshot{{ID: "l1", PaneGroups: []domain.PaneGroupSnapshot{{
					ID: "g1",
					Panes: []domain.PaneSnapshot{
						{ID: "p-shell", Title: "pane-1", Mode: "shell", Status: "idle",
							ShellHistory: []string{"export AWS_SECRET=never-shown"}, Meta: map[string]string{"note": "private"}},
						{ID: "p-busy", Title: "pane-2", GivenName: "builder", Mode: "shell", Program: "claude"},
						{ID: "p-quiet", Title: "pane-3", GivenName: "reviewer", Mode: "shell", Program: "claude"},
						{ID: "p-ask", Title: "pane-4", GivenName: "asker", Mode: "shell", Program: "codex",
							Meta: map[string]string{domain.PaneMetaAttention: domain.AttentionNeedsYou, domain.PaneMetaAttentionReason: domain.AttentionReasonApproval}},
						{ID: "p-make", Title: "pane-5", Mode: "shell", Program: "make"},
					},
				}}}},
			}},
		},
		screens: map[string]*msg.MsgPaneVTReply{
			"p-busy":  {PaneID: "p-busy", Interactive: true, Screen: []string{"\x1b[1mthinking\x1b[0m", "* Working… (esc to interrupt)"}},
			"p-quiet": {PaneID: "p-quiet", Interactive: true, Screen: []string{"> ", "? for shortcuts"}},
		},
		outputs: map[string]*domain.PaneSnapshot{
			"p-shell": {ID: "p-shell", Output: "one\ntwo\n\x1b[31mthree\x1b[0m\n"},
		},
	}
}

func callTool(t *testing.T, tools *sessionTools, name, args string) (map[string]any, string, bool) {
	t.Helper()
	text, isErr := tools.call(context.Background(), name, json.RawMessage(args))
	var out map[string]any
	_ = json.Unmarshal([]byte(text), &out)
	return out, text, isErr
}

func toolNames(tools *sessionTools) []string {
	var names []string
	for _, s := range tools.specs() {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	return names
}

func TestMCPServeListsOnlyReadToolsByDefault(t *testing.T) {
	got := strings.Join(toolNames(newSessionTools(fixtureSession(), false)), ",")
	want := "list_channels,list_panes,read_board,read_pane,wait_for_pane_state"
	if got != want {
		t.Fatalf("read-only tools/list = %s, want %s", got, want)
	}
	got = strings.Join(toolNames(newSessionTools(fixtureSession(), true)), ",")
	want = "board_post,channel_send,list_channels,list_panes,read_board,read_pane,send_input,start_pane,wait_for_pane_state"
	if got != want {
		t.Fatalf("--allow-write tools/list = %s, want %s", got, want)
	}
}

func TestMCPServeWriteToolDisabledNeverReachesSession(t *testing.T) {
	f := fixtureSession()
	tools := newSessionTools(f, false)
	for _, name := range []string{"send_input", "board_post", "channel_send", "start_pane"} {
		_, text, isErr := callTool(t, tools, name, `{"pane":"p-shell","as_pane":"p-shell","input":"ls","text":"hi","humanoid":"h","channel":"slack","to":"u","kind":"shell"}`)
		if !isErr || !strings.Contains(text, "--allow-write") {
			t.Fatalf("%s without --allow-write: isErr=%v text=%q", name, isErr, text)
		}
	}
	if len(f.gates)+len(f.sends)+len(f.posts)+len(f.channel)+len(f.commands) != 0 {
		t.Fatalf("a disabled write touched the session: gates=%d sends=%d posts=%d channel=%d commands=%d",
			len(f.gates), len(f.sends), len(f.posts), len(f.channel), len(f.commands))
	}
}

func TestMCPServeGateRefusalStopsTheWrite(t *testing.T) {
	f := fixtureSession()
	f.gateResp = &msg.MsgCLIResponse{OK: false, Error: "policy: approval.always_gate[0] gates mcp_send_input"}
	tools := newSessionTools(f, true)
	_, text, isErr := callTool(t, tools, "send_input", `{"pane":"builder","input":"rm -rf build"}`)
	if !isErr || !strings.Contains(text, "approval.always_gate[0]") {
		t.Fatalf("refused gate: isErr=%v text=%q", isErr, text)
	}
	if len(f.sends) != 0 {
		t.Fatalf("a refused gate still sent: %v", f.sends)
	}
	for _, name := range []string{"board_post", "channel_send", "start_pane"} {
		_, _, isErr := callTool(t, tools, name, `{"as_pane":"p-shell","text":"hi","humanoid":"h","channel":"slack","to":"u","kind":"shell"}`)
		if !isErr {
			t.Fatalf("%s ran past a refused gate", name)
		}
	}
	if len(f.posts)+len(f.channel)+len(f.commands) != 0 {
		t.Fatalf("a refused gate still acted: posts=%v channel=%v commands=%v", f.posts, f.channel, f.commands)
	}
}

func TestMCPServeGateUnansweredFailsClosed(t *testing.T) {
	f := fixtureSession()
	f.gateErr = errors.New("timeout waiting for reply")
	_, text, isErr := callTool(t, newSessionTools(f, true), "send_input", `{"pane":"p-shell","input":"ls"}`)
	if !isErr || !strings.Contains(text, "refusing") {
		t.Fatalf("unanswered gate: isErr=%v text=%q", isErr, text)
	}
	if len(f.sends) != 0 {
		t.Fatalf("an unanswered gate still sent: %v", f.sends)
	}
}

func TestMCPServeSendInputGatesWhatWouldBeTyped(t *testing.T) {
	f := fixtureSession()
	tools := newSessionTools(f, true)
	if _, text, isErr := callTool(t, tools, "send_input", `{"pane":"builder","input":"go test ./..."}`); isErr {
		t.Fatalf("send_input: %s", text)
	}
	if len(f.gates) != 1 {
		t.Fatalf("gates = %d, want 1", len(f.gates))
	}
	g := f.gates[0]
	if g.Tool != "send_input" || g.Caller != "mcp" || g.Target != "p-busy" || g.ShellCommand != "go test ./..." {
		t.Fatalf("gate request = %+v", g)
	}
	if len(f.sends) != 1 || f.sends[0] != "p-busy|shell|go test ./..." {
		t.Fatalf("sends = %v (given-name must resolve to the pane id)", f.sends)
	}
	if _, text, isErr := callTool(t, tools, "send_input", `{"pane":"p-quiet","input":"review it","mode":"prompt"}`); isErr {
		t.Fatalf("prompt send: %s", text)
	}
	if f.gates[1].ShellCommand != "" {
		t.Fatalf("a prompt is not a shell command: %+v", f.gates[1])
	}
	if _, _, isErr := callTool(t, tools, "send_input", `{"pane":"nope","input":"ls"}`); !isErr {
		t.Fatal("an unknown pane must fail")
	}
}

func TestMCPServeListPanesStatesAndNoPrivateFields(t *testing.T) {
	out, text, isErr := callTool(t, newSessionTools(fixtureSession(), false), "list_panes", `{}`)
	if isErr {
		t.Fatal(text)
	}
	for _, forbidden := range []string{"AWS_SECRET", "never-shown", "private", "meta", "history"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("list_panes leaked %q: %s", forbidden, text)
		}
	}
	got := map[string]string{}
	kinds := map[string]string{}
	for _, tab := range out["tabs"].([]any) {
		for _, p := range tab.(map[string]any)["panes"].([]any) {
			pm := p.(map[string]any)
			got[pm["id"].(string)] = pm["state"].(string)
			kinds[pm["id"].(string)] = pm["kind"].(string)
		}
	}
	want := map[string]string{"p-shell": "idle", "p-busy": "working", "p-quiet": "idle", "p-ask": "needs-you", "p-make": "working"}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("state[%s] = %q, want %q", id, got[id], w)
		}
	}
	if kinds["p-busy"] != "claude" || kinds["p-ask"] != "codex" || kinds["p-shell"] != "shell" || kinds["p-make"] != "program" {
		t.Errorf("kinds = %v", kinds)
	}
}

func TestMCPServeReadPane(t *testing.T) {
	tools := newSessionTools(fixtureSession(), false)
	out, text, isErr := callTool(t, tools, "read_pane", `{"pane":"builder"}`)
	if isErr || out["source"] != "screen" || !strings.Contains(text, "esc to interrupt") || strings.Contains(text, "\\u001b") {
		t.Fatalf("screen read: isErr=%v %s", isErr, text)
	}
	out, text, isErr = callTool(t, tools, "read_pane", `{"pane":"p-shell","lines":2}`)
	if isErr || out["source"] != "output" {
		t.Fatalf("output read: %s", text)
	}
	lines := out["lines"].([]any)
	if len(lines) != 2 || lines[0] != "two" || lines[1] != "three" {
		t.Fatalf("last 2 lines = %v", lines)
	}
}

func TestMCPServeReadBoardNoRecorderIsAnError(t *testing.T) {
	f := fixtureSession()
	tools := newSessionTools(f, false)
	if _, text, isErr := callTool(t, tools, "read_board", `{}`); !isErr || !strings.Contains(text, "recorder") {
		t.Fatalf("no recorder must not read as an empty board: %s", text)
	}
	f.board = &board.QueryReply{Threads: []board.Thread{{Key: "p-busy/1",
		Root:    &msg.MsgBoardPost{PaneID: "p-busy", Persona: "builder", Text: "build green", TS: 1},
		Replies: []*msg.MsgBoardPost{{PaneID: "p-quiet", Persona: "reviewer", Text: "ack", TS: 2}}}}}
	_, text, isErr := callTool(t, tools, "read_board", `{"limit":5}`)
	if isErr || !strings.Contains(text, "build green") || !strings.Contains(text, "ack") {
		t.Fatalf("board read: %s", text)
	}
}

func TestMCPServeListChannels(t *testing.T) {
	f := fixtureSession()
	f.humanoid = []msg.HumanoidInfo{{Name: "support", Active: true, SystemPrompt: "do-not-leak-this-prompt",
		Channels: []msg.ChannelStatus{{Type: "slack", Connected: true, Governance: "human"}}}}
	_, text, isErr := callTool(t, newSessionTools(f, false), "list_channels", `{}`)
	if isErr || !strings.Contains(text, `"governance": "human"`) || !strings.Contains(text, "support") {
		t.Fatalf("list_channels: %s", text)
	}
	if strings.Contains(text, "do-not-leak-this-prompt") {
		t.Fatalf("list_channels leaked the system prompt: %s", text)
	}
}

func TestMCPServeChannelSendReportsTheDraft(t *testing.T) {
	f := fixtureSession()
	f.chReply = &msg.MsgHumanoidMCPSendReply{Outcome: "drafted", Governance: "human", Queued: 1}
	out, text, isErr := callTool(t, newSessionTools(f, true), "channel_send", `{"humanoid":"support","channel":"slack","to":"C1","text":"hello"}`)
	if isErr || out["outcome"] != "drafted" {
		t.Fatalf("channel_send: %s", text)
	}
	if len(f.gates) != 1 || f.gates[0].Target != "support/slack" || f.gates[0].ShellCommand != "" {
		t.Fatalf("gate = %+v", f.gates)
	}
	if len(f.channel) != 1 || f.channel[0].Content != "hello" || f.channel[0].RecipientID != "C1" {
		t.Fatalf("channel request = %+v", f.channel)
	}
}

func TestMCPServeStartPaneUsesTheCommandTable(t *testing.T) {
	f := fixtureSession()
	tools := newSessionTools(f, true)
	for _, c := range []struct{ args, want string }{
		{`{"kind":"shell"}`, "|pane new"},
		{`{"kind":"claude","prompt":"fix the flaky test","near_pane":"builder"}`, "p-busy|pane new --claude fix the flaky test"},
		{`{"kind":"agent","agent":"reviewer-bot"}`, "|agent spawn reviewer-bot"},
	} {
		if _, text, isErr := callTool(t, tools, "start_pane", c.args); isErr {
			t.Fatalf("%s: %s", c.args, text)
		}
		if got := f.commands[len(f.commands)-1]; got != c.want {
			t.Fatalf("%s ran %q, want %q", c.args, got, c.want)
		}
	}
	for _, bad := range []string{
		`{"kind":"claude","prompt":"line one\nline two"}`,
		`{"kind":"claude","prompt":"x ##secret list"}`,
		`{"kind":"agent","agent":"a b"}`,
		`{"kind":"vim"}`,
	} {
		n := len(f.commands)
		if _, _, isErr := callTool(t, tools, "start_pane", bad); !isErr || len(f.commands) != n {
			t.Fatalf("%s must be refused before it runs", bad)
		}
	}
}

func TestMCPServeWaitForPaneState(t *testing.T) {
	f := fixtureSession()
	tools := newSessionTools(f, false)
	tools.poll = 10 * time.Millisecond

	// Already there.
	out, text, _ := callTool(t, tools, "wait_for_pane_state", `{"pane":"asker","state":"needs-you","timeout_seconds":1}`)
	if out["reached"] != true {
		t.Fatalf("needs-you now: %s", text)
	}
	// Times out.
	out, text, _ = callTool(t, tools, "wait_for_pane_state", `{"pane":"builder","state":"idle","timeout_seconds":0.1}`)
	if out["reached"] != false || out["state"] != "working" {
		t.Fatalf("timeout: %s", text)
	}
	// Reaches idle when the screen settles.
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.mu.Lock()
		f.screens["p-busy"] = &msg.MsgPaneVTReply{PaneID: "p-busy", Interactive: true, Screen: []string{"> "}}
		f.mu.Unlock()
	}()
	out, text, _ = callTool(t, tools, "wait_for_pane_state", `{"pane":"builder","state":"idle","timeout_seconds":5}`)
	if out["reached"] != true || out["state"] != "idle" {
		t.Fatalf("idle: %s", text)
	}
	// Exited on the daemon's exit event.
	f.procs = make(chan msg.MsgPaneProcess, 1)
	go func() {
		time.Sleep(30 * time.Millisecond)
		f.procs <- msg.MsgPaneProcess{PaneID: "p-make", Event: "exit", Program: "make"}
	}()
	out, text, _ = callTool(t, tools, "wait_for_pane_state", `{"pane":"p-make","state":"exited","timeout_seconds":5}`)
	if out["reached"] != true || out["state"] != "exited" {
		t.Fatalf("exited: %s", text)
	}
	if _, _, isErr := callTool(t, tools, "wait_for_pane_state", `{"pane":"builder","state":"asleep"}`); !isErr {
		t.Fatal("an unknown state must fail")
	}
}

// Over the real stdio loop: tools/list says exactly what is enabled, and a
// call that waits does not hold up the calls behind it.
func TestMCPServeOverStdio(t *testing.T) {
	f := fixtureSession()
	tools := newSessionTools(f, false)
	tools.poll = 10 * time.Millisecond
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serveStdioMCP(inR, outW, stdioMCPServer{name: "rysh", tools: func() any { return tools.specs() }, call: tools.call, callTimeout: 10 * time.Second})
		outW.Close()
	}()
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	next := func() map[string]any {
		t.Helper()
		if !sc.Scan() {
			t.Fatal("no reply")
		}
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	write := func(s string) { _, _ = io.WriteString(inW, s+"\n") }

	write(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if next()["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != "rysh" {
		t.Fatal("serverInfo name")
	}
	write(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	listed := next()["result"].(map[string]any)["tools"].([]any)
	if len(listed) != len(sessionReadToolSpecs) {
		t.Fatalf("read-only server listed %d tools, want %d", len(listed), len(sessionReadToolSpecs))
	}

	write(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"wait_for_pane_state","arguments":{"pane":"builder","state":"idle","timeout_seconds":2}}}`)
	write(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_panes","arguments":{}}}`)
	first := next()
	if first["id"].(float64) != 4 {
		t.Fatalf("list_panes waited behind wait_for_pane_state: first reply id %v", first["id"])
	}
	if second := next(); second["id"].(float64) != 3 {
		t.Fatalf("second reply id %v", second["id"])
	}
	inW.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop on EOF")
	}
}
