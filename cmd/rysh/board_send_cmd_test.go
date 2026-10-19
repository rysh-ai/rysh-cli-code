// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
)

func TestParseBoardSendFlagsAnywhere(t *testing.T) {
	b, err := parseBoardSendArgs("send", []string{"--id", "msg-0042", "--to", "wkr-01", "--board", "alpha",
		"--thread", "p/1", "--as", "ceo", "--", "build", "the", "parser"})
	if err != nil {
		t.Fatal(err)
	}
	if b.to != "wkr-01" || b.cmdID != "msg-0042" || b.board != "alpha" || b.threadID != "p/1" ||
		b.as != "ceo" || b.text != "build the parser" {
		t.Errorf("parsed %+v", b)
	}
}

func TestParseBoardSendRefusals(t *testing.T) {
	cases := []struct {
		sub  string
		args []string
		want string
	}{
		{"send", []string{"--", "no recipient"}, "--to"},
		{"send", []string{"--to", "w"}, "message"},
		{"send", []string{"--to", "w", "stray", "--", "x"}, "after --"},
		{"send", []string{"--to", "w", "--id", "a b", "--", "x"}, "invalid command id"},
		{"ack", []string{}, "exactly one"},
		{"ack", []string{"c1", "c2"}, "exactly one"},
		{"ack", []string{"bad id"}, "invalid command id"},
		{"inbox", []string{"stray"}, "no positional"},
	}
	for _, tc := range cases {
		if _, err := parseBoardSendArgs(tc.sub, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %v: err = %v, want %q", tc.sub, tc.args, err, tc.want)
		}
	}
}

func TestParseBoardSendFileOnlyAndAckNote(t *testing.T) {
	if b, err := parseBoardSendArgs("send", []string{"--to", "w", "--file", "/tmp/x.md"}); err != nil || b.file != "/tmp/x.md" {
		t.Errorf("--file alone should be a valid send: %+v %v", b, err)
	}
	b, err := parseBoardSendArgs("ack", []string{"--note", "on it", "msg-0042"})
	if err != nil || b.cmdID != "msg-0042" || b.text != "on it" {
		t.Errorf("ack parsed %+v %v", b, err)
	}
	b, err = parseBoardSendArgs("inbox", []string{"--all", "--json"})
	if err != nil || !b.all || !b.jsonOut {
		t.Errorf("inbox parsed %+v %v", b, err)
	}
}

// A status post used to swallow --to without a word; it now says where the
// delivering verb is (design 050).
func TestBoardPostRefusesTo(t *testing.T) {
	_, err := parseBoardArgs([]string{"post", "--to", "wkr-01", "--", "hello"})
	if err == nil || !strings.Contains(err.Error(), "board send") {
		t.Fatalf("post --to: err = %v, want a pointer to `board send`", err)
	}
}

func TestCheckSendFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "order.md")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := checkSendFile(f); err != nil || got != f {
		t.Errorf("checkSendFile(%q) = %q, %v", f, got, err)
	}
	for _, bad := range []string{filepath.Join(dir, "missing.md"), dir, "/dev/null"} {
		if _, err := checkSendFile(bad); err == nil {
			t.Errorf("checkSendFile(%q) accepted", bad)
		}
	}
}

func TestRenderInbox(t *testing.T) {
	empty := renderInbox("alpha", nil, false)
	if !strings.Contains(empty, "the recorder answered") || !strings.Contains(empty, "--all") {
		t.Errorf("empty inbox must say the recorder answered and point at --all: %q", empty)
	}
	out := renderInbox("alpha", []board.InboxItem{{CmdID: "msg-1", From: "ceo", Text: "do it", ThreadID: "p/msg-1",
		File: "/tmp/o.md", Answered: true, AckTS: 1}}, true)
	for _, want := range []string{"msg-1", "from ceo", "do it", "file: /tmp/o.md", "thread: p/msg-1", "answered"} {
		if !strings.Contains(out, want) {
			t.Errorf("inbox output missing %q:\n%s", want, out)
		}
	}
}

func TestParseBoardListenArgs(t *testing.T) {
	a, err := parseBoardListenArgs([]string{"--once", "--as", "p1", "--fleet", "Alpha", "--state-dir", "/x"})
	if err != nil || a.as != "p1" || a.board != "alpha" || a.stateDir != "/x" || !a.once {
		t.Errorf("parsed %+v, %v", a, err)
	}
	if _, err := parseBoardListenArgs([]string{"stray"}); err == nil {
		t.Error("a positional argument must be refused")
	}
}
