// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"fmt"
	"strings"
)

// zellijSkipped are layout-level nodes that describe templates or floating
// panes, not the tabs a layout opens.
var zellijSkipped = map[string]bool{
	"default_tab_template": true, "tab_template": true, "pane_template": true,
	"new_tab_template": true, "swap_tiled_layout": true, "swap_floating_layout": true,
}

// ParseZellij builds a plan from a Zellij KDL layout. Each `tab` becomes a
// tab; a layout with no tabs becomes one tab called name. A pane split with
// split_direction="vertical" (children side by side) becomes lanes; the
// default horizontal split stacks panes in a lane. Plugin panes (tab bar,
// status bar, file browser) have no rysh equivalent and are skipped.
func ParseZellij(data []byte, home, name string) (*Plan, error) {
	nodes, err := ParseKDL(data)
	if err != nil {
		return nil, fmt.Errorf("parse zellij layout: %w", err)
	}
	var layout *KDLNode
	for _, n := range nodes {
		if n.Name == "layout" {
			layout = n
		}
	}
	if layout == nil {
		return nil, fmt.Errorf("not a zellij layout: no top-level `layout { … }` node")
	}
	p := &Plan{Source: "zellij", Name: name, Inputs: []string{}}
	root := ""
	var tabs, loose []*KDLNode
	for _, c := range layout.Children {
		switch {
		case c.Name == "cwd" && len(c.Args) > 0:
			root = homePath(c.Args[0], home)
		case c.Name == "tab":
			tabs = append(tabs, c)
		case c.Name == "pane":
			loose = append(loose, c)
		case c.Name == "floating_panes":
			p.note("floating panes are not imported (rysh has no floating panes)")
		case zellijSkipped[c.Name]:
		}
	}
	if len(tabs) == 0 {
		tabs = []*KDLNode{{Name: "tab", Props: map[string]string{"name": name}, Children: loose}}
	}
	for i, t := range tabs {
		tname := t.Props["name"]
		if tname == "" {
			tname = fmt.Sprintf("tab %d", i+1)
		}
		cwd := joinCwd(root, t.Props["cwd"], home)
		tree := zellijTree(p, tname, t, cwd, home)
		lanes, flat := tree.lanes()
		if flat {
			p.note("tab %q: a split nested inside a lane was flattened into that lane (rysh lanes stack panes top to bottom)", tname)
		}
		tab := Tab{Name: tname}
		for _, l := range lanes {
			tab.Lanes = append(tab.Lanes, Lane{Panes: l})
		}
		if len(tab.Lanes) == 0 {
			tab.Lanes = []Lane{{Panes: []Pane{{Cwd: cwd}}}}
		}
		p.Tabs = append(p.Tabs, tab)
	}
	return p, nil
}

// paneTree is a layout split tree whose leaves are rysh panes.
type paneTree struct {
	split    Split
	pane     *Pane
	children []*paneTree
}

func (t *paneTree) leaves() []Pane {
	if t.pane != nil {
		return []Pane{*t.pane}
	}
	var out []Pane
	for _, c := range t.children {
		out = append(out, c.leaves()...)
	}
	return out
}

// lanes maps the tree onto lanes of stacked panes, as lanesFromLayout does
// for tmux.
func (t *paneTree) lanes() (lanes [][]Pane, flattened bool) {
	// A container holding a single split is that split.
	for t.pane == nil && len(t.children) == 1 && t.children[0].pane == nil {
		t = t.children[0]
	}
	columns := []*paneTree{t}
	if t.pane == nil && t.split == SplitSideBySide {
		columns = t.children
	}
	for _, col := range columns {
		if l := col.leaves(); len(l) > 0 {
			lanes = append(lanes, l)
		}
		if col.pane == nil {
			if col.split == SplitSideBySide && col != t {
				flattened = true
			}
			for _, c := range col.children {
				if c.pane == nil {
					flattened = true
				}
			}
		}
	}
	return lanes, flattened
}

// zellijTree converts a tab or pane node and its pane children.
func zellijTree(p *Plan, tab string, n *KDLNode, cwd, home string) *paneTree {
	var panes []*KDLNode
	plugin := false
	for _, c := range n.Children {
		switch c.Name {
		case "pane":
			panes = append(panes, c)
		case "plugin":
			plugin = true
		}
	}
	if n.Name == "pane" {
		cwd = joinCwd(cwd, n.Props["cwd"], home)
	}
	if plugin || n.Props["plugin"] != "" {
		p.note("tab %q: a plugin pane was skipped (rysh has no Zellij plugins)", tab)
		return &paneTree{split: SplitStacked}
	}
	if n.Name == "pane" && len(panes) == 0 {
		return &paneTree{pane: &Pane{Name: n.Props["name"], Cwd: cwd, Command: zellijCommand(n)}}
	}
	t := &paneTree{split: SplitStacked}
	if n.Props["split_direction"] == "vertical" {
		t.split = SplitSideBySide
	}
	for _, c := range panes {
		sub := zellijTree(p, tab, c, cwd, home)
		if sub.pane == nil && len(sub.children) == 0 {
			continue // a skipped plugin pane
		}
		t.children = append(t.children, sub)
	}
	return t
}

// zellijCommand renders a pane's command="…" plus its `args` child, or its
// edit="file" (opened in $EDITOR, as Zellij does).
func zellijCommand(n *KDLNode) string {
	if f := n.Props["edit"]; f != "" {
		return "${EDITOR:-vi} " + shellWord(f)
	}
	cmd := n.Props["command"]
	if cmd == "" {
		return ""
	}
	words := []string{shellWord(cmd)}
	for _, c := range n.Children {
		if c.Name == "args" {
			for _, a := range c.Args {
				words = append(words, shellWord(a))
			}
		}
	}
	return strings.Join(words, " ")
}

// shellWord quotes s for a POSIX shell only when it needs it.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=@%+,") == "" {
		return s
	}
	return shellQuote(s)
}

// shellQuote renders s as one single-quoted POSIX shell word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
