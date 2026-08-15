// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"os"
	"strings"
)

// mergeEnv returns the parent process environment with extra "KEY=VALUE" entries
// appended (later entries win in exec semantics, so these override inherited
// values).
func mergeEnv(extra []string) []string {
	if len(extra) == 0 {
		return os.Environ()
	}
	base := os.Environ()
	out := make([]string, 0, len(base)+len(extra))
	out = append(out, base...)
	out = append(out, extra...)
	return out
}

// sanitizeToolName coerces a name into the character set Anthropic's tool API
// accepts (^[a-zA-Z0-9_-]{1,64}$): invalid runes become '_', the result is
// truncated to 64 runes, and an empty result falls back to "tool".
func sanitizeToolName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if s == "" {
		s = "tool"
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
