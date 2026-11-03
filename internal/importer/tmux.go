// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"fmt"
	"strings"
)

// TmuxListPanesFormat is the -F format `rysh import tmux` passes to
// `tmux list-panes -a`. One line per pane, tab-separated; ParseTmuxLive reads
// exactly these fields. #{pane_id} ties a pane to its leaf in
// #{window_layout}, which names panes by id, not by index.
const TmuxListPanesFormat = "#{session_name}\t#{window_index}\t#{window_name}\t#{window_layout}\t#{pane_id}\t#{pane_current_path}\t#{pane_start_command}"

// TmuxListPanesArgs is the full argv (after "tmux") for a live import.
var TmuxListPanesArgs = []string{"list-panes", "-a", "-F", TmuxListPanesFormat}

const tmuxFields = 7

// Split is how a layout node divides its area among its children.
type Split int

const (
	SplitNone       Split = iota // a leaf: one pane
	SplitSideBySide              // tmux "{…}": children left to right
	SplitStacked                 // tmux "[…]": children top to bottom
)

// LayoutNode is one cell of a tmux window layout.
type LayoutNode struct {
	Split    Split
	PaneID   string // leaves only: the number in #{pane_id} without "%"
	Children []*LayoutNode
}

// leaves lists the pane ids of n in reading order.
func (n *LayoutNode) leaves() []string {
	if n.Split == SplitNone {
		return []string{n.PaneID}
	}
	var out []string
	for _, c := range n.Children {
		out = append(out, c.leaves()...)
	}
	return out
}

// ParseTmuxLayout parses a #{window_layout} string, e.g.
// "b25d,238x58,0,0{119x58,0,0,1,118x58,120,0[…]}". The leading checksum and
// every cell's geometry are read past; only the split tree and pane ids are
// kept.
func ParseTmuxLayout(s string) (*LayoutNode, error) {
	comma := strings.IndexByte(s, ',')
	if comma != 4 {
		return nil, fmt.Errorf("tmux layout %q: missing checksum", s)
	}
	lp := &layoutParser{s: s, i: comma + 1}
	n, err := lp.cell()
	if err != nil {
		return nil, fmt.Errorf("tmux layout %q: %w", s, err)
	}
	if lp.i != len(s) {
		return nil, fmt.Errorf("tmux layout %q: trailing text at %d", s, lp.i)
	}
	return n, nil
}

type layoutParser struct {
	s string
	i int
}

// number reads a decimal run.
func (lp *layoutParser) number() (string, error) {
	start := lp.i
	for lp.i < len(lp.s) && lp.s[lp.i] >= '0' && lp.s[lp.i] <= '9' {
		lp.i++
	}
	if lp.i == start {
		return "", fmt.Errorf("expected a number at %d", start)
	}
	return lp.s[start:lp.i], nil
}

func (lp *layoutParser) expect(c byte) error {
	if lp.i >= len(lp.s) || lp.s[lp.i] != c {
		return fmt.Errorf("expected %q at %d", c, lp.i)
	}
	lp.i++
	return nil
}

// cell parses WxH,X,Y followed by ",<id>" (a leaf) or a {…} / […] group.
func (lp *layoutParser) cell() (*LayoutNode, error) {
	if _, err := lp.number(); err != nil {
		return nil, err
	}
	if err := lp.expect('x'); err != nil {
		return nil, err
	}
	for _, sep := range []byte{0, ',', ','} {
		if sep != 0 {
			if err := lp.expect(sep); err != nil {
				return nil, err
			}
		}
		if _, err := lp.number(); err != nil {
			return nil, err
		}
	}
	if lp.i >= len(lp.s) {
		return nil, fmt.Errorf("unexpected end at %d", lp.i)
	}
	switch lp.s[lp.i] {
	case ',':
		lp.i++
		id, err := lp.number()
		if err != nil {
			return nil, err
		}
		return &LayoutNode{Split: SplitNone, PaneID: id}, nil
	case '{', '[':
		open := lp.s[lp.i]
		closer, split := byte('}'), SplitSideBySide
		if open == '[' {
			closer, split = ']', SplitStacked
		}
		lp.i++
		n := &LayoutNode{Split: split}
		for {
			c, err := lp.cell()
			if err != nil {
				return nil, err
			}
			n.Children = append(n.Children, c)
			if lp.i < len(lp.s) && lp.s[lp.i] == ',' {
				lp.i++
				continue
			}
			if err := lp.expect(closer); err != nil {
				return nil, err
			}
			return n, nil
		}
	}
	return nil, fmt.Errorf("unexpected %q at %d", lp.s[lp.i], lp.i)
}

// lanesFromLayout maps a layout tree onto rysh's tab shape (lanes of stacked
// panes). A side-by-side root becomes one lane per child; a stacked root is a
// single lane. Anything nested deeper than rysh can show is flattened into its
// lane in reading order, and the caller is told so (flattened == true).
func lanesFromLayout(n *LayoutNode) (lanes [][]string, flattened bool) {
	var columns []*LayoutNode
	if n.Split == SplitSideBySide {
		columns = n.Children
	} else {
		columns = []*LayoutNode{n}
	}
	for _, col := range columns {
		lanes = append(lanes, col.leaves())
		if col.Split == SplitStacked {
			for _, c := range col.Children {
				if c.Split != SplitNone {
					flattened = true
				}
			}
		} else if col.Split == SplitSideBySide {
			flattened = true
		}
	}
	return lanes, flattened
}

type tmuxPane struct {
	id, cwd, cmd string
}

type tmuxWindow struct {
	session, index, name, layout string
	panes                        []tmuxPane
}

// ParseTmuxLive builds a plan from `tmux list-panes -a -F
// TmuxListPanesFormat` output. Each window becomes a tab. With more than one
// session in the output, tabs are named "<session>:<window>"; session limits
// the import to one session.
func ParseTmuxLive(out, home, session string) (*Plan, error) {
	var windows []*tmuxWindow
	byKey := map[string]*tmuxWindow{}
	for ln, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != tmuxFields {
			return nil, fmt.Errorf("tmux list-panes line %d: %d fields, want %d", ln+1, len(f), tmuxFields)
		}
		if session != "" && f[0] != session {
			continue
		}
		key := f[0] + "\x00" + f[1]
		w := byKey[key]
		if w == nil {
			w = &tmuxWindow{session: f[0], index: f[1], name: f[2], layout: f[3]}
			byKey[key] = w
			windows = append(windows, w)
		}
		w.panes = append(w.panes, tmuxPane{
			id:  strings.TrimPrefix(f[4], "%"),
			cwd: homePath(f[5], home),
			cmd: unquoteTmuxCommand(f[6]),
		})
	}
	if len(windows) == 0 {
		if session != "" {
			return nil, fmt.Errorf("tmux has no session %q", session)
		}
		return nil, fmt.Errorf("tmux reported no panes (is the tmux server running?)")
	}

	multi := false
	first := windows[0].session
	for _, w := range windows {
		if w.session != first {
			multi = true
		}
	}
	p := &Plan{Source: "tmux", Inputs: []string{"tmux " + strings.Join(TmuxListPanesArgs[:2], " ")}}
	if session != "" {
		p.Name = session
	} else if !multi {
		p.Name = first
	}
	for _, w := range windows {
		name := w.name
		if multi {
			name = w.session + ":" + w.name
		}
		tab := Tab{Name: name}
		byID := map[string]tmuxPane{}
		for _, tp := range w.panes {
			byID[tp.id] = tp
		}
		node, err := ParseTmuxLayout(w.layout)
		if err != nil {
			return nil, fmt.Errorf("window %s:%s: %w", w.session, w.index, err)
		}
		lanes, flat := lanesFromLayout(node)
		if flat {
			p.note("tab %q: a split nested inside a lane was flattened into that lane (rysh lanes stack panes top to bottom)", name)
		}
		placed := map[string]bool{}
		for _, ids := range lanes {
			var lane Lane
			for _, id := range ids {
				tp, ok := byID[id]
				if !ok {
					continue
				}
				placed[id] = true
				lane.Panes = append(lane.Panes, Pane{Cwd: tp.cwd, Command: tp.cmd})
			}
			if len(lane.Panes) > 0 {
				tab.Lanes = append(tab.Lanes, lane)
			}
		}
		// A pane the layout string does not name still gets a place.
		for _, tp := range w.panes {
			if !placed[tp.id] {
				if len(tab.Lanes) == 0 {
					tab.Lanes = append(tab.Lanes, Lane{})
				}
				last := &tab.Lanes[len(tab.Lanes)-1]
				last.Panes = append(last.Panes, Pane{Cwd: tp.cwd, Command: tp.cmd})
			}
		}
		p.Tabs = append(p.Tabs, tab)
	}
	return p, nil
}

// unquoteTmuxCommand undoes the quoting tmux applies when it prints a start
// command given as a single argument ("vim ." -> vim .). Anything else — an
// unquoted command or one built from several quoted words — is kept as is.
func unquoteTmuxCommand(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s
	}
	inner := s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if c == '\\' && i+1 < len(inner) {
			i++
			b.WriteByte(inner[i])
			continue
		}
		if c == '"' {
			return s // several quoted words: leave it to the shell
		}
		b.WriteByte(c)
	}
	return b.String()
}
