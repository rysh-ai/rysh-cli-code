// SPDX-License-Identifier: Apache-2.0

// Package sshtest runs a throwaway OpenSSH server on 127.0.0.1 for the
// feature #14 integration tests (design 070). Everything it uses — host key,
// client key, sshd_config, ssh_config, known_hosts — is generated in the
// test's temp directory. It never reads the developer's ~/.ssh, never uses
// their agent, and stops only the sshd process it started (by recorded PID).
//
// The tests that use it are gated: they run only with RYSH_TEST_REMOTE_SSH=1,
// and skip (saying so) when /usr/sbin/sshd or ssh-keygen is missing.
package sshtest

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// EnvGate is the variable that turns the gated tests on.
const EnvGate = "RYSH_TEST_REMOTE_SSH"

// Alias is the ssh_config Host entry the throwaway config defines.
const Alias = "feat14-test"

// Server is one running throwaway sshd.
type Server struct {
	Dir        string // temp dir holding every file below
	Port       int
	ConfigFile string // ssh_config for `ssh -F`
	KnownHosts string
	HostKeyPub string // the server's real host key line
	pid        int
	cmd        *exec.Cmd
}

// Gate skips t unless the gated tests are on and the tools exist.
func Gate(t *testing.T) (sshd, keygen string) {
	t.Helper()
	if os.Getenv(EnvGate) != "1" {
		t.Skipf("SKIP: set %s=1 to run the throwaway-sshd integration tests", EnvGate)
	}
	sshd = "/usr/sbin/sshd"
	if _, err := os.Stat(sshd); err != nil {
		t.Skipf("SKIP: %s not available on this host", sshd)
	}
	var err error
	if keygen, err = exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("SKIP: ssh-keygen not available on this host")
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("SKIP: ssh client not available on this host")
	}
	return sshd, keygen
}

// Start generates keys and configs in a temp dir and starts sshd.
func Start(t *testing.T) *Server {
	t.Helper()
	sshd, keygen := Gate(t)
	dir := t.TempDir()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	gen := func(name string) {
		out, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-C", "feat14-throwaway", "-f", filepath.Join(dir, name)).CombinedOutput()
		if err != nil {
			t.Fatalf("ssh-keygen %s: %v\n%s", name, err, out)
		}
	}
	gen("host_key")
	gen("client_key")
	gen("other_host_key") // for the wrong-host-key case
	pub, err := os.ReadFile(filepath.Join(dir, "client_key.pub"))
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(filepath.Join(dir, "authorized_keys"), pub, 0o600))

	port := freePort(t)
	s := &Server{Dir: dir, Port: port, ConfigFile: filepath.Join(dir, "ssh_config"), KnownHosts: filepath.Join(dir, "known_hosts")}
	must(t, os.WriteFile(filepath.Join(dir, "sshd_config"), []byte(fmt.Sprintf(`Port %d
ListenAddress 127.0.0.1
HostKey %s
PidFile %s
AuthorizedKeysFile %s
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
StrictModes no
PermitRootLogin no
AllowUsers %s
`, port, filepath.Join(dir, "host_key"), filepath.Join(dir, "sshd.pid"), filepath.Join(dir, "authorized_keys"), u.Username)), 0o600))
	must(t, os.WriteFile(s.ConfigFile, []byte(fmt.Sprintf(`Host %s
  HostName 127.0.0.1
  Port %d
  User %s
  IdentityFile %s
  IdentitiesOnly yes
  IdentityAgent none
  UserKnownHostsFile %s
  GlobalKnownHostsFile /dev/null
  ConnectTimeout 10
`, Alias, port, u.Username, filepath.Join(dir, "client_key"), s.KnownHosts)), 0o600))
	s.HostKeyPub = hostLine(t, port, filepath.Join(dir, "host_key.pub"))
	s.TrustRealHostKey(t)

	// On a failed test, say what the far side did: sshd's log and whether the
	// remote command ever started. The job's temp dir is gone after the run
	// (the dev-host runner), so this is the only record. Registered before the
	// sshd stop below, so it runs after it and sees the whole log.
	t.Cleanup(func() {
		if t.Failed() {
			// The login name is masked: test logs end up in evidence files.
			t.Logf("sshtest: %s\nsshd log:\n%s", s.wrapperRan(), strings.ReplaceAll(s.Log(), u.Username, "<user>"))
		}
	})

	cmd := exec.Command(sshd, "-D", "-e", "-f", filepath.Join(dir, "sshd_config"))
	logf, err := os.Create(filepath.Join(dir, "sshd.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sshd: %v", err)
	}
	s.cmd, s.pid = cmd, cmd.Process.Pid
	t.Cleanup(func() {
		_ = s.cmd.Process.Signal(os.Interrupt) // the recorded pid only
		done := make(chan struct{})
		go func() { _ = s.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = s.cmd.Process.Kill()
			<-done
		}
		_ = logf.Close()
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sshd did not listen on %d", port)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return s
}

// PID is the recorded sshd pid.
func (s *Server) PID() int { return s.pid }

// TrustRealHostKey writes known_hosts with the server's real key.
func (s *Server) TrustRealHostKey(t *testing.T) {
	must(t, os.WriteFile(s.KnownHosts, []byte(s.HostKeyPub+"\n"), 0o600))
}

// TrustWrongHostKey writes known_hosts with a different key for the same
// host, as after a server swap or a man in the middle.
func (s *Server) TrustWrongHostKey(t *testing.T) {
	must(t, os.WriteFile(s.KnownHosts, []byte(hostLine(t, s.Port, filepath.Join(s.Dir, "other_host_key.pub"))+"\n"), 0o600))
}

// ForgetHostKey empties known_hosts: an unknown host.
func (s *Server) ForgetHostKey(t *testing.T) {
	must(t, os.WriteFile(s.KnownHosts, nil, 0o600))
}

// CleanEnvWrapper writes an executable script that runs bin with a minimal
// environment, so whatever the login shell on the "remote" sources never
// reaches the remote-serve process. Absolute path, own name, never on PATH.
func (s *Server) CleanEnvWrapper(t *testing.T, bin string) string {
	p := filepath.Join(s.Dir, "remote-rysh-clean-env.sh")
	script := fmt.Sprintf("#!/bin/sh\necho \"$$\" >> %q\nexec /usr/bin/env -i HOME=%q PATH=/usr/bin:/bin:/usr/sbin:/sbin TMPDIR=%q %q \"$@\"\n",
		s.ranFile(), os.Getenv("HOME"), os.TempDir(), bin)
	must(t, os.WriteFile(p, []byte(script), 0o700))
	return p
}

func (s *Server) ranFile() string { return filepath.Join(s.Dir, "remote-rysh-started") }

// wrapperRan says whether ssh ever started the remote command: a login that
// succeeds but never runs it points at the account's login shell, not rysh.
func (s *Server) wrapperRan() string {
	b, err := os.ReadFile(s.ranFile())
	if err != nil {
		return "the remote command never started (no clean-env wrapper run)"
	}
	return fmt.Sprintf("the clean-env wrapper started %d time(s)", strings.Count(string(b), "\n"))
}

// Log returns sshd's log.
func (s *Server) Log() string {
	b, _ := os.ReadFile(filepath.Join(s.Dir, "sshd.log"))
	return string(b)
}

func hostLine(t *testing.T, port int, pubPath string) string {
	b, err := os.ReadFile(pubPath)
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(b))
	return fmt.Sprintf("[127.0.0.1]:%d %s %s", port, f[0], f[1])
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
