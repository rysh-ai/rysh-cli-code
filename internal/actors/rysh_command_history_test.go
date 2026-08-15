// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// startInProcessNATS spins up an in-process (no TCP socket) NATS server and
// returns a connected client. The server and connection are torn down via
// t.Cleanup.
func startInProcessNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := &natsserver.Options{
		Host:       "127.0.0.1",
		Port:       -1, // irrelevant with DontListen
		NoLog:      true,
		NoSigs:     true,
		DontListen: true, // in-process only
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("nats server not ready")
	}
	t.Cleanup(ns.Shutdown)

	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		ns.Shutdown()
		t.Fatalf("connect in-process nats: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// capturedMsg records a single decoded conversation message and the subject it
// was published on.
type capturedMsg struct {
	subject string
	content string
}

// collectPaneMessages drains the subscription for up to ~1s, decoding every
// MsgConversationAppend / MsgConversationHistoryAppend it sees.
func collectPaneMessages(t *testing.T, sub *nats.Subscription, codecs *msg.CodecRegistry) []capturedMsg {
	t.Helper()
	var caps []capturedMsg
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		m, err := sub.NextMsg(150 * time.Millisecond)
		if err != nil {
			break
		}
		var env msg.NATSEnvelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			t.Fatalf("unmarshal envelope on %s: %v", m.Subject, err)
		}
		decoded, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			continue
		}
		var content string
		switch dm := decoded.(type) {
		case *msg.MsgConversationAppend:
			if dm.Message != nil {
				content = dm.Message.Content
			}
		case *msg.MsgConversationHistoryAppend:
			if dm.Message != nil {
				content = dm.Message.Content
			}
		default:
			continue
		}
		caps = append(caps, capturedMsg{subject: m.Subject, content: content})
	}
	return caps
}

// TestRyshCommandAppearsInShellAndRyshModes verifies that a `##` system command
// entered while in shell mode is:
//  1. recorded in the shell command history (so it is recallable in shell mode),
//  2. recorded in the rysh command history,
//  3. published to the merged/shell output buffer (visible in shell mode), and
//  4. mirrored into the rysh output buffer (visible after switching to rysh mode).
func TestRyshCommandAppearsInShellAndRyshModes(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	const paneID = "test-pane"

	sub, err := nc.SubscribeSync(msg.T("pane", paneID, ">"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}

	w := &WorkspaceActor{
		pub:          pub,
		activePaneID: paneID,
		tabs:         []*tabInfo{{id: "tab-1", title: "tab"}},
		activeTabIdx: 0,
	}

	// Type a ## system command while in shell mode. ctx is unused on the
	// ##help path, so nil is safe here.
	w.handleSubmitInput(nil, &msg.MsgSubmitInput{Text: "##help", Mode: "shell"})
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush after submit: %v", err)
	}

	caps := collectPaneMessages(t, sub, codecs)

	// hasOn returns true if any captured message on subject contains want.
	hasOn := func(subject, want string) bool {
		for _, c := range caps {
			if c.subject == subject && strings.Contains(c.content, want) {
				return true
			}
		}
		return false
	}

	shellHistory := msg.T("pane", paneID, "history", "shell")
	ryshHistory := msg.T("pane", paneID, "history", "rysh")
	mergedOutput := msg.T("pane", paneID, "output")
	ryshOutput := msg.T("pane", paneID, "output", "rysh")

	if !hasOn(shellHistory, "##help") {
		t.Errorf("expected ##help recorded in shell history (%s); captured=%+v", shellHistory, caps)
	}
	if !hasOn(ryshHistory, "##help") {
		t.Errorf("expected ##help recorded in rysh history (%s); captured=%+v", ryshHistory, caps)
	}
	if !hasOn(mergedOutput, "available ## commands:") {
		t.Errorf("expected ##help result in merged/shell output (%s); captured=%+v", mergedOutput, caps)
	}
	if !hasOn(ryshOutput, "available ## commands:") {
		t.Errorf("expected ##help result mirrored to rysh output (%s); captured=%+v", ryshOutput, caps)
	}
	// The executed command line itself must be echoed onto both screens.
	if !hasOn(mergedOutput, "##help") {
		t.Errorf("expected ##help command echoed in merged/shell output (%s); captured=%+v", mergedOutput, caps)
	}
	if !hasOn(ryshOutput, "##help") {
		t.Errorf("expected ##help command echoed in rysh output (%s); captured=%+v", ryshOutput, caps)
	}
}

// TestRyshModeImplicitCommandStaysInRyshBuffer verifies that a command typed
// while already in rysh mode (no explicit ## prefix) keeps its prior behavior:
// the result goes to the rysh output buffer only, and is NOT mirrored to the
// merged/shell output buffer.
func TestRyshModeImplicitCommandStaysInRyshBuffer(t *testing.T) {
	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)

	const paneID = "test-pane-2"

	sub, err := nc.SubscribeSync(msg.T("pane", paneID, ">"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}

	w := &WorkspaceActor{
		pub:          pub,
		activePaneID: paneID,
		tabs:         []*tabInfo{{id: "tab-1", title: "tab"}},
		activeTabIdx: 0,
	}

	// In rysh mode, "help" is an implicit ## command.
	w.handleSubmitInput(nil, &msg.MsgSubmitInput{Text: "help", Mode: "rysh"})
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush after submit: %v", err)
	}

	caps := collectPaneMessages(t, sub, codecs)

	ryshOutput := msg.T("pane", paneID, "output", "rysh")
	mergedOutput := msg.T("pane", paneID, "output")

	sawRysh := false
	sawMerged := false
	for _, c := range caps {
		if c.subject == ryshOutput && strings.Contains(c.content, "available ## commands:") {
			sawRysh = true
		}
		if c.subject == mergedOutput && strings.Contains(c.content, "available ## commands:") {
			sawMerged = true
		}
	}
	if !sawRysh {
		t.Errorf("expected implicit rysh-mode command result in rysh output (%s); captured=%+v", ryshOutput, caps)
	}
	if sawMerged {
		t.Errorf("did not expect implicit rysh-mode command result in merged output (%s); captured=%+v", mergedOutput, caps)
	}
}

// TestRyshCommandRecallableInTypedMode verifies that an explicit `##` command
// entered from prompt or chat mode is recorded in that mode's history (so the
// up-arrow recalls it there), in addition to the always-on rysh history. This
// is the fix for: ## commands typed in ai/chat mode were only recorded in
// shell+rysh history and so were not recallable via up-arrow in those modes.
func TestRyshCommandRecallableInTypedMode(t *testing.T) {
	cases := []struct {
		name        string
		mode        string
		wantHistory string // per-mode history topic suffix the command must appear on
	}{
		{name: "prompt", mode: "prompt", wantHistory: "ai"},
		{name: "chat", mode: "chat", wantHistory: "chat"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := startInProcessNATS(t)
			codecs := msg.DefaultCodecRegistry()
			pub := msg.NewNATSPublisher(nc, codecs)

			const paneID = "test-pane-mode"

			sub, err := nc.SubscribeSync(msg.T("pane", paneID, ">"))
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			if err := nc.Flush(); err != nil {
				t.Fatalf("flush subscription: %v", err)
			}

			w := &WorkspaceActor{
				pub:          pub,
				activePaneID: paneID,
				tabs:         []*tabInfo{{id: "tab-1", title: "tab"}},
				activeTabIdx: 0,
			}

			// Type a ## system command while in the given mode. ctx is unused on
			// the ##help path, so nil is safe here.
			w.handleSubmitInput(nil, &msg.MsgSubmitInput{Text: "##help", Mode: tc.mode})
			if err := nc.Flush(); err != nil {
				t.Fatalf("flush after submit: %v", err)
			}

			caps := collectPaneMessages(t, sub, codecs)

			hasOn := func(subject, want string) bool {
				for _, c := range caps {
					if c.subject == subject && strings.Contains(c.content, want) {
						return true
					}
				}
				return false
			}

			modeHistory := msg.T("pane", paneID, "history", tc.wantHistory)
			ryshHistory := msg.T("pane", paneID, "history", "rysh")

			if !hasOn(modeHistory, "##help") {
				t.Errorf("expected ##help recorded in %s-mode history (%s); captured=%+v", tc.mode, modeHistory, caps)
			}
			if !hasOn(ryshHistory, "##help") {
				t.Errorf("expected ##help recorded in rysh history (%s); captured=%+v", ryshHistory, caps)
			}
		})
	}
}
