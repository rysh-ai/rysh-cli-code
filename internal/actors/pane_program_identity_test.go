// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package actors

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// F-15's residue: strict mode learned to call the native-installer claude by
// its name (T2), but the pane's own program name still came straight from the
// kernel, so `##pane list`, `##rysh`, the board agent status and every
// MsgPaneProcess said "running 2.1.296" for a claude.

// startProgramAs runs a stand-in binary at path, exec'd through link with
// argv[0] = arg0, in its own process group, and waits until the kernel names it
// wantComm. It returns the pid, which is also the pgid.
func startProgramAs(t *testing.T, path, link, arg0, wantComm string) int {
	t.Helper()
	buildSleeper(t, path)
	exe := path
	if link != "" {
		if err := os.Symlink(path, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		exe = link
	}
	cmd := &exec.Cmd{Path: exe, Args: []string{arg0, "60"}}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start %s: %v", exe, err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	var comm string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if comm = processName(pid); comm == wantComm {
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("processName(%d) = %q, want %q: the fixture does not reproduce the layout", pid, comm, wantComm)
	return 0
}

// processEvents subscribes to a pane's process events.
func processEvents(t *testing.T, nc *nats.Conn, paneID string) *nats.Subscription {
	t.Helper()
	sub, err := nc.SubscribeSync(msg.T("pane", paneID, "process"))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return sub
}

func nextProcessEvent(t *testing.T, sub *nats.Subscription, codecs *msg.CodecRegistry) *msg.MsgPaneProcess {
	t.Helper()
	m, err := sub.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("no process event: %v", err)
	}
	var env msg.NATSEnvelope
	if err := json.Unmarshal(m.Data, &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	decoded, err := codecs.Decode(env.TypeTag, env.Payload)
	if err != nil {
		t.Fatalf("decode %s: %v", env.TypeTag, err)
	}
	ev, ok := decoded.(*msg.MsgPaneProcess)
	if !ok {
		t.Fatalf("decoded %T, want *msg.MsgPaneProcess", decoded)
	}
	return ev
}

func TestPaneProgramNamesAVersionNamedClaudeByItsRealName(t *testing.T) {
	const version = "9.9.998"
	root := t.TempDir()
	versions := filepath.Join(root, "share", "claude", "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pid := startProgramAs(t, filepath.Join(versions, version), filepath.Join(root, "claude"), "claude", version)

	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	p := &PaneActor{id: "f15-pane", pub: msg.NewNATSPublisher(nc, codecs)}
	sub := processEvents(t, nc, p.id)

	p.syncForegroundProgram(pid)
	if got := p.Program(); got != "claude" {
		t.Errorf("Program() = %q, want \"claude\": `##pane list` would say running %s", got, got)
	}
	if got := int(p.programPGID.Load()); got != pid {
		t.Errorf("programPGID = %d, want %d: the courier finds the inbox socket by it", got, pid)
	}
	ev := nextProcessEvent(t, sub, codecs)
	if ev.Event != "start" || ev.Program != "claude" || ev.PGID != pid {
		t.Errorf("start event = %+v, want start of \"claude\" with pgid %d", *ev, pid)
	}

	// A poll that sees the same process must not announce it again.
	p.syncForegroundProgram(pid)
	if m, err := sub.NextMsg(200 * time.Millisecond); err == nil {
		t.Errorf("a second sync of the same program published %q", m.Data)
	}

	p.syncForegroundProgram(-1)
	ev = nextProcessEvent(t, sub, codecs)
	if ev.Event != "exit" || ev.Program != "claude" {
		t.Errorf("exit event = %+v, want exit of \"claude\"", *ev)
	}
	if got := p.Program(); got != "" {
		t.Errorf("Program() after the pane is gone = %q, want \"\"", got)
	}
}

// An ordinary program keeps the kernel's name: only a known agent CLI is
// renamed, so `--running <name>` still matches what `ps` says for everything
// else.
func TestPaneProgramKeepsTheKernelNameOfAnOrdinaryProgram(t *testing.T) {
	const name = "ryshplainprog"
	pid := startProgramAs(t, filepath.Join(t.TempDir(), name), "", name, name)

	nc := startInProcessNATS(t)
	codecs := msg.DefaultCodecRegistry()
	p := &PaneActor{id: "plain-pane", pub: msg.NewNATSPublisher(nc, codecs)}
	sub := processEvents(t, nc, p.id)

	p.syncForegroundProgram(pid)
	if got := p.Program(); got != name {
		t.Errorf("Program() = %q, want %q", got, name)
	}
	if ev := nextProcessEvent(t, sub, codecs); ev.Event != "start" || ev.Program != name {
		t.Errorf("start event = %+v, want start of %q", *ev, name)
	}
}
