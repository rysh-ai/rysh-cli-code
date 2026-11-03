// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// MsgCLIPaneProfile — `rysh family apply`'s profile door (design 093 H2): the
// same writes as `##pane profile set|clear`, with the prompt as text.

func cliProfile(t *testing.T, w *WorkspaceActor, nc *nats.Conn, m *msg.MsgCLIPaneProfile) (*msg.MsgCLIPaneProfileReply, []string) {
	t.Helper()
	rec := recordSubject(t, nc, msg.T("pane", "*", "inbox"))
	r := w.handleCLIPaneProfile(m)
	settle(t, nc)
	subjects, payloads := rec.seen()
	var writes []string
	for i, subj := range subjects {
		var sm msg.MsgPaneSetMeta
		if json.Unmarshal(payloads[i], &sm) != nil || sm.Key == "" {
			continue
		}
		writes = append(writes, paneIDFromInbox(subj)+":"+sm.Key+"="+sm.Value)
	}
	return r, writes
}

func TestCLIPaneProfile_SetWritesTheTextToTheExactPath(t *testing.T) {
	w, nc, base := newProfileTestWorkspace(t)
	r, writes := cliProfile(t, w, nc, &msg.MsgCLIPaneProfile{Op: "set", PaneID: "pP1",
		Tools: []string{"family_ask", "family_reply", "family_status"}, Prompt: "You are Ada.\n## not a command"})
	if !r.OK {
		t.Fatalf("set refused: %s", r.Error)
	}
	stored := filepath.Join(base, ".rysh", domain.PaneProfileDirName, "pP1.md")
	want := []string{
		"pP1:" + domain.PaneMetaAgentPromptFile + "=" + stored,
		"pP1:" + domain.PaneMetaAgentTools + "=family_ask,family_reply,family_status",
	}
	if !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes = %v\nwant %v", writes, want)
	}
	b, err := os.ReadFile(stored)
	if err != nil || string(b) != "You are Ada.\n## not a command" {
		t.Fatalf("stored = %q, %v", b, err)
	}
	if fi, _ := os.Stat(stored); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
}

func TestCLIPaneProfile_GetReadsThroughThePaneCheck(t *testing.T) {
	w, nc, _ := newProfileTestWorkspace(t)
	r, writes := cliProfile(t, w, nc, &msg.MsgCLIPaneProfile{Op: "get", PaneID: "pP2"})
	if !r.OK || !r.HasPrompt || r.Prompt != "You are Bo.\nBe brief." || !reflect.DeepEqual(r.Tools, []string{"family_ask", "family_reply"}) {
		t.Fatalf("get = %+v", r)
	}
	if len(writes) != 0 {
		t.Errorf("get wrote %v", writes)
	}
	r, _ = cliProfile(t, w, nc, &msg.MsgCLIPaneProfile{Op: "get", PaneID: "pP1"})
	if !r.OK || r.HasPrompt || len(r.Tools) != 0 {
		t.Errorf("get on an unprofiled pane = %+v", r)
	}
}

func TestCLIPaneProfile_GetReportsASymlinkedPrompt(t *testing.T) {
	w, nc, base := newProfileTestWorkspace(t)
	stored := filepath.Join(base, ".rysh", domain.PaneProfileDirName, "pP2.md")
	other := filepath.Join(base, "elsewhere.md")
	if err := os.WriteFile(other, []byte("evil"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(stored); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, stored); err != nil {
		t.Fatal(err)
	}
	r, _ := cliProfile(t, w, nc, &msg.MsgCLIPaneProfile{Op: "get", PaneID: "pP2"})
	if !r.OK || !r.HasPrompt || r.Prompt != "" || !strings.Contains(r.PromptErr, "symlink") {
		t.Fatalf("get = %+v", r)
	}
}

func TestCLIPaneProfile_Clear(t *testing.T) {
	w, nc, base := newProfileTestWorkspace(t)
	r, writes := cliProfile(t, w, nc, &msg.MsgCLIPaneProfile{Op: "clear", PaneID: "pP2"})
	if !r.OK {
		t.Fatal(r.Error)
	}
	want := []string{"pP2:" + domain.PaneMetaAgentTools + "=", "pP2:" + domain.PaneMetaAgentPromptFile + "="}
	if !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes = %v", writes)
	}
	if _, err := os.Stat(filepath.Join(base, ".rysh", domain.PaneProfileDirName, "pP2.md")); !os.IsNotExist(err) {
		t.Errorf("prompt file kept: %v", err)
	}
}

func TestCLIPaneProfile_Refusals(t *testing.T) {
	w, nc, _ := newProfileTestWorkspace(t)
	big := strings.Repeat("p", maxProfilePromptBytes+1)
	cases := []struct {
		m    msg.MsgCLIPaneProfile
		want string
	}{
		{msg.MsgCLIPaneProfile{Op: "set", PaneID: "ada", Tools: []string{"family_ask"}, Prompt: "x"}, "pane not found"}, // a name, not an id
		{msg.MsgCLIPaneProfile{Op: "get", PaneID: ""}, "pane not found"},
		{msg.MsgCLIPaneProfile{Op: "set", PaneID: "pP1", Prompt: "x"}, "tool allow-list"},
		{msg.MsgCLIPaneProfile{Op: "set", PaneID: "pP1", Tools: []string{"bash;rm"}, Prompt: "x"}, "invalid tool name"},
		{msg.MsgCLIPaneProfile{Op: "set", PaneID: "pP1", Tools: []string{"family_ask"}, Prompt: " \n"}, "needs a prompt"},
		{msg.MsgCLIPaneProfile{Op: "set", PaneID: "pP1", Tools: []string{"family_ask"}, Prompt: big}, "larger than"},
		{msg.MsgCLIPaneProfile{Op: "wibble", PaneID: "pP1"}, "unknown op"},
	}
	for _, c := range cases {
		m := c.m
		r, writes := cliProfile(t, w, nc, &m)
		if r.OK || !strings.Contains(r.Error, c.want) {
			t.Errorf("%+v: reply %+v, want error with %q", c.m.Op, r, c.want)
		}
		if len(writes) != 0 {
			t.Errorf("%+v: refused but wrote %v", c.m.Op, writes)
		}
	}
}
