// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"strings"
	"testing"
)

// TestMain makes this package's tests hermetic with respect to the environment
// they are run FROM.
//
// The problem it fixes, concretely. Load() applies ~45 RYSH_* environment
// overrides on top of whatever the config file said — that is a feature, and it
// is what lets a session be steered without editing yaml. But it means a config
// test asserting "the file's session_name wins" is only testing the code when
// no RYSH_SESSION is set in the shell that launched `go test`.
//
// Every pane rysh opens exports RYSH_SESSION/RYSH_TAB/RYSH_LANE/RYSH_STACK/
// RYSH_PANE into its shell (actors.paneIdentityEnv). So running the suite from
// inside a rysh pane — which is where anyone developing rysh runs it — made
// five tests in this package fail with the developer's own live session name:
//
//	config_test.go:47: SessionName = "macmini-rysh-elect", want "explicit-cfg"
//
// The tests were right and the code was right; the environment was reaching
// past both. Five failures, one cause, and they had been red long enough that
// the whole suite was being scrolled past.
//
// isolateConfigEnv already existed and cleared four RYSH_* vars by name. A
// hand-maintained allowlist against 45 env reads is the same drift the ##
// command table was restructured to kill: it knew nothing about RYSH_SESSION,
// and it could not know about the 46th. So this clears by PREFIX, from the
// actual environment, and a variable added to config.go tomorrow is covered
// without anyone remembering this file exists.
//
// This weakens nothing. No assertion changes; a test that WANTS an override
// still sets it with t.Setenv, which is scoped and restored per test. What goes
// away is only the ambient contamination — and the one override that was
// previously "covered" by accident now has a deliberate test below.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		key, _, ok := strings.Cut(kv, "=")
		if ok && strings.HasPrefix(key, "RYSH_") {
			_ = os.Unsetenv(key)
		}
	}
	os.Exit(m.Run())
}

// TestRyshSessionEnvOverridesTheConfigFile is the coverage TestMain would
// otherwise have removed.
//
// Before the cleanup above, RYSH_SESSION had no test of its own anywhere in
// this package. It was exercised only by leaking into five tests that were
// asserting something else — which is not coverage, because those tests fail
// when it works. This asserts the override deliberately, so the behaviour is
// pinned by a test that WANTS it rather than by five that are ruined by it.
func TestRyshSessionEnvOverridesTheConfigFile(t *testing.T) {
	isolateConfigEnv(t)
	writeConfig(t, "rysh:\n  session_name: \"from-file\"\n")

	// Without the override the file wins.
	cfg, err := loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.SessionName != "from-file" {
		t.Fatalf("SessionName = %q, want from-file — the config file should win when no env is set",
			cfg.SessionName)
	}

	// With it, the environment wins.
	t.Setenv("RYSH_SESSION", "from-env")
	cfg, err = loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.SessionName != "from-env" {
		t.Errorf("SessionName = %q, want from-env — RYSH_SESSION must override the config file",
			cfg.SessionName)
	}

	// And an empty value is "unset", not "the session is named empty string".
	t.Setenv("RYSH_SESSION", "")
	cfg, err = loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.SessionName != "from-file" {
		t.Errorf("SessionName = %q, want from-file — an empty RYSH_SESSION must not override",
			cfg.SessionName)
	}
}
