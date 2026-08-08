package actors

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestEarliestDeadline covers the single arbitration point that lets the two
// users of the rawReadLoop PTY read deadline — the idle raw-publish flush
// (rawPublishIdleFlush ~8ms) and the cursor-hidden exit debounce — coexist
// without either's clear stomping the other's pending wakeup.
//
// This is the pure-helper proxy for Fix A: when an interactive program buffers
// a single small chunk and goes idle (no second chunk), the loop must still
// wake within the idle window to flush it. Driving the real PTY loop is
// impractical in a unit test, so we test the decision logic directly: given a
// pending raw-flush deadline and/or a pending debounce deadline, the loop arms
// the EARLIEST one (and clears the deadline only when neither is pending).
func TestEarliestDeadline(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	idle := base.Add(rawPublishIdleFlush) // ~8ms: the raw-flush wakeup
	debounce := base.Add(3 * time.Second) // the exit-debounce wakeup
	zero := time.Time{}

	cases := []struct {
		name    string
		a, b    time.Time
		wantAt  time.Time
		wantSet bool
	}{
		{"both unset clears deadline", zero, zero, zero, false},
		{"only raw-flush set", idle, zero, idle, true},
		{"only debounce set", zero, debounce, debounce, true},
		{"raw-flush earlier than debounce", idle, debounce, idle, true},
		{"debounce earlier than raw-flush", debounce, idle, idle, true},
		{"equal deadlines pick that time", idle, idle, idle, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at, ok := earliestDeadline(tc.a, tc.b)
			if ok != tc.wantSet {
				t.Fatalf("earliestDeadline(%v,%v) set=%v, want %v", tc.a, tc.b, ok, tc.wantSet)
			}
			if !at.Equal(tc.wantAt) {
				t.Fatalf("earliestDeadline(%v,%v) at=%v, want %v", tc.a, tc.b, at, tc.wantAt)
			}
		})
	}

	// The idle-flush wakeup must be well within the chunk-coalesce delay so the
	// tail is published promptly rather than waiting on the next keystroke.
	if rawPublishIdleFlush >= rawPublishMaxDelay {
		t.Fatalf("rawPublishIdleFlush (%v) must be < rawPublishMaxDelay (%v)",
			rawPublishIdleFlush, rawPublishMaxDelay)
	}
}

// A pane shell must not inherit the RYSH_WEB_* variables that configured the
// daemon hosting it. They describe one specific web server; passing them on
// makes `rysh create` from inside a pane produce a control-mode daemon aimed at
// another daemon's port.
func TestPaneShellEnv_StripsWebServerVars(t *testing.T) {
	for _, kv := range [][2]string{
		{"RYSH_WEB_CONTROL", "true"},
		{"RYSH_WEB_AUTO_START", "true"},
		{"RYSH_WEB_PORT", "51811"},
		{"RYSH_WEB_HOST", "0.0.0.0"},
	} {
		t.Setenv(kv[0], kv[1])
	}
	t.Setenv("RYSH_KEEP_ME", "yes")

	var sawKeeper bool
	for _, kv := range paneShellEnv() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "RYSH_WEB_CONTROL", "RYSH_WEB_AUTO_START", "RYSH_WEB_PORT", "RYSH_WEB_HOST":
			t.Errorf("%s leaked into the pane shell environment", name)
		case "RYSH_KEEP_ME":
			sawKeeper = true
		}
	}
	if !sawKeeper {
		t.Error("paneShellEnv dropped an unrelated variable — only RYSH_WEB_* should go")
	}
}

// TestPaneIdentityEnv pins the contract a program in a pane depends on: it can
// read its OWN pane out of the environment, under the same names `rysh script`
// exports, instead of asking which pane is active and getting the user's focus.
func TestPaneIdentityEnv(t *testing.T) {
	got := paneIdentityEnv("my-session", "tab-1", "lane-2", "stack-3", "pane-9")
	want := []string{
		"RYSH_SESSION=my-session",
		"RYSH_TAB=tab-1",
		"RYSH_LANE=lane-2",
		"RYSH_STACK=stack-3",
		"RYSH_PANE=pane-9",
	}
	if len(got) != len(want) {
		t.Fatalf("paneIdentityEnv returned %d entries, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestPaneIdentityEnvOverridesInherited checks the assumption startShell relies
// on when it appends the identity AFTER os.Environ(): a later duplicate wins.
// If it did not, a pane opened from a shell that already carried RYSH_PANE — a
// nested rysh, or a pane spawned from another pane — would report its parent's
// pane, and every command a program sent "to itself" would land elsewhere.
func TestPaneIdentityEnvOverridesInherited(t *testing.T) {
	inherited := []string{"RYSH_PANE=parent-pane", "RYSH_SESSION=parent-session"}
	cmd := exec.Command("sh", "-c", "printf '%s|%s' \"$RYSH_PANE\" \"$RYSH_SESSION\"")
	cmd.Env = append(inherited,
		paneIdentityEnv("child-session", "tab-1", "lane-2", "stack-3", "child-pane")...)
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("cannot run sh: %v", err)
	}
	if string(out) != "child-pane|child-session" {
		t.Fatalf("pane env = %q, want %q — the appended identity did not win over "+
			"the inherited one", out, "child-pane|child-session")
	}
}
