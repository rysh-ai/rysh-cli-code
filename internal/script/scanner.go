package script

import "strings"

// scanner tracks just enough bash lexical state to answer one question per
// line: is this line in statement position, or is it data?
//
// It is not a bash parser and does not try to be. It knows about the three
// things that let a "##" line appear without being a statement:
//
//   - heredoc bodies — `cat <<EOF` ... `EOF`; a "##pane info" in there is text
//     the script is writing, not a command it is running
//   - quotes that span lines — an unterminated ' or " carries into the next
//     line, so what looks like a statement is the middle of a string
//   - backslash continuations — the next line is the tail of this command
//
// Getting any of these wrong means rewriting data into a command, which is the
// worst failure this package can have. It is also why `rysh script --print`
// exists: whatever the scanner concludes, the author can see the result.
type scanner struct {
	// heredocs are the delimiters we are waiting for, in the order bash will
	// consume them. A single line may open several (`cat <<A <<B`).
	heredocs []heredoc
	// inHeredoc is true while reading a heredoc body.
	inHeredoc bool
	// openQuote is the quote character carried over from the previous line
	// (0 when none).
	openQuote byte
	// continued is true when the previous line ended in a backslash.
	continued bool
}

type heredoc struct {
	delim string
	// tabs is true for <<- , which strips leading tabs from the terminator.
	tabs bool
}

// inCode reports whether the next line to be fed starts in statement position.
func (s *scanner) inCode() bool {
	return !s.inHeredoc && s.openQuote == 0 && !s.continued
}

// feed advances the state by one line.
func (s *scanner) feed(line string) {
	if s.inHeredoc {
		s.feedHeredocBody(line)
		return
	}
	s.feedCode(line)
}

// feedHeredocBody consumes a line of heredoc body, closing the heredoc when the
// line is exactly its terminator.
func (s *scanner) feedHeredocBody(line string) {
	if len(s.heredocs) == 0 {
		s.inHeredoc = false
		return
	}
	h := s.heredocs[0]
	candidate := line
	if h.tabs {
		candidate = strings.TrimLeft(candidate, "\t")
	}
	// bash also tolerates trailing whitespace on the terminator in practice;
	// being lenient here risks ending a heredoc early, so match exactly.
	if candidate == h.delim {
		s.heredocs = s.heredocs[1:]
		s.inHeredoc = len(s.heredocs) > 0
	}
}

// feedCode scans a line in statement position, recording any heredoc it opens,
// any quote it leaves unterminated, and whether it ends in a continuation.
func (s *scanner) feedCode(line string) {
	quote := s.openQuote
	s.continued = false

	// A comment line cannot open a heredoc or a string. Checking this first
	// keeps `# cat <<EOF` from arming a heredoc that never arrives — including,
	// importantly, every "##" line we are about to rewrite, whose body is not
	// bash and must not be scanned as if it were.
	if quote == 0 {
		if t := strings.TrimLeft(line, " \t"); strings.HasPrefix(t, "#") {
			return
		}
	}

	for i := 0; i < len(line); i++ {
		c := line[i]

		if c == '\\' {
			if i == len(line)-1 {
				// Trailing backslash: the command continues on the next line.
				// (Inside single quotes a backslash is literal, but a trailing
				// one still cannot end the line meaningfully, so treat both the
				// same.)
				if quote != '\'' {
					s.continued = true
				}
				break
			}
			if quote != '\'' {
				i++ // skip the escaped byte
				continue
			}
		}

		switch quote {
		case 0:
			switch c {
			case '\'', '"':
				quote = c
			case '#':
				// An unquoted # in a word-initial position starts a comment;
				// nothing after it can open a heredoc or a string.
				if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' ||
					line[i-1] == ';' || line[i-1] == '&' || line[i-1] == '|' ||
					line[i-1] == '(' {
					goto endOfLine
				}
			case '<':
				if n, ok := parseHeredocOpener(line, i); ok {
					s.heredocs = append(s.heredocs, n.doc)
					i = n.next - 1
				}
			}
		case '\'':
			if c == '\'' {
				quote = 0
			}
		case '"':
			if c == '"' {
				quote = 0
			}
		}
	}

endOfLine:
	s.openQuote = quote
	// Heredoc bodies begin on the line after the opener — but only once any
	// continuation has finished, and never while a string is still open.
	if len(s.heredocs) > 0 && quote == 0 && !s.continued {
		s.inHeredoc = true
	}
}

// heredocOpen is a parsed `<<WORD` opener.
type heredocOpen struct {
	doc  heredoc
	next int // index just past the delimiter
}

// parseHeredocOpener recognises `<<WORD`, `<<-WORD`, `<<'WORD'`, `<<"WORD"` at
// position i (which must be the first '<'). It deliberately rejects `<<<`
// (a here-STRING, which has no body) and a bare `<` redirect.
func parseHeredocOpener(line string, i int) (heredocOpen, bool) {
	if i+1 >= len(line) || line[i+1] != '<' {
		return heredocOpen{}, false
	}
	j := i + 2
	if j < len(line) && line[j] == '<' {
		return heredocOpen{}, false // <<< here-string
	}
	var tabs bool
	if j < len(line) && line[j] == '-' {
		tabs = true
		j++
	}
	for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
		j++
	}
	if j >= len(line) {
		return heredocOpen{}, false
	}

	// The delimiter may be quoted (which turns off expansion inside the body —
	// irrelevant here, but the quotes are not part of the terminator).
	var delim strings.Builder
	switch line[j] {
	case '\'', '"':
		q := line[j]
		j++
		for j < len(line) && line[j] != q {
			delim.WriteByte(line[j])
			j++
		}
		if j < len(line) {
			j++ // closing quote
		}
	default:
		for j < len(line) && !isDelimTerminator(line[j]) {
			if line[j] == '\\' && j+1 < len(line) {
				j++
			}
			delim.WriteByte(line[j])
			j++
		}
	}
	if delim.Len() == 0 {
		return heredocOpen{}, false
	}
	return heredocOpen{doc: heredoc{delim: delim.String(), tabs: tabs}, next: j}, true
}

// isDelimTerminator reports whether c ends an unquoted heredoc delimiter word.
func isDelimTerminator(c byte) bool {
	switch c {
	case ' ', '\t', ';', '&', '|', '<', '>', '(', ')':
		return true
	}
	return false
}
