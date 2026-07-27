package tui

import "testing"

// Regression: a clipboard paste in chat mode must submit the real content, not
// the literal "[clipboard text]" placeholder. Chat is LLM-facing like prompt, so
// both wrap the paste in <text-pasted> tags; command modes (shell/rysh/…) leave
// the line untouched (the caller discards the stored paste).
func TestExpandPastedPlaceholder(t *testing.T) {
	const pasted = "line one\nline two"
	wrapped := "<text-pasted>\n" + pasted + "\n</text-pasted>"

	cases := []struct {
		name   string
		mode   string
		text   string
		pasted string
		want   string
	}{
		{"chat expands", "chat", "[clipboard text]", pasted, wrapped},
		{"chat expands with leading text", "chat", "look: [clipboard text]", pasted, "look: " + wrapped},
		{"prompt still expands", "prompt", "[clipboard text]", pasted, wrapped},
		{"shell leaves placeholder untouched", "shell", "[clipboard text]", pasted, "[clipboard text]"},
		{"rysh leaves placeholder untouched", "rysh", "[clipboard text]", pasted, "[clipboard text]"},
		{"empty paste is a no-op", "chat", "[clipboard text]", "", "[clipboard text]"},
		{"only the first placeholder is expanded", "chat", "[clipboard text] [clipboard text]", pasted, wrapped + " [clipboard text]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := expandPastedPlaceholder(tc.mode, tc.text, tc.pasted); got != tc.want {
				t.Errorf("expandPastedPlaceholder(%q, %q, %q) = %q, want %q",
					tc.mode, tc.text, tc.pasted, got, tc.want)
			}
		})
	}
}
