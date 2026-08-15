// SPDX-License-Identifier: Apache-2.0

package session

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// SpawnDaemon launches a detached `rysh daemon <name>` process for an existing
// (but currently stopped) session, mirroring the spawn path the CLI uses on
// attach. workDir is the working directory the daemon starts in (normally the
// session's recorded Path); configFile, when non-empty, is propagated via
// --config so the spawned daemon resolves the same rysh directory / registry as
// the caller.
//
// The child inherits the caller's environment (so RYSH_SESSION_SOURCE and other
// RYSH_* settings carry over) with RYSH_SESSION pinned to name. It starts in its
// own session/process group (see daemonSysProcAttr) with no controlling
// terminal and is released, so it outlives the caller — exactly like the CLI's
// own daemon spawn.
func SpawnDaemon(name, workDir, configFile string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("session name is empty")
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	args := []string{"daemon", name}
	if strings.TrimSpace(configFile) != "" {
		args = append(args, "--config", configFile)
	}
	cmd := exec.Command(exe, args...)
	daemonSysProcAttr(cmd)
	if strings.TrimSpace(workDir) != "" {
		cmd.Dir = workDir
	}
	// Inherit the caller's environment (front-end source, API keys, …) but pin
	// the session name so the child is unambiguous.
	cmd.Env = append(os.Environ(), "RYSH_SESSION="+name)
	// Detach from the terminal — the daemon redirects its own stdio.
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn daemon: %w", err)
	}
	_ = cmd.Process.Release()
	return nil
}

// WaitReady polls the registry until the named session's daemon has recorded a
// live PID and NATS port (i.e. it is ready to accept connections), or until the
// timeout elapses. It is the same readiness gate the CLI uses after spawning a
// daemon.
func WaitReady(store *Store, name string, timeout time.Duration) (Record, error) {
	deadline := time.Now().Add(timeout)
	for {
		rec, err := store.Get(name)
		if err == nil && rec.NATSPort > 0 && rec.PID > 0 && ProcessAlive(rec.PID) {
			return rec, nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return Record{}, err
			}
			return rec, fmt.Errorf("timed out waiting for session %q daemon to start", name)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
