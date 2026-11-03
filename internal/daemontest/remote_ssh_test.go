// SPDX-License-Identifier: Apache-2.0

package daemontest_test

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/daemontest"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/remote"
	"github.com/rysh-ai/rysh-cli-code/internal/remote/sshtest"
)

// sshOpts are the options a local rysh would use for the throwaway host: the
// throwaway ssh_config via -F, and the binary under test on the "remote".
func sshOpts(t *testing.T, srv *sshtest.Server, s *daemontest.Session) remote.SSHOptions {
	return remote.SSHOptions{
		Host:         sshtest.Alias,
		Session:      s.Name,
		ConfigFile:   srv.ConfigFile,
		RemoteBinary: srv.CleanEnvWrapper(t, s.Bin),
		RemoteConfig: s.Config,
		RemoteDir:    s.Dir,
	}
}

// TestRemoteSSHTypedLineEchoes is S1 over a real ssh: the system ssh binary,
// a real sshd, remote-serve on the far end, a line typed into a real pane.
func TestRemoteSSHTypedLineEchoes(t *testing.T) {
	srv := sshtest.Start(t)
	s := daemontest.Fresh(t)
	conn, err := remote.DialSSH(sshOpts(t, srv, s))
	if err != nil {
		t.Fatal(err)
	}
	c := remote.NewClient(conn, remote.ClientOptions{AckTimeout: 10 * time.Second})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := c.Handshake(ctx); err != nil {
		t.Fatalf("handshake over ssh: %v\nssh stderr: %s", err, conn.Stderr())
	}
	pane := firstPane(t, c)
	const marker = "feat14-ssh-echo-91c2"
	key := base64.StdEncoding.EncodeToString([]byte("echo " + marker + "\r"))
	if err := c.Do(ctx, remote.Command{Type: remote.CmdRawKeystroke, Pane: pane, Payload: key}); err != nil {
		t.Fatalf("keystroke over ssh: %v", err)
	}
	waitScreen(t, c, pane, marker)
}

// TestRemoteSSHWrongHostKeyRefused: a known_hosts entry that does not match
// the server is a refused connection, never a prompt or an auto-accept.
func TestRemoteSSHWrongHostKeyRefused(t *testing.T) {
	srv := sshtest.Start(t)
	s := daemontest.Shared(t)
	cases := []struct {
		name string
		set  func(*testing.T)
	}{
		{"unknown key", srv.ForgetHostKey},
		{"changed key", srv.TrustWrongHostKey},
	}
	for _, tc := range cases {
		tc.set(t)
		before, _ := os.ReadFile(srv.KnownHosts)
		conn, err := remote.DialSSH(sshOpts(t, srv, s))
		if err != nil {
			t.Fatal(err)
		}
		c := remote.NewClient(conn, remote.ClientOptions{})
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err = c.Handshake(ctx)
		cancel()
		_ = c.Close()
		if err == nil {
			t.Fatalf("%s: connection accepted", tc.name)
		}
		<-conn.Exited()
		if why := remote.ClassifySSHError(conn.Stderr()); !strings.Contains(why, "host key") {
			t.Fatalf("%s: refusal not reported as a host-key problem: %q (stderr %q)", tc.name, why, conn.Stderr())
		}
		// No TOFU: ssh did not add or replace the key.
		if after, _ := os.ReadFile(srv.KnownHosts); string(after) != string(before) {
			t.Fatalf("%s: known_hosts was modified", tc.name)
		}
	}
}

func firstPane(t *testing.T, c *remote.Client) string {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case f, ok := <-c.Frames():
			if !ok {
				t.Fatalf("stream ended: %v", c.Err())
			}
			if f.T == remote.FrameLayout {
				for i := range f.Tabs {
					if ids := domain.PaneIDsInTab(&f.Tabs[i]); len(ids) > 0 {
						return ids[0]
					}
				}
			}
		case <-deadline:
			t.Fatal("no layout with a pane")
		}
	}
}

func waitScreen(t *testing.T, c *remote.Client, pane, marker string) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case f, ok := <-c.Frames():
			if !ok {
				t.Fatalf("stream ended: %v", c.Err())
			}
			if f.T == remote.FrameScreen && f.Pane == pane &&
				(strings.Contains(f.Text, marker) || strings.Contains(strings.Join(f.Screen, "\n"), marker)) {
				return
			}
		case <-deadline:
			t.Fatalf("marker %q never came back from pane %s", marker, pane)
		}
	}
}
