// SPDX-License-Identifier: Apache-2.0

// Package ansakeys turns key NAMES ("esc", "ctrl+c", "down*3") into the bytes a
// terminal would send for them, and says which of them are dangerous.
//
// It exists for `rysh ansa keys` / `##ansa keys`: the way an operator or an
// agent presses a key in another pane. The incident behind it (2026-11-01): a
// claude hung mid-turn with seven messages queued behind it, and one Esc in its
// pane would have freed it, but nothing could press Esc. `rysh ansa send`
// types text and submits it; tab-msg never touches the PTY.
//
// NAMES ONLY, NEVER RAW BYTES OR TEXT. Every key here is in a closed table, so
// an audit line that says "esc*2" says exactly what was written, and this path
// cannot be used to type a sentence into an agent. Typing text is what
// `rysh ansa send` is for, with its own composer handling. A printable
// character (a digit, "y") is refused for the same reason: it can answer a
// menu, and a menu answer belongs behind the same guard as Enter.
//
// It has no dependencies, so the CLI (for an early, local error) and the
// daemon (the authoritative check) parse with the same table.
package ansakeys

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// MaxRepeat bounds one key's repeat count ("esc*10"), and MaxTotal bounds the
// key presses in one send. Small on purpose: this is for unsticking a pane,
// not for driving one, and the daemon spends a short pause per press.
const (
	MaxRepeat = 10
	MaxTotal  = 16
)

// Press is one named key, pressed Count times.
type Press struct {
	// Name is the canonical spelling ("esc", "ctrl+c"), whatever alias was typed.
	Name  string
	Count int
	// Bytes is what one press writes to the PTY.
	Bytes []byte
	// Guarded is true for a key that can submit, confirm, complete or signal
	// something in the target. It needs --force.
	Guarded bool
}

type key struct {
	bytes   []byte
	guarded bool
}

// keys is the closed table. Arrow, Home, End use the normal-mode CSI form
// (ESC [ A); readline, zsh, Ink (claude) and vim all accept it, also while a
// program has asked for application cursor mode.
//
// SAFE (no --force): keys that move, delete or cancel, and cannot submit,
// confirm or signal anything. Everything else is guarded, including every
// ctrl+<letter> except ctrl+u: ctrl+m is Enter on the wire, ctrl+i is Tab,
// ctrl+j is a newline, ctrl+c and ctrl+z signal the foreground job, ctrl+d
// ends a shell. A key nobody has classified is guarded, so the safe list can
// only grow by an explicit edit here.
var keys = map[string]key{
	"esc":       {[]byte{0x1b}, false},
	"backspace": {[]byte{0x7f}, false},
	"delete":    {[]byte("\x1b[3~"), false},
	"up":        {[]byte("\x1b[A"), false},
	"down":      {[]byte("\x1b[B"), false},
	"right":     {[]byte("\x1b[C"), false},
	"left":      {[]byte("\x1b[D"), false},
	"home":      {[]byte("\x1b[H"), false},
	"end":       {[]byte("\x1b[F"), false},
	"pageup":    {[]byte("\x1b[5~"), false},
	"pagedown":  {[]byte("\x1b[6~"), false},

	// Guarded. Enter submits a prompt or accepts a permission prompt; Tab
	// completes; shift+tab cycles claude's permission mode.
	"enter":     {[]byte{'\r'}, true},
	"tab":       {[]byte{'\t'}, true},
	"shift+tab": {[]byte("\x1b[Z"), true},
}

// safeCtrl lists the ctrl+<letter> keys that are safe. ctrl+u clears the input
// line; ANSA itself sends it before every typed delivery.
var safeCtrl = map[byte]bool{'u': true}

var aliases = map[string]string{
	"escape":  "esc",
	"return":  "enter",
	"backtab": "shift+tab",
	"bs":      "backspace",
	"del":     "delete",
	"pgup":    "pageup",
	"pgdn":    "pagedown",
}

// lookup resolves one lower-cased name to its canonical spelling and key.
func lookup(name string) (string, key, bool) {
	if a, ok := aliases[name]; ok {
		name = a
	}
	if k, ok := keys[name]; ok {
		return name, k, true
	}
	// ctrl+<letter>, also spelled ctrl-<letter>.
	for _, p := range []string{"ctrl+", "ctrl-"} {
		if rest, ok := strings.CutPrefix(name, p); ok && len(rest) == 1 && rest[0] >= 'a' && rest[0] <= 'z' {
			c := rest[0]
			return "ctrl+" + rest, key{bytes: []byte{c - 'a' + 1}, guarded: !safeCtrl[c]}, true
		}
	}
	return "", key{}, false
}

// Parse reads key specs: each is a name, optionally with a repeat count
// ("esc*2"). Names are case-insensitive. Anything not in the table is an
// error: there is no fallback that turns an unknown word into bytes.
func Parse(specs []string) ([]Press, error) {
	var out []Press
	total := 0
	for _, raw := range specs {
		spec := strings.ToLower(strings.TrimSpace(raw))
		if spec == "" {
			continue
		}
		if strings.HasPrefix(spec, "@") {
			return nil, fmt.Errorf("%q is a target, but a key name, not a target, was expected here; "+
				"name one target, then the keys", raw)
		}
		name, count := spec, 1
		if i := strings.IndexByte(spec, '*'); i >= 0 {
			name = spec[:i]
			n, err := strconv.Atoi(spec[i+1:])
			if err != nil || n < 1 || n > MaxRepeat {
				return nil, fmt.Errorf("bad repeat count in %q: use <key>*N with N from 1 to %d", raw, MaxRepeat)
			}
			count = n
		}
		canon, k, ok := lookup(name)
		if !ok {
			return nil, fmt.Errorf("unknown key %q; known keys: %s", name, strings.Join(Names(), ", "))
		}
		total += count
		if total > MaxTotal {
			return nil, fmt.Errorf("too many key presses: at most %d in one send", MaxTotal)
		}
		out = append(out, Press{Name: canon, Count: count, Bytes: k.bytes, Guarded: k.guarded})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no keys named; known keys: %s", strings.Join(Names(), ", "))
	}
	return out, nil
}

// Canonical spells the presses one way, for receipts and the audit line.
func Canonical(ps []Press) string {
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		if p.Count > 1 {
			parts = append(parts, fmt.Sprintf("%s*%d", p.Name, p.Count))
		} else {
			parts = append(parts, p.Name)
		}
	}
	return strings.Join(parts, " ")
}

// Total is the number of key presses, repeats included.
func Total(ps []Press) int {
	n := 0
	for _, p := range ps {
		n += p.Count
	}
	return n
}

// Writes expands the presses into one write per key press, in order. Each is
// a fresh copy, so a caller cannot change the table.
func Writes(ps []Press) [][]byte {
	var out [][]byte
	for _, p := range ps {
		for i := 0; i < p.Count; i++ {
			out = append(out, append([]byte(nil), p.Bytes...))
		}
	}
	return out
}

// GuardedNames lists the guarded keys among the presses, each once, in order.
func GuardedNames(ps []Press) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range ps {
		if p.Guarded && !seen[p.Name] {
			seen[p.Name] = true
			out = append(out, p.Name)
		}
	}
	return out
}

// Names lists the key names for help text, safe keys first.
func Names() []string {
	var safe, guarded []string
	for n, k := range keys {
		if k.guarded {
			guarded = append(guarded, n)
		} else {
			safe = append(safe, n)
		}
	}
	sort.Strings(safe)
	sort.Strings(guarded)
	return append(append(safe, "ctrl+u"), append(guarded, "ctrl+<a-z>")...)
}
