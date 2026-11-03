// SPDX-License-Identifier: Apache-2.0

package importer

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

type warpConfig struct {
	Name    string `yaml:"name"`
	Windows []struct {
		Tabs []struct {
			Title  string     `yaml:"title"`
			Layout warpLayout `yaml:"layout"`
		} `yaml:"tabs"`
	} `yaml:"windows"`
}

type warpLayout struct {
	Cwd            string `yaml:"cwd"`
	SplitDirection string `yaml:"split_direction"`
	Commands       []struct {
		Exec string `yaml:"exec"`
	} `yaml:"commands"`
	Panes []warpLayout `yaml:"panes"`
}

// ParseWarp builds a plan from a Warp launch configuration. Every tab of
// every window becomes a rysh tab. split_direction "vertical" (a vertical
// divider: panes side by side) becomes lanes, as Zellij's and iTerm2's
// vertical splits do; "horizontal" stacks panes in a lane.
func ParseWarp(data []byte, home string) (*Plan, error) {
	var cfg warpConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse warp launch configuration: %w", err)
	}
	if len(cfg.Windows) == 0 {
		return nil, fmt.Errorf("not a warp launch configuration: no windows")
	}
	p := &Plan{Source: "warp", Name: cfg.Name, Inputs: []string{}}
	if len(cfg.Windows) > 1 {
		p.note("%d Warp windows were merged into one rysh session's tabs, in order", len(cfg.Windows))
	}
	n := 0
	for _, w := range cfg.Windows {
		for _, t := range w.Tabs {
			n++
			name := t.Title
			if name == "" {
				name = fmt.Sprintf("tab %d", n)
			}
			lanes, flat := warpTree(t.Layout, "", home).lanes()
			if flat {
				p.note("tab %q: a split nested inside a lane was flattened into that lane (rysh lanes stack panes top to bottom)", name)
			}
			tab := Tab{Name: name}
			for _, l := range lanes {
				tab.Lanes = append(tab.Lanes, Lane{Panes: l})
			}
			p.Tabs = append(p.Tabs, tab)
		}
	}
	if len(p.Tabs) == 0 {
		return nil, fmt.Errorf("warp launch configuration %q has no tabs", cfg.Name)
	}
	return p, nil
}

func warpTree(l warpLayout, parentCwd, home string) *paneTree {
	cwd := joinCwd(parentCwd, l.Cwd, home)
	if len(l.Panes) == 0 {
		var cmds []string
		for _, c := range l.Commands {
			cmds = append(cmds, c.Exec)
		}
		return &paneTree{pane: &Pane{Cwd: cwd, Command: joinCommands(cmds...)}}
	}
	t := &paneTree{split: SplitStacked}
	if l.SplitDirection == "vertical" {
		t.split = SplitSideBySide
	}
	for _, c := range l.Panes {
		t.children = append(t.children, warpTree(c, cwd, home))
	}
	return t
}
