package tools

// Tests for rysh_command (design 008 RA3). The two properties worth protecting
// are (a) it publishes to the WORKSPACE inbox — the only subject whose handler
// understands "##" — and (b) its approval allowlist is safe-by-default.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// newTestNATS starts an in-process NATS server and returns a connected client.
func newTestNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := &server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true}
	srv, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func TestRyshCommandRequiresApproval(t *testing.T) {
	tool := NewRyshCommandTool(nil, "s", "p")
	cases := []struct {
		command string
		want    bool // want approval required
	}{
		// Read-only — must not prompt.
		{"tab list", false},
		{"##tab list", false},
		{"pane list", false},
		{"agent list", false},
		{"session info", false},
		{"share status", false},
		{"humanoid channels", false},
		{"upstream shares", false},
		{"cost", false},
		{"cost week", false},
		{"help", false},
		{"TAB LIST", false}, // case-insensitive

		// Mutating — must prompt.
		{"tab new", true},
		{"pane close", true},
		{"agent delete reviewer", true},
		{"session switch other", true},
		{"share pane control", true},
		{"worktree merge", true},
		{"humanoid deactivate bot", true},

		// Unrecognised — safe by default.
		{"frobnicate the thing", true},
		{"", true},

		// "####" is a remote-control escape for a subscriber session, not a
		// local read: never auto-approve it even though "list" follows.
		{"####tab list", true},
	}
	for _, tc := range cases {
		params, err := json.Marshal(RyshCommandParams{Command: tc.command})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if got := tool.RequiresApproval(params); got != tc.want {
			t.Errorf("RequiresApproval(%q) = %v, want %v", tc.command, got, tc.want)
		}
	}
}

func TestRyshCommandRequiresApprovalOnMalformedParams(t *testing.T) {
	tool := NewRyshCommandTool(nil, "s", "p")
	if !tool.RequiresApproval(json.RawMessage(`{not json`)) {
		t.Fatal("malformed params must require approval, not be waved through")
	}
}

func TestNormalizeRyshCommand(t *testing.T) {
	cases := []struct{ in, want string }{
		{"tab list", "tab list"},
		{"##tab list", "tab list"},
		{"  ##tab list  ", "tab list"},
		// Only ONE "##" pair is stripped, so a "####" escape survives
		// re-prefixing unchanged rather than being silently retargeted.
		{"####remote cmd", "##remote cmd"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := normalizeRyshCommand(tc.in); got != tc.want {
			t.Errorf("normalizeRyshCommand(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRyshCommandPublishesToWorkspaceInbox is the regression guard for the whole
// point of the tool: it must reach the workspace inbox (where "##" is handled),
// never the pane inbox (where pane_send goes and "##" would be run as a shell
// command).
func TestRyshCommandPublishesToWorkspaceInbox(t *testing.T) {
	nc := newTestNATS(t)

	wsSub, err := nc.SubscribeSync(msg.T("ws", "sess1", "inbox"))
	if err != nil {
		t.Fatalf("subscribe ws: %v", err)
	}
	paneSub, err := nc.SubscribeSync(msg.T("pane", "pane1", "inbox"))
	if err != nil {
		t.Fatalf("subscribe pane: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	tool := NewRyshCommandTool(nc, "sess1", "pane1")
	params, _ := json.Marshal(RyshCommandParams{Command: "tab list"})

	// No responder publishes rysh output, so Execute returns after the quiet
	// window with an explicit "no output" note — the publish still happened.
	out, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Content == "" {
		t.Error("expected an explanatory message when no output arrives")
	}

	received, err := wsSub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("nothing published to the workspace inbox: %v", err)
	}
	var env msg.NATSEnvelope
	if err := json.Unmarshal(received.Data, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.TypeTag != msg.TagSubmitInput {
		t.Errorf("TypeTag = %q, want %q", env.TypeTag, msg.TagSubmitInput)
	}
	var submit msg.MsgSubmitInput
	if err := json.Unmarshal(env.Payload, &submit); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	// Exactly one "##" prefix — not zero (would run as a shell command), not
	// two (would become a remote-control escape).
	if submit.Text != "##tab list" {
		t.Errorf("Text = %q, want %q", submit.Text, "##tab list")
	}
	if submit.PaneID != "pane1" {
		t.Errorf("PaneID = %q, want %q", submit.PaneID, "pane1")
	}

	if _, err := paneSub.NextMsg(200 * time.Millisecond); err == nil {
		t.Error("published to the pane inbox — that bypasses the '##' handler")
	}
}

// TestRyshCommandCollectsOutput proves the tool reads the command's result back
// off the pane's rysh conversation topic, so the model can relay it.
func TestRyshCommandCollectsOutput(t *testing.T) {
	nc := newTestNATS(t)

	// Stand in for WorkspaceActor: on any workspace input, emit rysh output.
	_, err := nc.Subscribe(msg.T("ws", "sess1", "inbox"), func(_ *nats.Msg) {
		payload, _ := json.Marshal(&msg.MsgConversationAppend{
			Message: &msg.ConversationMessage{Content: "tab 1: main\ntab 2: build\n"},
		})
		data, _ := json.Marshal(msg.NATSEnvelope{
			TypeTag: "MsgConversationAppend",
			Payload: payload,
		})
		_ = nc.Publish(msg.T("pane", "pane1", "output", "rysh"), data)
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	tool := NewRyshCommandTool(nc, "sess1", "pane1")
	params, _ := json.Marshal(RyshCommandParams{Command: "tab list"})

	out, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out.Content, "tab 1: main") || !strings.Contains(out.Content, "tab 2: build") {
		t.Errorf("output not collected, got %q", out.Content)
	}
	if out.Metadata["command"] != "##tab list" {
		t.Errorf("metadata command = %q", out.Metadata["command"])
	}
}

func TestRyshCommandValidation(t *testing.T) {
	tool := NewRyshCommandTool(nil, "sess1", "pane1")

	// Empty command.
	params, _ := json.Marshal(RyshCommandParams{Command: "  "})
	out, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Error == "" || out.ErrorKind != ErrKindValidation {
		t.Errorf("empty command should be a validation error, got %+v", out)
	}

	// Pipeline events are not system commands.
	params, _ = json.Marshal(RyshCommandParams{Command: "##>event:print:hi"})
	out, err = tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Error == "" || out.ErrorKind != ErrKindValidation {
		t.Errorf("'##>' pipeline event should be rejected, got %+v", out)
	}

	// No pane to target.
	noPane := NewRyshCommandTool(nil, "sess1", "")
	params, _ = json.Marshal(RyshCommandParams{Command: "tab list"})
	out, err = noPane.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out.Error == "" || out.ErrorKind != ErrKindValidation {
		t.Errorf("missing pane should be a validation error, got %+v", out)
	}
}
