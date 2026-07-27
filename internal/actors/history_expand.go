package actors

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// Bash-style history expansion
// ---------------------------------------------------------------------------
//
// rysh owns the shell input line (the user types into rysh's prompt, not the
// shell's readline), so it performs history expansion itself against its own
// per-pane shell history. This keeps the feature consistent across input modes
// (interactive and legacy) and across shells (bash, zsh, sh, fish), and means
// the expanded command — not the "!!" shorthand — is what gets echoed, sent to
// the PTY, and recorded in history (matching bash semantics).
//
// Supported event designators:
//
//	!!            the previous command
//	!n            command number n (1-based, absolute position in history)
//	!-n           the n-th previous command (!-1 == !!)
//	!string       most recent command starting with string
//	!?string?     most recent command containing string (trailing ? optional)
//	!$            last word (last argument) of the previous command
//	!^            first argument of the previous command
//	!*            all arguments of the previous command
//
// Quick substitution (only when '^' is the first character of the line):
//
//	^old^new      repeat the previous command, replacing the first old with new
//	^old^new^     trailing '^' form (anything after it is appended)
//
// NOT supported (left untouched): word/modifier designators such as `!!:2`,
// `:s/a/b/`, `:h`, `:t`. A token containing ':' will therefore generally fail
// to resolve and is reported as "event not found".
//
// A '!' is treated literally unless it is immediately followed by a recognized
// designator start: '!', '$', '^', '*', '?', a digit, '-' followed by a digit,
// or a word-start character (letter, '_', '/', '.', '~'). A '!' at end of line,
// inside single quotes, or preceded by a backslash is always literal. This is
// intentionally a touch more conservative than bash so that ordinary commands
// containing '!' (e.g. `find . ! -name x`, `echo "done!"`, `a != b`) are never
// mangled.

// expandHistory expands history references in a single shell input line using
// history (oldest first, newest last) as the event source.
//
// It returns (expanded, changed, error). When changed is false the input is
// returned unmodified. When error is non-nil the reference could not be
// resolved; callers should report the error and NOT run the command, matching
// bash (which prints "event not found" / "substitution failed" and aborts).
func expandHistory(input string, history []string) (string, bool, error) {
	// Quick substitution applies only when '^' is the very first character.
	if strings.HasPrefix(input, "^") {
		return expandQuickSub(input, history)
	}
	// Fast path: nothing to expand.
	if !strings.ContainsRune(input, '!') {
		return input, false, nil
	}

	var b strings.Builder
	changed := false
	inSingle := false
	for i := 0; i < len(input); {
		c := input[i]

		// Backslash escapes the next character (history expansion ignores it).
		// Copy both verbatim and let the shell handle the backslash.
		if c == '\\' && !inSingle {
			b.WriteByte(c)
			if i+1 < len(input) {
				b.WriteByte(input[i+1])
				i += 2
			} else {
				i++
			}
			continue
		}

		// Single quotes suppress history expansion until the closing quote.
		if c == '\'' {
			inSingle = !inSingle
			b.WriteByte(c)
			i++
			continue
		}

		if c == '!' && !inSingle {
			repl, consumed, didChange, err := parseBang(input, i, history)
			if err != nil {
				return "", false, err
			}
			b.WriteString(repl)
			i += consumed
			if didChange {
				changed = true
			}
			continue
		}

		b.WriteByte(c)
		i++
	}
	return b.String(), changed, nil
}

// parseBang handles a '!' at input[pos]. It returns the replacement text, how
// many input bytes were consumed, whether an actual expansion occurred, and any
// resolution error. When the '!' is literal, repl is "!", consumed is 1, and
// didChange is false.
func parseBang(input string, pos int, history []string) (repl string, consumed int, didChange bool, err error) {
	// '!' at end of line is literal.
	if pos+1 >= len(input) {
		return "!", 1, false, nil
	}
	c := input[pos+1]
	switch {
	case c == '!': // !! -> previous command
		cmd, ok := lastCommand(history)
		if !ok {
			return "", 0, false, fmt.Errorf("!!: event not found")
		}
		return cmd, 2, true, nil

	case c == '$' || c == '^' || c == '*': // word designators on previous command
		cmd, ok := lastCommand(history)
		if !ok {
			return "", 0, false, fmt.Errorf("!%c: event not found", c)
		}
		word, werr := selectWord(cmd, c)
		if werr != nil {
			return "", 0, false, werr
		}
		return word, 2, true, nil

	case c == '?': // !?string? -> most recent command containing string
		// Read until the closing '?', whitespace, a quote, or end.
		j := pos + 2
		for j < len(input) && input[j] != '?' && !isSpace(input[j]) &&
			input[j] != '"' && input[j] != '\'' {
			j++
		}
		needle := input[pos+2 : j]
		consumed = j - pos
		if j < len(input) && input[j] == '?' { // include closing '?'
			consumed++
		}
		if needle == "" {
			return "!?", 2, false, nil // nothing to search for: treat literally
		}
		if cmd, ok := mostRecentContaining(history, needle); ok {
			return cmd, consumed, true, nil
		}
		return "", 0, false, fmt.Errorf("!?%s?: event not found", needle)

	case c == '-': // !-n -> n-th previous command
		j := pos + 2
		for j < len(input) && isDigit(input[j]) {
			j++
		}
		if j == pos+2 { // no digits after '-': literal
			return "!", 1, false, nil
		}
		n := atoi(input[pos+2 : j])
		idx := len(history) - n
		if n <= 0 || idx < 0 || idx >= len(history) {
			return "", 0, false, fmt.Errorf("%s: event not found", input[pos:j])
		}
		return history[idx], j - pos, true, nil

	case isDigit(c): // !n -> absolute command number (1-based)
		j := pos + 1
		for j < len(input) && isDigit(input[j]) {
			j++
		}
		n := atoi(input[pos+1 : j])
		idx := n - 1
		if n <= 0 || idx < 0 || idx >= len(history) {
			return "", 0, false, fmt.Errorf("%s: event not found", input[pos:j])
		}
		return history[idx], j - pos, true, nil

	case isWordStart(c): // !string -> most recent command starting with string
		// The event name extends over word-continuation characters only, so it
		// stops at whitespace, quotes, and shell metacharacters (e.g. the
		// closing quote in `echo "!ls"`).
		j := pos + 1
		for j < len(input) && isWordCont(input[j]) {
			j++
		}
		prefix := input[pos+1 : j]
		if cmd, ok := mostRecentPrefix(history, prefix); ok {
			return cmd, j - pos, true, nil
		}
		return "", 0, false, fmt.Errorf("!%s: event not found", prefix)

	default:
		// '!' followed by space, '=', '(', '"', ')', etc. -> literal.
		return "!", 1, false, nil
	}
}

// expandQuickSub handles the ^old^new[^trailing] quick-substitution form, which
// repeats the previous command replacing the first occurrence of old with new.
func expandQuickSub(input string, history []string) (string, bool, error) {
	s := input[1:] // drop the leading '^'
	var oldStr, newStr, trailing string
	if i1 := strings.Index(s, "^"); i1 < 0 {
		oldStr, newStr, trailing = s, "", ""
	} else {
		oldStr = s[:i1]
		r := s[i1+1:]
		if i2 := strings.Index(r, "^"); i2 < 0 {
			newStr, trailing = r, ""
		} else {
			newStr, trailing = r[:i2], r[i2+1:]
		}
	}
	if oldStr == "" {
		// "^" with no search text is not a substitution; leave it alone.
		return input, false, nil
	}
	cmd, ok := lastCommand(history)
	if !ok {
		return "", false, fmt.Errorf("%s: event not found", input)
	}
	if !strings.Contains(cmd, oldStr) {
		return "", false, fmt.Errorf("%s: substitution failed", input)
	}
	return strings.Replace(cmd, oldStr, newStr, 1) + trailing, true, nil
}

// selectWord returns the word indicated by a '$', '^' or '*' designator applied
// to cmd: '$' = last word, '^' = first argument, '*' = all arguments.
func selectWord(cmd string, designator byte) (string, error) {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "", fmt.Errorf("!%c: bad word specifier", designator)
	}
	switch designator {
	case '$':
		return fields[len(fields)-1], nil
	case '^':
		if len(fields) < 2 {
			return "", fmt.Errorf("!^: bad word specifier")
		}
		return fields[1], nil
	case '*':
		if len(fields) < 2 {
			return "", nil // no arguments: bash expands !* to the empty string
		}
		return strings.Join(fields[1:], " "), nil
	}
	return "", fmt.Errorf("!%c: bad word specifier", designator)
}

func lastCommand(history []string) (string, bool) {
	if len(history) == 0 {
		return "", false
	}
	return history[len(history)-1], true
}

func mostRecentPrefix(history []string, prefix string) (string, bool) {
	for i := len(history) - 1; i >= 0; i-- {
		if strings.HasPrefix(history[i], prefix) {
			return history[i], true
		}
	}
	return "", false
}

func mostRecentContaining(history []string, needle string) (string, bool) {
	for i := len(history) - 1; i >= 0; i-- {
		if strings.Contains(history[i], needle) {
			return history[i], true
		}
	}
	return "", false
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isSpace(c byte) bool { return c == ' ' || c == '\t' }

// isWordStart reports whether c can begin a !string event designator.
func isWordStart(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	case c == '_' || c == '/' || c == '.' || c == '~':
		return true
	}
	return false
}

// isWordCont reports whether c may continue a !string event name. It stops the
// token at whitespace, quotes, and shell metacharacters so references embedded
// in larger commands (e.g. the closing quote in `echo "!ls"`) terminate
// correctly.
func isWordCont(c byte) bool {
	return isWordStart(c) || isDigit(c) || c == '-'
}

// atoi parses a run of ASCII digits. The caller guarantees s contains only
// digits, so errors are impossible; on overflow it saturates harmlessly large.
func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		n = n*10 + int(s[i]-'0')
		if n > 1<<30 { // saturate to avoid overflow; bounds check rejects it anyway
			return 1 << 30
		}
	}
	return n
}
