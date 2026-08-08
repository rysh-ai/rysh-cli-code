package main

// `rysh exec` — the generic door into the "##" command language (design 021 §3.3).
//
// Before this, the only non-interactive way to run a ## command was the
// `rysh --<name> ...` flag form, gated on ryshCmdNames: a literal map in this
// file that had to be edited by hand every time a command was added. It had
// drifted to 31 of the 52 words the dispatch table actually answers to, so
// ##secret, ##var, ##mode, ##policy, ##worktree, ##mcp, ##forge and nine others
// could not be reached from a script at all.
//
// `rysh exec` takes the command as text and sends it straight to the table, so
// there is nothing to keep in sync. The flag forms survive as aliases and are
// now generated from the table itself (see ryshCommandWords).
//
// Exit codes:
//
//	0  the command ran and reported success
//	1  the command ran and reported failure, OR we could not reach the session
//
// Only commands whose table entry sets statusAware report failure reliably;
// --json surfaces that as "status_aware" so a script can tell a meaningful 0
// from an uninstrumented one rather than trusting it blindly.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/actors"
	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// execResult is the --json line. Kept deliberately small: a script that wants
// more should ask the daemon, not parse prose.
type execResult struct {
	OK          bool   `json:"ok"`
	Status      int    `json:"status"`
	Command     string `json:"command"`
	Output      string `json:"output,omitempty"`
	Error       string `json:"error,omitempty"`
	StatusAware bool   `json:"status_aware"`
}

// runExecCmd implements `rysh exec [flags] [--] <## command>`.
func runExecCmd(cfg config.Config, args []string) error {
	// args[0] == "exec".
	rest := args[1:]

	rest, sess := extractStringFlag(rest, "--session")
	rest, tabID := extractStringFlag(rest, "--tab-id")
	rest, paneID := extractStringFlag(rest, "--pane-id")
	rest, asJSON := extractBoolFlag(rest, "--json")

	// Everything after "--" is the command body verbatim, so a body may begin
	// with a dash without being mistaken for a flag.
	if i := indexOf(rest, "--"); i >= 0 {
		rest = rest[i+1:]
	}

	command := strings.TrimSpace(strings.Join(rest, " "))
	command = strings.TrimPrefix(command, "##")
	if command == "" {
		return errors.New(progname.Rewrite(
			"usage: rysh exec [--session <name>] [--tab-id <id>] [--pane-id <id>] [--json] -- '##<command> [args...]'"))
	}

	if sess == "" {
		sess = cfg.SessionName
	}
	store, err := session.NewStore(cfg)
	if err != nil {
		return err
	}

	resp, err := cli.RyshCommandResult(store, sess, tabID, paneID, command)
	if err != nil {
		// Transport failure: the command never ran. Distinct from a command
		// that ran and refused, though both exit 1 — a script reading --json
		// sees ok=false with no output.
		if asJSON {
			printExecJSON(execResult{Status: 1, Command: command, Error: err.Error()})
			os.Exit(1)
		}
		return err
	}

	aware := actors.RyshCommandIsStatusAware(commandWord(command))

	if asJSON {
		status := 0
		if !resp.OK {
			status = 1
		}
		printExecJSON(execResult{
			OK:          resp.OK,
			Status:      status,
			Command:     command,
			Output:      resp.Output,
			Error:       resp.Error,
			StatusAware: aware,
		})
		if !resp.OK {
			os.Exit(1)
		}
		return nil
	}

	out := strings.TrimRight(resp.Output, "\n")
	if out != "" {
		fmt.Println(out)
	}
	if !resp.OK {
		// The daemon's own prose already explains the failure — repeating it as
		// "rysh: <error>" on stderr says the same thing twice, which in a
		// script that catches the failure is pure noise. Exit non-zero quietly
		// when the command explained itself; speak up only when it did not, so
		// a failure is never silent.
		if out == "" {
			if resp.Error != "" {
				return errors.New(resp.Error)
			}
			return errors.New("rysh command failed")
		}
		os.Exit(1)
	}
	return nil
}

// printExecJSON writes the result as one line on stdout.
func printExecJSON(r execResult) {
	fmt.Println(jsonLine(r))
}

// jsonLine marshals v to a single line, degrading to a minimal error object
// rather than printing nothing — a script parsing stdout should always get
// valid JSON.
func jsonLine(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"ok":false,"status":1,"error":%q}`, err.Error())
	}
	return string(b)
}

// commandWord returns the leading command word of a ## command body, which is
// what the dispatch table is keyed on.
func commandWord(command string) string {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// indexOf returns the position of want in args, or -1.
func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}
