// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

func hostLayout(host string, tabs ...domain.TabSnapshot) *MsgRemoteHostLayout {
	m := &MsgRemoteHostLayout{Host: host}
	for _, t := range tabs {
		m.Tabs = append(m.Tabs, RemoteHostTab{ShareID: remoteHostShareID(host, t.ID), Title: t.Title, Tab: t})
	}
	return m
}

func wsWithHost(host string) *WorkspaceActor {
	w := &WorkspaceActor{}
	w.remoteHosts = map[string]*remoteHostEntry{host: {alias: host, pid: actor.NewPID("local", "host-"+host), state: remoteStateConnected}}
	return w
}

func TestRemoteHostLayoutAddsControlTabsMarkedWithHost(t *testing.T) {
	w := wsWithHost("devbox")
	w.applyRemoteHostLayout(hostLayout("devbox", rhTab("t1", "p1"), rhTab("t2", "p2")))
	if len(w.mirrorTabs) != 2 {
		t.Fatalf("mirror tabs = %d", len(w.mirrorTabs))
	}
	mt := w.mirrorTabs[0]
	if mt.host != "devbox" || mt.mode != "control" || mt.listenerPID == nil || !mt.hasData {
		t.Fatalf("tab = %+v", mt)
	}
	// Control mode: interactive panes take keystrokes (ControllingShareID).
	mt.liveInteractive = map[string]bool{"p1": true}
	d := mt.displayTab(false)
	p := domain.FindPaneInTab(&d, mirrorPaneID(mt.shareID, "p1"))
	if p == nil || !p.RemoteInteractive || p.ControllingShareID != mt.shareID {
		t.Fatalf("pane = %+v", p)
	}
	// The title names the host.
	if title := mt.snapshotTitle(); !strings.Contains(title, "devbox") || !strings.Contains(title, "remote t1") {
		t.Fatalf("title = %q", title)
	}
}

func TestRemoteHostLayoutDropsTabsGoneOnRemoteOnly(t *testing.T) {
	w := wsWithHost("devbox")
	w.remoteHosts["other"] = &remoteHostEntry{alias: "other", pid: actor.NewPID("local", "host-other")}
	w.applyRemoteHostLayout(hostLayout("devbox", rhTab("t1", "p1"), rhTab("t2", "p2")))
	w.applyRemoteHostLayout(hostLayout("other", rhTab("t1", "p9")))
	w.applyRemoteHostLayout(hostLayout("devbox", rhTab("t2", "p2")))
	var got []string
	for _, mt := range w.mirrorTabs {
		got = append(got, mt.shareID)
	}
	want := []string{remoteHostShareID("devbox", "t2"), remoteHostShareID("other", "t1")}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("tabs = %v, want %v", got, want)
	}
}

func TestRemoteHostLayoutForUnknownHostIgnored(t *testing.T) {
	w := wsWithHost("devbox")
	w.applyRemoteHostLayout(hostLayout("stranger", rhTab("t1", "p1")))
	if len(w.mirrorTabs) != 0 {
		t.Fatal("a host that was never added must not create tabs")
	}
}

func TestRemoteHostStatusShownInTitle(t *testing.T) {
	w := wsWithHost("devbox")
	w.applyRemoteHostLayout(hostLayout("devbox", rhTab("t1", "p1")))
	w.applyRemoteHostStatus(&MsgRemoteHostStatus{Host: "devbox", State: remoteStateDisconnected, Note: "connect failed: timed out"})
	if title := w.mirrorTabs[0].snapshotTitle(); !strings.Contains(title, "disconnected") {
		t.Fatalf("title = %q", title)
	}
	if e := w.remoteHosts["devbox"]; e.state != remoteStateDisconnected || !strings.Contains(e.note, "timed out") {
		t.Fatalf("entry = %+v", e)
	}
	w.applyRemoteHostStatus(&MsgRemoteHostStatus{Host: "devbox", State: remoteStateConnected})
	if title := w.mirrorTabs[0].snapshotTitle(); strings.Contains(title, "disconnected") {
		t.Fatalf("title after reconnect = %q", title)
	}
}

func TestUpstreamUnsubscribeLeavesRemoteHostTabsAlone(t *testing.T) {
	w := wsWithHost("devbox")
	w.applyRemoteHostLayout(hostLayout("devbox", rhTab("t1", "p1")))
	var out strings.Builder
	w.handleUpstreamUnsubscribe(&out, "")
	if len(w.mirrorTabs) != 1 {
		t.Fatalf("##upstream unsubscribe removed a remote-host tab: %s", out.String())
	}
}

func TestRemoteCommandUsageAndValidation(t *testing.T) {
	w := &WorkspaceActor{}
	for _, args := range [][]string{
		{"add"},
		{"add", "-oProxyCommand=sh"},
		{"add", "a;b"},
		{"add", "devbox", "--session", "a b"},
		{"add", "devbox", "--bogus", "x"},
		{"remove", "nope"},
		{"frobnicate"},
	} {
		var out strings.Builder
		if err := w.handleRemoteCommand(&out, args); err == nil {
			t.Errorf("##remote %s accepted:\n%s", strings.Join(args, " "), out.String())
		}
	}
	if len(w.remoteHosts) != 0 {
		t.Fatal("a refused add registered a host")
	}
}

func TestRemoteListShowsHosts(t *testing.T) {
	w := wsWithHost("devbox")
	w.remoteHosts["devbox"].session = "dev-1"
	w.applyRemoteHostLayout(hostLayout("devbox", rhTab("t1", "p1")))
	var out strings.Builder
	if err := w.handleRemoteCommand(&out, []string{"list"}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"devbox", "dev-1", remoteStateConnected, "1"} {
		if !strings.Contains(s, want) {
			t.Errorf("list missing %q:\n%s", want, s)
		}
	}
}

func TestRemoteAddArgsFromConfig(t *testing.T) {
	got := remoteAddArgs(config.RemoteHostConfig{Alias: "devbox", Session: "dev", Dir: "/w"})
	if strings.Join(got, " ") != "devbox --session dev --dir /w" {
		t.Fatalf("args = %v", got)
	}
	if _, err := parseRemoteAdd(got); err != nil {
		t.Fatal(err)
	}
	if _, err := parseRemoteAdd(remoteAddArgs(config.RemoteHostConfig{Alias: "-oProxyCommand=x"})); err == nil {
		t.Fatal("a bad configured alias must be refused like a typed one")
	}
}
