// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// gap02 slice 2: `rysh service install|uninstall|status` keeps a session
// running across logout and reboot. It prints by default and writes only with
// --write; it never runs launchctl or systemctl.

func serviceTestEnv(t *testing.T, goos string) (serviceEnv, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	return serviceEnv{goos: goos, home: t.TempDir(), exe: "/opt/rysh/bin/rysh", workDir: "/home/me/work", out: &out}, &out
}

func serviceCfg(session string) config.Config {
	return config.Config{SessionName: session}
}

func TestServiceInstallPrintsAndWritesNothingByDefault(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		env, out := serviceTestEnv(t, goos)
		if err := runServiceCmd(serviceCfg("work"), []string{"service", "install"}, env); err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
		path := serviceUnitPath(goos, env.home, "work")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s: dry run wrote %s", goos, path)
		}
		s := out.String()
		for _, want := range []string{"DRY RUN", path, "daemon", "work", "--write"} {
			if !strings.Contains(s, want) {
				t.Fatalf("%s: output lacks %q:\n%s", goos, want, s)
			}
		}
	}
}

func TestServiceInstallWriteDarwin(t *testing.T) {
	env, out := serviceTestEnv(t, "darwin")
	if err := runServiceCmd(serviceCfg("work"), []string{"service", "install", "--write"}, env); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(env.home, "Library", "LaunchAgents", "ai.rysh.session.work.plist")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		"<string>ai.rysh.session.work</string>",
		"<string>/opt/rysh/bin/rysh</string>", "<string>daemon</string>", "<string>work</string>",
		"<key>WorkingDirectory</key>\n\t<string>/home/me/work</string>",
		"<key>KeepAlive</key>", "<key>RunAtLoad</key>", serviceMarker,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("plist lacks %q:\n%s", want, s)
		}
	}
	if !strings.Contains(out.String(), "launchctl bootstrap gui/") {
		t.Fatalf("no load hint:\n%s", out.String())
	}
}

func TestServiceInstallWriteLinuxQuotesExecStart(t *testing.T) {
	env, out := serviceTestEnv(t, "linux")
	env.exe = "/opt/my rysh/rysh"
	env.workDir = "/home/me/my work"
	if err := runServiceCmd(serviceCfg("work"), []string{"service", "install", "--write"}, env); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(env.home, ".config", "systemd", "user", "rysh-session-work.service"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{
		`ExecStart="/opt/my rysh/rysh" "daemon" "work"`,
		"WorkingDirectory=/home/me/my work",
		"Restart=always", "WantedBy=default.target", serviceMarker,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("unit lacks %q:\n%s", want, s)
		}
	}
	for _, want := range []string{"systemctl --user enable --now rysh-session-work.service", "loginctl enable-linger"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestSystemdQuoteEscapesSpecifiersAndVariables(t *testing.T) {
	if got, want := systemdQuote(`/a/%h/$X/"q"\b`), `"/a/%%h/$$X/\"q\"\\b"`; got != want {
		t.Fatalf("systemdQuote = %s, want %s", got, want)
	}
}

func TestServiceInstallRefusesToOverwriteADifferentFile(t *testing.T) {
	env, _ := serviceTestEnv(t, "linux")
	path := serviceUnitPath("linux", env.home, "work")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[Service]\nExecStart=/bin/mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runServiceCmd(serviceCfg("work"), []string{"service", "install", "--write"}, env)
	if err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatalf("overwrote a foreign unit: err=%v", err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "/bin/mine") {
		t.Fatal("foreign unit changed")
	}
	// Re-installing our own identical unit is fine.
	env2, _ := serviceTestEnv(t, "linux")
	for i := 0; i < 2; i++ {
		if err := runServiceCmd(serviceCfg("work"), []string{"service", "install", "--write"}, env2); err != nil {
			t.Fatalf("re-install %d: %v", i, err)
		}
	}
}

func TestServiceUninstall(t *testing.T) {
	env, out := serviceTestEnv(t, "linux")
	if err := runServiceCmd(serviceCfg("work"), []string{"service", "install", "--write"}, env); err != nil {
		t.Fatal(err)
	}
	path := serviceUnitPath("linux", env.home, "work")
	out.Reset()
	if err := runServiceCmd(serviceCfg("work"), []string{"service", "uninstall"}, env); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("dry-run uninstall removed the unit")
	}
	if !strings.Contains(out.String(), "systemctl --user disable --now rysh-session-work.service") {
		t.Fatalf("no unload hint:\n%s", out.String())
	}
	if err := runServiceCmd(serviceCfg("work"), []string{"service", "uninstall", "--write"}, env); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("uninstall --write kept the unit")
	}
	// A file rysh did not write is never removed.
	if err := os.WriteFile(path, []byte("[Service]\nExecStart=/bin/mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runServiceCmd(serviceCfg("work"), []string{"service", "uninstall", "--write"}, env); err == nil {
		t.Fatal("removed a unit rysh did not write")
	}
}

func TestServiceStatus(t *testing.T) {
	env, out := serviceTestEnv(t, "darwin")
	if err := runServiceCmd(serviceCfg("work"), []string{"service", "status"}, env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not installed") {
		t.Fatalf("status before install:\n%s", out.String())
	}
	_ = runServiceCmd(serviceCfg("work"), []string{"service", "install", "--write"}, env)
	out.Reset()
	if err := runServiceCmd(serviceCfg("work"), []string{"service", "status"}, env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "installed") || strings.Contains(out.String(), "not installed") {
		t.Fatalf("status after install:\n%s", out.String())
	}
}

func TestServiceSessionFlagAndValidation(t *testing.T) {
	env, out := serviceTestEnv(t, "linux")
	if err := runServiceCmd(serviceCfg(""), []string{"service", "install", "--session", "night-build"}, env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rysh-session-night-build.service") {
		t.Fatalf("--session ignored:\n%s", out.String())
	}
	for _, bad := range []string{"a b", "../x", "a.b", "a/b", "-x", ""} {
		if err := runServiceCmd(serviceCfg(""), []string{"service", "install", "--session", bad}, env); err == nil {
			t.Fatalf("accepted session %q", bad)
		}
	}
	// No --session and no configured session: "default".
	out.Reset()
	if err := runServiceCmd(serviceCfg(""), []string{"service", "install"}, env); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rysh-session-default.service") {
		t.Fatalf("default session:\n%s", out.String())
	}
}

func TestServiceUnsupportedOSAndUsage(t *testing.T) {
	env, _ := serviceTestEnv(t, "windows")
	if err := runServiceCmd(serviceCfg("work"), []string{"service", "install"}, env); err == nil {
		t.Fatal("windows should be refused")
	}
	env, _ = serviceTestEnv(t, "linux")
	for _, args := range [][]string{{"service"}, {"service", "start"}, {"service", "install", "--bogus"}} {
		if err := runServiceCmd(serviceCfg("work"), args, env); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
