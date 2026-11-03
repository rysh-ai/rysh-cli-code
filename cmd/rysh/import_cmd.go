// SPDX-License-Identifier: Apache-2.0

package main

// rysh import — bring another tool's setup into rysh (design 065).
//
//	rysh import tmux [file] [--session NAME]   live tmux sessions, or a tmuxinator/tmuxp file
//	rysh import zellij <layout.kdl>            a Zellij KDL layout
//	rysh import warp <file>                    a Warp launch configuration
//	rysh import iterm2 <file> [--arrangement]  an iTerm2 saved arrangement (XML plist)
//	rysh import mcp [file]                     .mcp.json (default) or a Claude Desktop config
//	rysh import claude [file]                  Claude Code permissions -> policy.yaml (stricter only)
//	  [--apply] [--json] [--dir DIR]
//	rysh import --undo [--force] [--dir DIR]   restore the files the last import changed
//
// Without --apply nothing is written: the plan is printed (or emitted as JSON
// with --json). --apply backs up every file it replaces before writing, and
// --undo restores that backup. Start commands are never run by the import;
// they land in a .rysh script the user runs with `rysh script`.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/importer"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
)

// importDeps are the import's only links to the machine: the home directory
// (to show paths as $HOME/…) and the tmux client. Tests replace both.
type importDeps struct {
	home string
	tmux func(args []string) (string, error)
}

func defaultImportDeps() importDeps {
	home, _ := os.UserHomeDir()
	return importDeps{
		home: home,
		tmux: func(args []string) (string, error) {
			out, err := exec.Command("tmux", args...).Output()
			if err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) && len(ee.Stderr) > 0 {
					return "", fmt.Errorf("tmux %s: %s", args[0], strings.TrimSpace(string(ee.Stderr)))
				}
				return "", fmt.Errorf("tmux %s: %w", args[0], err)
			}
			return string(out), nil
		},
	}
}

func runImportCmd(args []string) error {
	return runImport(args, os.Stdout, defaultImportDeps())
}

const importUsage = `usage: rysh import tmux [file] [--session NAME] [--apply] [--json] [--dir DIR]
       rysh import zellij <layout.kdl> [--apply] [--json] [--dir DIR]
       rysh import warp <launch-config.yaml> [--apply] [--json] [--dir DIR]
       rysh import iterm2 <arrangement.plist> [--arrangement NAME] [--apply] [--json] [--dir DIR]
       rysh import mcp [.mcp.json | claude_desktop_config.json] [--apply] [--json] [--dir DIR]
       rysh import claude [settings.json] [--apply] [--json] [--dir DIR]
       rysh import --undo [--force] [--dir DIR]`

func runImport(args []string, out io.Writer, deps importDeps) error {
	var (
		apply, asJSON, undo, force bool
		session, dir, arrangement  string
		pos                        []string
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--apply":
			apply = true
		case "--json":
			asJSON = true
		case "--undo":
			undo = true
		case "--force":
			force = true
		case "--session", "--dir", "--arrangement":
			if i+1 >= len(args) {
				return fmt.Errorf("%s needs a value\n%s", a, progname.Rewrite(importUsage))
			}
			switch a {
			case "--session":
				session = args[i+1]
			case "--arrangement":
				arrangement = args[i+1]
			default:
				dir = args[i+1]
			}
			i++
		case "-h", "--help":
			fmt.Fprintln(out, progname.Rewrite(importUsage))
			return nil
		default:
			if strings.HasPrefix(a, "-") {
				return fmt.Errorf("unknown flag %s\n%s", a, progname.Rewrite(importUsage))
			}
			pos = append(pos, a)
		}
	}
	root := dir
	if root == "" {
		root = mustGetwd()
	}

	if undo {
		if len(pos) > 0 || apply || asJSON {
			return errors.New(progname.Rewrite("rysh import --undo takes no source"))
		}
		undoFn := importer.Undo
		if force {
			undoFn = importer.UndoForce
		}
		m, err := undoFn(root)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "undid the %s import of %s:\n", m.Source, m.Created)
		for _, f := range m.Files {
			verb := "restored"
			if !f.Existed {
				verb = "removed "
			}
			fmt.Fprintf(out, "  %s %s\n", verb, f.Path)
		}
		return nil
	}
	if force {
		return errors.New("--force only applies to --undo")
	}
	if len(pos) == 0 {
		return errors.New(progname.Rewrite(importUsage))
	}

	plan, err := buildImportPlan(root, pos, session, arrangement, deps)
	if err != nil {
		return err
	}
	plan.Context = importer.ContextFiles(root)

	if !apply {
		if asJSON {
			data, err := plan.JSON()
			if err != nil {
				return err
			}
			_, err = out.Write(data)
			return err
		}
		_, err := io.WriteString(out, progname.Rewrite(plan.Text()))
		return err
	}

	changes := plan.Changes(root)
	m, err := importer.Apply(root, plan.Source, changes, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "imported from %s (backup: %s; undo with: %s)\n", plan.Source, m.Dir, progname.Rewrite("rysh import --undo"))
	for _, c := range changes {
		fmt.Fprintf(out, "  wrote %s\n", c.Path)
		if strings.HasSuffix(c.Path, ".rysh") {
			fmt.Fprintf(out, "  build the layout inside your session with: %s\n", progname.Rewrite("rysh script "+c.Path))
		}
	}
	for _, s := range plan.Secrets {
		fmt.Fprintf(out, "  %s (%s) was not imported: %s\n", s.Name, s.Where, s.HowTo())
	}
	return nil
}

// buildImportPlan reads the source named by pos[0].
func buildImportPlan(root string, pos []string, session, arrangement string, deps importDeps) (*importer.Plan, error) {
	source := pos[0]
	if len(pos) > 2 {
		return nil, fmt.Errorf("too many arguments\n%s", progname.Rewrite(importUsage))
	}
	file := ""
	if len(pos) == 2 {
		file = pos[1]
	}
	readFile := func() ([]byte, error) {
		if file == "" {
			return nil, fmt.Errorf("rysh import %s needs a file\n%s", source, progname.Rewrite(importUsage))
		}
		return os.ReadFile(file)
	}
	if arrangement != "" && source != "iterm2" {
		return nil, errors.New("--arrangement applies to iterm2 only")
	}
	if session != "" && source != "tmux" {
		return nil, errors.New("--session applies to live tmux only")
	}
	var (
		plan *importer.Plan
		err  error
	)
	switch source {
	case "tmux":
		if file == "" {
			live, terr := deps.tmux(importer.TmuxListPanesArgs)
			if terr != nil {
				return nil, terr
			}
			plan, err = importer.ParseTmuxLive(live, deps.home, session)
			break
		}
		if session != "" {
			return nil, errors.New("--session applies to live tmux only, not to a file")
		}
		data, rerr := readFile()
		if rerr != nil {
			return nil, rerr
		}
		plan, err = importer.ParseTmuxConfig(data, deps.home)
		if err == nil {
			plan.Inputs = []string{importDisplayPath(file, deps.home)}
		}
	case "zellij":
		data, rerr := readFile()
		if rerr != nil {
			return nil, rerr
		}
		name := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		plan, err = importer.ParseZellij(data, deps.home, name)
		if err == nil {
			plan.Inputs = []string{importDisplayPath(file, deps.home)}
		}
	case "warp", "iterm2":
		data, rerr := readFile()
		if rerr != nil {
			return nil, rerr
		}
		if source == "warp" {
			plan, err = importer.ParseWarp(data, deps.home)
		} else {
			plan, err = importer.ParseITerm2(data, deps.home, arrangement)
		}
		if err == nil {
			plan.Inputs = []string{importDisplayPath(file, deps.home)}
		}
	case "claude":
		var files []importer.NamedFile
		if file != "" {
			data, rerr := os.ReadFile(file)
			if rerr != nil {
				return nil, rerr
			}
			files = append(files, importer.NamedFile{Label: importDisplayPath(file, deps.home), Data: data})
		} else {
			for _, name := range []string{"settings.json", "settings.local.json"} {
				data, rerr := os.ReadFile(filepath.Join(root, ".claude", name))
				if os.IsNotExist(rerr) {
					continue
				}
				if rerr != nil {
					return nil, rerr
				}
				files = append(files, importer.NamedFile{Label: ".claude/" + name, Data: data})
			}
			if len(files) == 0 {
				return nil, fmt.Errorf("no .claude/settings.json or .claude/settings.local.json in %s; name a file: rysh import claude <settings.json>", importDisplayPath(root, deps.home))
			}
		}
		existing, rerr := os.ReadFile(filepath.Join(root, ".rysh", "policy.yaml"))
		if rerr != nil && !os.IsNotExist(rerr) {
			return nil, rerr
		}
		plan, err = importer.ParseClaude(files, existing)
	case "mcp":
		label := importDisplayPath(file, deps.home)
		if file == "" {
			file, label = filepath.Join(root, ".mcp.json"), ".mcp.json"
		}
		data, rerr := os.ReadFile(file)
		if rerr != nil {
			if os.IsNotExist(rerr) && len(pos) == 1 {
				return nil, fmt.Errorf("no .mcp.json in %s; name a file: rysh import mcp <file> (e.g. Claude Desktop's claude_desktop_config.json)", importDisplayPath(root, deps.home))
			}
			return nil, rerr
		}
		existing, rerr := os.ReadFile(filepath.Join(root, ".rysh", "mcp.json"))
		if rerr != nil && !os.IsNotExist(rerr) {
			return nil, rerr
		}
		plan, err = importer.ParseMCP(data, existing, label)
	default:
		return nil, fmt.Errorf("unknown import source %q (have: tmux, zellij, warp, iterm2, mcp, claude)\n%s", source, progname.Rewrite(importUsage))
	}
	return plan, err
}

// importDisplayPath shows an input file as $HOME/… when it lives there.
func importDisplayPath(file, home string) string {
	if abs, err := filepath.Abs(file); err == nil {
		file = abs
	}
	if home != "" && strings.HasPrefix(file, home+string(filepath.Separator)) {
		return "$HOME/" + filepath.ToSlash(strings.TrimPrefix(file, home+string(filepath.Separator)))
	}
	return file
}
