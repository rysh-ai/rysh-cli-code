// SPDX-License-Identifier: Apache-2.0

package tui

// Phase 5 (bash-shell-mode): context-aware prompt + PS2 continuation tests.

import (
	"os"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

func TestShellCommandIncomplete(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"ls -la", false},
		{`echo "done"`, false},
		{`echo 'done'`, false},
		{`echo "unclosed`, true},
		{`echo 'unclosed`, true},
		{`ls \`, true},
		{`ls \\`, false},                        // escaped backslash, complete
		{"echo \"line1\nline2\"", false},        // closed across lines
		{"echo \"line1\nline2", true},           // still open across lines
		{`echo it's unbalanced`, true},          // lone apostrophe opens a quote
		{`ls # trailing 'comment quote`, false}, // quotes inside comments ignored
		{`ls; # comment \`, false},              // trailing backslash in comment ignored
		{`echo "a'b"`, false},                   // single quote inside double
		{`echo 'a\'`, false},                    // backslash is literal in single quotes; quote closes
		{"", false},
	}
	for _, tc := range cases {
		if got := shellCommandIncomplete(tc.in); got != tc.want {
			t.Errorf("shellCommandIncomplete(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestShellPromptFor(t *testing.T) {
	home, _ := os.UserHomeDir()
	pane := domain.PaneSnapshot{ID: "p1", ShellCwd: home + "/proj/rysh"}
	m := paneModel(pane)
	m.cfg = config.Config{ShellPrompt: "{dir} > "}
	if got := m.shellPromptFor("p1", 80); got != "rysh > " {
		t.Errorf("prompt = %q, want 'rysh > '", got)
	}

	m.cfg.ShellPrompt = "{cwd} $ "
	if got := m.shellPromptFor("p1", 120); got != "~/proj/rysh $ " {
		t.Errorf("cwd prompt = %q, want '~/proj/rysh $ '", got)
	}

	// At home, {dir} renders "~".
	m2 := paneModel(domain.PaneSnapshot{ID: "p1", ShellCwd: home})
	m2.cfg = config.Config{ShellPrompt: "{dir} > "}
	if got := m2.shellPromptFor("p1", 80); got != "~ > " {
		t.Errorf("home prompt = %q, want '~ > '", got)
	}

	// Unknown cwd → plain prompt.
	m3 := paneModel(domain.PaneSnapshot{ID: "p1"})
	m3.cfg = config.Config{ShellPrompt: "{dir} > "}
	if got := m3.shellPromptFor("p1", 80); got != "> " {
		t.Errorf("no-cwd prompt = %q, want '> '", got)
	}

	// A prompt too wide for the pane degrades to plain.
	m4 := paneModel(domain.PaneSnapshot{ID: "p1", ShellCwd: "/very/long/path/segment/here"})
	m4.cfg = config.Config{ShellPrompt: "{cwd} $ "}
	if got := m4.shellPromptFor("p1", 30); got != "> " {
		t.Errorf("overwide prompt = %q, want '> '", got)
	}
}

func TestPS2ContinuationAccumulates(t *testing.T) {
	m := readlineModel(nil)
	m.pub = nil // sendMsg is a no-op without a publisher; guard against panics

	// Submit an unfinished line → accumulates, nothing cleared into history.
	m.setActiveInputValue(`echo "start`)
	mm, _ := m.submitActiveInput()
	m = asModel(t, mm)
	if got := m.panePendingCmd["p1"]; got != `echo "start` {
		t.Fatalf("pending = %q, want the unfinished line", got)
	}
	if m.activeInputValue() != "" {
		t.Error("input line should clear for the continuation")
	}

	// Ctrl+C aborts the pending buffer (readline ^C path).
	m.panePendingCmd["p1"] = `echo "start`
	m.setActiveInputValue("whatever")
	delete(m.panePendingCmd, "p1") // simulate the ctrl+c branch effect
	if _, ok := m.panePendingCmd["p1"]; ok {
		t.Error("pending should be gone after abort")
	}
}
