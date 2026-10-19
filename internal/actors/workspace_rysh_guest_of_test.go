// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
)

// `##rysh status` (design 028 §9 limit 7) says whose NATS server this daemon's
// state lives in.

func TestRyshStatusWarnsForAMismatchedHost(t *testing.T) {
	g := bus.GuestOf{ServerID: "NSRV", Port: 24242, HostSession: "test", HostWorkspace: "/stray",
		HostPID: 77, Mismatch: true, Warning: "this daemon is a GUEST of another session"}
	w := &WorkspaceActor{sessionName: "rysh-macminim5", guestOf: func() (bus.GuestOf, bool) { return g, true }}
	out := ryshSelfCmd(t, w, "status")
	for _, want := range []string{"rysh-macminim5", "guest of NATS server NSRV", "24242", "test", "/stray", "WARNING", "GUEST of another session"} {
		if !strings.Contains(out, want) {
			t.Errorf("##rysh status missing %q:\n%s", want, out)
		}
	}
}

func TestRyshStatusMatchingHostHasNoWarning(t *testing.T) {
	g := bus.GuestOf{ServerID: "NSRV", Port: 24242, HostSession: "s1", HostWorkspace: "/p"}
	w := &WorkspaceActor{sessionName: "s1", guestOf: func() (bus.GuestOf, bool) { return g, true }}
	out := ryshSelfCmd(t, w, "status")
	if !strings.Contains(out, "guest of NATS server NSRV") || strings.Contains(out, "WARNING") {
		t.Errorf("a matching host: want guest_of shown and no WARNING:\n%s", out)
	}
}

func TestRyshStatusNonGuest(t *testing.T) {
	for name, fn := range map[string]func() (bus.GuestOf, bool){
		"own server": func() (bus.GuestOf, bool) { return bus.GuestOf{}, false },
		"not wired":  nil,
	} {
		w := &WorkspaceActor{sessionName: "s", guestOf: fn}
		out := ryshSelfCmd(t, w, "status")
		if !strings.Contains(out, "guest_of: none") || strings.Contains(out, "WARNING") {
			t.Errorf("%s: want guest_of: none:\n%s", name, out)
		}
		js := ryshSelfCmd(t, w, "status", "--json")
		var v map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(js)), &v); err != nil {
			t.Fatalf("%s: --json is not JSON: %v\n%s", name, err, js)
		}
		if v["guest_of"] != nil || v["session"] != "s" {
			t.Errorf("%s: --json = %v; want session s and guest_of null", name, v)
		}
	}
}

func TestRyshStatusJSONCarriesGuestOf(t *testing.T) {
	g := bus.GuestOf{ServerID: "NSRV", Port: 24242, HostSession: "test", HostWorkspace: "/stray", Mismatch: true, Warning: "w"}
	w := &WorkspaceActor{sessionName: "me", guestOf: func() (bus.GuestOf, bool) { return g, true }}
	js := ryshSelfCmd(t, w, "status", "--json")
	var v struct {
		Session string       `json:"session"`
		GuestOf *bus.GuestOf `json:"guest_of"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(js)), &v); err != nil {
		t.Fatalf("--json is not JSON: %v\n%s", err, js)
	}
	if v.GuestOf == nil || *v.GuestOf != g || v.Session != "me" {
		t.Errorf("--json = %+v; want session me and the full guest_of", v)
	}
}

// ORDER-13: a session-only difference shows as a NOTE, never as a WARNING.
func TestRyshStatusSessionOnlyMismatchIsANoteNotAWarning(t *testing.T) {
	g := bus.GuestOf{ServerID: "NSRV", Port: 24242, HostSession: "app-sidecar", HostWorkspace: "/p",
		SessionMismatch: true, Note: `host is session "app-sidecar" of this same workspace`}
	w := &WorkspaceActor{sessionName: "cli", guestOf: func() (bus.GuestOf, bool) { return g, true }}
	out := ryshSelfCmd(t, w, "status")
	if !strings.Contains(out, "NOTE") || !strings.Contains(out, "app-sidecar") || strings.Contains(out, "WARNING") {
		t.Errorf("want a NOTE naming the host and no WARNING:\n%s", out)
	}
}
