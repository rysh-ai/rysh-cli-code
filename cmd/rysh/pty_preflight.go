// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/platform"
)

// ptylessCommands are the subcommands that do useful work without ever opening
// a pane: registry, evaluation, session bookkeeping, and talking to a daemon
// that lives somewhere else (e.g. `rysh send` to a session running in WSL).
//
// Anything NOT in this set is treated as session-opening — including the
// no-argument form and a bare session name — so a new PTY-backed command is
// refused by default on a host that cannot allocate one, rather than silently
// slipping through.
var ptylessCommands = map[string]bool{
	"detach":         true,
	"list-sessions":  true,
	"delete-session": true,
	"stop":           true,
	"send":           true,
	"exec":           true,
	"script":         true,
	"prompt":         true,
	"install":        true,
	"list-packages":  true,
	// Registry operations, exactly the same class as install/list-packages:
	// they read or write ~/.rysh and the package index and never touch a pane.
	"search":         true,
	"update":         true,
	// `board` talks to a daemon over NATS — post, reply, tail — which is the
	// case this list's own comment names for `send`. It was added after this
	// map was written and never added to it, so native Windows refused
	// `rysh board` with the session-start guidance, even for a session running
	// in WSL. Caught the first time anything ever executed the Windows binary.
	"board":          true,
	"eval":           true,
	"forge":          true,
	"onboard":        true,
	"doctor":         true,
	"channel":        true,
	"version":        true,
	"--version":      true,
	"-V":             true,
	"help":           true,
	"-h":             true,
	"--help":         true,
}

// requirePTY refuses a session-opening command on a host with no PTY support.
//
// Without this, a native-Windows build starts a session, renders a UI, and then
// fails on the first pane with a low-level "operation not supported" — after
// writing a session record and spawning a daemon. Failing up front, naming the
// supported path, is the honest behaviour (design 011: Windows ships as WSL2
// until ConPTY is implemented).
func requirePTY(args []string) error {
	if platform.PTYSupported {
		return nil
	}
	if len(args) > 0 && ptylessCommands[strings.ToLower(args[0])] {
		return nil
	}
	return fmt.Errorf(progname.Rewrite(`cannot start a rysh session: %s

Every rysh pane is a PTY-backed shell, so sessions cannot run here.

The supported path on Windows is WSL2, where the Linux build runs unmodified:
    wsl --install
    wsl
    curl -fsSL https://packages.rysh.ai/install.sh | sh
See docs/wsl.md for the tested setup.

This build can still run the commands that need no pane, for example:
    rysh send <session> <input>     talk to a session running in WSL
    rysh list-sessions
    rysh install <package>
    rysh eval`), platform.PTYUnsupportedReason)
}
