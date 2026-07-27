package actors

import (
	"fmt"
	"strings"
)

// commandPrefixes are command-wrapper prefixes that should be skipped
// to find the actual command being executed.
var commandPrefixes = map[string]bool{
	"sudo":   true,
	"env":    true,
	"nohup":  true,
	"nice":   true,
	"time":   true,
	"strace": true,
	"xargs":  true,
}

// extractShellCommands parses a shell input string and returns a list of
// command names (first token of each command segment). Returns error if
// the input contains constructs that cannot be reliably parsed, such as
// subshell substitutions.
//
// This is a best-effort parser for share restriction enforcement. It is NOT
// a security sandbox — determined users can bypass it via aliases, PATH
// manipulation, or script files. Use "view" share mode for high-security
// scenarios.
func extractShellCommands(input string) ([]string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, nil
	}

	// Reject inputs with subshell constructs that are hard to parse reliably.
	if strings.Contains(input, "$(") || strings.Contains(input, "`") {
		return nil, fmt.Errorf("subshell constructs not supported")
	}

	// Split by shell operators.
	segments := splitByOperators(input)

	var commands []string
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		cmd := extractFirstCommand(seg)
		if cmd != "" {
			commands = append(commands, cmd)
		}
	}
	return commands, nil
}

// splitByOperators splits a command line by shell operators (&&, ||, ;, |).
func splitByOperators(input string) []string {
	// Replace multi-char operators with unique delimiters first to avoid
	// partial matches (e.g., "||" should not be treated as two "|").
	s := input
	s = strings.ReplaceAll(s, "&&", "\x00")
	s = strings.ReplaceAll(s, "||", "\x01")

	var segments []string
	current := strings.Builder{}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '\x00' || ch == '\x01' || ch == ';' || ch == '|' {
			segments = append(segments, current.String())
			current.Reset()
		} else {
			current.WriteByte(ch)
		}
	}
	if current.Len() > 0 {
		segments = append(segments, current.String())
	}
	return segments
}

// extractFirstCommand extracts the command name from a single command segment,
// skipping env assignments (KEY=val) and known wrapper prefixes (sudo, env, etc.).
func extractFirstCommand(segment string) string {
	tokens := strings.Fields(segment)
	inPrefix := false // true while we're still consuming a prefix command and its flags
	for i, tok := range tokens {
		// Skip environment variable assignments (KEY=value).
		if strings.Contains(tok, "=") && !strings.HasPrefix(tok, "-") {
			continue
		}
		// Skip known prefix commands, but look at the next token.
		if commandPrefixes[tok] && i+1 < len(tokens) {
			inPrefix = true
			continue
		}
		// While inside a prefix command's argument span, skip flags and
		// their arguments (e.g., sudo -u root → skip "-u" and "root").
		if inPrefix && strings.HasPrefix(tok, "-") {
			continue
		}
		// If the previous token was a flag (like -u), this token is the
		// flag's argument (like "root"), not the actual command. But only
		// while we're still within a prefix command's options.
		if inPrefix && i > 0 && strings.HasPrefix(tokens[i-1], "-") {
			// This is a flag argument (e.g., "root" in "sudo -u root whoami").
			// Skip it and keep looking.
			continue
		}
		// Skip standalone flags that precede the command (e.g., env -i cmd).
		if strings.HasPrefix(tok, "-") {
			continue
		}
		// This token is the command name. Strip any path prefix.
		parts := strings.Split(tok, "/")
		return parts[len(parts)-1]
	}
	return ""
}

// toStringSet converts a string slice to a set (map[string]bool).
func toStringSet(items []string) map[string]bool {
	s := make(map[string]bool, len(items))
	for _, item := range items {
		s[item] = true
	}
	return s
}

// appendUnique appends items from add to base, skipping duplicates.
func appendUnique(base, add []string) []string {
	seen := toStringSet(base)
	result := append([]string{}, base...)
	for _, item := range add {
		if !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	return result
}
