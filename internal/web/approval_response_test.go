// SPDX-License-Identifier: Apache-2.0

package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"

	"github.com/nats-io/nats.go"
)

// The `approval_response` ws command is the seam between every non-TUI client
// and a blocked tool call (E-12b). A gated tool sits in the orchestrator's
// waitForApproval until a MsgApprovalResponse arrives on
// pane.<paneid>.approval.response; this command is the only thing that puts one
// there on behalf of the desktop UI, the browser UI and the phone.
//
// Until now nothing tested it. A renderer test that asserts a button calls
// sendCommand proves the phone SPEAKS; it proves nothing about anything
// listening. These tests cover the listening half: the exact subject, the tag
// the orchestrator's codec decodes, and the decision surviving the trip.

// newApprovalTestServer starts a server wired to an in-process bus and returns
// its port plus the raw NATS connection to subscribe on.
func newApprovalTestServer(t *testing.T) (int, *nats.Conn) {
	t.Helper()
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	port := freePort(t)
	s := NewServer(port, "approval-session", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/health", port))
	return port, nc
}

// TestApprovalResponseReachesTheWaitingTool: the decision lands on the subject
// waitForApproval subscribes to, tagged so its codec decodes it, with the
// request id and reason intact. Get the subject wrong and the phone's tap is
// answered by nobody — the tool blocks to its five-minute timeout and then
// counts as a rejection.
func TestApprovalResponseReachesTheWaitingTool(t *testing.T) {
	for _, decision := range []string{"yes", "yes_always", "no", "no_with_explanation", "choice_selected"} {
		t.Run(decision, func(t *testing.T) {
			port, nc := newApprovalTestServer(t)

			sub, err := nc.SubscribeSync(msg.T("pane", "pane-1", "approval", "response"))
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}

			conn := dialWS(t, port, "")
			sendWSCommand(t, conn, "approval_response", map[string]any{
				"pane_id":    "pane-1",
				"request_id": "req-7",
				"decision":   decision,
				"reason":     "because",
			})

			m, err := sub.NextMsg(5 * time.Second)
			if err != nil {
				t.Fatalf("nothing published to pane.pane-1.approval.response: %v", err)
			}
			var env msg.NATSEnvelope
			if err := json.Unmarshal(m.Data, &env); err != nil {
				t.Fatalf("decode envelope: %v", err)
			}
			if env.TypeTag != msg.TagApprovalResponse {
				t.Fatalf("published tag %q, want %q", env.TypeTag, msg.TagApprovalResponse)
			}
			var resp msg.MsgApprovalResponse
			if err := json.Unmarshal(env.Payload, &resp); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			if string(resp.Decision) != decision {
				t.Errorf("decision = %q, want %q", resp.Decision, decision)
			}
			if resp.RequestID != "req-7" {
				t.Errorf("request_id = %q, want req-7", resp.RequestID)
			}
			if resp.Reason != "because" {
				t.Errorf("reason = %q, want %q", resp.Reason, "because")
			}
		})
	}
}

// TestApprovalResponseFailuresAreVisible — the fail-visible rule this package
// already applies to webpane_input (TestWebPaneInputFailuresAreVisible).
//
// A rejected approval_response used to be dropped in silence: the whole handler
// hung off `if json.Unmarshal(...) == nil && cmd.PaneID != ""` with no else. On
// a phone that is indistinguishable from a slow one — the dialog closes, the
// user believes they answered, and the tool blocks until it times out. Worse
// for an unknown decision string, which the pane echo counted as "approved"
// while the orchestrator's switch did not approve it.
func TestApprovalResponseFailuresAreVisible(t *testing.T) {
	cases := []struct {
		name    string
		params  map[string]any
		wantErr string
	}{
		{
			name:    "no pane",
			params:  map[string]any{"request_id": "req-7", "decision": "yes"},
			wantErr: "pane_id",
		},
		{
			name:    "unknown decision",
			params:  map[string]any{"pane_id": "pane-1", "request_id": "req-7", "decision": "approve"},
			wantErr: "decision",
		},
		{
			name:    "empty decision",
			params:  map[string]any{"pane_id": "pane-1", "request_id": "req-7"},
			wantErr: "decision",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			port, nc := newApprovalTestServer(t)
			sub, err := nc.SubscribeSync(msg.T("pane", "pane-1", "approval", "response"))
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}

			conn := dialWS(t, port, "")
			sendWSCommand(t, conn, "approval_response", tc.params)

			data := readWSType(t, conn, "approval_error", 5*time.Second)
			if e, _ := data["error"].(string); !strings.Contains(e, tc.wantErr) {
				t.Errorf("approval_error = %q, want it to mention %q", e, tc.wantErr)
			}
			// And nothing reached the orchestrator: half-answering is worse
			// than not answering, because the tool would proceed on it.
			if m, err := sub.NextMsg(300 * time.Millisecond); err == nil {
				t.Errorf("a rejected approval_response still published: %s", m.Data)
			}
		})
	}
}
