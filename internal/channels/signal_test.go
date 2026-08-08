package channels

// Tests for the Signal adapter (C4, design 001 §4.4 / §6): pure mapping
// helpers (receive-envelope → InboundMessage, group-vs-recipient routing,
// step suppression) plus an integration-style round-trip against a fake
// signal-cli daemon on a UNIX socket — no signal-cli install required.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// testGroupID is a realistic signal-cli group id (base64 of 32 bytes).
const testGroupID = "abcdEFGHijklMNOPqrstUVWXyz0123456789abcdEFG="

func TestSignalInboundToMessage(t *testing.T) {
	tests := []struct {
		name   string
		params string
		wantOK bool
		want   InboundMessage
	}{
		{
			name: "1:1 message",
			params: `{"account":"+15550001111","envelope":{
				"source":"+15552223333","sourceUuid":"11111111-2222-3333-4444-555555555555",
				"sourceName":"Alice","timestamp":1720000000123,
				"dataMessage":{"message":"hello there"}}}`,
			wantOK: true,
			want: InboundMessage{
				SenderID:   "+15552223333",
				SenderName: "Alice",
				Content:    "hello there",
				ThreadID:   "+15552223333",
				Metadata: map[string]string{
					"timestamp":   "1720000000123",
					"group_id":    "",
					"source_uuid": "11111111-2222-3333-4444-555555555555",
				},
			},
		},
		{
			name: "group message threads on group id",
			params: `{"envelope":{
				"source":"+15552223333","sourceUuid":"11111111-2222-3333-4444-555555555555",
				"sourceName":"Alice","timestamp":42,
				"dataMessage":{"message":"hi group","groupInfo":{"groupId":"` + testGroupID + `"}}}}`,
			wantOK: true,
			want: InboundMessage{
				SenderID:   "+15552223333",
				SenderName: "Alice",
				Content:    "hi group",
				ThreadID:   testGroupID,
				Metadata: map[string]string{
					"timestamp":   "42",
					"group_id":    testGroupID,
					"source_uuid": "11111111-2222-3333-4444-555555555555",
				},
			},
		},
		{
			name: "missing sourceName falls back to source",
			params: `{"envelope":{
				"source":"+15552223333","timestamp":7,
				"dataMessage":{"message":"no name"}}}`,
			wantOK: true,
			want: InboundMessage{
				SenderID:   "+15552223333",
				SenderName: "+15552223333",
				Content:    "no name",
				ThreadID:   "+15552223333",
				Metadata: map[string]string{
					"timestamp":   "7",
					"group_id":    "",
					"source_uuid": "",
				},
			},
		},
		{
			name: "missing source falls back to sourceUuid",
			params: `{"envelope":{
				"sourceUuid":"11111111-2222-3333-4444-555555555555","timestamp":7,
				"dataMessage":{"message":"uuid only"}}}`,
			wantOK: true,
			want: InboundMessage{
				SenderID:   "11111111-2222-3333-4444-555555555555",
				SenderName: "11111111-2222-3333-4444-555555555555",
				Content:    "uuid only",
				ThreadID:   "11111111-2222-3333-4444-555555555555",
				Metadata: map[string]string{
					"timestamp":   "7",
					"group_id":    "",
					"source_uuid": "11111111-2222-3333-4444-555555555555",
				},
			},
		},
		{
			name: "no dataMessage (receipt) skipped",
			params: `{"envelope":{"source":"+15552223333","timestamp":9,
				"receiptMessage":{"when":9,"isDelivery":true}}}`,
			wantOK: false,
		},
		{
			name: "empty message text (typing indicator shape) skipped",
			params: `{"envelope":{"source":"+15552223333","timestamp":9,
				"dataMessage":{"message":""}}}`,
			wantOK: false,
		},
		{
			name:   "invalid json skipped",
			params: `{"envelope":`,
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := signalInboundToMessage([]byte(tt.params))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.SenderID != tt.want.SenderID {
				t.Errorf("SenderID = %q, want %q", got.SenderID, tt.want.SenderID)
			}
			if got.SenderName != tt.want.SenderName {
				t.Errorf("SenderName = %q, want %q", got.SenderName, tt.want.SenderName)
			}
			if got.Content != tt.want.Content {
				t.Errorf("Content = %q, want %q", got.Content, tt.want.Content)
			}
			if got.ThreadID != tt.want.ThreadID {
				t.Errorf("ThreadID = %q, want %q", got.ThreadID, tt.want.ThreadID)
			}
			for k, v := range tt.want.Metadata {
				if got.Metadata[k] != v {
					t.Errorf("Metadata[%q] = %q, want %q", k, got.Metadata[k], v)
				}
			}
		})
	}
}

func TestIsSignalGroupID(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{testGroupID, true},
		{"+15552223333", false},                         // phone number
		{"11111111-2222-3333-4444-555555555555", false}, // ACI uuid (dashes)
		{"", false},                                     // empty
		{"abc", false},                                  // too short
		{"not!valid@base64#with$symbols%andmorelength", false}, // bad charset
	}
	for _, tt := range tests {
		if got := isSignalGroupID(tt.in); got != tt.want {
			t.Errorf("isSignalGroupID(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestSignalRecipientParams(t *testing.T) {
	tests := []struct {
		name        string
		recipientID string
		threadID    string
		wantKey     string
		wantVal     string
	}{
		{"group thread routes to groupId", "+15552223333", testGroupID, "groupId", testGroupID},
		{"1:1 thread routes to recipient", "+15552223333", "+15552223333", "recipient", "+15552223333"},
		{"no thread, phone recipient", "+15552223333", "", "recipient", "+15552223333"},
		{"no thread, group recipient", testGroupID, "", "groupId", testGroupID},
		{"uuid recipient routes to recipient", "11111111-2222-3333-4444-555555555555", "", "recipient", "11111111-2222-3333-4444-555555555555"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := signalRecipientParams(tt.recipientID, tt.threadID)
			if tt.wantKey == "groupId" {
				got, ok := params["groupId"].(string)
				if !ok || got != tt.wantVal {
					t.Fatalf("params = %v, want groupId=%q", params, tt.wantVal)
				}
				if _, has := params["recipient"]; has {
					t.Errorf("params contain both groupId and recipient: %v", params)
				}
				return
			}
			got, ok := params["recipient"].([]string)
			if !ok || len(got) != 1 || got[0] != tt.wantVal {
				t.Fatalf("params = %v, want recipient=[%q]", params, tt.wantVal)
			}
			if _, has := params["groupId"]; has {
				t.Errorf("params contain both recipient and groupId: %v", params)
			}
		})
	}
}

// TestSignalStepSuppressed asserts the §4.7 rule: Kind=="step" outbound is
// suppressed entirely — nil error, nothing written — even when the adapter
// is not connected (a normal send in the same state errors).
func TestSignalStepSuppressed(t *testing.T) {
	a := NewSignalAdapter(msg.ChannelConfig{Number: "+15550001111", SidecarAddr: "/nonexistent.sock"})

	if err := a.Send(context.Background(), OutboundMessage{
		RecipientID: "+15552223333",
		Content:     "Running tests...",
		Kind:        OutboundKindStep,
	}); err != nil {
		t.Fatalf("step send should be suppressed with nil error, got: %v", err)
	}

	// Contrast: a normal message on a disconnected adapter must error,
	// proving the step path returned before any transport work.
	if err := a.Send(context.Background(), OutboundMessage{
		RecipientID: "+15552223333",
		Content:     "real message",
	}); err == nil {
		t.Fatal("normal send on disconnected adapter should error")
	}
}

// fakeSignalDaemon accepts one connection on ln and speaks just enough
// newline-delimited JSON-RPC to test the adapter: it answers each "send"
// request (with an error object when failSend is set), and after the first
// send it pushes a "receive" notification carrying pushParams.
func fakeSignalDaemon(t *testing.T, ln net.Listener, pushParams string, failSend bool, gotSends chan<- map[string]any) {
	t.Helper()
	conn, err := ln.Accept()
	if err != nil {
		return // listener closed during shutdown
	}
	defer conn.Close()

	dec := json.NewDecoder(conn)
	for {
		var req struct {
			JSONRPC string         `json:"jsonrpc"`
			Method  string         `json:"method"`
			Params  map[string]any `json:"params"`
			ID      string         `json:"id"`
		}
		if err := dec.Decode(&req); err != nil {
			return // connection closed
		}
		if req.Method != "send" {
			continue
		}
		select {
		case gotSends <- req.Params:
		default:
		}

		var resp string
		if failSend {
			resp = fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"error":{"code":-32602,"message":"Unregistered user"}}`, req.ID)
		} else {
			resp = fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"result":{"timestamp":1720000000123}}`, req.ID)
		}
		if _, err := conn.Write([]byte(resp + "\n")); err != nil {
			return
		}
		if pushParams != "" {
			// Compact to a single line — the daemon protocol is one frame
			// per line, and test fixtures are multi-line for readability.
			var buf bytes.Buffer
			raw := `{"jsonrpc":"2.0","method":"receive","params":` + pushParams + `}`
			if err := json.Compact(&buf, []byte(raw)); err != nil {
				t.Errorf("bad pushParams fixture: %v", err)
				return
			}
			buf.WriteByte('\n')
			if _, err := conn.Write(buf.Bytes()); err != nil {
				return
			}
			pushParams = "" // push only once
		}
	}
}

// startSignalTestPair spins up a fake daemon on a UNIX socket in a temp dir
// and a started adapter attached to it.
func startSignalTestPair(t *testing.T, pushParams string, failSend bool) (*SignalAdapter, chan map[string]any) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "sig.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	gotSends := make(chan map[string]any, 8)
	go fakeSignalDaemon(t, ln, pushParams, failSend, gotSends)

	a := NewSignalAdapter(msg.ChannelConfig{Number: "+15550001111", SidecarAddr: sock})
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { a.Stop() })
	return a, gotSends
}

// TestSignalRoundTrip drives a send RPC and an inbound receive notification
// through a fake daemon over a real UNIX socket — no signal-cli required.
func TestSignalRoundTrip(t *testing.T) {
	pushParams := `{"account":"+15550001111","envelope":{
		"source":"+15552223333","sourceUuid":"11111111-2222-3333-4444-555555555555",
		"sourceName":"Alice","timestamp":1720000000123,
		"dataMessage":{"message":"ping from phone"}}}`

	a, gotSends := startSignalTestPair(t, pushParams, false)

	if st := a.Status(); !st.Connected || st.Details == "" {
		t.Fatalf("Status after Start = %+v, want connected with details", st)
	}

	// Outbound: the daemon answers with a success result.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Send(ctx, OutboundMessage{
		RecipientID: "+15552223333",
		ThreadID:    "+15552223333",
		Content:     "hello signal",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// The fake daemon saw the correct RPC params.
	select {
	case params := <-gotSends:
		if params["account"] != "+15550001111" {
			t.Errorf("send account = %v, want +15550001111", params["account"])
		}
		if params["message"] != "hello signal" {
			t.Errorf("send message = %v, want %q", params["message"], "hello signal")
		}
		rec, ok := params["recipient"].([]any)
		if !ok || len(rec) != 1 || rec[0] != "+15552223333" {
			t.Errorf("send recipient = %v, want [+15552223333]", params["recipient"])
		}
		if _, has := params["groupId"]; has {
			t.Errorf("1:1 send must not carry groupId, params = %v", params)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake daemon never received the send RPC")
	}

	// Inbound: the pushed receive notification surfaces on InboundCh.
	select {
	case in := <-a.InboundCh():
		if in.SenderID != "+15552223333" || in.SenderName != "Alice" {
			t.Errorf("inbound sender = %q/%q, want +15552223333/Alice", in.SenderID, in.SenderName)
		}
		if in.Content != "ping from phone" {
			t.Errorf("inbound content = %q, want %q", in.Content, "ping from phone")
		}
		if in.ThreadID != "+15552223333" {
			t.Errorf("inbound thread = %q, want +15552223333", in.ThreadID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("receive notification never surfaced on InboundCh")
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st := a.Status(); st.Connected {
		t.Errorf("Status after Stop reports connected")
	}
}

// TestSignalSendRPCError asserts that a JSON-RPC error object from the
// daemon surfaces as a Send error.
func TestSignalSendRPCError(t *testing.T) {
	a, _ := startSignalTestPair(t, "", true)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := a.Send(ctx, OutboundMessage{RecipientID: "+15559998888", Content: "will fail"})
	if err == nil {
		t.Fatal("Send should surface the RPC error object")
	}
	if want := "Unregistered user"; !strings.Contains(err.Error(), want) {
		t.Errorf("Send error = %q, want it to contain %q", err.Error(), want)
	}
}

// TestSignalGroupSend asserts group-thread outbound routes via groupId.
func TestSignalGroupSend(t *testing.T) {
	a, gotSends := startSignalTestPair(t, "", false)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Send(ctx, OutboundMessage{
		RecipientID: testGroupID,
		ThreadID:    testGroupID,
		Content:     "hello group",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case params := <-gotSends:
		if params["groupId"] != testGroupID {
			t.Errorf("send groupId = %v, want %q", params["groupId"], testGroupID)
		}
		if _, has := params["recipient"]; has {
			t.Errorf("group send must not carry recipient, params = %v", params)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fake daemon never received the group send RPC")
	}
}
