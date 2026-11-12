// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

// security-lead P-1/P-2 (pre-read 11-08): the hosted runner sends API text to
// `rysh prompt --literal --stdin`: the text arrives exactly (no trailing
// newline dropped, never an argv) and no prefix of it ("##>") is read as a
// directive.

func TestParsePromptArgsStdinAndLiteral(t *testing.T) {
	in := "##> not a pipeline event\n\nsecond line\n\n"
	o, err := parsePromptArgs([]string{"prompt", "--session", "work", "--pane-id", "p1", "--timeout", "30s", "--json", "--literal", "--stdin"}, strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if o.Text != in || !o.Literal || o.PaneID != "p1" || o.Session != "work" || !o.JSON {
		t.Fatalf("opts = %+v", o)
	}
	m := promptMessage(o.Text, o.Literal)
	if !m.Literal || m.Prompt != in {
		t.Fatalf("message = %+v", m)
	}
	if promptMessage("x", false).Literal {
		t.Fatal("literal without --literal")
	}
	// Old form unchanged.
	o, err = parsePromptArgs([]string{"prompt", "--", "fix", "it"}, strings.NewReader("ignored"))
	if err != nil || o.Text != "fix it" || o.Literal {
		t.Fatalf("plain: %+v %v", o, err)
	}
	for _, args := range [][]string{
		{"prompt", "--stdin", "--pane-id", "p1", "--", "also text"}, // two sources
		{"prompt", "--literal", "--", "x"},                          // literal needs a pane id
	} {
		if _, err := parsePromptArgs(args, strings.NewReader("x")); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if _, err := parsePromptArgs([]string{"prompt", "--stdin", "--pane-id", "p1"}, strings.NewReader(" \n")); err == nil {
		t.Error("blank stdin accepted")
	}
}

// security-lead P-3: the runner sends '... --literal --stdin --'. This rysh
// reads stdin; an older rysh (no --literal/--stdin) finds no text after '--'
// and refuses, instead of prompting the words '--literal --stdin'.
func TestPromptStdinAcceptsATrailingDoubleDash(t *testing.T) {
	o, err := parsePromptArgs([]string{"prompt", "--pane-id", "p1", "--json", "--literal", "--stdin", "--"}, strings.NewReader("hello"))
	if err != nil || o.Text != "hello" || !o.Literal {
		t.Fatalf("opts=%+v err=%v", o, err)
	}
}
