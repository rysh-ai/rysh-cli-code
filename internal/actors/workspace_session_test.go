// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// newSessionTestWorkspace builds a minimal WorkspaceActor wired to an in-process
// NATS publisher and a temp session registry, then seeds it with the given
// records. It returns the workspace (with sessionName/cfg set) ready to run
// ##session commands via handleRyshCommand.
func newSessionTestWorkspace(t *testing.T, current string, records ...session.Record) *WorkspaceActor {
	t.Helper()
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())

	dir := t.TempDir()
	cfg := config.Config{
		SessionName:      current,
		SessionDir:       dir,
		SessionSource:    "cli",
		ConfigFile:       "/etc/rysh/rysh.config.yaml",
		RyshDir:          "/etc/rysh/.rysh",
		WorkingDirectory: "/work/" + current,
	}
	store, err := session.NewStore(cfg)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	for _, r := range records {
		if _, err := store.Upsert(r); err != nil {
			t.Fatalf("seed record %q: %v", r.Name, err)
		}
	}
	return &WorkspaceActor{
		pub:         pub,
		cfg:         cfg,
		sessionName: current,
		// Leave tabs empty so cmdSessionInfo skips the per-tab queryPaneCount
		// NATS round-trips (no tab actors are running in this unit test).
		tabs: nil,
	}
}

func TestSessionInfoCommand(t *testing.T) {
	w := newSessionTestWorkspace(t, "alpha", session.Record{
		Name: "alpha", Path: "/work/alpha", State: "stopped", Source: "cli",
	})

	out, _ := w.handleRyshCommand(nil, "", "rysh", "session", false, ryshBuiltinCmd) // default -> info

	for _, want := range []string{
		"session info",
		"name        : alpha",
		// Two distinct facts, so a session opened from the "wrong" front-end
		// reads honestly: which front-end is rendering it now, and which one
		// created it.
		"front-end   : rysh command line",
		"created by  : rysh command line",
		"config file : /etc/rysh/rysh.config.yaml",
		"rysh dir    : /etc/rysh/.rysh",
		"working dir : /work/alpha",
		"tabs        : 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("##session info missing %q\n---\n%s", want, out)
		}
	}
	// Nothing degrades when the front-end that created the session is the one
	// showing it, so the caveat block must stay off.
	if strings.Contains(out, "rendered here with") {
		t.Errorf("##session info listed degradations for its own session\n---\n%s", out)
	}
}

// TestSessionInfoShowsDegradations covers the cross-front-end case: a terminal
// showing a session the desktop app created must say so and list what it cannot
// paint. The pre-attach note on stderr scrolls away with the TUI's first
// repaint, so this command is the durable copy the user can go back to.
func TestSessionInfoShowsDegradations(t *testing.T) {
	w := newSessionTestWorkspace(t, "designs", session.Record{
		Name: "designs", Path: "/work/designs", State: "stopped", Source: session.SourceApp,
	})

	out, _ := w.handleRyshCommand(nil, "", "rysh", "session", false, ryshBuiltinCmd)

	if !strings.Contains(out, "created by  : rysh desktop app") {
		t.Errorf("##session info should name the creating front-end\n---\n%s", out)
	}
	if !strings.Contains(out, "rendered here with") {
		t.Errorf("##session info should list what degrades here\n---\n%s", out)
	}
	if !strings.Contains(out, "##web headless on") {
		t.Errorf("the web-pane caveat should carry its workaround\n---\n%s", out)
	}
}

func TestSessionListCommand(t *testing.T) {
	w := newSessionTestWorkspace(t, "alpha",
		session.Record{Name: "alpha", Path: "/work/alpha", State: "stopped", Source: "cli"},
		session.Record{Name: "beta", Path: "/work/beta", State: "stopped", Source: "cli"},
	)

	out, _ := w.handleRyshCommand(nil, "", "rysh", "session list", false, ryshBuiltinCmd)

	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Errorf("##session list should mention both sessions\n---\n%s", out)
	}
	if !strings.Contains(out, "> alpha") {
		t.Errorf("##session list should mark current session 'alpha' with '>'\n---\n%s", out)
	}
	if strings.Contains(out, "> beta") {
		t.Errorf("##session list should NOT mark 'beta' as current\n---\n%s", out)
	}
}

func TestSessionSwitchSameSession(t *testing.T) {
	w := newSessionTestWorkspace(t, "alpha",
		session.Record{Name: "alpha", Path: "/work/alpha", State: "stopped", Source: "cli"},
	)

	out, _ := w.handleRyshCommand(nil, "", "rysh", "session switch alpha", false, ryshBuiltinCmd)
	if !strings.Contains(out, "already on session") {
		t.Errorf("switching to the current session should report it is already active\n---\n%s", out)
	}
}

func TestSessionSwitchNotFound(t *testing.T) {
	w := newSessionTestWorkspace(t, "alpha",
		session.Record{Name: "alpha", Path: "/work/alpha", State: "stopped", Source: "cli"},
	)

	out, _ := w.handleRyshCommand(nil, "", "rysh", "session switch ghost", false, ryshBuiltinCmd)
	if !strings.Contains(out, "not found") {
		t.Errorf("switching to an unknown session should report it is not found\n---\n%s", out)
	}
}

func TestSessionSwitchMissingName(t *testing.T) {
	w := newSessionTestWorkspace(t, "alpha")

	out, _ := w.handleRyshCommand(nil, "", "rysh", "session switch", false, ryshBuiltinCmd)
	if !strings.Contains(out, "usage:") {
		t.Errorf("##session switch with no name should print usage\n---\n%s", out)
	}
}

func TestSessionReloadCommand(t *testing.T) {
	w := newSessionTestWorkspace(t, "alpha",
		session.Record{Name: "alpha", Path: "/work/alpha", State: "stopped", Source: "cli"},
	)

	out, _ := w.handleRyshCommand(nil, "", "rysh", "session reload", false, ryshBuiltinCmd)
	for _, want := range []string{
		`reloaded session "alpha"`,
		"workspace state flushed to KV",
		"session registry record refreshed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("##session reload missing %q\n---\n%s", want, out)
		}
	}
}

func TestSessionUnknownSubcommand(t *testing.T) {
	w := newSessionTestWorkspace(t, "alpha")

	out, _ := w.handleRyshCommand(nil, "", "rysh", "session bogus", false, ryshBuiltinCmd)
	if !strings.Contains(out, "usage:") {
		t.Errorf("unknown ##session subcommand should print usage\n---\n%s", out)
	}
}
