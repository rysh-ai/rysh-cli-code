// SPDX-License-Identifier: Apache-2.0

package main

// D-55 B design §4: `rysh doctor` prints the project's bus port, where the port
// comes from, and whether this project's daemon serves it. No test here starts
// a daemon: live sessions are records whose PID is this test process, and the
// port probe is deps.dialNATS.

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

const busTestSession = "proj-def456"

func busPortState(t *testing.T, yaml, envPort string) config.Config {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RYSH_SESSION", "")
	t.Setenv("RYSH_SESSION_DIR", "")
	t.Setenv("RYSH_NATS_PORT", envPort)
	if err := os.WriteFile("rysh.config.yaml", []byte("rysh:\n  session_name: "+busTestSession+"\n"+yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return config.Load()
}

func liveSessionOn(t *testing.T, cfg config.Config, name string, port int) {
	t.Helper()
	store, err := session.NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(session.Record{Name: name, Path: t.TempDir(), State: "detached",
		PID: os.Getpid(), NATSPort: port}); err != nil {
		t.Fatal(err)
	}
}

func busDeps(listening bool) doctorDeps {
	d := fakeDeps()
	d.dialNATS = func(int, time.Duration) error {
		if listening {
			return nil
		}
		return errors.New("connection refused")
	}
	d.getenv = os.Getenv
	return d
}

func busCheck(t *testing.T, checks []doctorCheck) doctorCheck {
	t.Helper()
	c, ok := findCheck(checks, "daemon", "bus port")
	if !ok {
		t.Fatalf("doctor prints no bus port line; checks: %+v", checks)
	}
	return c
}

func wantDetail(t *testing.T, c doctorCheck, status string, parts ...string) {
	t.Helper()
	if c.Status != status {
		t.Errorf("status = %s, want %s: %+v", c.Status, status, c)
	}
	for _, p := range parts {
		if !strings.Contains(c.Detail, p) {
			t.Errorf("detail %q should contain %q", c.Detail, p)
		}
	}
}

func TestDoctorBusPortServedByThisProject(t *testing.T) {
	cfg := busPortState(t, "nats:\n  port: 24310\n", "")
	liveSessionOn(t, cfg, busTestSession, 24310)
	c := busCheck(t, doctorChecks(cfg, busDeps(true)))
	wantDetail(t, c, doctorPass, "24310", "rysh.config.yaml", "this project's daemon", busTestSession)
}

func TestDoctorBusPortHeldByAnotherProject(t *testing.T) {
	cfg := busPortState(t, "nats:\n  port: 24310\n", "")
	liveSessionOn(t, cfg, "other-proj-abc123", 24310)
	c := busCheck(t, doctorChecks(cfg, busDeps(true)))
	wantDetail(t, c, doctorWarn, "24310", "other-proj-abc123")
	if !strings.Contains(c.Fix, "nats.port") {
		t.Errorf("fix %q should say how to choose another nats.port", c.Fix)
	}
}

func TestDoctorBusPortHeldByAnUnknownListener(t *testing.T) {
	cfg := busPortState(t, "nats:\n  port: 24310\n", "")
	c := busCheck(t, doctorChecks(cfg, busDeps(true)))
	wantDetail(t, c, doctorWarn, "24310", "no rysh session")
	if !strings.Contains(c.Fix, "nats.port") {
		t.Errorf("fix %q should say how to choose another nats.port", c.Fix)
	}
}

func TestDoctorBusPortFree(t *testing.T) {
	cfg := busPortState(t, "nats:\n  port: 24310\n", "")
	c := busCheck(t, doctorChecks(cfg, busDeps(false)))
	wantDetail(t, c, doctorPass, "24310", "free")
}

// A daemon started before `rysh setup` wrote the project's port still runs on
// the old one until it restarts.
func TestDoctorBusPortDaemonOnAnotherPort(t *testing.T) {
	cfg := busPortState(t, "nats:\n  port: 24310\n", "")
	liveSessionOn(t, cfg, busTestSession, 24242)
	c := busCheck(t, doctorChecks(cfg, busDeps(false)))
	wantDetail(t, c, doctorWarn, "24310", "24242")
	if !strings.Contains(c.Fix, "rysh stop") {
		t.Errorf("fix %q should say to restart the daemon", c.Fix)
	}
}

func TestDoctorBusPortSharedDefault(t *testing.T) {
	cfg := busPortState(t, "", "")
	liveSessionOn(t, cfg, "other-proj-abc123", 24242)
	c := busCheck(t, doctorChecks(cfg, busDeps(true)))
	wantDetail(t, c, doctorPass, "24242", "shared default", "other-proj-abc123", "guest")
}

func TestDoctorBusPortFromTheEnvironment(t *testing.T) {
	cfg := busPortState(t, "nats:\n  port: 24310\n", "26004")
	c := busCheck(t, doctorChecks(cfg, busDeps(false)))
	wantDetail(t, c, doctorPass, "26004", "RYSH_NATS_PORT")
}
