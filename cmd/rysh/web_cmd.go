// SPDX-License-Identifier: Apache-2.0

package main

// `rysh web` — the top-level web-viewer command design 007 promised. The web
// server always runs INSIDE a session's daemon (it is a WorkspaceActor child
// serving that session's bus), so this command does not boot a server: it
// drives the in-daemon `##rysh web start|stop` through the same path as
// `rysh send`. What it adds over raw send is a printable URL: the daemon prints
// its address into the pane, which is invisible for a detached/headless
// session.
//
// The UI is guarded by a username/password login, so a session with no login
// stored yet needs one passing through: `--username`/`--password` are forwarded
// to the daemon, which stores them for later starts.

import (
	"errors"
	"fmt"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"strconv"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// webCmdDefaultPort mirrors the daemon-side default (workspace_web_args.go).
const webCmdDefaultPort = 23232

// firstFlagVal returns the value of whichever of the given flag names appears
// first with a value — flagVal takes a single name, and the login flags each
// have a short alias.
func firstFlagVal(args []string, names ...string) string {
	for _, n := range names {
		if v := flagVal(args, n); v != "" {
			return v
		}
	}
	return ""
}

// buildWebStartCommand composes the in-daemon `##rysh web start` invocation.
// Pure so the flag plumbing is testable without a session.
func buildWebStartCommand(port int, host, username, password string, control bool) string {
	var b strings.Builder
	b.WriteString("##rysh web start")
	if port > 0 {
		b.WriteString(" --port " + strconv.Itoa(port))
	}
	if host != "" {
		b.WriteString(" --host " + host)
	}
	if username != "" {
		b.WriteString(" --username " + username)
	}
	if password != "" {
		b.WriteString(" --password " + password)
	}
	if control {
		b.WriteString(" --control")
	}
	return b.String()
}

// webCmdURL renders the URL the daemon will serve, matching the daemon's
// control-mode loopback downgrade so we never print an address that the
// server refuses to bind.
func webCmdURL(port int, host string, control bool) string {
	if host == "" || control {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%d/", host, port)
}

// runWebCmd implements `rysh web start|stop <session> [flags]`.
func runWebCmd(cfg config.Config, args []string) error {
	usage := errors.New(progname.Rewrite("usage: rysh web start <session-name> [--control] [--port <n>] [--host <h>] [--username <u> --password <p>]\n") +
		progname.Rewrite("       rysh web stop <session-name>"))
	if len(args) < 3 {
		return usage
	}
	action, sessName := args[1], args[2]
	store, err := session.NewStore(cfg)
	if err != nil {
		return err
	}

	switch action {
	case "start":
		flags := args[3:]
		port := webCmdDefaultPort
		if v := flagVal(flags, "--port"); v != "" {
			p, err := strconv.Atoi(v)
			if err != nil || p <= 0 || p > 65535 {
				return fmt.Errorf("invalid --port %q", v)
			}
			port = p
		}
		host := flagVal(flags, "--host")
		control := hasFlag(flags, "--control")
		username := firstFlagVal(flags, "--username", "--user")
		password := firstFlagVal(flags, "--password", "--pass")
		// Half a login never reaches the daemon: it would be rejected there,
		// one process and one pane away from the person who mistyped it.
		if (username == "") != (password == "") {
			return errors.New("pass both --username and --password, or neither to use the stored login")
		}

		cmd := buildWebStartCommand(port, host, username, password, control)
		if err := cli.PaneSendInput(store, sessName, "", cmd, "shell"); err != nil {
			return err
		}
		fmt.Printf("requested web viewer in session %q\n", sessName)
		fmt.Printf("  open:   %s\n", webCmdURL(port, host, control))
		if username != "" {
			fmt.Printf("  login:  %s (stored for this workspace)\n", username)
		} else if !control {
			fmt.Printf("  login:  the workspace's stored login — set one with `##rysh web auth` if the start is refused\n")
		}
		if control {
			fmt.Printf("  mode:   control (bind forced to 127.0.0.1)\n")
		}
		fmt.Printf(progname.Rewrite("  verify: rysh send %s '##rysh web status'\n"), sessName)
		return nil

	case "stop":
		if err := cli.PaneSendInput(store, sessName, "", "##rysh web stop", "shell"); err != nil {
			return err
		}
		fmt.Printf("requested web viewer stop in session %q\n", sessName)
		return nil

	default:
		return usage
	}
}
