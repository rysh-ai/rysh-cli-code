// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// SSHOptions describes one remote-serve connection. rysh only names a host
// alias; OpenSSH resolves everything else (HostName, ProxyJump, HostKeyAlias,
// IdentityFile, keep-alives) from the user's own ssh_config. rysh never reads,
// copies or passes a key.
type SSHOptions struct {
	Host         string // an ssh_config Host alias (or user@host)
	Session      string // the remote rysh session; empty = remote default
	ConfigFile   string // ssh -F; empty = the user's own config (tests pass a throwaway one)
	RemoteBinary string // the rysh binary on the remote; default rysh_local (on the remote PATH)
	RemoteDir    string // cd here first (the remote workspace), like `cd <ws> && rysh_local attach`
	RemoteConfig string // the remote rysh.config.yaml, passed as --config
	SSHBinary    string // default: "ssh" from PATH
	// Interactive leaves BatchMode off so a first connect from a terminal can
	// ask for a passphrase. The daemon never sets it: it has no terminal, and a
	// prompt there would hang a reconnect. StrictHostKeyChecking=yes holds
	// either way — an unknown or changed host key is an error, never a prompt.
	Interactive bool
}

var (
	hostAliasRE   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@-]{0,252}$`)
	sessionNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// ValidateHostAlias accepts a plain ssh_config alias or user@host. Anything
// that could be read as an ssh option or a shell word is refused.
func ValidateHostAlias(h string) error {
	if !hostAliasRE.MatchString(h) {
		return fmt.Errorf("invalid ssh host alias %q: use a Host entry from your ~/.ssh/config", h)
	}
	return nil
}

// ValidateSessionName accepts the session names rysh itself creates.
func ValidateSessionName(s string) error {
	if s == "" {
		return nil
	}
	if !sessionNameRE.MatchString(s) {
		return fmt.Errorf("invalid session name %q", s)
	}
	return nil
}

// shellQuote single-quotes s for the remote login shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// SSHArgs returns ssh's argv (without the binary) for a connection.
func SSHArgs(o SSHOptions) ([]string, error) {
	if err := ValidateHostAlias(o.Host); err != nil {
		return nil, err
	}
	if err := ValidateSessionName(o.Session); err != nil {
		return nil, err
	}
	for _, p := range []string{o.RemoteBinary, o.RemoteDir, o.RemoteConfig, o.ConfigFile} {
		if strings.ContainsAny(p, "\x00\n\r") {
			return nil, fmt.Errorf("invalid path %q", p)
		}
	}
	args := []string{}
	if o.ConfigFile != "" {
		args = append(args, "-F", o.ConfigFile)
	}
	args = append(args,
		"-T",
		// No TOFU: an unknown or changed host key fails the connection.
		"-o", "StrictHostKeyChecking=yes",
		// Our command replaces a RemoteCommand/RequestTTY the host entry may
		// carry for interactive use (e.g. `rysh_local attach`).
		"-o", "RemoteCommand=none",
		"-o", "RequestTTY=no",
		// A second connection to a host must not fail on its LocalForward ports.
		"-o", "ClearAllForwardings=yes",
	)
	if !o.Interactive {
		args = append(args, "-o", "BatchMode=yes")
	}
	bin := "rysh_local"
	if o.RemoteBinary != "" {
		bin = shellQuote(o.RemoteBinary)
	}
	remote := bin
	if o.RemoteConfig != "" {
		remote += " --config " + shellQuote(o.RemoteConfig)
	}
	remote += " remote-serve"
	if o.Session != "" {
		remote += " --session " + shellQuote(o.Session)
	}
	if o.RemoteDir != "" {
		remote = "cd " + shellQuote(o.RemoteDir) + " && exec " + remote
	}
	args = append(args, "--", o.Host, remote)
	return args, nil
}

// ClassifySSHError turns ssh's stderr into a short reason for the user.
func ClassifySSHError(stderr string) string {
	s := strings.ToLower(stderr)
	switch {
	case strings.Contains(s, "host key verification failed"),
		strings.Contains(s, "host key is known"),
		strings.Contains(s, "remote host identification has changed"),
		strings.Contains(s, "no matching host key"):
		return "host key not verified (rysh never accepts an unknown or changed host key; add it to known_hosts yourself)"
	case strings.Contains(s, "permission denied"):
		return "authentication failed (check your ssh agent / IdentityFile for this host)"
	case strings.Contains(s, "command not found"), strings.Contains(s, "no such file or directory"):
		return "rysh not installed on the remote (rysh_local not found on its PATH)"
	case strings.Contains(s, "could not resolve"), strings.Contains(s, "connection refused"),
		strings.Contains(s, "timed out"), strings.Contains(s, "no route"),
		strings.Contains(s, "connection closed"), strings.Contains(s, "broken pipe"):
		return "connect failed: " + lastLine(stderr)
	}
	if l := lastLine(stderr); l != "" {
		return l
	}
	return "ssh exited"
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// SSHConn is a running `ssh … remote-serve`: its stdout/stdin are the frame
// stream.
type SSHConn struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr *tailBuffer
	waited chan struct{}
	once   sync.Once
}

// DialSSH starts ssh for o. The connection is usable at once; the handshake
// (Client.Handshake) proves the far end is remote-serve.
func DialSSH(o SSHOptions) (*SSHConn, error) {
	args, err := SSHArgs(o)
	if err != nil {
		return nil, err
	}
	bin := o.SSHBinary
	if bin == "" {
		if bin, err = exec.LookPath("ssh"); err != nil {
			return nil, errors.New("ssh not found on PATH")
		}
	}
	cmd := exec.Command(bin, args...)
	ownProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	tb := &tailBuffer{max: 4096}
	cmd.Stderr = tb
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ssh: %w", err)
	}
	c := &SSHConn{cmd: cmd, stdin: stdin, stdout: stdout, stderr: tb, waited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(c.waited)
	}()
	return c, nil
}

// PID is ssh's process id (also its process group id).
func (c *SSHConn) PID() int { return c.cmd.Process.Pid }

func (c *SSHConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *SSHConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

// Stderr returns the tail of ssh's stderr.
func (c *SSHConn) Stderr() string { return c.stderr.String() }

// Exited is closed once ssh has exited.
func (c *SSHConn) Exited() <-chan struct{} { return c.waited }

// Close ends the connection: stdin is closed (remote-serve exits on EOF), then
// the recorded process group gets SIGTERM and, if needed, SIGKILL.
func (c *SSHConn) Close() error {
	c.once.Do(func() {
		_ = c.stdin.Close()
		select {
		case <-c.waited:
			return
		case <-time.After(300 * time.Millisecond):
		}
		terminateGroup(c.PID(), false)
		select {
		case <-c.waited:
		case <-time.After(2 * time.Second):
			terminateGroup(c.PID(), true)
			<-c.waited
		}
	})
	return nil
}

// Wait blocks until ssh exits or ctx ends.
func (c *SSHConn) Wait(ctx context.Context) error {
	select {
	case <-c.waited:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
