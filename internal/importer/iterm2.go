// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"
)

// ParseITerm2 builds a plan from an iTerm2 saved window arrangement, in an
// XML property list: either iTerm2's preferences file (its "Window
// Arrangements" key; arrangement picks one, else "Default Arrangement Name",
// else the only one) or a bare array of windows. A binary plist is refused
// with the plutil command that converts it.
//
// In an arrangement a Splitter with isVertical set has a vertical divider —
// panes side by side — and becomes lanes; otherwise its panes stack. A pane's
// start command is its profile's Command, only when the profile says Custom
// Command: Yes (otherwise iTerm2 runs the login shell, and so does rysh).
func ParseITerm2(data []byte, home, arrangement string) (*Plan, error) {
	if bytes.HasPrefix(data, []byte("bplist")) {
		return nil, fmt.Errorf("binary plist: convert it first with  plutil -convert xml1 -o arrangement.xml <file>")
	}
	root, err := parsePlist(data)
	if err != nil {
		return nil, fmt.Errorf("parse iterm2 plist: %w", err)
	}
	p := &Plan{Source: "iterm2", Inputs: []string{}}
	windows, ok := root.([]any)
	if !ok {
		d, isDict := root.(map[string]any)
		if !isDict {
			return nil, fmt.Errorf("not an iTerm2 arrangement: want a dict or an array of windows")
		}
		arrs, _ := d["Window Arrangements"].(map[string]any)
		if len(arrs) == 0 {
			return nil, fmt.Errorf("no saved window arrangements in this plist")
		}
		name := arrangement
		if name == "" {
			name, _ = d["Default Arrangement Name"].(string)
		}
		if _, ok := arrs[name]; !ok {
			if arrangement != "" || len(arrs) > 1 {
				var have []string
				for k := range arrs {
					have = append(have, k)
				}
				sort.Strings(have)
				return nil, fmt.Errorf("no arrangement %q (have: %s); pick one with --arrangement", name, strings.Join(have, ", "))
			}
			for k := range arrs {
				name = k
			}
		}
		windows, _ = arrs[name].([]any)
		p.Name = name
	}
	if len(windows) > 1 {
		p.note("%d iTerm2 windows were merged into one rysh session's tabs, in order", len(windows))
	}
	n := 0
	for _, w := range windows {
		wd, _ := w.(map[string]any)
		tabs, _ := wd["Tabs"].([]any)
		for _, t := range tabs {
			n++
			td, _ := t.(map[string]any)
			rootView, _ := td["Root"].(map[string]any)
			tree := iterm2Tree(rootView, home)
			name, _ := td["Title"].(string)
			if name == "" {
				name = firstSessionName(tree)
			}
			if name == "" {
				name = fmt.Sprintf("tab %d", n)
			}
			lanes, flat := tree.lanes()
			if flat {
				p.note("tab %q: a split nested inside a lane was flattened into that lane (rysh lanes stack panes top to bottom)", name)
			}
			tab := Tab{Name: name}
			for _, l := range lanes {
				tab.Lanes = append(tab.Lanes, Lane{Panes: l})
			}
			if len(tab.Lanes) == 0 {
				tab.Lanes = []Lane{{Panes: []Pane{{}}}}
			}
			p.Tabs = append(p.Tabs, tab)
		}
	}
	if len(p.Tabs) == 0 {
		return nil, fmt.Errorf("the arrangement has no tabs")
	}
	return p, nil
}

func firstSessionName(t *paneTree) string {
	if t.pane != nil {
		return t.pane.Name
	}
	for _, c := range t.children {
		if n := firstSessionName(c); n != "" {
			return n
		}
	}
	return ""
}

func iterm2Tree(v map[string]any, home string) *paneTree {
	if v["View Type"] == "Splitter" {
		t := &paneTree{split: SplitStacked}
		if vert, _ := v["isVertical"].(bool); vert {
			t.split = SplitSideBySide
		}
		subs, _ := v["Subviews"].([]any)
		for _, s := range subs {
			if sd, ok := s.(map[string]any); ok {
				t.children = append(t.children, iterm2Tree(sd, home))
			}
		}
		return t
	}
	s, _ := v["Session"].(map[string]any)
	pane := &Pane{}
	pane.Name, _ = s["Session Name"].(string)
	if wd, _ := s["Working Directory"].(string); wd != "" {
		pane.Cwd = homePath(wd, home)
	}
	if b, ok := s["Bookmark"].(map[string]any); ok && b["Custom Command"] == "Yes" {
		pane.Command, _ = b["Command"].(string)
	}
	return &paneTree{pane: pane}
}

// parsePlist decodes an XML property list into Go values: dict ->
// map[string]any, array -> []any, string/date/data -> string, integer/real
// -> string, true/false -> bool.
func parsePlist(data []byte) (any, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil, fmt.Errorf("no <plist> element")
		}
		if err != nil {
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok {
			if se.Name.Local != "plist" {
				return nil, fmt.Errorf("root element is <%s>, want <plist>", se.Name.Local)
			}
			v, end, err := plistValue(dec)
			if err != nil {
				return nil, err
			}
			if end {
				return nil, fmt.Errorf("empty <plist>")
			}
			return v, nil
		}
	}
}

// plistValue reads the next value element; end is true when the enclosing
// element closed first.
func plistValue(dec *xml.Decoder) (v any, end bool, err error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, false, err
		}
		switch t := tok.(type) {
		case xml.EndElement:
			return nil, true, nil
		case xml.StartElement:
			switch t.Name.Local {
			case "dict":
				m := map[string]any{}
				for {
					key, end, err := plistKey(dec)
					if err != nil {
						return nil, false, err
					}
					if end {
						return m, false, nil
					}
					val, vend, err := plistValue(dec)
					if err != nil {
						return nil, false, err
					}
					if vend {
						return nil, false, fmt.Errorf("dict key %q has no value", key)
					}
					m[key] = val
				}
			case "array":
				var a []any
				for {
					val, end, err := plistValue(dec)
					if err != nil {
						return nil, false, err
					}
					if end {
						return a, false, nil
					}
					a = append(a, val)
				}
			case "true", "false":
				if err := dec.Skip(); err != nil {
					return nil, false, err
				}
				return t.Name.Local == "true", false, nil
			case "string", "integer", "real", "date", "data":
				var s string
				if err := dec.DecodeElement(&s, &t); err != nil {
					return nil, false, err
				}
				return s, false, nil
			default:
				return nil, false, fmt.Errorf("unknown plist element <%s>", t.Name.Local)
			}
		}
	}
}

func plistKey(dec *xml.Decoder) (key string, end bool, err error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false, err
		}
		switch t := tok.(type) {
		case xml.EndElement:
			return "", true, nil
		case xml.StartElement:
			if t.Name.Local != "key" {
				return "", false, fmt.Errorf("dict: want <key>, got <%s>", t.Name.Local)
			}
			var s string
			if err := dec.DecodeElement(&s, &t); err != nil {
				return "", false, err
			}
			return s, false, nil
		}
	}
}
