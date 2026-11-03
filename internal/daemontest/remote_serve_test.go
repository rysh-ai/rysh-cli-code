// SPDX-License-Identifier: Apache-2.0

package daemontest_test

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/daemontest"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/remote"
)

// serveProc is `rysh remote-serve` as a subprocess on pipes — the far end of an
// ssh connection with the ssh taken out. Only the recorded process is killed.
type serveProc struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out io.ReadCloser
}

func (p *serveProc) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *serveProc) Write(b []byte) (int, error) { return p.in.Write(b) }
func (p *serveProc) Close() error {
	_ = p.in.Close()
	done := make(chan struct{})
	go func() { _ = p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = p.cmd.Process.Kill() // the pid this test started, nothing else
		<-done
	}
	return nil
}

func startRemoteServe(t *testing.T, s *daemontest.Session) *serveProc {
	t.Helper()
	cmd := exec.Command(s.Bin, "--config", s.Config, "remote-serve", "--session", s.Name)
	cmd.Dir = s.Dir
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return &serveProc{cmd: cmd, in: in, out: out}
}

// TestRemoteServeAgainstLiveDaemon drives S1 end to end without ssh: hello,
// the session's layout, a shell line typed into a real pane, and its output
// coming back as a screen frame.
func TestRemoteServeAgainstLiveDaemon(t *testing.T) {
	s := daemontest.Fresh(t)
	p := startRemoteServe(t, s)
	c := remote.NewClient(p, remote.ClientOptions{AckTimeout: 10 * time.Second})
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h, err := c.Handshake(ctx)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if h.Session != s.Name || h.Proto != remote.ProtoVersion {
		t.Fatalf("hello = %+v", h)
	}

	var pane string
	deadline := time.After(30 * time.Second)
	for pane == "" {
		select {
		case f := <-c.Frames():
			if f.T == remote.FrameLayout {
				for i := range f.Tabs {
					if ids := domain.PaneIDsInTab(&f.Tabs[i]); len(ids) > 0 {
						pane = ids[0]
					}
				}
			}
		case <-deadline:
			t.Fatal("no layout with a pane")
		}
	}

	const marker = "feat14-remote-echo-7f3a"
	// Typed as raw keys, the way the TUI sends them, so the echo proves the
	// keystroke path and not just a command API.
	key := base64.StdEncoding.EncodeToString([]byte("echo " + marker + "\r"))
	if err := c.Do(ctx, remote.Command{Type: remote.CmdRawKeystroke, Pane: pane, Payload: key}); err != nil {
		t.Fatalf("raw keystroke: %v", err)
	}
	deadline = time.After(30 * time.Second)
	for {
		select {
		case f := <-c.Frames():
			if f.T == remote.FrameScreen && f.Pane == pane &&
				(strings.Contains(f.Text, marker) || strings.Contains(strings.Join(f.Screen, "\n"), marker)) {
				return
			}
		case <-deadline:
			t.Fatalf("the typed line never came back from pane %s", pane)
		}
	}
}

// TestRemoteServeRefusesOtherProtocol: a local rysh speaking another protocol
// is refused before anything reaches the session.
func TestRemoteServeRefusesOtherProtocol(t *testing.T) {
	s := daemontest.Shared(t)
	p := startRemoteServe(t, s)
	c := remote.NewClient(p, remote.ClientOptions{Proto: remote.ProtoVersion + 41})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.Handshake(ctx); !errors.Is(err, remote.ErrProtoMismatch) {
		t.Fatalf("want ErrProtoMismatch, got %v", err)
	}
}

// TestRemoteServeMissingSession: no session, no stream — an error on stderr and
// a non-zero exit, never a session started on the remote's behalf.
func TestRemoteServeMissingSession(t *testing.T) {
	s := daemontest.Shared(t)
	out, code := s.CLI(t, "remote-serve", "--session", "no-such-session-feat14")
	if code == 0 || !strings.Contains(out, "no-such-session-feat14") {
		t.Fatalf("exit %d, output:\n%s", code, out)
	}
}
