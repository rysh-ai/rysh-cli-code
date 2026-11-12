// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Steer (boat gap #6): add a message to a pane's running turn without
// stopping it. rysh's own agent gets a queued, literal prompt (it waits
// behind the run in flight); a harness (claude, codex) gets the text typed
// in, which the harness takes as a mid-turn message.

func TestSteerQueuesALiteralPromptForRyshsAgent(t *testing.T) {
	bus := &fakePaneBus{}
	typed := 0
	how, err := steerPane(bus, func(string, string) error { typed++; return nil }, "pane-a", "also update the docs")
	if err != nil {
		t.Fatal(err)
	}
	if typed != 0 || len(bus.sent) != 1 || bus.subjects[0] != msg.T("pane", "pane-a", "inbox") {
		t.Fatalf("typed=%d sent=%v subjects=%v", typed, bus.sent, bus.subjects)
	}
	p, ok := bus.sent[0].(*msg.MsgPaneExecPrompt)
	if !ok || p.Prompt != "also update the docs" || !p.Queue || !p.Literal {
		t.Fatalf("message = %#v, want a Queue + Literal prompt", bus.sent[0])
	}
	if !strings.Contains(how, "queued") {
		t.Fatalf("how = %q", how)
	}
}

func TestSteerTypesIntoAHarnessPane(t *testing.T) {
	bus := &fakePaneBus{program: "claude"}
	var gotPane, gotText string
	how, err := steerPane(bus, func(p, s string) error { gotPane, gotText = p, s; return nil }, "pane-b", "use the v2 API")
	if err != nil {
		t.Fatal(err)
	}
	if gotPane != "pane-b" || gotText != "use the v2 API" || len(bus.sent) != 0 {
		t.Fatalf("typed %q/%q, published %v", gotPane, gotText, bus.sent)
	}
	if !strings.Contains(how, "claude") {
		t.Fatalf("how = %q", how)
	}
}

func TestSteerSendsNothingOnAnError(t *testing.T) {
	bus := &fakePaneBus{probeErr: errors.New("nats: timeout")}
	if _, err := steerPane(bus, func(string, string) error { t.Fatal("typed"); return nil }, "x", "hi"); err == nil || !strings.Contains(err.Error(), "nothing was sent") {
		t.Fatalf("err = %v", err)
	}
	if len(bus.sent) != 0 {
		t.Fatal("published to a pane that did not answer")
	}
	if _, err := steerPane(&fakePaneBus{}, nil, "x", "   "); err == nil {
		t.Fatal("empty text accepted")
	}
	bad := &fakePaneBus{program: "codex"}
	if _, err := steerPane(bad, func(string, string) error { return errors.New("refused") }, "x", "hi"); err == nil {
		t.Fatal("a failed typed delivery was not an error")
	}
}
