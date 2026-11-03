// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
)

// Screen fixtures are synthetic: written for this test from the public shape of
// each CLI's prompt, never copied from a real session (a real screen can hold
// anything, secrets included).

func screen(lines ...string) []string {
	rows := append([]string{}, lines...)
	// A pane is taller than its content: trailing blank rows are normal.
	return append(rows, "", "", "")
}

func filler(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "ok  \trysh/internal/pkg" + strings.Repeat("x", i%7) + "\t0.12s"
	}
	return out
}

func TestDetectAgentPromptPositives(t *testing.T) {
	cases := map[string][]string{
		"claude permission, full width": screen(
			"╭──────────────────────────────────────────────────────╮",
			"│ Bash command                                         │",
			"│                                                      │",
			"│   go test ./internal/actors/...                      │",
			"│   Run the actor tests                                │",
			"│                                                      │",
			"│ Do you want to proceed?                              │",
			"│ ❯ 1. Yes                                             │",
			"│   2. Yes, and don't ask again for go test commands   │",
			"│   3. No, and tell Claude what to do differently (esc)│",
			"╰──────────────────────────────────────────────────────╯",
		),
		"claude edit permission, 48 columns": screen(
			"╭──────────────────────────────────────────────╮",
			"│ Edit file                                    │",
			"│ internal/tui/model_view.go                   │",
			"│ Do you want to make this edit to model_view.g│",
			"│ ❯ 1. Yes                                     │",
			"│   2. Yes, allow all edits during this session│",
			"│   3. No, and tell Claude what to do different│",
			"╰──────────────────────────────────────────────╯",
		),
		"claude question menu": screen(
			" ☐ Approach",
			"",
			" Which library should we use?",
			"",
			" ❯ 1. Option A",
			"   2. Option B",
			"   3. Type something.",
			"",
			" Enter to select · ↑/↓ to navigate · Esc to cancel",
		),
		"codex command approval": screen(
			"  Would you like to run the following command?",
			"",
			"  $ make build",
			"",
			"› 1. Yes, proceed (y)",
			"  2. Yes, and don't ask again for this command (a)",
			"  3. No, and tell Codex what to do differently (esc)",
			"",
			"  Press enter to confirm or esc to cancel",
		),
		"gemini shell confirm": screen(
			"╭──────────────────────────────────╮",
			"│ ?  Shell make build              │",
			"│                                  │",
			"│ Allow execution of: 'make'?      │",
			"│                                  │",
			"│ ● 1. Yes, allow once             │",
			"│   2. Yes, allow always ...       │",
			"│   3. No, suggest changes (esc)   │",
			"╰──────────────────────────────────╯",
		),
		"gemini edit confirm": screen(
			"│ Apply this change?               │",
			"│ ● 1. Yes, allow once             │",
			"│   2. No, suggest changes (esc)   │",
		),
	}
	for name, rows := range cases {
		if !detectAgentPrompt(rows) {
			t.Errorf("%s: not detected", name)
		}
	}
}

func TestDetectAgentPromptNegatives(t *testing.T) {
	prompt := []string{
		"│ Do you want to proceed?                              │",
		"│ ❯ 1. Yes                                             │",
		"│   3. No, and tell Claude what to do differently (esc)│",
	}
	cases := map[string][]string{
		"empty":                     nil,
		"only blank rows":           screen("", ""),
		"prompt long scrolled away": screen(append(append([]string{}, prompt...), filler(20)...)...),
		"still working": screen(append(append([]string{}, prompt...),
			"✻ Thinking… (12s · esc to interrupt)")...),
		"question in code, no options": screen(
			"func ask() {",
			`	fmt.Println("Do you want to proceed?")`,
			"}",
		),
		"numbered list, no question": screen(
			"Steps:",
			"1. Yes, the build passes",
			"2. The tests pass",
		),
		"normal shell output": screen(filler(10)...),
	}
	for name, rows := range cases {
		if detectAgentPrompt(rows) {
			t.Errorf("%s: detected, want no prompt", name)
		}
	}
}
