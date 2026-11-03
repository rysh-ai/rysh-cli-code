// SPDX-License-Identifier: Apache-2.0

package main

// Tests for the session `rysh setup` activates the assistant in (founder live
// Telegram test, 2026-10-27): setup in a fresh directory used to target
// session "default" — or the RYSH_SESSION a rysh pane exports — and every
// daemon shares NATS port 24242 by default, so `##humanoid spawn assistant`
// was answered by ANOTHER project's daemon, which looked for the skill file
// under its own working directory.
//
// No test here starts a daemon or reaches a real bus: the daemon spawn, the
// ## command path and the bus probe are seams, and the one probe test runs
// its own NATS server on a kernel-assigned port.

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bustoken"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// e33SessionSeams records every daemon spawn and ## command instead of
// running it, and makes the bus probe report responders daemons answering
// for any session name. A spawn writes a live record for this directory, as a
// real daemon of this project would.
func e33SessionSeams(t *testing.T, responders int) (spawned, cmds *[]string) {
	t.Helper()
	var sp, cm []string
	prevSpawn, prevCmd, prevProbe := assistantSpawnDaemon, assistantRyshCommand, assistantBusProbe
	assistantSpawnDaemon = func(name, logLevel, configPath string, shared bool) (daemonHandle, error) {
		sp = append(sp, name)
		writeLiveRecord(t, name, mustGetwd())
		return daemonHandle{}, nil
	}
	assistantRyshCommand = func(store *session.Store, name, tabID, paneID, command string) error {
		cm = append(cm, name+": "+command)
		return nil
	}
	assistantBusProbe = func(cfg config.Config, name string) (int, error) { return responders, nil }
	t.Cleanup(func() {
		assistantSpawnDaemon, assistantRyshCommand, assistantBusProbe = prevSpawn, prevCmd, prevProbe
	})
	return &sp, &cm
}

// writeLiveRecord registers session name as live in this directory's store:
// this test process's own PID, a placeholder port nothing dials (the ##
// command path is a seam), and path as the project root.
func writeLiveRecord(t *testing.T, name, path string) {
	t.Helper()
	store, err := session.NewStore(config.Load())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(session.Record{Name: name, Path: path, State: "detached", PID: os.Getpid(), NATSPort: 1}); err != nil {
		t.Fatal(err)
	}
}

// inForeignPane makes the run look like it happens inside a pane of a live
// session named "default" that belongs to another directory — the founder's
// exact situation.
func inForeignPane(t *testing.T) {
	t.Setenv("RYSH_SESSION", "default")
	t.Setenv("RYSH_PANE", "e33-pane-of-another-project")
}

func e33SetupEnv() assistantEnv {
	return assistantEnv{
		in:   bufio.NewReader(strings.NewReader("")),
		deps: e33Deps([]string{"E33_FAKE_KEY", "TG_E33_FAKE"}, botStub{stubAdapter: stubAdapter{typ: "telegram"}, bot: "e33_fake_bot"}),
	}
}

// TestDerivedSessionNameIsPerDirectory: the derived name is a valid NATS
// subject token, never "default", and two projects with the same basename get
// different names.
func TestDerivedSessionNameIsPerDirectory(t *testing.T) {
	a, b := filepath.Join(t.TempDir(), "proj"), filepath.Join(t.TempDir(), "proj")
	na, nb := derivedSessionName(a), derivedSessionName(b)
	if na == "default" || na == "" {
		t.Fatalf("derivedSessionName(%q) = %q, want a per-directory name", a, na)
	}
	if na == nb {
		t.Errorf("two directories named proj share session %q", na)
	}
	if na != derivedSessionName(a) {
		t.Error("derivedSessionName is not stable for one directory")
	}
	if !strings.HasPrefix(na, "proj-") || strings.ContainsAny(na, ". *>\t/") {
		t.Errorf("derived name %q: want proj-<hash>, a single NATS token", na)
	}
}

// TestSetupFreshDirInForeignPaneGetsOwnSession: setup in a fresh directory,
// run inside a pane of another project's live "default", targets a session
// of its own — the binding line names it and rysh.config.yaml keeps it under
// rysh.session_name, so a later `rysh` here opens the same session.
func TestSetupFreshDirInForeignPaneGetsOwnSession(t *testing.T) {
	setupWorkspace(t)
	inForeignPane(t)
	out, err := runSetupCaptured(t, setupHeadlessArgs(), e33SetupEnv())
	if err != nil {
		t.Fatalf("runSetup: %v\n%s", err, masked(out))
	}
	wd, _ := os.Getwd()
	want := derivedSessionName(wd)
	if want == "default" {
		t.Fatalf("derived session for a fresh dir is %q", want)
	}
	if !strings.Contains(out, "→ session "+want+" in "+wd) {
		t.Errorf("binding line does not name session %q:\n%s", want, masked(out))
	}
	t.Setenv("RYSH_SESSION", "")
	if got := config.LoadFrom("rysh.config.yaml").SessionName; got != want {
		t.Errorf("rysh.config.yaml session = %q, want %q", got, want)
	}
}

// TestSetupStartInFreshDirSpawnsOwnDaemonWithAbsoluteSkillPath: with --start,
// the daemon spawned is this directory's own session, and `humanoid spawn`
// names the skill file by absolute path, so it resolves whatever the
// daemon's working directory.
func TestSetupStartInFreshDirSpawnsOwnDaemonWithAbsoluteSkillPath(t *testing.T) {
	setupWorkspace(t)
	inForeignPane(t)
	spawned, cmds := e33SessionSeams(t, 0)
	out, err := runSetupCaptured(t, append(setupHeadlessArgs(), "--start", "--no-verify"), e33SetupEnv())
	if err != nil {
		t.Fatalf("runSetup: %v\n%s", err, masked(out))
	}
	want := derivedSessionName(mustGetwd())
	if len(*spawned) != 1 || (*spawned)[0] != want {
		t.Fatalf("spawned %q, want exactly [%s]", *spawned, want)
	}
	skill := filepath.Join(mustGetwd(), ".rysh", "humanoids", "assistant", "SKILL.md")
	if len(*cmds) == 0 || (*cmds)[0] != want+": humanoid spawn "+skill {
		t.Errorf("first ## command = %q, want %q", *cmds, want+": humanoid spawn "+skill)
	}
	for _, c := range *cmds {
		if !strings.HasPrefix(c, want+": ") {
			t.Errorf("## command sent to another session: %q", c)
		}
	}
}

// TestSetupRefusesSessionServedByAnotherDirectory: a session name this
// directory has no live daemon for, but which a daemon answers on the shared
// bus, belongs to another project. Setup refuses before spawning anything and
// prints the exact command that gives this directory its own session.
func TestSetupRefusesSessionServedByAnotherDirectory(t *testing.T) {
	setupWorkspace(t)
	spawned, cmds := e33SessionSeams(t, 1)
	out, err := runSetupCaptured(t, append(setupHeadlessArgs(), "--session", "e33-shared", "--start", "--no-verify"), e33SetupEnv())
	if err == nil {
		t.Fatalf("setup activated the assistant in a session another directory serves:\n%s", masked(out))
	}
	fix := "rysh setup --session " + derivedSessionName(mustGetwd())
	for _, want := range []string{`session "e33-shared"`, "another", fix} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err, want)
		}
	}
	if len(*spawned) != 0 || len(*cmds) != 0 {
		t.Errorf("refused run still spawned %q / sent %q", *spawned, *cmds)
	}
}

// TestSetupOwnLiveSession: this directory's own live daemon is reused when it
// is the only one answering for the name, and refused when a second daemon
// (another directory's, same name) answers too — the request would go to
// whichever replies first.
func TestSetupOwnLiveSession(t *testing.T) {
	for _, tc := range []struct {
		responders int
		ok         bool
	}{{1, true}, {2, false}} {
		t.Run(map[bool]string{true: "reused", false: "duplicate refused"}[tc.ok], func(t *testing.T) {
			setupWorkspace(t)
			writeLiveRecord(t, "e33-mine", mustGetwd())
			spawned, cmds := e33SessionSeams(t, tc.responders)
			out, err := runSetupCaptured(t, append(setupHeadlessArgs(), "--session", "e33-mine", "--start", "--no-verify"), e33SetupEnv())
			if len(*spawned) != 0 {
				t.Errorf("spawned %q although the session is live here", *spawned)
			}
			if tc.ok {
				if err != nil || len(*cmds) == 0 || !strings.HasPrefix((*cmds)[0], "e33-mine: humanoid spawn ") {
					t.Errorf("err %v, cmds %q:\n%s", err, *cmds, masked(out))
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "2 daemons") || len(*cmds) != 0 {
				t.Errorf("err %v, cmds %q — want a refusal naming 2 daemons", err, *cmds)
			}
		})
	}
}

// TestSetupInPaneOfThisDirectoryUsesThatSession: a pane's RYSH_SESSION is
// honoured when that session is live in THIS directory.
func TestSetupInPaneOfThisDirectoryUsesThatSession(t *testing.T) {
	setupWorkspace(t)
	writeLiveRecord(t, "e33-here", mustGetwd())
	t.Setenv("RYSH_SESSION", "e33-here")
	t.Setenv("RYSH_PANE", "e33-pane-here")
	out, err := runSetupCaptured(t, setupHeadlessArgs(), e33SetupEnv())
	if err != nil {
		t.Fatalf("runSetup: %v\n%s", err, masked(out))
	}
	if !strings.Contains(out, "→ session e33-here in ") {
		t.Errorf("binding line does not name the pane's own session:\n%s", masked(out))
	}
}

// TestSetupWarnsTopLevelSessionName: `session_name:` at the top level of
// rysh.config.yaml is ignored by the loader; setup says where it belongs.
func TestSetupWarnsTopLevelSessionName(t *testing.T) {
	setupWorkspace(t)
	if err := os.WriteFile("rysh.config.yaml", []byte("session_name: e33-misplaced\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runSetupCaptured(t, setupHeadlessArgs(), e33SetupEnv())
	if err != nil {
		t.Fatalf("runSetup: %v\n%s", err, masked(out))
	}
	if !strings.Contains(out, "top-level session_name") || !strings.Contains(out, "rysh:\n    session_name: e33-misplaced") {
		t.Errorf("no warning for the misplaced session_name:\n%s", masked(out))
	}
}

// TestSessionBusRespondersCounts runs the real probe against its own NATS
// server on a kernel-assigned port (never 24242): it counts each daemon that
// answers for the name, ignores other names, and reads a port nothing
// listens on as zero.
func TestSessionBusRespondersCounts(t *testing.T) {
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("test NATS server not ready")
	}
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	// A fake daemon answers its snapshot subject the way the workspace actor
	// does: on the reply subject carried in the envelope.
	answer := func(name string) {
		if _, err := nc.Subscribe(name+".ws.snapshot", func(m *nats.Msg) {
			var env struct {
				ReplyTo string `json:"r"`
			}
			if json.Unmarshal(m.Data, &env) == nil && env.ReplyTo != "" {
				_ = nc.Publish(env.ReplyTo, []byte(`{}`))
			}
		}); err != nil {
			t.Fatal(err)
		}
	}
	answer("e33-one")
	answer("e33-two")
	answer("e33-two")
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	cfg := config.Load()
	cfg.NATS.Mode = "embedded"
	cfg.NATS.Port = ns.Addr().(*net.TCPAddr).Port
	for name, want := range map[string]int{"e33-none": 0, "e33-one": 1, "e33-two": 2} {
		if got, err := sessionBusResponders(cfg, name, 300*time.Millisecond); err != nil || got != want {
			t.Errorf("responders(%s) = %d, %v; want %d", name, got, err, want)
		}
	}

	// A port nothing listens on: the server's own, after it shut down.
	closed := cfg
	ns2, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns2.Start()
	if !ns2.ReadyForConnections(5 * time.Second) {
		t.Fatal("second test NATS server not ready")
	}
	closed.NATS.Port = ns2.Addr().(*net.TCPAddr).Port
	ns2.Shutdown()
	ns2.WaitForShutdown()
	if got, err := sessionBusResponders(closed, "e33-one", 300*time.Millisecond); err != nil || got != 0 {
		t.Errorf("closed port: responders = %d, %v; want 0, nil", got, err)
	}
	// A private (kernel-assigned) port is never shared, so nothing is probed.
	private := cfg
	private.NATS.Port = 0
	if got, err := sessionBusResponders(private, "e33-one", 300*time.Millisecond); err != nil || got != 0 {
		t.Errorf("private port: responders = %d, %v; want 0, nil", got, err)
	}
}

// gap01 slice 3: a session NATS that refuses our token is a server that IS
// there; setup must not report "nothing listening" (0, nil).
func TestSessionBusRespondersRefusedIsNotFree(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	tok, _ := bustoken.New()
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: natsserver.RANDOM_PORT, Authorization: tok, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("not ready")
	}
	defer ns.Shutdown()
	var cfg config.Config
	cfg.NATS.Port = ns.Addr().(*net.TCPAddr).Port
	// HOME holds no token for that port.
	if n, err := sessionBusResponders(cfg, "s", 200*time.Millisecond); err == nil {
		t.Fatalf("refused server read as free: (%d, nil)", n)
	}
	// With the token, the probe connects (no responders on this bare server).
	if err := bustoken.Write(cfg.NATS.Port, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := sessionBusResponders(cfg, "s", 200*time.Millisecond); err != nil {
		t.Fatalf("with the token: %v", err)
	}
}
