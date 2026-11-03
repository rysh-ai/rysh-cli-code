// SPDX-License-Identifier: Apache-2.0

package main

// `rysh hook emit` — the receiving end of the claude bridge (gap G8,
// internal/hooks/bridge.go). A bridged claude pane's --settings file runs it
// for PreToolUse, PostToolUse, Notification, Stop and UserPromptSubmit; it
// reads claude's hook input on stdin, runs the workspace's .rysh/hooks.yaml
// in this process (never in the daemon), and exits with claude's codes:
// 0 go on, 2 refuse the tool call.
//
//	rysh hook emit --rysh-dir <abs .rysh> [--pane-id <p>] [--session <n>] [--event <claude event>]

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rysh-ai/rysh-cli-code/internal/hooks"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
)

const hookUsage = "usage: rysh hook emit --rysh-dir <absolute .rysh dir> [--pane-id <pane>] [--session <name>] [--event <claude event>]"

func parseHookEmitArgs(args []string) (hooks.EmitOptions, error) {
	var o hooks.EmitOptions
	if len(args) < 2 || args[1] != "emit" {
		return o, errors.New(progname.Rewrite(hookUsage))
	}
	rest := args[2:]
	rest, o.RyshDir = extractStringFlag(rest, "--rysh-dir")
	rest, o.PaneID = extractStringFlag(rest, "--pane-id")
	rest, o.Session = extractStringFlag(rest, "--session")
	rest, o.Event = extractStringFlag(rest, "--event")
	if len(rest) > 0 {
		return o, fmt.Errorf("unexpected argument %q\n%s", rest[0], progname.Rewrite(hookUsage))
	}
	if o.RyshDir == "" || !filepath.IsAbs(o.RyshDir) {
		return o, errors.New(progname.Rewrite(hookUsage))
	}
	return o, nil
}

// runHookCmd returns the process exit code.
func runHookCmd(args []string) int {
	o, err := parseHookEmitArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		// A misconfigured bridge must not open the gate, nor keep claude from
		// stopping: the line's own --event decides, if it was parsed.
		return hooks.FailureExit(o.Event)
	}
	return hooks.Emit(context.Background(), os.Stdin, os.Stdout, os.Stderr, o)
}
