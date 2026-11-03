// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package daemontest_test

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/daemontest"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/remote/sshtest"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// window is the local rysh session, driven the way its TUI drives it: over
// the session's own NATS.
type window struct {
	t *testing.T
	s *daemontest.Session
	c *cli.Client
}

func openWindow(t *testing.T, s *daemontest.Session) *window {
	t.Helper()
	store, err := session.NewStore(config.LoadFrom(s.Config))
	if err != nil {
		t.Fatal(err)
	}
	c, err := cli.Connect(store, s.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return &window{t: t, s: s, c: c}
}

func (w *window) snapshot() domain.WorkspaceSnapshot {
	w.t.Helper()
	msg.SetSessionPrefix(w.s.Name)
	r, err := w.c.RequestFromSubject(msg.T("ws", "snapshot"), &msg.MsgGetWorkspaceSnapshot{Fresh: true}, 10*time.Second)
	if err != nil {
		w.t.Fatalf("snapshot: %v", err)
	}
	return r.(*msg.MsgWorkspaceSnapshotReply).Snapshot
}

// remoteTab waits for a tab of host and returns it.
func (w *window) remoteTab(host string) domain.TabSnapshot {
	w.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		for _, tab := range w.snapshot().Tabs {
			if strings.HasPrefix(tab.ID, "mirror:rh."+host+".") && len(domain.PaneIDsInTab(&tab)) > 0 &&
				!strings.HasPrefix(domain.PaneIDsInTab(&tab)[0], "mirror-pending:") {
				return tab
			}
		}
		if time.Now().After(deadline) {
			list, _ := w.s.Exec(w.t, "##remote list")
			w.t.Fatalf("no remote tab for %s; ##remote list:\n%s", host, list)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// focusTab moves the local focus onto tabID with next-tab, as ctrl+t n does.
func (w *window) focusTab(tabID string) {
	w.t.Helper()
	for i := 0; i < 20; i++ {
		if w.snapshot().ActiveTabID == tabID {
			return
		}
		msg.SetSessionPrefix(w.s.Name)
		if err := w.c.Send(&msg.MsgFocusNextTab{}); err != nil {
			w.t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	w.t.Fatalf("could not focus %s", tabID)
}

// typeKeys sends raw keys to a pane exactly as the TUI does for a
// control-mode remote pane.
func (w *window) typeKeys(paneID, text string) {
	w.t.Helper()
	msg.SetSessionPrefix(w.s.Name)
	if err := w.c.Send(&msg.MsgRemoteForwardCommand{
		CommandType:  "raw_keystroke",
		Payload:      base64.StdEncoding.EncodeToString([]byte(text)),
		TargetPaneID: paneID,
	}); err != nil {
		w.t.Fatal(err)
	}
}

// waitOutput waits until the local view of paneID shows marker.
func (w *window) waitOutput(paneID, marker string) {
	w.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		snap := w.snapshot()
		for i := range snap.Tabs {
			if p := domain.FindPaneInTab(&snap.Tabs[i], paneID); p != nil && strings.Contains(p.Output, marker) {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	w.t.Fatalf("%q never appeared in the local view of %s", marker, paneID)
}

func waitRemoteList(t *testing.T, s *daemontest.Session, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var out string
	for time.Now().Before(deadline) {
		out = s.MustSucceed(t, "##remote list")
		ok := true
		for _, w := range want {
			ok = ok && strings.Contains(out, w)
		}
		if ok {
			return out
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("##remote list never showed %v:\n%s", want, out)
	return ""
}

func addThrowawayHost(t *testing.T, srv *sshtest.Server, near, far *daemontest.Session) {
	t.Helper()
	near.MustSucceed(t, fmt.Sprintf("##remote add %s --ssh-config %s --bin %s --config %s --dir %s --session %s",
		sshtest.Alias, srv.ConfigFile, srv.CleanEnvWrapper(t, far.Bin), far.Config, far.Dir, far.Name))
}

// TestRemotePanesInLocalWindow is S2 end to end: `##remote add` in one rysh
// shows another rysh's tab (over a real ssh), and keys typed into it in the
// local window run in the remote pane and come back — no nesting.
func TestRemotePanesInLocalWindow(t *testing.T) {
	srv := sshtest.Start(t)
	far := daemontest.Fresh(t)
	near := daemontest.Fresh(t)
	addThrowawayHost(t, srv, near, far)
	waitRemoteList(t, near, sshtest.Alias, "connected")

	w := openWindow(t, near)
	tab := w.remoteTab(sshtest.Alias)
	if !strings.Contains(tab.Title, "⇄ "+sshtest.Alias) {
		t.Fatalf("remote tab title %q does not name the host", tab.Title)
	}
	pane := domain.PaneIDsInTab(&tab)[0]
	w.focusTab(tab.ID)
	const marker = "feat14-window-echo-5b1e"
	w.typeKeys(pane, "echo "+marker+"\r")
	w.waitOutput(pane, marker)

	// A new pane created from the local window is created on the remote, in
	// the remote tab — and no local pane appears.
	localPanes := func() int {
		n := 0
		for _, lt := range w.snapshot().Tabs {
			if !strings.HasPrefix(lt.ID, "mirror:") {
				n += len(domain.PaneIDsInTab(&lt))
			}
		}
		return n
	}
	before := localPanes()
	msg.SetSessionPrefix(near.Name)
	if err := w.c.Send(&msg.MsgCreatePane{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		rt := w.remoteTab(sshtest.Alias)
		if len(domain.PaneIDsInTab(&rt)) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote tab never got a second pane: %v", domain.PaneIDsInTab(&rt))
		}
		time.Sleep(200 * time.Millisecond)
	}
	if after := localPanes(); after != before {
		t.Fatalf("a local pane was created (%d -> %d)", before, after)
	}

	// The local window's own tab is untouched: the line ran remotely only.
	for _, lt := range w.snapshot().Tabs {
		if strings.HasPrefix(lt.ID, "mirror:") {
			continue
		}
		for _, id := range domain.PaneIDsInTab(&lt) {
			if p := domain.FindPaneInTab(&lt, id); p != nil && strings.Contains(p.Output, marker) {
				t.Fatalf("remote input leaked into local pane %s", id)
			}
		}
	}
}

// TestRemotePanesReattachAfterConnectionKilled is S4 end to end: the ssh
// transport dies (killed by its pid, which ##remote list reports — the only
// process this test signals besides its own), the host shows the drop, the
// local rysh reconnects with backoff and reattaches to the SAME remote pane,
// which kept running on the remote.
func TestRemotePanesReattachAfterConnectionKilled(t *testing.T) {
	srv := sshtest.Start(t)
	far := daemontest.Fresh(t)
	near := daemontest.Fresh(t)
	addThrowawayHost(t, srv, near, far)
	list := waitRemoteList(t, near, sshtest.Alias, "connected")
	pid := sshPIDFromList(t, list)

	w := openWindow(t, near)
	tab := w.remoteTab(sshtest.Alias)
	pane := domain.PaneIDsInTab(&tab)[0]
	w.focusTab(tab.ID)
	w.typeKeys(pane, "export FEAT14_KEPT=still-here-3c; echo exported-ok\r")
	w.waitOutput(pane, "exported-ok") // the keys arrived before the transport dies

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill ssh pid %d: %v", pid, err)
	}
	// Reconnected: a new ssh process, the same tab and pane.
	deadline := time.Now().Add(90 * time.Second)
	for {
		out := near.MustSucceed(t, "##remote list")
		if strings.Contains(out, "connected") && !strings.Contains(out, "disconnected") {
			if np := sshPIDFromList(t, out); np != pid {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("never reconnected:\n%s", out)
		}
		time.Sleep(200 * time.Millisecond)
	}
	again := w.remoteTab(sshtest.Alias)
	if again.ID != tab.ID || domain.FindPaneInTab(&again, pane) == nil {
		t.Fatalf("reattached to a different tab/pane: %s vs %s", again.ID, tab.ID)
	}
	// The remote shell kept its state across the drop.
	w.focusTab(again.ID)
	w.typeKeys(pane, "echo kept=$FEAT14_KEPT\r")
	w.waitOutput(pane, "kept=still-here-3c")
}

// sshPIDFromList reads the SSH-PID column of the throwaway host's row.
func sshPIDFromList(t *testing.T, list string) int {
	t.Helper()
	for _, line := range strings.Split(list, "\n") {
		f := strings.Fields(line)
		if len(f) >= 5 && f[0] == sshtest.Alias {
			if pid, err := strconv.Atoi(f[4]); err == nil && pid > 1 {
				return pid
			}
		}
	}
	t.Fatalf("no ssh pid for %s in:\n%s", sshtest.Alias, list)
	return 0
}

// TestRemotePaneAgentStateShowsLocally is S3 end to end: the state the remote
// rysh observes for its pane (here: a program busy writing output, then done)
// shows in the local window's snapshot — what the agents list, the board's
// flow and the TUI read — tagged with the host.
func TestRemotePaneAgentStateShowsLocally(t *testing.T) {
	srv := sshtest.Start(t)
	far := daemontest.Fresh(t)
	near := daemontest.Fresh(t)
	addThrowawayHost(t, srv, near, far)
	waitRemoteList(t, near, sshtest.Alias, "connected")
	w := openWindow(t, near)
	tab := w.remoteTab(sshtest.Alias)
	pane := domain.PaneIDsInTab(&tab)[0]
	w.focusTab(tab.ID)
	w.typeKeys(pane, "for i in $(seq 1 40); do echo tick$i; sleep 0.25; done\r")
	w.waitMeta(pane, domain.PaneMetaActivity, domain.PaneActivityWorking)
	w.waitMeta(pane, domain.PaneMetaRemoteHost, sshtest.Alias)
	w.waitMeta(pane, domain.PaneMetaActivity, domain.PaneActivityIdle)
}

// waitMeta waits until the local view of paneID carries meta key=want.
func (w *window) waitMeta(paneID, key, want string) {
	w.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	got := ""
	for time.Now().Before(deadline) {
		snap := w.snapshot()
		for i := range snap.Tabs {
			if p := domain.FindPaneInTab(&snap.Tabs[i], paneID); p != nil {
				got = p.Meta[key]
				if got == want {
					return
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	w.t.Fatalf("pane %s meta %s = %q, never %q", paneID, key, got, want)
}
