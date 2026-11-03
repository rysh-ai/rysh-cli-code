// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// R-B1 (root-order-053): the server's subject ACL, on by default, lets a
// proxied client publish only under "ws.<workspaceID>." and "_INBOX."
// (rysh-server internal/nats/subject_acl.go newSubjectACL), and closes the
// connection on the first subject outside them. RemoteUpstreamActor connects
// through /workspaces/<WorkspaceName()>/nats, so every remote pane subject must
// sit under that same workspace id, not the local session name.

// serverACLAllows mirrors rysh-server newSubjectACL(workspaceID).allowed.
func serverACLAllows(workspaceID, subject string) bool {
	for _, p := range []string{"ws." + workspaceID + ".", "_INBOX."} {
		if strings.HasPrefix(subject, p) {
			return true
		}
	}
	return false
}

func startPlainNATS(t *testing.T) *nats.Conn {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatalf("new nats: %v", err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("nats not ready")
	}
	t.Cleanup(ns.Shutdown)
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatalf("connect nats: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// exactSubs subscribes to each subject exactly (no wildcard) and counts what
// arrives on it.
func exactSubs(t *testing.T, nc *nats.Conn, subjects []string) map[string]chan *nats.Msg {
	t.Helper()
	out := map[string]chan *nats.Msg{}
	for _, s := range subjects {
		ch := make(chan *nats.Msg, 8)
		if _, err := nc.ChanSubscribe(s, ch); err != nil {
			t.Fatalf("subscribe %s: %v", s, err)
		}
		out[s] = ch
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return out
}

func TestRemoteUpstreamSubjectsUseUpstreamWorkspace(t *testing.T) {
	const (
		wsID    = "7b0f8a52-3c1d-4e6a-9b2f-5d4c3b2a1908"
		session = "my-session"
		pane    = "0a1b2c3d-1111-4222-8333-444455556666"
	)
	nc := startPlainNATS(t)
	r := NewRemoteUpstreamActor(pane, "build", session, config.UpstreamConfig{Workspace: wsID}, nil, nil)
	r.remoteNC = nc
	r.connected = true

	want := []string{
		"ws." + wsID + ".pane." + pane + ".status",
		"ws." + wsID + ".pane." + pane + ".output",
		"ws." + wsID + ".pane." + pane + ".output.shell",
		"ws." + wsID + ".pane." + pane + ".output.history.ai",
	}
	old := []string{
		"ws." + session + ".pane." + pane + ".status",
		"ws." + session + ".pane." + pane + ".output",
		"ws." + session + ".pane." + pane + ".output.shell",
		"ws." + session + ".pane." + pane + ".output.history.ai",
	}
	subs := exactSubs(t, nc, append(append([]string{}, want...), old...))

	r.publishRemoteStatus("connected")
	r.forwardToRemote("hello")
	r.forwardPerModeToRemote("shell", "ls")
	r.forwardPerModeToRemote("history.ai", "entry")
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	for _, s := range want {
		if !serverACLAllows(wsID, s) {
			t.Fatalf("expected subject %s is outside the server ACL", s)
		}
		select {
		case m := <-subs[s]:
			if !serverACLAllows(wsID, m.Subject) {
				t.Errorf("published %s, which the server ACL refuses", m.Subject)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("nothing published on %s (the workspace-id subject the server ACL allows)", s)
		}
	}
	for _, s := range old {
		if serverACLAllows(wsID, s) {
			t.Fatalf("control: session subject %s should be outside the ACL", s)
		}
		select {
		case <-subs[s]:
			t.Errorf("published on session-name subject %s: the server ACL closes the connection", s)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// With no workspace configured the remote subject falls back to the same
// "default" that proxyPath uses, so path and subjects never disagree.
func TestRemoteUpstreamSubjectsMatchProxyPathDefault(t *testing.T) {
	const pane = "0a1b2c3d-1111-4222-8333-444455556666"
	nc := startPlainNATS(t)
	r := NewRemoteUpstreamActor(pane, "", "my-session", config.UpstreamConfig{}, nil, nil)
	r.remoteNC = nc
	r.connected = true
	subs := exactSubs(t, nc, []string{"ws.default.pane." + pane + ".status"})
	r.publishRemoteStatus("connected")
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	select {
	case <-subs["ws.default.pane."+pane+".status"]:
	case <-time.After(2 * time.Second):
		t.Error("status not published under the proxyPath workspace (default)")
	}
}
