// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// ansiEscapeRegex strips terminal escape sequences that leak out of the PTY.
//
//   - CSI sequences (ECMA-48 compliant):
//     ESC [ <parameter bytes 0x30-0x3F>* <intermediate bytes 0x20-0x2F>* <final byte 0x40-0x7E>
//     This covers standard sequences (colors, cursor) as well as private-mode
//     sequences using >, <, =, ? prefixes (e.g. Kitty keyboard protocol \x1b[>1u,
//     bracketed paste \x1b[?2004h, mouse reporting \x1b[<35;12;5M, etc.)
//   - OSC sequences:  ESC ] ... BEL  or  ESC ] ... ST
//   - DCS sequences:  ESC P ... ST  (Device Control String)
//   - APC sequences:  ESC _ ... ST  (Application Program Command)
//   - PM  sequences:  ESC ^ ... ST  (Privacy Message)
//   - Character-set / other single-char ESC sequences
//   - 8-bit C1 OSC (\x9d) sequences
//   - Bare OSC fragments whose ESC was consumed by PTY processing
//   - Stray BEL, backspace, and carriage-return
var ansiEscapeRegex = regexp.MustCompile(
	// ESC-introduced sequences (7-bit):
	`\x1b(?:` +
		`\][^\x07\x1b]*(?:\x07|\x1b\\)?` + // OSC: ESC ] ... BEL/ST
		`|[P_^][^\x1b]*(?:\x1b\\)?` + // DCS/APC/PM: ESC P/_ /^ ... ST
		`|\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]` + // CSI: full ECMA-48 spec
		`|[()#][0-9A-Za-z]` + // Character-set designators
		`|.` + // Any other single-char ESC sequence
		`)` +
		// 8-bit C1 OSC (\x9d):
		`|\x9d[^\x07\x9c\n]*(?:\x07|\x9c)?` +
		// Bare OSC fragment (missing ESC):
		`|]\d{1,3};[^\x07\n]*(?:\x07)?` +
		// Stray control characters (BEL, BS, CR, readline markers SOH/STX):
		`|\x07|\x08|\r|\x01|\x02`,
)

// stripAnsiEscapes removes ANSI/VT100 escape sequences from s, then drops any
// raw control characters the regex does not cover (see stripDangerousControls).
func stripAnsiEscapes(s string) string {
	return stripDangerousControls(ansiEscapeRegex.ReplaceAllString(s, ""), false)
}

// sgrPrefixRe matches an SGR sequence anchored at the start of the input, used
// to step over colour sequences that sanitizeShellChunk deliberately preserved.
var sgrPrefixRe = regexp.MustCompile(`^\x1b\[[0-9;:]*m`)

// stripDangerousControls removes raw control characters that would otherwise be
// written verbatim into the user's terminal.
//
// This is the shell-output counterpart of the TUI's sanitizePastedText. The
// escape-sequence regexes above only match *sequences* (ESC + introducer +
// terminator); a lone control byte sitting in a file is invisible to them, and
// the render path (buildPanePanel) emits pane.Output verbatim so colours
// survive. So a `cat` of any file containing, say, SO (0x0e) used to switch the
// host terminal into the line-drawing charset — corrupting every pane on the
// screen, not just this one, until the pane was closed.
//
// Kept: \n (line structure) and \t (column alignment in ls//git output).
// Dropped: every other C0 control, DEL, the C1 range U+0080–U+009F, and invalid
// UTF-8 bytes (which desync width accounting and wreck the layout).
//
// When keepSGR is set, well-formed SGR colour sequences are copied through
// intact — they are the one escape sanitizeShellChunk intentionally preserves.
func stripDangerousControls(s string, keepSGR bool) string {
	if s == "" {
		return s
	}
	// Fast path: nothing to do for pure printable ASCII + \n \t.
	if !strings.ContainsFunc(s, func(r rune) bool {
		return (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f) || r == utf8.RuneError
	}) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		// ESC: keep a preserved SGR sequence whole, drop any other stray ESC.
		if c == 0x1b {
			if keepSGR {
				if m := sgrPrefixRe.FindString(s[i:]); m != "" {
					b.WriteString(m)
					i += len(m)
					continue
				}
			}
			i++
			continue
		}
		// ASCII fast path.
		if c < utf8.RuneSelf {
			if c < 0x20 || c == 0x7f {
				if c == '\n' || c == '\t' {
					b.WriteByte(c)
				}
			} else {
				b.WriteByte(c)
			}
			i++
			continue
		}
		// Multi-byte: decode so C1 controls are tested as runes, never as UTF-8
		// continuation bytes (0x80-0xBF), which must survive intact.
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			i++ // invalid byte — drop it
			continue
		}
		if r >= 0x80 && r <= 0x9f {
			i += size
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}

// splitTrailingPartial splits a PTY chunk into the part that is safe to render
// now and a short tail that must be held until the next read.
//
// The PTY hands us output at arbitrary byte boundaries, and two tails are
// destructive if processed in isolation:
//
//   - a trailing bare "\r": the PTY translates \n to \r\n, so a read that lands
//     between the pair leaves a chunk ending in CR. collapseCarriageReturns
//     treats that as an in-place line rewrite and collapses the line to "",
//     silently deleting a line of output.
//   - a trailing incomplete escape: emitting the lone ESC lets it recombine
//     with the next chunk's bytes into a live escape sequence in the buffer
//     (e.g. "\x1b" + "[2J" = erase-display, sent straight to the terminal).
//
// The held tail is capped so a pathological stream (a file of bare ESCs) can
// never buffer unboundedly; past the cap the tail is emitted and the existing
// sanitizers deal with it.
func splitTrailingPartial(s string) (emit, hold string) {
	const maxHold = 64
	if s == "" {
		return s, ""
	}
	if strings.HasSuffix(s, "\r") {
		return s[:len(s)-1], "\r"
	}
	// Walk back over a possible unterminated escape sequence: ESC followed by
	// parameter/intermediate bytes with no final byte yet.
	for i := len(s) - 1; i >= 0 && len(s)-i <= maxHold; i-- {
		if s[i] != 0x1b {
			continue
		}
		if escapeIsComplete(s[i:]) {
			break // the last ESC in s terminates; nothing to hold
		}
		return s[:i], s[i:]
	}
	return s, ""
}

// escapeIsComplete reports whether the escape sequence starting at s[0] (an
// ESC) has already received its terminator, so it can be emitted as-is.
func escapeIsComplete(s string) bool {
	if len(s) < 2 {
		return false // lone trailing ESC
	}
	switch s[1] {
	case '[': // CSI: ends at a final byte 0x40-0x7E
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return true
			}
		}
		return false
	case ']', 'P', '_', '^': // OSC/DCS/APC/PM: end at BEL or ST
		return strings.ContainsAny(s, "\x07") || strings.Contains(s, "\x1b\\")
	default:
		return true // two-byte escape, already whole
	}
}

// ansiEscapeNoCRRegex is ansiEscapeRegex minus the carriage-return
// alternative: used by sanitizeShellChunk, which handles \r separately via
// collapseCarriageReturns (overwrite semantics) instead of deleting it.
var ansiEscapeNoCRRegex = regexp.MustCompile(
	`\x1b(?:` +
		`\][^\x07\x1b]*(?:\x07|\x1b\\)?` + // OSC: ESC ] ... BEL/ST
		`|[P_^][^\x1b]*(?:\x1b\\)?` + // DCS/APC/PM: ESC P/_ /^ ... ST
		`|\[[\x30-\x3f]*[\x20-\x2f]*[\x40-\x7e]` + // CSI: full ECMA-48 spec
		`|[()#][0-9A-Za-z]` + // Character-set designators
		`|.` + // Any other single-char ESC sequence
		`)` +
		// 8-bit C1 OSC (\x9d):
		`|\x9d[^\x07\x9c\n]*(?:\x07|\x9c)?` +
		// Bare OSC fragment (missing ESC):
		`|]\d{1,3};[^\x07\n]*(?:\x07)?` +
		// Stray control characters (BEL, BS, readline markers SOH/STX) — NOT \r:
		`|\x07|\x08|\x01|\x02`,
)

// sgrSequenceRe matches a complete SGR (color/style) CSI sequence: parameters
// limited to digits, ';' and ':' (colon covers ISO-8613 direct-color forms),
// final byte 'm'.
var sgrSequenceRe = regexp.MustCompile(`^\x1b\[[0-9;:]*m$`)

// sanitizeShellChunk removes terminal escape sequences from a PTY chunk
// EXCEPT SGR color/style sequences, so shell output keeps its colors in the
// pane scrollback while cursor movement, OSC titles, bracketed-paste guards
// etc. are still dropped. Carriage returns are collapsed with terminal
// overwrite semantics rather than deleted. Used when ShellColorOutput is on;
// stripAnsiEscapes remains the plain-text path (sharing, AI context, legacy).
func sanitizeShellChunk(s string) string {
	s = ansiEscapeNoCRRegex.ReplaceAllStringFunc(s, func(m string) string {
		if sgrSequenceRe.MatchString(m) {
			return m
		}
		return ""
	})
	// Collapse \r overwrites FIRST: carriage returns carry meaning here
	// (progress bars rewrite their line), and the control filter below removes
	// them. Afterwards no \r remains, so the order is not merely convenient —
	// it is required for progress-bar output to reduce to its final frame.
	s = collapseCarriageReturns(s)
	// Then drop raw control bytes the regex cannot see (SO/SI/FF/VT/NUL/...),
	// which would otherwise be written straight to the host terminal. SGR
	// sequences preserved above are stepped over intact.
	return stripDangerousControls(s, true)
}

// collapseCarriageReturns applies carriage-return overwrite semantics within
// each line of s: on a terminal "aaa\rbbb" displays as "bbb", so a linear
// scrollback buffer keeps only the text after the last CR of each physical
// line. "\r\n" is first normalized to "\n". A line ending in a bare CR (a
// progress bar mid-rewrite at a chunk boundary) collapses to "" — matching
// the existing isLineRedraw behavior of not appending in-place rewrites.
func collapseCarriageReturns(s string) string {
	if !strings.Contains(s, "\r") {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if !strings.Contains(s, "\r") {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if idx := strings.LastIndexByte(line, '\r'); idx >= 0 {
			lines[i] = line[idx+1:]
		}
	}
	return strings.Join(lines, "\n")
}

// isLineRedraw reports whether a raw PTY chunk is a pure in-place line redraw:
// it begins with a carriage return and, once ANSI/control sequences are
// stripped, contains no newline. The shell emits exactly this — a carriage
// return, an erase-to-EOL and the line content ("\r\x1b[K<line>") — when it
// redraws its current line without advancing. bash does it on every
// SIGWINCH/resize, re-issuing its prompt; stripAnsiEscapes removes the \r and
// the \x1b[K, leaving just "<line>".
//
// A carriage return means "overwrite the current line", which a linear
// scrollback buffer already reflects, so such a chunk adds nothing. Appending
// it anyway is what stacked "bash-3.2$ bash-3.2$ bash-3.2$ ..." onto a pane
// across the burst of resizes it receives while its layout settles. Callers
// skip appending a chunk for which this returns true.
func isLineRedraw(chunk []byte, stripped string) bool {
	return len(chunk) > 0 && chunk[0] == '\r' && !strings.Contains(stripped, "\n")
}

// shellPromptRe matches common shell prompts at the end of output:
//
//	bash-3.2$  |  $  |  user@host:~$  |  [user@host dir]$  |  #  |  %
var shellPromptRe = regexp.MustCompile(`(?m)^[^\n]*[\$#%]\s*$`)

// stripShellPrompts removes lines that look like a shell prompt from the text
// buffer output. Since rysh generates its own "$ <cmd>" echo in executeShell,
// prompt lines from the PTY are redundant and should be stripped.
func stripShellPrompts(output string) string {
	output = shellPromptRe.ReplaceAllString(output, "")
	// Clean up excess blank lines left behind.
	for strings.Contains(output, "\n\n\n") {
		output = strings.ReplaceAll(output, "\n\n\n", "\n\n")
	}
	output = strings.TrimLeft(output, "\n")
	return output
}

// maxPaneBuffer is the default maximum number of bytes retained in a pane
// output buffer when no `[ui] shell_buffer_bytes` is configured (the config
// default is 256 KiB; this constant is the last-resort fallback for a zero
// config, e.g. actors constructed in tests with config.Config{}).
const maxPaneBuffer = 20000
