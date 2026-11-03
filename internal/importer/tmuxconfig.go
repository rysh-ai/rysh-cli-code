// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"fmt"
	"math"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParseTmuxConfig builds a plan from a tmuxinator or tmuxp project file,
// telling them apart by shape: tmuxp names its session `session_name` and its
// windows `window_name`; tmuxinator keys each window by its name.
func ParseTmuxConfig(data []byte, home string) (*Plan, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse tmux project file: %w", err)
	}
	switch {
	case doc["session_name"] != nil:
		return parseTmuxp(doc, home)
	case doc["windows"] != nil || doc["tabs"] != nil:
		if doc["name"] != nil || doc["project_name"] != nil {
			return parseTmuxinator(doc, home)
		}
	}
	return nil, fmt.Errorf("not a tmuxinator or tmuxp file (expected `name:` + `windows:`, or `session_name:`)")
}

// str returns v as a string ("" for nil and non-scalars).
func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int, int64, float64, bool:
		return fmt.Sprint(t)
	}
	return ""
}

// strList reads a string or a list of strings.
func strList(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		var out []string
		for _, e := range t {
			if m, ok := e.(map[string]any); ok { // tmuxp: {cmd: "…"}
				e = m["cmd"]
			}
			if s := str(e); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	if s := str(v); s != "" {
		return []string{s}
	}
	return nil
}

func parseTmuxinator(doc map[string]any, home string) (*Plan, error) {
	name := str(doc["name"])
	if name == "" {
		name = str(doc["project_name"])
	}
	root := str(doc["root"])
	if root == "" {
		root = str(doc["project_root"])
	}
	root = homePath(root, home)
	pre := strList(doc["pre_window"])
	if pre == nil {
		pre = strList(doc["pre_tab"])
	}
	windows, _ := doc["windows"].([]any)
	if windows == nil {
		windows, _ = doc["tabs"].([]any)
	}
	if len(windows) == 0 {
		return nil, fmt.Errorf("tmuxinator project %q has no windows", name)
	}
	p := &Plan{Source: "tmux", Name: name, Inputs: []string{}}
	if doc["pre"] != nil || doc["on_project_start"] != nil {
		p.note("project-level pre / on_project_start hooks are not imported (they ran once in tmux, outside any pane)")
	}
	for i, w := range windows {
		wm, ok := w.(map[string]any)
		if !ok || len(wm) != 1 {
			return nil, fmt.Errorf("tmuxinator window %d: want a one-key map `- <name>: …`", i+1)
		}
		var wname string
		var wval any
		for k, v := range wm {
			wname, wval = k, v
		}
		wroot, layout := root, ""
		var panes []string
		switch v := wval.(type) {
		case nil:
			panes = []string{""}
		case string:
			panes = []string{v}
		case map[string]any:
			wroot = joinCwd(root, str(v["root"]), home)
			layout = str(v["layout"])
			if v["pre"] != nil {
				p.note("tab %q: the window `pre` command is not imported", wname)
			}
			list, _ := v["panes"].([]any)
			for _, pane := range list {
				panes = append(panes, tmuxinatorPane(pane))
			}
			if len(panes) == 0 {
				panes = []string{""}
			}
		default:
			return nil, fmt.Errorf("tmuxinator window %q: unsupported value", wname)
		}
		var built []Pane
		for _, cmd := range panes {
			built = append(built, Pane{Cwd: wroot, Command: joinCommands(append(append([]string{}, pre...), cmd)...)})
		}
		p.Tabs = append(p.Tabs, arrange(p, wname, layout, built))
	}
	return p, nil
}

// tmuxinatorPane flattens one pane entry: a command, nothing, a list of
// commands, or `name: command(s)` (the name is a tmux title; the commands run).
func tmuxinatorPane(v any) string {
	switch t := v.(type) {
	case map[string]any:
		for _, cmds := range t {
			return joinCommands(strList(cmds)...)
		}
		return ""
	default:
		return joinCommands(strList(v)...)
	}
}

func parseTmuxp(doc map[string]any, home string) (*Plan, error) {
	name := str(doc["session_name"])
	root := homePath(str(doc["start_directory"]), home)
	before := strList(doc["shell_command_before"])
	windows, _ := doc["windows"].([]any)
	if len(windows) == 0 {
		return nil, fmt.Errorf("tmuxp session %q has no windows", name)
	}
	p := &Plan{Source: "tmux", Name: name, Inputs: []string{}}
	if doc["before_script"] != nil {
		p.note("before_script is not imported (it ran once in tmux, outside any pane)")
	}
	for i, w := range windows {
		wm, ok := w.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tmuxp window %d: want a map", i+1)
		}
		wname := str(wm["window_name"])
		if wname == "" {
			wname = fmt.Sprintf("window %d", i+1)
		}
		wroot := joinCwd(root, str(wm["start_directory"]), home)
		wbefore := append(append([]string{}, before...), strList(wm["shell_command_before"])...)
		list, _ := wm["panes"].([]any)
		if len(list) == 0 {
			list = []any{nil}
		}
		var built []Pane
		for _, pane := range list {
			cwd, cmds := wroot, []string(nil)
			switch t := pane.(type) {
			case nil:
			case string:
				if t != "blank" && t != "pane" {
					cmds = []string{t}
				}
			case map[string]any:
				cwd = joinCwd(wroot, str(t["start_directory"]), home)
				cmds = append(strList(t["shell_command_before"]), strList(t["shell_command"])...)
			}
			built = append(built, Pane{Cwd: cwd, Command: joinCommands(append(append([]string{}, wbefore...), cmds...)...)})
		}
		p.Tabs = append(p.Tabs, arrange(p, wname, str(wm["layout"]), built))
	}
	return p, nil
}

// arrange lays panes out in lanes according to a tmux layout name (or a raw
// #{window_layout} string), noting every approximation on the plan.
func arrange(p *Plan, tab, layout string, panes []Pane) Tab {
	groups := namedLayout(p, tab, layout, len(panes))
	t := Tab{Name: tab}
	for _, g := range groups {
		var lane Lane
		for _, i := range g {
			lane.Panes = append(lane.Panes, panes[i])
		}
		t.Lanes = append(t.Lanes, lane)
	}
	return t
}

// namedLayout returns pane indexes per lane for a layout over n panes.
func namedLayout(p *Plan, tab, layout string, n int) [][]int {
	seq := func(from, to int) []int {
		var out []int
		for i := from; i < to; i++ {
			out = append(out, i)
		}
		return out
	}
	switch layout {
	case "", "even-vertical":
		return [][]int{seq(0, n)}
	case "even-horizontal":
		var out [][]int
		for i := 0; i < n; i++ {
			out = append(out, []int{i})
		}
		return out
	case "main-vertical":
		if n == 1 {
			return [][]int{{0}}
		}
		return [][]int{{0}, seq(1, n)}
	case "main-horizontal":
		if n > 1 {
			p.note("tab %q: main-horizontal (main pane above a row) is shown as one lane, main pane first", tab)
		}
		return [][]int{seq(0, n)}
	case "tiled":
		cols := int(math.Ceil(math.Sqrt(float64(n))))
		if n > 1 {
			p.note("tab %q: tiled is approximated as %d lane(s) of stacked panes", tab, cols)
		}
		var out [][]int
		next := 0
		for c := 0; c < cols && next < n; c++ {
			size := (n - next + (cols - c) - 1) / (cols - c)
			out = append(out, seq(next, next+size))
			next += size
		}
		return out
	}
	if strings.ContainsAny(layout, "{[") || strings.Count(layout, ",") >= 3 {
		if node, err := ParseTmuxLayout(layout); err == nil {
			lanes, flat := lanesFromLayout(node)
			if flat {
				p.note("tab %q: a split nested inside a lane was flattened into that lane (rysh lanes stack panes top to bottom)", tab)
			}
			// A saved layout string names tmux pane ids, which mean nothing in
			// a fresh session: place panes by reading order instead.
			var out [][]int
			next := 0
			for _, l := range lanes {
				var g []int
				for range l {
					if next < n {
						g = append(g, next)
						next++
					}
				}
				if len(g) > 0 {
					out = append(out, g)
				}
			}
			if next < n {
				if len(out) == 0 {
					out = append(out, nil)
				}
				out[len(out)-1] = append(out[len(out)-1], seq(next, n)...)
			}
			return out
		}
	}
	p.note("tab %q: unknown layout %q; panes are stacked in one lane", tab, layout)
	return [][]int{seq(0, n)}
}
