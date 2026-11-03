// SPDX-License-Identifier: Apache-2.0

package main

// D-55 option B: `rysh setup` in a NEW project (one that gets its own derived
// session name) writes a NATS port of its own into rysh.config.yaml, from the
// 24300-24899 band, so the project's daemon never joins another project's bus.
// Existing projects, a config that already names a port, and a RYSH_NATS_PORT
// override are left alone. No test here starts a daemon: the spawn, the ##
// command path and the bus probe are seams (e33SessionSeams).

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

func inProjectBand(p int) bool {
	return p >= projectBusPortBase && p < projectBusPortBase+projectBusPortSpan
}

func loadSetupConfig(t *testing.T) config.Config {
	t.Helper()
	t.Setenv("RYSH_NATS_PORT", "")
	t.Setenv("RYSH_SESSION", "")
	return config.LoadFrom("rysh.config.yaml")
}

func TestSetupFreshDirWritesABusPortOfItsOwn(t *testing.T) {
	setupWorkspace(t)
	t.Setenv("RYSH_NATS_PORT", "")
	e33SessionSeams(t, 0)
	out, err := runSetupCaptured(t, setupHeadlessArgs(), e33SetupEnv())
	if err != nil {
		t.Fatalf("runSetup: %v\n%s", err, masked(out))
	}
	cfg := loadSetupConfig(t)
	if !inProjectBand(cfg.NATS.Port) || !cfg.NATS.OwnPort {
		t.Fatalf("rysh.config.yaml nats port = %d (own %v); want a port of its own in %d-%d",
			cfg.NATS.Port, cfg.NATS.OwnPort, projectBusPortBase, projectBusPortBase+projectBusPortSpan-1)
	}
	if !strings.Contains(out, "nats port") {
		t.Errorf("setup should say which bus port it chose:\n%s", masked(out))
	}
}

func TestSetupKeepsAPortTheConfigAlreadyNames(t *testing.T) {
	setupWorkspace(t)
	t.Setenv("RYSH_NATS_PORT", "")
	e33SessionSeams(t, 0)
	if err := os.WriteFile("rysh.config.yaml", []byte("nats:\n  port: 25555\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runSetupCaptured(t, setupHeadlessArgs(), e33SetupEnv()); err != nil {
		t.Fatalf("runSetup: %v\n%s", err, masked(out))
	}
	if got := loadSetupConfig(t).NATS.Port; got != 25555 {
		t.Errorf("nats.port = %d; setup must never change a port the config names", got)
	}
}

func TestSetupInAnExistingProjectWritesNoPort(t *testing.T) {
	setupWorkspace(t)
	t.Setenv("RYSH_NATS_PORT", "")
	e33SessionSeams(t, 0)
	// An existing project: its session name is already saved.
	if err := os.WriteFile("rysh.config.yaml", []byte("rysh:\n  session_name: old-proj-abc123\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runSetupCaptured(t, setupHeadlessArgs(), e33SetupEnv()); err != nil {
		t.Fatalf("runSetup: %v\n%s", err, masked(out))
	}
	if cfg := loadSetupConfig(t); cfg.NATS.Port != 24242 || cfg.NATS.OwnPort {
		t.Errorf("nats port = %d (own %v); an existing project keeps the 24242 default", cfg.NATS.Port, cfg.NATS.OwnPort)
	}
}

func TestSetupWithRYSHNATSPortWritesNoPort(t *testing.T) {
	setupWorkspace(t)
	t.Setenv("RYSH_NATS_PORT", "26003")
	e33SessionSeams(t, 0)
	if out, err := runSetupCaptured(t, setupHeadlessArgs(), e33SetupEnv()); err != nil {
		t.Fatalf("runSetup: %v\n%s", err, masked(out))
	}
	if cfg := loadSetupConfig(t); cfg.NATS.Port != 24242 {
		t.Errorf("nats port in the file = %d; with RYSH_NATS_PORT set, setup writes none", cfg.NATS.Port)
	}
}

func TestSetupSkipsAPortASessionRecordUses(t *testing.T) {
	wd, _ := setupWorkspace(t)
	t.Setenv("RYSH_NATS_PORT", "")
	e33SessionSeams(t, 0)
	first := pickProjectBusPort(wd, func(int) bool { return false })
	if !inProjectBand(first) {
		t.Fatalf("pickProjectBusPort = %d, want a port in the band", first)
	}
	store, err := session.NewStore(config.Load())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(session.Record{Name: "another-proj-1a2b3c", Path: filepath.Join(t.TempDir(), "another"),
		State: "detached", PID: os.Getpid(), NATSPort: first}); err != nil {
		t.Fatal(err)
	}
	if out, err := runSetupCaptured(t, setupHeadlessArgs(), e33SetupEnv()); err != nil {
		t.Fatalf("runSetup: %v\n%s", err, masked(out))
	}
	if got := loadSetupConfig(t).NATS.Port; got == first || !inProjectBand(got) {
		t.Errorf("nats port = %d; want a band port other than %d, which another session's record uses", got, first)
	}
}

func TestPickProjectBusPort(t *testing.T) {
	a, b := "/Users/me/projects/alpha", "/Users/me/projects/beta"
	none := func(int) bool { return false }
	pa, pb := pickProjectBusPort(a, none), pickProjectBusPort(b, none)
	if !inProjectBand(pa) || !inProjectBand(pb) {
		t.Fatalf("ports %d, %d; want both in the band", pa, pb)
	}
	if pa == pb {
		t.Errorf("two directories got the same start port %d", pa)
	}
	if pickProjectBusPort(a, none) != pa {
		t.Error("the start port is not stable for one directory")
	}
	if got := pickProjectBusPort(a, func(p int) bool { return p == pa }); got == pa || !inProjectBand(got) {
		t.Errorf("with %d in use got %d; want the next free band port", pa, got)
	}
	if got := pickProjectBusPort(a, func(int) bool { return true }); got != 0 {
		t.Errorf("a full band gave %d; want 0", got)
	}
}

// security-lead N1: the daemon turns the refusal on exactly when
// the project's own file names the port, and off when RYSH_NATS_PORT set it.
func TestDaemonBusConfigRefusesForeignHostOnlyForAnOwnPort(t *testing.T) {
	for _, tc := range []struct {
		name, file, env string
		want            bool
	}{
		{"own port from the file", "nats:\n  port: 24310\n", "", true},
		{"env port overrides the file", "nats:\n  port: 24310\n", "26004", false},
		{"shared default", "nats:\n  mode: embedded\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "rysh.config.yaml")
			if err := os.WriteFile(path, []byte(tc.file), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("RYSH_NATS_PORT", tc.env)
			if got := daemonRefusesForeignHost(config.LoadFrom(path)); got != tc.want {
				t.Errorf("RefuseForeignHost = %v, want %v", got, tc.want)
			}
		})
	}
}

// security-lead (optional, F-23 style): the daemon's own bus.Config literal in
// main.go names the refusal. A behavioural test of bus.New passes whether or
// not the daemon is told, so the wiring line itself is pinned here.
func TestDaemonBusConfigLiteralNamesTheRefusal(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	call := regexp.MustCompile(`(?s)bus\.New\(bus\.Config\{(.*?)\}\)`)
	daemon := 0
	for _, m := range call.FindAllStringSubmatch(string(src), -1) {
		if !strings.Contains(m[1], "Workspace:") { // the attach client names no workspace
			continue
		}
		daemon++
		if !strings.Contains(m[1], "RefuseForeignHost: daemonRefusesForeignHost(cfg)") {
			t.Errorf("the daemon's bus.Config does not set RefuseForeignHost: daemonRefusesForeignHost(cfg) (D-55 B):\n\tbus.Config{%s}", strings.TrimSpace(m[1]))
		}
	}
	if daemon != 1 {
		t.Errorf("found %d daemon bus.New(bus.Config{...}) call sites with a Workspace in main.go, want 1 — if it moved, move this test with it", daemon)
	}
}
