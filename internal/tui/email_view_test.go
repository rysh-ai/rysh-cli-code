package tui

import "testing"

func TestReplySubject(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Hello there", "Re: Hello there"},
		{"Re: Hello", "Re: Hello"},
		{"re: lowercase", "re: lowercase"},
		{"  spaced  ", "Re: spaced"},
		{"", "Re: "},
	}
	for _, c := range cases {
		if got := replySubject(c.in); got != c.want {
			t.Errorf("replySubject(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestShortFrom(t *testing.T) {
	cases := []struct {
		in   string
		w    int
		want string
	}{
		{"GitHub <noreply@github.com>", 20, "GitHub"},
		{"jane@example.com", 20, "jane"},
		{"\"Jane Doe\" <jane@x.io>", 20, "Jane Doe"},
		{"averylongdisplayname", 6, "averyl"},
		{"  spaced@host  ", 20, "spaced"},
	}
	for _, c := range cases {
		if got := shortFrom(c.in, c.w); got != c.want {
			t.Errorf("shortFrom(%q, %d) = %q, want %q", c.in, c.w, got, c.want)
		}
	}
}

func TestWrapPlain(t *testing.T) {
	got := wrapPlain("abcdef", 3)
	want := []string{"abc", "def"}
	if len(got) != len(want) {
		t.Fatalf("wrapPlain len = %d (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("wrapPlain[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// Explicit newlines are preserved as separate lines.
	if lines := wrapPlain("a\nb", 10); len(lines) != 2 || lines[0] != "a" || lines[1] != "b" {
		t.Errorf("wrapPlain newline handling = %v", lines)
	}
	// Always at least one line.
	if lines := wrapPlain("", 5); len(lines) != 1 {
		t.Errorf("wrapPlain(\"\") = %v, want one empty line", lines)
	}
}

func TestPadTrunc(t *testing.T) {
	if got := padTrunc("hi", 5); got != "hi   " {
		t.Errorf("padTrunc pad = %q, want %q", got, "hi   ")
	}
	if got := padTrunc("hello world", 5); got != "hello" {
		t.Errorf("padTrunc trunc = %q, want %q", got, "hello")
	}
	if got := padTrunc("x", 0); got != "" {
		t.Errorf("padTrunc(_, 0) = %q, want empty", got)
	}
}
