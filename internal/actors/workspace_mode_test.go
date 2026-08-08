package actors

import "testing"

// Every mode the pane layer accepts must be typeable in `##mode new|delete`.
// These drifted apart once: "email" was a valid pane mode (and shown by
// `##mode list`) but normalizeModeName returned "" for it, so a pane a humanoid
// had flipped to email answered `##mode delete email` with `unknown mode "email"`
// and stayed stuck there.
func TestNormalizeModeNameCoversEveryValidPaneMode(t *testing.T) {
	for mode := range validPaneModes {
		if got := normalizeModeName(mode); got != mode {
			t.Errorf("normalizeModeName(%q) = %q, want %q", mode, got, mode)
		}
	}
}

func TestNormalizeModeNameAliases(t *testing.T) {
	cases := map[string]string{
		"sh":       "shell",
		"SHELL":    "shell",
		"ai":       "prompt",
		"prompt":   "prompt",
		"ext":      "external",
		"email":    "email",
		"mail":     "email",
		"inbox":    "email",
		" Email  ": "email",
		"browser":  "web",
		"bogus":    "",
		"":         "",
	}
	for in, want := range cases {
		if got := normalizeModeName(in); got != want {
			t.Errorf("normalizeModeName(%q) = %q, want %q", in, got, want)
		}
	}
}
