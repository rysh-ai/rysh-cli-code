// SPDX-License-Identifier: Apache-2.0

package ansakeys

import (
	"bytes"
	"strings"
	"testing"
)

// The bytes each key name must produce. Written out by hand rather than derived
// from the table, so a typo in the table cannot also be a typo in the test.
func TestParseMapsEveryNameToItsBytes(t *testing.T) {
	cases := []struct {
		spec string
		want []byte
	}{
		{"esc", []byte{0x1b}},
		{"escape", []byte{0x1b}},
		{"enter", []byte{'\r'}},
		{"return", []byte{'\r'}},
		{"tab", []byte{'\t'}},
		{"shift+tab", []byte("\x1b[Z")},
		{"backtab", []byte("\x1b[Z")},
		{"backspace", []byte{0x7f}},
		{"delete", []byte("\x1b[3~")},
		{"up", []byte("\x1b[A")},
		{"down", []byte("\x1b[B")},
		{"right", []byte("\x1b[C")},
		{"left", []byte("\x1b[D")},
		{"home", []byte("\x1b[H")},
		{"end", []byte("\x1b[F")},
		{"pageup", []byte("\x1b[5~")},
		{"pagedown", []byte("\x1b[6~")},
		{"ctrl+a", []byte{0x01}},
		{"ctrl+c", []byte{0x03}},
		{"ctrl+d", []byte{0x04}},
		{"ctrl+u", []byte{0x15}},
		{"ctrl+z", []byte{0x1a}},
		{"ctrl-c", []byte{0x03}},
		// Names are case-insensitive: ESC, Esc and esc are the same key.
		{"ESC", []byte{0x1b}},
		{"Ctrl+C", []byte{0x03}},
	}
	for _, tc := range cases {
		ps, err := Parse([]string{tc.spec})
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.spec, err)
			continue
		}
		if len(ps) != 1 || ps[0].Count != 1 {
			t.Errorf("Parse(%q) = %+v, want one press", tc.spec, ps)
			continue
		}
		if !bytes.Equal(ps[0].Bytes, tc.want) {
			t.Errorf("Parse(%q) bytes = %q, want %q", tc.spec, ps[0].Bytes, tc.want)
		}
	}
}

func TestParseRepeatCount(t *testing.T) {
	ps, err := Parse([]string{"esc*2", "down*3", "enter"})
	if err != nil {
		t.Fatal(err)
	}
	if got := Canonical(ps); got != "esc*2 down*3 enter" {
		t.Errorf("Canonical = %q", got)
	}
	if got := Total(ps); got != 6 {
		t.Errorf("Total = %d, want 6", got)
	}
	// Writes expands repeats into one write per key press, in order.
	w := Writes(ps)
	want := [][]byte{{0x1b}, {0x1b}, []byte("\x1b[B"), []byte("\x1b[B"), []byte("\x1b[B"), {'\r'}}
	if len(w) != len(want) {
		t.Fatalf("Writes gave %d writes, want %d: %q", len(w), len(want), w)
	}
	for i := range want {
		if !bytes.Equal(w[i], want[i]) {
			t.Errorf("write %d = %q, want %q", i, w[i], want[i])
		}
	}
	// A write is a copy: changing it must not change the table.
	w[0][0] = 'X'
	again, _ := Parse([]string{"esc"})
	if again[0].Bytes[0] != 0x1b {
		t.Fatal("Writes handed out the table's own backing array")
	}
}

// Every refusal names what was wrong. None of these may fall through to a
// guess: an unknown key that became "some bytes" is text typed into an agent.
func TestParseRefuses(t *testing.T) {
	cases := []struct {
		name  string
		specs []string
		want  string
	}{
		{"nothing", nil, "no keys"},
		{"blank", []string{"  "}, "no keys"},
		{"unknown name", []string{"esc", "hello"}, `unknown key "hello"`},
		{"printable char", []string{"y"}, `unknown key "y"`},
		{"digit", []string{"1"}, `unknown key "1"`},
		{"raw bytes", []string{`\x1b`}, "unknown key"},
		{"ctrl non-letter", []string{"ctrl+1"}, `unknown key "ctrl+1"`},
		{"ctrl two letters", []string{"ctrl+ab"}, "unknown key"},
		{"zero repeat", []string{"esc*0"}, "repeat"},
		{"negative repeat", []string{"esc*-1"}, "repeat"},
		{"word repeat", []string{"esc*two"}, "repeat"},
		{"repeat too big", []string{"esc*11"}, "repeat"},
		{"empty repeat", []string{"esc*"}, "repeat"},
		{"too many in total", []string{"down*10", "up*7"}, "at most 16"},
		{"target typed as key", []string{"@worker"}, "a key name, not a target"},
	}
	for _, tc := range cases {
		_, err := Parse(tc.specs)
		if err == nil {
			t.Errorf("%s: Parse(%q) succeeded, want an error containing %q", tc.name, tc.specs, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Parse(%q) error %q does not contain %q", tc.name, tc.specs, err, tc.want)
		}
	}
}

// The safety classification. Safe keys cannot submit, confirm, complete or
// signal anything; every other key is guarded and needs --force. The list of
// safe keys is closed: a key nobody classified is guarded.
func TestGuardedClassification(t *testing.T) {
	safe := []string{"esc", "backspace", "delete", "up", "down", "left", "right",
		"home", "end", "pageup", "pagedown", "ctrl+u"}
	guarded := []string{"enter", "tab", "shift+tab", "ctrl+c", "ctrl+d", "ctrl+z",
		"ctrl+a", "ctrl+j", "ctrl+m", "ctrl+i", "ctrl+o", "ctrl+r"}
	for _, k := range safe {
		ps, err := Parse([]string{k})
		if err != nil {
			t.Fatalf("Parse(%q): %v", k, err)
		}
		if g := GuardedNames(ps); len(g) != 0 {
			t.Errorf("%s is classified guarded, want safe", k)
		}
	}
	for _, k := range guarded {
		ps, err := Parse([]string{k})
		if err != nil {
			t.Fatalf("Parse(%q): %v", k, err)
		}
		if g := GuardedNames(ps); len(g) != 1 || g[0] != ps[0].Name {
			t.Errorf("%s is classified safe, want guarded (got %v)", k, g)
		}
	}
	// ctrl+m is CR and ctrl+i is TAB on the wire: a harmless-looking spelling
	// of a guarded byte must not slip through. Each name is listed once.
	ps, _ := Parse([]string{"esc", "ctrl+m", "enter*2", "enter"})
	if g := GuardedNames(ps); strings.Join(g, ",") != "ctrl+m,enter" {
		t.Errorf("GuardedNames = %v, want [ctrl+m enter]", g)
	}
}

// Aliases canonicalise, so the audit line always spells a key one way.
func TestCanonicalSpelling(t *testing.T) {
	ps, err := Parse([]string{"Escape", "RETURN", "ctrl-C", "backtab"})
	if err != nil {
		t.Fatal(err)
	}
	if got := Canonical(ps); got != "esc enter ctrl+c shift+tab" {
		t.Errorf("Canonical = %q", got)
	}
}

// Help lists every base name, so a usage line never goes stale.
func TestNamesListsTheTable(t *testing.T) {
	names := strings.Join(Names(), " ")
	for _, k := range []string{"esc", "enter", "tab", "backspace", "up", "down", "left", "right", "ctrl+<a-z>"} {
		if !strings.Contains(names, k) {
			t.Errorf("Names() = %q, missing %q", names, k)
		}
	}
}
