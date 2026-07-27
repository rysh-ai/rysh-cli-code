package actors

import "testing"

func TestNextToken(t *testing.T) {
	tok, rest := nextToken("  add ig-discover rest here ")
	if tok != "add" || rest != "ig-discover rest here" {
		t.Errorf("got tok=%q rest=%q", tok, rest)
	}
	tok, rest = nextToken("solo")
	if tok != "solo" || rest != "" {
		t.Errorf("single token: tok=%q rest=%q", tok, rest)
	}
	if tok, _ := nextToken("   "); tok != "" {
		t.Errorf("blank should yield empty token, got %q", tok)
	}
}

func TestNextQuotedToken(t *testing.T) {
	// Double-quoted multi-field cron spec is one token.
	tok, rest := nextQuotedToken(`"0 9 * * *" --pane research ##auto web run x`)
	if tok != "0 9 * * *" || rest != "--pane research ##auto web run x" {
		t.Errorf("got tok=%q rest=%q", tok, rest)
	}
	// Single quotes too.
	tok, _ = nextQuotedToken(`'@every 15m' rest`)
	if tok != "@every 15m" {
		t.Errorf("single-quoted: %q", tok)
	}
	// Unquoted descriptor falls back to whitespace split.
	tok, rest = nextQuotedToken("@daily some input")
	if tok != "@daily" || rest != "some input" {
		t.Errorf("unquoted: tok=%q rest=%q", tok, rest)
	}
	// Unterminated quote takes the remainder.
	tok, rest = nextQuotedToken(`"0 9 * * * unterminated`)
	if tok != "0 9 * * * unterminated" || rest != "" {
		t.Errorf("unterminated: tok=%q rest=%q", tok, rest)
	}
}

func TestTruncateCronAndDash(t *testing.T) {
	if got := truncateCron("abcdefgh", 5); got != "abcd…" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncateCron("abc", 10); got != "abc" {
		t.Errorf("no-op truncate = %q", got)
	}
	if orDash("") != "—" || orDash("x") != "x" {
		t.Error("orDash")
	}
}

func TestFirstLineCron(t *testing.T) {
	if got := firstLineCron("  line one\nline two\n"); got != "line one" {
		t.Errorf("firstLineCron = %q", got)
	}
}

func TestExtractSchedule(t *testing.T) {
	cases := []struct{ in, sched, rest string }{
		{`"0 9 * * *" --pane p echo hi`, "0 9 * * *", "--pane p echo hi"},
		{`0 9 * * * --pane p echo hi`, "0 9 * * *", "--pane p echo hi"}, // unquoted 5-field (CLI path)
		{`@every 15m echo hi`, "@every 15m", "echo hi"},                 // @every takes 2
		{`@daily echo hi`, "@daily", "echo hi"},                         // descriptor takes 1
		{`'@every 1m' rest`, "@every 1m", "rest"},                       // single-quoted
		{`30 8 * * 1-5 @tango do it`, "30 8 * * 1-5", "@tango do it"},
	}
	for _, c := range cases {
		s, r := extractSchedule(c.in)
		if s != c.sched || r != c.rest {
			t.Errorf("extractSchedule(%q) = (%q,%q), want (%q,%q)", c.in, s, r, c.sched, c.rest)
		}
	}
}
