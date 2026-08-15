// SPDX-License-Identifier: Apache-2.0

package session

import (
	"os"
	"testing"
)

// TestSelfHeal covers the daemon-side record truth check: a record that
// reflects the live daemon is left alone; a missing, foreign, stopped, or
// wrong-port record is replaced with the daemon's identity, preserving TUI
// bookkeeping when the record was otherwise ours.
func TestSelfHeal(t *testing.T) {
	livePID := os.Getpid() // a PID that is definitely alive (this test process)
	self := Record{
		Name: "s", Path: "/proj", State: "detached", PID: livePID,
		NATSPort: 4222, Version: "dev", BinHash: "abc", Source: SourceApp,
	}

	// Truthful detached record → untouched.
	if healed, changed := SelfHeal(self, true, self); changed {
		t.Errorf("truthful detached record should be untouched: %+v", healed)
	}

	// Truthful running record with TUIs → untouched (attach flow owns it).
	running := self
	running.State = "running"
	running.TUIPIDs = []int{livePID}
	if _, changed := SelfHeal(running, true, self); changed {
		t.Error("truthful running record should be untouched")
	}

	// Missing record → rewritten as the daemon's own identity, detached.
	healed, changed := SelfHeal(Record{}, false, self)
	if !changed || healed.State != "detached" || healed.PID != livePID || healed.NATSPort != 4222 {
		t.Errorf("missing record not healed: changed=%v %+v", changed, healed)
	}

	// Clobbered to stopped/PID 0 (stale build or manual edit) → healed.
	clobbered := Record{Name: "s", Path: "/proj", State: "stopped"}
	healed, changed = SelfHeal(clobbered, true, self)
	if !changed || healed.State != "detached" || healed.PID != livePID {
		t.Errorf("stopped record not healed: changed=%v %+v", changed, healed)
	}

	// Foreign PID → replaced wholesale (foreign TUI bookkeeping dropped).
	foreign := self
	foreign.PID = livePID + 1
	foreign.TUIPIDs = []int{livePID}
	healed, changed = SelfHeal(foreign, true, self)
	if !changed || healed.PID != livePID || len(healed.TUIPIDs) != 0 {
		t.Errorf("foreign record not healed: changed=%v %+v", changed, healed)
	}

	// Ours but state stopped with live TUIs → healed to running, TUIs kept.
	oursStopped := self
	oursStopped.State = "stopped"
	oursStopped.TUIPIDs = []int{livePID}
	healed, changed = SelfHeal(oursStopped, true, self)
	if !changed || healed.State != "running" || len(healed.TUIPIDs) != 1 || healed.TUIPIDs[0] != livePID {
		t.Errorf("ours-stopped record should heal to running with TUIs kept: changed=%v %+v", changed, healed)
	}

	// Ours but wrong NATS port → healed to the daemon's port.
	wrongPort := self
	wrongPort.NATSPort = 9999
	healed, changed = SelfHeal(wrongPort, true, self)
	if !changed || healed.NATSPort != 4222 {
		t.Errorf("wrong-port record not healed: changed=%v %+v", changed, healed)
	}
}

// TestSelfHealPreservesProvenance pins the rule that makes cross-front-end
// sessions work at all: Source records which front-end CREATED the session and
// is stamped once. A terminal daemon serving an app-created session must not
// relabel it "cli" on the next heal tick — that would erase the reason the
// terminal degrades the session's web panes, and the notes would silently stop.
func TestSelfHealPreservesProvenance(t *testing.T) {
	livePID := os.Getpid()
	// A CLI daemon (Source "cli") now serving a session the app created.
	self := Record{Name: "s", Path: "/proj", State: "detached", PID: livePID, NATSPort: 4222, Source: SourceCLI}
	existing := self
	existing.State = "stopped" // forces a heal
	existing.Source = SourceApp

	healed, changed := SelfHeal(existing, true, self)
	if !changed {
		t.Fatal("stopped record should heal")
	}
	if healed.Source != SourceApp {
		t.Errorf("Source = %q, want %q — heal must not rewrite provenance", healed.Source, SourceApp)
	}

	// A record with no source is legacy: the daemon's own value fills it in.
	legacy := existing
	legacy.Source = ""
	if healed, _ := SelfHeal(legacy, true, self); healed.Source != SourceCLI {
		t.Errorf("legacy Source = %q, want the daemon's %q", healed.Source, SourceCLI)
	}
}

// TestSelfHealPreservesWebEndpoint keeps a connected desktop app attached
// across a heal. The web endpoint is maintained live by the daemon
// (UpdateWebEndpoint) rather than written at boot, so healing from the boot
// record would drop it and strand the app on the next tick.
func TestSelfHealPreservesWebEndpoint(t *testing.T) {
	livePID := os.Getpid()
	self := Record{Name: "s", Path: "/proj", State: "detached", PID: livePID, NATSPort: 4222, Source: SourceCLI}
	existing := self
	existing.State = "stopped" // forces a heal
	existing.WebPort = 23232

	healed, changed := SelfHeal(existing, true, self)
	if !changed {
		t.Fatal("stopped record should heal")
	}
	if healed.WebPort != 23232 {
		t.Errorf("web endpoint = %d, want 23232", healed.WebPort)
	}
}
