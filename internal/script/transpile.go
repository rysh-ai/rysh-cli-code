// SPDX-License-Identifier: Apache-2.0

// Package script turns a .rysh file — a bash script whose statement-position
// "##" lines are rysh commands — into plain bash (design 021).
//
// The whole design rests on one property of bash: "#" opens a comment at the
// start of any WORD, not just a line. That means a file full of "##pane info"
// lines is already a valid bash script in which those lines do nothing, so the
// same file can be read by bash and by rysh. It also means "##" can only ever
// appear in statement position: `X=$(##pane info)` is not a comment, it is an
// unterminated `$(` — a hard syntax error. So there is no expression form, and
// capture happens through $RYSH_OUT instead (see Prelude).
//
// Transpiling to bash rather than interpreting the file ourselves is what keeps
// the language honest. Every construct bash has — loops, conditionals,
// functions, quoting, heredocs, pipes, `set -e`, traps, `$?` — keeps working
// because bash is still the thing running the script. An interpreter of our own
// would have to re-implement all of it, and would be subtly wrong about
// heredocs and multi-line constructs forever.
package script

import (
	"fmt"
	"strings"
)

// RyshFunc is the shell function a rysh line is rewritten into. It is defined
// by Prelude.
const RyshFunc = "__rysh"

// PromptFunc is the shell function `##prompt <text>` is rewritten into.
const PromptFunc = "__rysh_prompt"

// PromptCommand is the one command word that is a SCRIPT builtin rather than an
// entry in the daemon's ## dispatch table.
//
// It cannot be a table command: running it means waiting for an agentic turn to
// finish, and the table's handlers run inside the WorkspaceActor, where
// blocking would freeze the whole workspace. So `##prompt refactor the parser`
// compiles to a call to `rysh prompt`, which watches the pane's phase events
// from outside the actor and blocks there instead.
//
// Spelling it "##prompt" rather than making scripts call `rysh prompt` directly
// is what keeps the polyglot property: a "##" line is a comment to bash, so the
// file still parses and still runs with the rysh work skipped. A bare
// `rysh prompt ...` line would be a command plain bash would try — and fail —
// to run.
const PromptCommand = "prompt"

// Result is a transpiled script.
type Result struct {
	// Bash is the transpiled source. It has exactly as many lines as the input
	// (see the package doc on line fidelity).
	Bash string
	// RyshLines are the 1-based source line numbers that were rewritten.
	RyshLines []int
}

// Error is a transpile failure tied to a source line.
type Error struct {
	Line int
	Msg  string
}

func (e *Error) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

// Transpile rewrites the "##" lines of src into calls to RyshFunc.
//
// Line fidelity is an invariant, not an aspiration: every source line produces
// exactly one output line, so bash's own diagnostics, `set -x` traces and
// $LINENO all point at the real line of the .rysh file. Nothing may be
// inserted — which is why the prelude is a separate file that sources this one
// rather than a header prepended to it.
func Transpile(src string) (*Result, error) {
	// Splitting on "\n" and rejoining preserves the input's trailing-newline
	// shape exactly, which keeps the line counts equal.
	lines := strings.Split(src, "\n")
	out := make([]string, len(lines))
	var ryshLines []int

	sc := &scanner{}
	for i, line := range lines {
		lineNo := i + 1
		code := sc.inCode()
		// The scanner must see every line, including ones we rewrite, so it can
		// track heredocs and quotes opened on this line.
		sc.feed(line)

		if !code {
			// Inside a heredoc body or an unterminated quote: this is data, not
			// a statement. Pass it through untouched.
			out[i] = line
			continue
		}
		body, rest, ok := splitRyshLine(line)
		if !ok {
			out[i] = line
			continue
		}
		if err := checkQuotes(body); err != nil {
			return nil, &Error{Line: lineNo, Msg: err.Error()}
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		fn, arg := RyshFunc, "##"+body
		if tail, isPrompt := splitPromptCommand(body); isPrompt {
			fn, arg = PromptFunc, tail
		}
		emitted := fmt.Sprintf(`%s%s "%s"`, indent, fn, arg)
		if rest != "" {
			emitted += " " + rest
		}
		out[i] = emitted
		ryshLines = append(ryshLines, lineNo)
	}

	return &Result{Bash: strings.Join(out, "\n"), RyshLines: ryshLines}, nil
}

// splitRyshLine recognises a rysh line and splits it into the command body and
// whatever bash tail follows it.
//
// A rysh line is `^[ \t]*##[A-Za-z]`, optionally preceded by `: ` — see
// colonForm below. Requiring a letter immediately after the "##" — no space —
// is what lets `## foo` and `### foo` stay ordinary bash comments: existing
// scripts full of "## SECTION" banners keep working, and an author who wants a
// comment that merely looks like a command has an escape hatch that costs one
// space.
//
// The tail is everything from the first unquoted, top-level control operator
// onward, emitted verbatim after the RyshFunc call. That is what makes
// `##agent run x || exit 1`, `##pane list | grep -c idle`, `##pane info > f`
// and a trailing `# comment` work — and it is the difference between a language
// that composes with bash and one that merely coexists with it.
func splitRyshLine(line string) (body, rest string, ok bool) {
	trimmed := stripColonForm(strings.TrimLeft(line, " \t"))
	if !strings.HasPrefix(trimmed, "##") {
		return "", "", false
	}
	after := trimmed[2:]
	if after == "" || !isASCIILetter(after[0]) {
		return "", "", false
	}

	cut := len(after)
	var sq, dq bool
	// expand counts open ${...} and $(...) groups. A '#' inside one is part of
	// the expansion — ${#arr[@]} is a length, ${v##*/} is a prefix strip —
	// not the start of a comment, and '|' inside one is a pattern alternative
	// rather than a pipe. Splitting there produced `__rysh "##new grid ${"`
	// and an unterminated quote from bash.
	expand := 0
	for i := 0; i < len(after); i++ {
		c := after[i]
		switch {
		case c == '\\' && !sq:
			i++ // the next byte is escaped, whatever it is
			continue
		case c == '\'' && !dq && expand == 0:
			sq = !sq
			continue
		case c == '"' && !sq:
			dq = !dq
			continue
		}
		if sq {
			continue
		}
		// $( and ${ open a group; the matching close ends it. Tracked even
		// inside double quotes, because "${#a[@]}" is just as expandable.
		if c == '$' && i+1 < len(after) && (after[i+1] == '(' || after[i+1] == '{') {
			expand++
			i++
			continue
		}
		if expand > 0 {
			if c == ')' || c == '}' {
				expand--
			}
			continue
		}
		if dq {
			continue
		}
		// Unquoted, top level: any of these ends the rysh command.
		if c == ';' || c == '|' || c == '&' || c == '>' || c == '<' || c == '#' {
			cut = i
			goto done
		}
	}
done:
	body = strings.TrimRight(after[:cut], " \t")
	rest = strings.TrimLeft(after[cut:], " \t")
	if body == "" {
		// e.g. "##|foo" — there is no command here.
		return "", "", false
	}
	return body, rest, true
}

// checkQuotes rejects a body that would not survive being spliced into a bash
// double-quoted string.
//
// The contract is deliberately one sentence — "the text after ## is a bash
// double-quoted string" — so $VAR, $(…) and backslash escapes behave exactly as
// they do everywhere else in the file. The cost is that an unbalanced quote
// would produce a bash syntax error on a line the author did not write. Catching
// it here means the message names the .rysh line and says what is wrong.
func checkQuotes(body string) error {
	var sq, dq bool
	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case c == '\\':
			i++
		case c == '\'' && !dq:
			sq = !sq
		case c == '"' && !sq:
			dq = !dq
		}
	}
	if dq {
		return fmt.Errorf(`unbalanced " in rysh command (a literal quote must be written \")`)
	}
	if sq {
		return fmt.Errorf("unbalanced ' in rysh command")
	}
	return nil
}

// splitPromptCommand reports whether a command body is the ##prompt builtin,
// returning the prompt text. `##prompt` with no text is not the builtin — it
// falls through to the dispatch table, which explains what to use instead.
func splitPromptCommand(body string) (text string, ok bool) {
	if !strings.HasPrefix(body, PromptCommand) {
		return "", false
	}
	rest := body[len(PromptCommand):]
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return "", false
	}
	return rest, true
}

// stripColonForm removes a leading `: ` from an already left-trimmed line.
//
// The colon form exists because the plain `##cmd` form has one gap in its
// bash-validity. Bash requires a non-empty body for every compound statement,
// and a "##" line is a comment to bash, so this file:
//
//	if [[ $x == y ]]; then
//	  ##pane info
//	fi
//
// is a SYNTAX ERROR under plain bash — the `if` body is empty. Everywhere else
// a "##" line is harmlessly inert, but as the sole statement of an if/for/
// while/case/function body it breaks the polyglot property outright.
//
// `:` is bash's no-op builtin, so `: ##pane info` is a complete statement that
// does nothing under bash and is still a rysh command here. It costs two
// characters and buys a file that is valid bash in every position. Use it when
// a rysh command is a block's only statement; `rysh script --check` finds the
// places that need it.
func stripColonForm(trimmed string) string {
	if !strings.HasPrefix(trimmed, ":") {
		return trimmed
	}
	rest := trimmed[1:]
	// Require whitespace after the colon: `:##pane` is not valid bash (it is
	// the command `:##pane`), so recognising it would let an author write
	// something this package claims is bash-valid when it is not.
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return trimmed
	}
	return strings.TrimLeft(rest, " \t")
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
