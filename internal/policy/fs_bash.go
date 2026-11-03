// SPDX-License-Identifier: Apache-2.0

package policy

// The bash pre-check for fs: rules (design 068 S3).
//
// Before a bash tool call runs, the command text is split into shell words and
// the paths it names are checked: any word naming a deny path refuses the call,
// and a write target — a redirection (> >> 2> &>), an argument of a known
// mutating command, sed -i files, dd of= — under a read_only path refuses it.
//
// This is a guard-rail, not a boundary. A string check cannot see paths built
// at run time: variables, $(…) and `…`, globs, scripts and interpreters, a
// relative path after a cd it cannot follow. Words it cannot resolve are
// skipped, not refused. The hard guarantee is the sandbox's mounts (#13), built
// from (*FSRules).Mounts.

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

type shWord struct {
	text       string
	op         bool // control operator (; | & && || ( ) newline) or redirection (> <)
	unresolved bool // contains $, `, or an unquoted glob character
}

// shSplit splits a command line into words and operators. Quotes are
// honoured; it does not expand anything.
func shSplit(cmd string) []shWord {
	var out []shWord
	var cur strings.Builder
	inWord, unresolved := false, false
	flush := func() {
		if inWord {
			out = append(out, shWord{text: cur.String(), unresolved: unresolved})
		}
		cur.Reset()
		inWord, unresolved = false, false
	}
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\\' && i+1 < len(rs):
			i++
			cur.WriteRune(rs[i])
			inWord = true
		case c == '\'':
			inWord = true
			for i++; i < len(rs) && rs[i] != '\''; i++ {
				cur.WriteRune(rs[i])
			}
		case c == '"':
			inWord = true
			for i++; i < len(rs) && rs[i] != '"'; i++ {
				if rs[i] == '\\' && i+1 < len(rs) {
					i++
				} else if rs[i] == '$' || rs[i] == '`' {
					unresolved = true
				}
				cur.WriteRune(rs[i])
			}
		case c == ' ' || c == '\t':
			flush()
		case c == '\n' || c == ';' || c == '|' || c == '&' || c == '(' || c == ')':
			// &> and &>> are redirections, not operators.
			if c == '&' && i+1 < len(rs) && rs[i+1] == '>' && !inWord {
				i++
				for i+1 < len(rs) && rs[i+1] == '>' {
					i++
				}
				out = append(out, shWord{text: ">", op: true})
				continue
			}
			flush()
			op := string(c)
			if (c == '|' || c == '&') && i+1 < len(rs) && rs[i+1] == c {
				op += string(c)
				i++
			}
			out = append(out, shWord{text: op, op: true})
		case c == '>' || c == '<':
			// A pending word of digits is a file descriptor: 2> 2>>.
			if inWord && !unresolved && isDigits(cur.String()) {
				cur.Reset()
				inWord = false
			} else {
				flush()
			}
			op := string(c)
			for i+1 < len(rs) && (rs[i+1] == '>' || rs[i+1] == '<' || rs[i+1] == '|') {
				op += string(rs[i+1])
				i++
			}
			if op == "<<" || op == "<<<" || op == "<<-" {
				op = "<<" // here-doc / here-string: the next word is not a path
			}
			out = append(out, shWord{text: op, op: true})
		default:
			if c == '$' || c == '`' || c == '*' || c == '?' || c == '[' {
				unresolved = true
			}
			cur.WriteRune(c)
			inWord = true
		}
	}
	flush()
	return out
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Commands whose non-flag arguments are all written (or removed).
var shWritesAll = map[string]bool{
	"rm": true, "mv": true, "tee": true, "touch": true, "mkdir": true, "rmdir": true,
	"chmod": true, "chown": true, "chgrp": true, "truncate": true, "shred": true, "unlink": true,
}

// Commands that write only their last argument.
var shWritesLast = map[string]bool{"cp": true, "ln": true, "install": true, "rsync": true}

// Words skipped in front of the real command.
var shPrefixes = map[string]bool{
	"sudo": true, "env": true, "nohup": true, "time": true, "command": true,
	"exec": true, "nice": true, "doas": true, "xargs": true,
}

type shAccess struct {
	word   string
	access FSAccess
}

// CheckBash checks a bash command's named paths against the rules (see the
// file comment for what it can and cannot see). cwd anchors relative paths.
// nil rules allow everything.
func (r *FSRules) CheckBash(cmd, cwd string) error {
	if r == nil {
		return nil
	}
	words := shSplit(cmd)
	var simple []shWord
	run := func() error {
		defer func() { simple = simple[:0] }()
		var accesses []shAccess
		var args []shWord
		for i := 0; i < len(simple); i++ {
			w := simple[i]
			if w.op {
				if i+1 < len(simple) && !simple[i+1].op {
					t := simple[i+1]
					i++
					if w.text == "<<" || t.unresolved || strings.HasPrefix(t.text, "&") {
						continue
					}
					acc := FSRead
					if strings.HasPrefix(w.text, ">") {
						acc = FSWrite
					}
					accesses = append(accesses, shAccess{t.text, acc})
				}
				continue
			}
			args = append(args, w)
		}
		// Drop leading VAR=value assignments and prefixes like sudo/env.
		for len(args) > 0 {
			a := args[0].text
			if eq := strings.IndexByte(a, '='); eq > 0 && !strings.ContainsAny(a[:eq], "/.-") {
				args = args[1:]
				continue
			}
			if shPrefixes[a] {
				args = args[1:]
				for len(args) > 0 && strings.HasPrefix(args[0].text, "-") {
					args = args[1:]
				}
				continue
			}
			break
		}
		if len(args) == 0 {
			return r.checkAccesses(accesses, cwd)
		}
		name := filepath.Base(args[0].text)
		if strings.Contains(args[0].text, "/") && !args[0].unresolved {
			accesses = append(accesses, shAccess{args[0].text, FSRead})
		}
		var operands []string
		sedInPlace := false
		for _, a := range args[1:] {
			if a.unresolved {
				continue
			}
			if strings.HasPrefix(a.text, "-") {
				if name == "sed" && strings.HasPrefix(a.text, "-i") {
					sedInPlace = true
				}
				continue
			}
			if name == "dd" {
				if v, ok := strings.CutPrefix(a.text, "of="); ok {
					accesses = append(accesses, shAccess{v, FSWrite})
					continue
				}
				if v, ok := strings.CutPrefix(a.text, "if="); ok {
					accesses = append(accesses, shAccess{v, FSRead})
					continue
				}
			}
			operands = append(operands, a.text)
		}
		for i, o := range operands {
			acc := FSRead
			switch {
			case shWritesAll[name]:
				acc = FSWrite
			case shWritesLast[name] && i == len(operands)-1 && len(operands) > 1:
				acc = FSWrite
			case name == "sed" && sedInPlace && i > 0:
				acc = FSWrite
			}
			accesses = append(accesses, shAccess{o, acc})
		}
		if err := r.checkAccesses(accesses, cwd); err != nil {
			return err
		}
		// Follow a literal cd for the commands after it.
		if name == "cd" && len(operands) == 1 {
			cwd = shResolve(operands[0], cwd)
		}
		return nil
	}
	for _, w := range words {
		if w.op && !strings.HasPrefix(w.text, ">") && !strings.HasPrefix(w.text, "<") {
			if err := run(); err != nil {
				return err
			}
			continue
		}
		simple = append(simple, w)
	}
	return run()
}

func (r *FSRules) checkAccesses(accesses []shAccess, cwd string) error {
	for _, a := range accesses {
		if a.word == "" {
			continue
		}
		if err := r.Check(shResolve(a.word, cwd), a.access); err != nil {
			return err
		}
	}
	return nil
}

// shResolve anchors a word: ~ and ~/ to the home dir, a relative word to cwd.
// The join keeps ".." so Check sees it resolved the way the shell would.
func shResolve(word, cwd string) string {
	if word == "~" || strings.HasPrefix(word, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return h + strings.TrimPrefix(word, "~")
		}
	}
	if filepath.IsAbs(word) {
		return word
	}
	return cwd + string(filepath.Separator) + word
}

// CheckBashFS checks a bash command against the active rules and logs a
// refusal like the other policy refusals. nil means the command may run.
func CheckBashFS(cmd, cwd string) error {
	err := ActiveFS().CheckBash(cmd, cwd)
	if err != nil {
		var d *FSDeniedError
		rule := ""
		if errors.As(err, &d) {
			rule = d.Rule
		}
		slog.Error("policy: bash command refused by fs rules", "rule", rule, "cwd", cwd)
	}
	return err
}
