// SPDX-License-Identifier: Apache-2.0

// Package importer implements `rysh import` (design 065): it reads another
// tool's setup — tmux sessions and tmuxinator/tmuxp files, Zellij layouts, and
// in later slices Warp, iTerm2, MCP configs and Claude Code permissions — into
// a Plan, previews that plan, and on --apply writes it into the project's
// .rysh directory behind a backup that `rysh import --undo` restores.
//
// Four rules shape every source:
//
//   - Preview is the default. Building a Plan, rendering it (Text, JSON) and
//     computing its Changes read nothing but the input and write nothing.
//   - Nothing is written without a backup (Apply), and Undo restores the last
//     import byte for byte.
//   - A secret VALUE is never read into a Plan. Secrets appear by NAME only,
//     with how to add them to rysh's own store.
//   - A start command is never run by the import. It becomes text in a .rysh
//     script that runs only when the user runs that script, in the new pane.
package importer

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Pane is one terminal pane: where it starts and what it runs there.
type Pane struct {
	Name    string `json:"name,omitempty"`
	Cwd     string `json:"cwd,omitempty"`     // "$HOME/…" for paths under the home directory
	Command string `json:"command,omitempty"` // shell text; run in the pane, never by the import
}

// Lane is one column of a rysh tab: panes stacked top to bottom.
type Lane struct {
	Panes []Pane `json:"panes"`
}

// Tab is one rysh tab: lanes left to right.
type Tab struct {
	Name  string `json:"name"`
	Lanes []Lane `json:"lanes"`
}

// Secret is a credential the source refers to, by NAME only. Where says what
// referred to it (e.g. `mcp server "github" env`), never what its value is.
type Secret struct {
	Name  string `json:"name"`
	Where string `json:"where"`
	How   string `json:"how,omitempty"` // how to give rysh the value; default: ##secret new
}

// HowTo is the instruction shown for a secret.
func (s Secret) HowTo() string {
	if s.How != "" {
		return s.How
	}
	return "add it with: ##secret new " + s.Name + " <value>"
}

// Plan is everything an import would create. It is pure data: building and
// rendering it has no side effects.
type Plan struct {
	Source  string      `json:"source"`
	Name    string      `json:"name,omitempty"` // project/session name; names the written script
	Inputs  []string    `json:"inputs"`
	Tabs    []Tab       `json:"tabs,omitempty"`
	Secrets []Secret    `json:"secrets,omitempty"`
	Context []string    `json:"context,omitempty"` // CLAUDE.md / AGENTS.md found; left in place
	MCP     []MCPEntry  `json:"mcp,omitempty"`     // servers added to .rysh/mcp.json
	Policy  *PolicyPlan `json:"policy,omitempty"`  // rules added to .rysh/policy.yaml
	Notes   []string    `json:"notes,omitempty"`

	mcpExisting []json.RawMessage // .rysh/mcp.json entries kept as they are
}

// note records an approximation or a skipped item once.
func (p *Plan) note(format string, args ...any) {
	n := fmt.Sprintf(format, args...)
	for _, have := range p.Notes {
		if have == n {
			return
		}
	}
	p.Notes = append(p.Notes, n)
}

// contextFileNames are agent instruction files rysh's agents already read in
// place. They are listed in the preview and never copied or rewritten.
var contextFileNames = []string{"AGENTS.md", "CLAUDE.md"}

// ContextFiles lists the agent instruction files present at root, sorted.
func ContextFiles(root string) []string {
	var out []string
	for _, name := range contextFileNames {
		if fi, err := os.Stat(filepath.Join(root, name)); err == nil && fi.Mode().IsRegular() {
			out = append(out, name)
		}
	}
	return out
}

// Text renders the preview. The output depends only on the Plan, so the same
// input always previews the same way.
func (p *Plan) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "rysh import %s — preview (nothing is written without --apply)\n", p.Source)
	for _, in := range p.Inputs {
		fmt.Fprintf(&b, "  from: %s\n", in)
	}
	if len(p.Tabs) > 0 {
		lanes, panes := p.counts()
		fmt.Fprintf(&b, "\nLayout: %d tab(s), %d lane(s), %d pane(s)\n", len(p.Tabs), lanes, panes)
		for ti, tab := range p.Tabs {
			fmt.Fprintf(&b, "  tab %d %q\n", ti+1, tab.Name)
			for li, lane := range tab.Lanes {
				fmt.Fprintf(&b, "    lane %d\n", li+1)
				for pi, pane := range lane.Panes {
					fmt.Fprintf(&b, "      pane %d", pi+1)
					if pane.Name != "" {
						fmt.Fprintf(&b, " %q", pane.Name)
					}
					cwd := pane.Cwd
					if cwd == "" {
						cwd = "(session directory)"
					}
					fmt.Fprintf(&b, "  cwd: %s\n", cwd)
					if pane.Command != "" {
						fmt.Fprintf(&b, "              start: %s\n", pane.Command)
					}
				}
			}
		}
		b.WriteString("  Start commands are not run now: they run when you run the generated script.\n")
	}
	if len(p.MCP) > 0 {
		fmt.Fprintf(&b, "\nMCP servers: %d to add to .rysh/mcp.json (each gated: requires_approval)\n", len(p.MCP))
		for _, e := range p.MCP {
			if e.Transport == "http" {
				fmt.Fprintf(&b, "  %s  http  %s\n", e.Name, e.URL)
				continue
			}
			fmt.Fprintf(&b, "  %s  stdio  %s\n", e.Name, strings.TrimSpace(e.Command+" "+strings.Join(e.Args, " ")))
		}
	}
	if p.Policy != nil {
		b.WriteString("\nPolicy (.rysh/policy.yaml; an import only makes it stricter):\n")
		if len(p.Policy.Gate) > 0 {
			b.WriteString("  add to approval.always_gate (rysh asks before running):\n")
			for _, g := range p.Policy.Gate {
				fmt.Fprintf(&b, "    %q\n", g)
			}
		}
		if len(p.Policy.Suggest) > 0 {
			b.WriteString("  suggested auto_approve rules, written as REVIEW comments only (not active):\n")
			for _, s := range p.Policy.Suggest {
				fmt.Fprintf(&b, "    %q\n", s)
			}
		}
	}
	if len(p.Secrets) > 0 {
		b.WriteString("\nSecrets and env (names only; no value was read or copied):\n")
		for _, s := range p.Secrets {
			fmt.Fprintf(&b, "  %s  — %s; %s\n", s.Name, s.Where, s.HowTo())
		}
	}
	if len(p.Context) > 0 {
		b.WriteString("\nAgent instruction files (left where they are; rysh agents already read them):\n")
		for _, c := range p.Context {
			fmt.Fprintf(&b, "  %s\n", c)
		}
	}
	if len(p.Notes) > 0 {
		b.WriteString("\nNotes:\n")
		for _, n := range p.Notes {
			fmt.Fprintf(&b, "  - %s\n", n)
		}
	}
	if ch := p.changePaths(); len(ch) > 0 {
		b.WriteString("\nWith --apply this writes (after backing up what it replaces):\n")
		for _, c := range ch {
			fmt.Fprintf(&b, "  %s\n", c)
		}
		b.WriteString("Undo with: rysh import --undo\n")
	} else {
		b.WriteString("\nNothing to write.\n")
	}
	return b.String()
}

// JSON renders the plan as indented JSON with a trailing newline, including
// the paths --apply would write.
func (p *Plan) JSON() ([]byte, error) {
	type out struct {
		*Plan
		Writes []string `json:"writes"`
	}
	w := p.changePaths()
	if w == nil {
		w = []string{}
	}
	data, err := json.MarshalIndent(out{p, w}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func (p *Plan) counts() (lanes, panes int) {
	for _, t := range p.Tabs {
		lanes += len(t.Lanes)
		for _, l := range t.Lanes {
			panes += len(l.Panes)
		}
	}
	return lanes, panes
}

// Changes is what --apply would write under root, as paths relative to root.
// It reads nothing from disk; it only depends on the plan.
func (p *Plan) Changes(root string) []Change {
	var out []Change
	if len(p.Tabs) > 0 {
		out = append(out, Change{Path: p.scriptPath(), Data: []byte(Script(p))})
	}
	if len(p.MCP) > 0 {
		out = append(out, Change{Path: ".rysh/mcp.json", Data: p.mcpStore()})
	}
	if p.Policy != nil {
		out = append(out, Change{Path: ".rysh/policy.yaml", Data: p.Policy.rendered})
	}
	return out
}

func (p *Plan) changePaths() []string {
	var out []string
	for _, c := range p.Changes("") {
		out = append(out, c.Path)
	}
	sort.Strings(out)
	return out
}

var slugUnsafe = regexp.MustCompile(`[^a-z0-9]+`)

// slug lowercases s and keeps [a-z0-9] runs joined by "-".
func slug(s string) string {
	return strings.Trim(slugUnsafe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// scriptPath is the .rysh script a layout import writes:
// .rysh/imports/<source>[-<name>].rysh.
func (p *Plan) scriptPath() string {
	base := slug(p.Source)
	if n := slug(p.Name); n != "" {
		base += "-" + n
	}
	return ".rysh/imports/" + base + ".rysh"
}

// homePath renders an absolute path under home as "$HOME/…", expands a
// leading "~", and leaves any other path as it is.
func homePath(path, home string) string {
	switch {
	case path == "":
		return ""
	case path == "~":
		return "$HOME"
	case strings.HasPrefix(path, "~/"):
		return "$HOME/" + strings.TrimPrefix(path, "~/")
	case strings.HasPrefix(path, "$HOME"):
		return path
	}
	if home != "" {
		h := strings.TrimRight(home, "/")
		if path == h {
			return "$HOME"
		}
		if strings.HasPrefix(path, h+"/") {
			return "$HOME/" + strings.TrimPrefix(path, h+"/")
		}
	}
	return path
}

// joinCwd resolves a child directory against its parent's: absolute (or
// home-relative) children stand alone, relative ones join.
func joinCwd(parent, child, home string) string {
	child = homePath(child, home)
	if child == "" {
		return parent
	}
	if strings.HasPrefix(child, "/") || strings.HasPrefix(child, "$HOME") || parent == "" {
		return child
	}
	return strings.TrimSuffix(filepath.ToSlash(filepath.Join(parent, child)), "/")
}

// joinCommands chains commands the way tmuxinator and tmuxp send them — one
// after another, each regardless of the last — dropping empty ones.
func joinCommands(cmds ...string) string {
	var keep []string
	for _, c := range cmds {
		if c = strings.TrimSpace(c); c != "" {
			keep = append(keep, c)
		}
	}
	return strings.Join(keep, "; ")
}
