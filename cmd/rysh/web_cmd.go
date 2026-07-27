package main

// `rysh web` — the top-level web-viewer command design 007 promised. The web
// server always runs INSIDE a session's daemon (it is a WorkspaceActor child
// serving that session's bus), so this command does not boot a server: it
// drives the in-daemon `##rysh web start|stop` through the same path as
// `rysh send`. What it adds over raw send is a complete, printable URL: the
// daemon prints its access token into the pane, which is invisible for a
// detached/headless session — so `rysh web start` generates the token
// CLIENT-side, passes it explicitly, and prints the full ?token= URL here.

import (
	"errors"
	"fmt"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"strconv"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
	"github.com/rysh-ai/rysh-cli-code/internal/web"
)

// webCmdDefaultPort mirrors the daemon-side default (workspace_web_args.go).
const webCmdDefaultPort = 23232

// buildWebStartCommand composes the in-daemon `##rysh web start` invocation.
// Pure so the flag plumbing is testable without a session.
func buildWebStartCommand(port int, host, token string, control, noToken bool) string {
	var b strings.Builder
	b.WriteString("##rysh web start")
	if port > 0 {
		b.WriteString(" --port " + strconv.Itoa(port))
	}
	if host != "" {
		b.WriteString(" --host " + host)
	}
	switch {
	case noToken:
		b.WriteString(" --no-token")
	case token != "":
		b.WriteString(" --token " + token)
	}
	if control {
		b.WriteString(" --control")
	}
	return b.String()
}

// webCmdURL renders the URL the daemon will serve, matching the daemon's
// control-mode loopback downgrade so we never print an address that the
// server refuses to bind.
func webCmdURL(port int, host, token string, control bool) string {
	if host == "" || control {
		host = "127.0.0.1"
	}
	u := fmt.Sprintf("http://%s:%d/", host, port)
	if token != "" {
		u += "?token=" + token
	}
	return u
}

// runWebCmd implements `rysh web start|stop <session> [flags]`.
func runWebCmd(cfg config.Config, args []string) error {
	usage := errors.New(progname.Rewrite("usage: rysh web start <session-name> [--control] [--port <n>] [--host <h>] [--no-token]\n") +
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
		noToken := hasFlag(flags, "--no-token", "--no-auth")
		token := flagVal(flags, "--token")
		if token == "" && !noToken {
			token = web.GenerateToken()
		}

		cmd := buildWebStartCommand(port, host, token, control, noToken)
		if err := cli.PaneSendInput(store, sessName, "", cmd, "shell"); err != nil {
			return err
		}
		fmt.Printf("requested web viewer in session %q\n", sessName)
		fmt.Printf("  open:   %s\n", webCmdURL(port, host, token, control))
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
