// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// `##rysh restart` and `rysh restart <session>` — pick up a freshly built rysh
// without leaving the session.
//
// After a new feature lands in the client, the binary on disk is new but the
// session's daemon (and the TUI attached to it) are still the old build. A
// restart is always a full one: it behaves as `--upgrade --force`, because the
// point is to run the new binary, and a restart the user typed is the answer
// to the upgrade guard's "panes are running" question.

// restartRequest is returned by runAttachUI when the TUI quit on
// `##rysh restart`. main() answers it by re-execing the binary.
type restartRequest struct{ session string }

func (r *restartRequest) Error() string {
	return fmt.Sprintf("restart of session %q requested", r.session)
}

// restartAttachArgs is the command line the TUI re-execs itself as:
// `rysh attach <session> --upgrade --force`, keeping --config and --log-level
// so the new daemon loads exactly what the old one did.
func restartAttachArgs(sessionName, configPath, logLevel string) []string {
	args := []string{"attach", sessionName, "--upgrade", "--force"}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	if logLevel != "" {
		args = append(args, "--log-level", logLevel)
	}
	return args
}

// reexecAttach replaces this process with the binary now on disk, attaching
// to the session with --upgrade --force. os.Executable names the path this
// process was started from, so a rebuild that replaced the file there is what
// runs next — the new client and, through --upgrade, the new daemon.
func reexecAttach(sessionName, configPath, logLevel string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("restart: resolve executable path: %w", err)
	}
	args := restartAttachArgs(sessionName, configPath, logLevel)
	fmt.Fprintf(os.Stderr, progname.Rewrite("[restarting session %q: rysh %s]\n"), sessionName, strings.Join(args, " "))
	return execReplace(exe, args)
}

// restartSession implements `rysh restart <session>`: stop the session's
// daemon (as --upgrade --force would) and start a fresh one from this binary,
// without attaching. It is what `##rysh restart` runs when it arrives at the
// daemon from something other than a TUI (web UI, `rysh send`), and it is
// usable directly from a script. delay lets the daemon that spawned it finish
// answering the command before it is stopped.
func restartSession(cfg config.Config, sessionName, configPath string, shared bool, delay time.Duration) error {
	store, err := session.NewStore(cfg)
	if err != nil {
		return err
	}
	cfg.SessionName = sessionName
	if delay > 0 {
		time.Sleep(delay)
	}
	old, err := store.Get(sessionName)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session %q not found", sessionName)
	} else if err != nil {
		return err
	}
	if old.PID > 0 && session.ProcessAlive(old.PID) {
		if err := upgradeDaemon(sessionName, old, true); err != nil {
			return fmt.Errorf("restart: %w", err)
		}
	}
	h, err := spawnDaemon(sessionName, cfg.LogLevel, configPath, shared)
	if err != nil {
		return err
	}
	defer h.cleanup()
	rec, err := waitForSession(store, sessionName, 10*time.Second, h)
	if err != nil {
		return daemonStartError(sessionName, err)
	}
	fmt.Printf("[session %q restarted: daemon PID %d, %s]\n", sessionName, rec.PID, describeBuild(rec.Version, rec.BinHash))
	fmt.Printf(progname.Rewrite("[terminals that were attached must reattach: rysh attach %s]\n"), sessionName)
	return nil
}
