// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// fakePaneBus answers the snapshot probe and records every publish.
type fakePaneBus struct {
	program  string
	probeErr error
	sent     []interface{}
	subjects []string
}

func (f *fakePaneBus) RequestFromSubject(subject string, m interface{}, _ time.Duration) (interface{}, error) {
	if f.probeErr != nil {
		return nil, f.probeErr
	}
	return &msg.MsgPaneSnapshotReply{Snapshot: domain.PaneSnapshot{Program: f.program}}, nil
}

func (f *fakePaneBus) SendToSubject(subject string, m interface{}) error {
	f.subjects = append(f.subjects, subject)
	f.sent = append(f.sent, m)
	return nil
}

// TestSendToPaneRefusesAPaneThatDoesNotAnswer: the pane inbox is
// fire-and-forget, so a wrong or short pane id used to print "sent". The probe
// makes it an error, and nothing is published.
func TestSendToPaneRefusesAPaneThatDoesNotAnswer(t *testing.T) {
	bus := &fakePaneBus{probeErr: errors.New("nats: timeout")}
	err := sendToPane(bus, "eb123825", "hello", "prompt")
	if err == nil {
		t.Fatal("no error for a pane that did not answer")
	}
	if !strings.Contains(err.Error(), "nothing was sent") {
		t.Errorf("error does not say nothing was sent: %v", err)
	}
	if len(bus.sent) != 0 {
		t.Errorf("published %d message(s) to a pane that did not answer", len(bus.sent))
	}
}

// TestSendToPaneRefusesAPromptToAProgramPane: --mode prompt is a
// MsgPaneExecPrompt, which drives rysh's own LLM. A pane running claude never
// sees it (F-25), so it is refused with the command that does reach claude.
func TestSendToPaneRefusesAPromptToAProgramPane(t *testing.T) {
	bus := &fakePaneBus{program: "claude"}
	err := sendToPane(bus, "pane-a", "do the thing", "prompt")
	if err == nil {
		t.Fatal("no error for --mode prompt to a pane running claude")
	}
	for _, want := range []string{"claude", "nothing was sent", "rysh ansa send pane-a"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if len(bus.sent) != 0 {
		t.Errorf("published %d message(s) that claude would never see", len(bus.sent))
	}
}

// TestSendToPaneDeliversToRyshPanes: a pane with no foreground program is
// rysh's own; prompt and shell go to its inbox exactly as before.
func TestSendToPaneDeliversToRyshPanes(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want interface{}
	}{
		{"prompt", &msg.MsgPaneExecPrompt{Prompt: "x"}},
		{"shell", &msg.MsgPaneExecShell{Command: "x"}},
		{"", &msg.MsgPaneExecShell{Command: "x"}},
	} {
		bus := &fakePaneBus{}
		if err := sendToPane(bus, "pane-a", "x", tc.mode); err != nil {
			t.Fatalf("mode %q: %v", tc.mode, err)
		}
		if len(bus.sent) != 1 || bus.subjects[0] != msg.T("pane", "pane-a", "inbox") {
			t.Fatalf("mode %q: sent %v to %v", tc.mode, bus.sent, bus.subjects)
		}
		switch w := tc.want.(type) {
		case *msg.MsgPaneExecPrompt:
			g, ok := bus.sent[0].(*msg.MsgPaneExecPrompt)
			if !ok || g.Prompt != w.Prompt {
				t.Errorf("mode %q: got %#v", tc.mode, bus.sent[0])
			}
		case *msg.MsgPaneExecShell:
			g, ok := bus.sent[0].(*msg.MsgPaneExecShell)
			if !ok || g.Command != w.Command {
				t.Errorf("mode %q: got %#v", tc.mode, bus.sent[0])
			}
		}
	}
}

// TestSendToPaneKeepsShellToAProgramPane: shell mode to a program pane is not
// what this fix is about; it is published as before.
func TestSendToPaneKeepsShellToAProgramPane(t *testing.T) {
	bus := &fakePaneBus{program: "claude"}
	if err := sendToPane(bus, "pane-a", "ls", "shell"); err != nil {
		t.Fatal(err)
	}
	if len(bus.sent) != 1 {
		t.Fatalf("sent %d, want 1", len(bus.sent))
	}
}
