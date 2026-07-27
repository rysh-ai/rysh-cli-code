package actors

import "testing"

// Phase 1 (bash-shell-mode): sanitizeShellChunk keeps SGR color/style
// sequences while removing every other escape family, and collapses \r
// rewrites with terminal overwrite semantics.

func TestSanitizeShellChunkKeepsSGR(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain text unchanged",
			in:   "hello world\n",
			want: "hello world\n",
		},
		{
			name: "simple color kept",
			in:   "\x1b[32mgreen\x1b[0m plain\n",
			want: "\x1b[32mgreen\x1b[0m plain\n",
		},
		{
			name: "256-color and truecolor kept",
			in:   "\x1b[38;5;208morange\x1b[m \x1b[38;2;10;20;30mrgb\x1b[0m\n",
			want: "\x1b[38;5;208morange\x1b[m \x1b[38;2;10;20;30mrgb\x1b[0m\n",
		},
		{
			name: "colon-form direct color kept",
			in:   "\x1b[38:2:1:2:3mx\x1b[0m",
			want: "\x1b[38:2:1:2:3mx\x1b[0m",
		},
		{
			name: "cursor movement removed",
			in:   "a\x1b[2Ab\x1b[10;20Hc",
			want: "abc",
		},
		{
			name: "erase-line removed, color kept",
			in:   "\x1b[K\x1b[1;31mbold red\x1b[0m",
			want: "\x1b[1;31mbold red\x1b[0m",
		},
		{
			name: "OSC title removed",
			in:   "\x1b]0;window title\x07ls\n",
			want: "ls\n",
		},
		{
			name: "bracketed paste and kitty protocol removed",
			in:   "\x1b[?2004hx\x1b[>1uy",
			want: "xy",
		},
		{
			name: "DCS removed",
			in:   "\x1bPq#0;stuff\x1b\\after",
			want: "after",
		},
		{
			name: "charset designator and BEL/BS/SOH/STX removed",
			in:   "\x1b(Bab\x07c\x08d\x01e\x02f",
			want: "abcdef",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeShellChunk(tc.in); got != tc.want {
				t.Errorf("sanitizeShellChunk(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCollapseCarriageReturns(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no CR untouched", "abc\ndef", "abc\ndef"},
		{"CRLF normalized", "abc\r\ndef\r\n", "abc\ndef\n"},
		{"overwrite keeps last segment", "10%\r20%\r100% done\n", "100% done\n"},
		{"trailing bare CR collapses to empty", "spinner frame\r", ""},
		{"CR overwrite with color", "\x1b[33m10%\x1b[0m\rdone\n", "done\n"},
		{"multiple lines each collapsed", "a\rb\nc\rd\n", "b\nd\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := collapseCarriageReturns(tc.in); got != tc.want {
				t.Errorf("collapseCarriageReturns(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeShellChunkProgressBar(t *testing.T) {
	// A curl/npm-style progress line: many \r rewrites within one chunk, then
	// a final state and newline. Only the final state must remain.
	in := "\x1b[K 10%\r\x1b[K 55%\r\x1b[K100%\ndone\n"
	want := "100%\ndone\n"
	if got := sanitizeShellChunk(in); got != want {
		t.Errorf("sanitizeShellChunk(%q) = %q, want %q", in, got, want)
	}
}
