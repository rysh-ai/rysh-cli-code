// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"fmt"
	"strings"
	"unicode"
)

// KDLNode is one node of a KDL document. Values are kept as their source text
// (strings unquoted, numbers and keywords verbatim): a Zellij layout only
// needs names, strings and the occasional boolean.
type KDLNode struct {
	Name     string
	Args     []string
	Props    map[string]string
	Children []*KDLNode
}

// ParseKDL parses the subset of KDL (v1, with v2's #true/#false keywords)
// that layout files use: nodes, string/raw-string/bare arguments, key=value
// properties, child blocks, ";" and newline terminators, // and nestable /* */
// comments, /- slashdash on nodes, arguments and properties, (type)
// annotations, and "\" line continuations.
func ParseKDL(data []byte) ([]*KDLNode, error) {
	k := &kdlParser{s: []rune(string(data)), line: 1}
	nodes, err := k.nodes(false)
	if err != nil {
		return nil, fmt.Errorf("kdl line %d: %w", k.line, err)
	}
	return nodes, nil
}

type kdlParser struct {
	s    []rune
	i    int
	line int
}

func (k *kdlParser) peek() rune {
	if k.i >= len(k.s) {
		return 0
	}
	return k.s[k.i]
}

func (k *kdlParser) at(prefix string) bool {
	p := []rune(prefix)
	if k.i+len(p) > len(k.s) {
		return false
	}
	for j, r := range p {
		if k.s[k.i+j] != r {
			return false
		}
	}
	return true
}

func (k *kdlParser) advance() rune {
	r := k.s[k.i]
	k.i++
	if r == '\n' {
		k.line++
	}
	return r
}

// space skips blanks and comments. Newlines are skipped only when nl is set;
// otherwise they end a node. A "\" continues a node onto the next line.
func (k *kdlParser) space(nl bool) error {
	for k.i < len(k.s) {
		r := k.peek()
		switch {
		case r == '\n' || r == '\r':
			if !nl {
				return nil
			}
			k.advance()
		case unicode.IsSpace(r) || r == '\uFEFF':
			k.advance()
		case k.at("//"):
			for k.i < len(k.s) && k.peek() != '\n' {
				k.advance()
			}
		case k.at("/*"):
			if err := k.blockComment(); err != nil {
				return err
			}
		case r == '\\':
			k.advance()
			for k.i < len(k.s) && k.peek() != '\n' {
				if k.at("//") {
					for k.i < len(k.s) && k.peek() != '\n' {
						k.advance()
					}
					break
				}
				if !unicode.IsSpace(k.peek()) {
					return fmt.Errorf("text after a line continuation")
				}
				k.advance()
			}
			if k.i < len(k.s) {
				k.advance()
			}
		default:
			return nil
		}
	}
	return nil
}

func (k *kdlParser) blockComment() error {
	depth := 0
	for k.i < len(k.s) {
		switch {
		case k.at("/*"):
			depth++
			k.advance()
			k.advance()
		case k.at("*/"):
			depth--
			k.advance()
			k.advance()
			if depth == 0 {
				return nil
			}
		default:
			k.advance()
		}
	}
	return fmt.Errorf("unterminated /* comment")
}

// nodes parses nodes until EOF (top level) or a closing "}" (inBlock).
func (k *kdlParser) nodes(inBlock bool) ([]*KDLNode, error) {
	var out []*KDLNode
	for {
		if err := k.space(true); err != nil {
			return nil, err
		}
		for k.peek() == ';' {
			k.advance()
			if err := k.space(true); err != nil {
				return nil, err
			}
		}
		if k.i >= len(k.s) {
			if inBlock {
				return nil, fmt.Errorf("unterminated { block")
			}
			return out, nil
		}
		if k.peek() == '}' {
			if !inBlock {
				return nil, fmt.Errorf("unexpected }")
			}
			k.advance()
			return out, nil
		}
		skip := false
		if k.at("/-") {
			k.i += 2
			skip = true
			if err := k.space(true); err != nil {
				return nil, err
			}
		}
		n, err := k.node()
		if err != nil {
			return nil, err
		}
		if !skip {
			out = append(out, n)
		}
	}
}

func (k *kdlParser) node() (*KDLNode, error) {
	k.annotation()
	name, _, err := k.value()
	if err != nil {
		return nil, err
	}
	n := &KDLNode{Name: name, Props: map[string]string{}}
	for {
		if err := k.space(false); err != nil {
			return nil, err
		}
		r := k.peek()
		switch {
		case k.i >= len(k.s) || r == '\n' || r == '\r' || r == ';':
			if r == ';' {
				k.advance()
			}
			return n, nil
		case r == '}':
			return n, nil // the enclosing block closes; leave "}" for nodes()
		}
		skip := false
		if k.at("/-") {
			k.i += 2
			skip = true
			if err := k.space(false); err != nil {
				return nil, err
			}
		}
		if k.peek() == '{' {
			k.advance()
			children, err := k.nodes(true)
			if err != nil {
				return nil, err
			}
			if !skip {
				n.Children = append(n.Children, children...)
			}
			continue
		}
		k.annotation()
		v, quoted, err := k.value()
		if err != nil {
			return nil, err
		}
		if !quoted && k.peek() == '=' {
			k.advance()
			k.annotation()
			pv, _, err := k.value()
			if err != nil {
				return nil, err
			}
			if !skip {
				n.Props[v] = pv
			}
			continue
		}
		if !skip {
			n.Args = append(n.Args, v)
		}
	}
}

// annotation skips a "(type)" prefix.
func (k *kdlParser) annotation() {
	if k.peek() != '(' {
		return
	}
	for k.i < len(k.s) && k.peek() != ')' {
		k.advance()
	}
	if k.i < len(k.s) {
		k.advance()
	}
}

// value reads a quoted string, a raw string, or a bare word.
func (k *kdlParser) value() (string, bool, error) {
	switch {
	case k.peek() == '"':
		s, err := k.quoted()
		return s, true, err
	case k.peek() == 'r' && (k.at(`r"`) || k.at("r#")):
		s, err := k.raw()
		return s, true, err
	}
	start := k.i
	for k.i < len(k.s) {
		r := k.peek()
		if unicode.IsSpace(r) || strings.ContainsRune(`{}();="\`, r) || (r == '/' && (k.at("//") || k.at("/*") || k.at("/-"))) {
			break
		}
		k.advance()
	}
	if k.i == start {
		if k.i >= len(k.s) {
			return "", false, fmt.Errorf("unexpected end of input")
		}
		return "", false, fmt.Errorf("unexpected %q", k.peek())
	}
	return strings.TrimPrefix(string(k.s[start:k.i]), "#"), false, nil
}

func (k *kdlParser) quoted() (string, error) {
	k.advance() // opening "
	var b strings.Builder
	for k.i < len(k.s) {
		r := k.advance()
		switch r {
		case '"':
			return b.String(), nil
		case '\\':
			if k.i >= len(k.s) {
				break
			}
			e := k.advance()
			switch e {
			case 'n':
				b.WriteRune('\n')
			case 't':
				b.WriteRune('\t')
			case 'r':
				b.WriteRune('\r')
			default:
				b.WriteRune(e) // \" \\ \/ and anything else literal
			}
		default:
			b.WriteRune(r)
		}
	}
	return "", fmt.Errorf("unterminated string")
}

func (k *kdlParser) raw() (string, error) {
	k.advance() // r
	hashes := 0
	for k.peek() == '#' {
		hashes++
		k.advance()
	}
	if k.peek() != '"' {
		return "", fmt.Errorf("malformed raw string")
	}
	k.advance()
	closer := `"` + strings.Repeat("#", hashes)
	start := k.i
	for k.i < len(k.s) {
		if k.at(closer) {
			s := string(k.s[start:k.i])
			for range []rune(closer) {
				k.advance()
			}
			return s, nil
		}
		k.advance()
	}
	return "", fmt.Errorf("unterminated raw string")
}
