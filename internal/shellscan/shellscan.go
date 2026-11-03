// SPDX-License-Identifier: Apache-2.0

// Package shellscan splits a shell command line the way bash would see its
// control structure, without running it: command segments separated by
// control operators outside quotes, and whether the line redirects output or
// substitutes commands. The approval gates use it; it is deliberately
// conservative — anything it cannot read with certainty is reported as unsafe.
package shellscan

import "strings"

// Result is what a scan found.
type Result struct {
	// Segments are the simple commands between control operators (&&, ||, ;,
	// |, &, newline) that appear outside quotes, trimmed. Quotes are kept.
	Segments []string
	// Redirect is true when an output redirection (>) appears outside quotes.
	Redirect bool
	// Subst is true for command or process substitution: $( or a backtick
	// outside single quotes, or <( / >( outside quotes.
	Subst bool
	// Unbalanced is true when a quote is left open or the line ends in a
	// backslash: the structure cannot be trusted.
	Unbalanced bool
	// Opaque is true for constructs this scanner does not model and so cannot
	// read safely: ANSI-C quoting $'...' (bash honours \' inside it), here-docs
	// and here-strings (<<, <<<: the body is not shell syntax, so a quote in it
	// would mislead the quote tracking), and an unquoted # at the start of a
	// word (a comment, whose quotes bash ignores). e20 review 2026-10-30.
	Opaque bool
}

// Unsafe reports whether the line hides work beyond its plain segments.
func (r Result) Unsafe() bool { return r.Redirect || r.Subst || r.Unbalanced || r.Opaque }

// Chained reports whether the line holds more than one command.
func (r Result) Chained() bool { return len(r.Segments) > 1 }

// Scan reads cmd.
func Scan(cmd string) Result {
	var r Result
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			r.Segments = append(r.Segments, s)
		}
		cur.Reset()
	}
	rs := []rune(cmd)
	const (
		none = iota
		single
		double
	)
	state := none
	wordStart := true // the next rune begins a word (line start, after blank or operator)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		next := rune(0)
		if i+1 < len(rs) {
			next = rs[i+1]
		}
		switch state {
		case single:
			cur.WriteRune(c)
			if c == '\'' {
				state = none
			}
			continue
		case double:
			cur.WriteRune(c)
			switch {
			case c == '\\' && next != 0:
				cur.WriteRune(next)
				i++
			case c == '"':
				state = none
			case c == '`', c == '$' && next == '(':
				r.Subst = true
			}
			continue
		}
		// Outside quotes.
		atWord := wordStart
		// bash starts a new word after blanks and after the metacharacters
		// < ( ) too ("cat <#x" opens a comment), so # there is a comment.
		wordStart = c == ' ' || c == '\t' || c == '<' || c == '(' || c == ')'
		switch {
		case c == '#' && atWord:
			r.Opaque = true
			cur.WriteRune(c)
		case c == '$' && next == '\'':
			r.Opaque = true
			cur.WriteRune(c)
		case c == '<' && next == '<':
			r.Opaque = true
			cur.WriteRune(c)
		case c == '\\':
			if next == 0 {
				r.Unbalanced = true
				continue
			}
			cur.WriteRune(c)
			cur.WriteRune(next)
			i++
		case c == '\'':
			state = single
			cur.WriteRune(c)
		case c == '"':
			state = double
			cur.WriteRune(c)
		case c == '`', c == '$' && next == '(':
			r.Subst = true
			cur.WriteRune(c)
		case (c == '<' || c == '>') && next == '(':
			r.Subst = true
			cur.WriteRune(c)
		case c == '>':
			r.Redirect = true
			cur.WriteRune(c)
		case c == ';', c == '\n', c == '|', c == '&':
			// && || and the single forms all end a segment; a doubled
			// operator is consumed whole.
			if (c == '&' || c == '|') && next == c {
				i++
			}
			flush()
			wordStart = true
		default:
			cur.WriteRune(c)
		}
	}
	if state != none {
		r.Unbalanced = true
	}
	flush()
	return r
}

// Fields splits one segment into words, honouring single and double quotes
// and backslash escapes, and dropping the quote characters. It is for
// recognising a command and its subcommand, not for running anything.
func Fields(seg string) []string {
	var out []string
	var cur strings.Builder
	inWord := false
	state := 0 // 0 none, 1 single, 2 double
	rs := []rune(seg)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch state {
		case 1:
			if c == '\'' {
				state = 0
			} else {
				cur.WriteRune(c)
			}
			continue
		case 2:
			if c == '\\' && i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
			} else if c == '"' {
				state = 0
			} else {
				cur.WriteRune(c)
			}
			continue
		}
		switch {
		case c == ' ' || c == '\t':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '\'':
			state, inWord = 1, true
		case c == '"':
			state, inWord = 2, true
		case c == '\\' && i+1 < len(rs):
			i++
			cur.WriteRune(rs[i])
			inWord = true
		default:
			cur.WriteRune(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}
