// SPDX-License-Identifier: Apache-2.0

package main

// gap02 slice 2 (design 074): `rysh service install|uninstall|status` — keep a
// session daemon running across logout and reboot on a host you leave on.
//
// It writes one OS unit per session that runs `rysh daemon <session>` in the
// workspace directory (rysh state is project-local): a launchd agent on macOS,
// a systemd user unit on Linux. Like `rysh assistant --install-daemon`, it
// wraps the existing daemon: no second process, no port.
//
// It is deliberately hands-off:
//   - Without --write it only prints the path, the unit and the next commands.
//   - It never runs launchctl or systemctl; loading is the user's own step.
//   - It never overwrites or removes a file it did not write (serviceMarker).

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
)

const serviceUsage = "usage: rysh service install|uninstall|status [--session <name>] [--write]"

// serviceMarker is in every unit rysh writes, so uninstall and re-install can
// tell its own file from one the user wrote.
const serviceMarker = "written by rysh service install"

// serviceNameRe is what a session name may be to become part of a file name
// and a launchd label.
var serviceNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]*$`)

// serviceEnv is everything the command reads from the machine; tests replace it.
type serviceEnv struct {
	goos, home, exe, workDir string
	out                      io.Writer
}

func defaultServiceEnv() (serviceEnv, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return serviceEnv{}, fmt.Errorf("resolve home dir: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return serviceEnv{}, fmt.Errorf("resolve executable path: %w", err)
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	wd, err := os.Getwd()
	if err != nil {
		return serviceEnv{}, fmt.Errorf("resolve working directory: %w", err)
	}
	return serviceEnv{goos: runtime.GOOS, home: home, exe: exe, workDir: wd, out: os.Stdout}, nil
}

// serviceLabel is the launchd label / systemd unit stem for a session.
func serviceLabel(goos, session string) string {
	if goos == "darwin" {
		return "ai.rysh.session." + session
	}
	return "rysh-session-" + session
}

// serviceUnitPath is where the unit for session lives, "" when goos has none.
func serviceUnitPath(goos, home, session string) string {
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", serviceLabel(goos, session)+".plist")
	case "linux":
		return filepath.Join(home, ".config", "systemd", "user", serviceLabel(goos, session)+".service")
	}
	return ""
}

// serviceLaunchdPlist is the macOS agent: started at login, restarted if it
// exits, throttled so a daemon that cannot start does not spin.
func serviceLaunchdPlist(exe, session, workDir string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- %s -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>daemon</string>
		<string>%s</string>
	</array>
	<key>WorkingDirectory</key>
	<string>%s</string>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
</dict>
</plist>
`, serviceMarker, xmlEscape(serviceLabel("darwin", session)), xmlEscape(exe), xmlEscape(session), xmlEscape(workDir))
}

// systemdQuote quotes one ExecStart argument: double quotes, with backslash
// and quote escaped, % doubled (specifiers) and $ doubled (variables).
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$")
	return `"` + r.Replace(s) + `"`
}

// serviceSystemdUnit is the Linux user unit. Surviving logout also needs
// lingering, which the install output says.
func serviceSystemdUnit(exe, session, workDir string) string {
	return fmt.Sprintf(`# %s
[Unit]
Description=rysh session %s
After=network-online.target

[Service]
ExecStart=%s %s %s
WorkingDirectory=%s
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`, serviceMarker, session, systemdQuote(exe), systemdQuote("daemon"), systemdQuote(session),
		strings.ReplaceAll(workDir, "%", "%%"))
}

// runServiceCmd implements `rysh service`.
func runServiceCmd(cfg config.Config, args []string, env serviceEnv) error {
	usage := errors.New(progname.Rewrite(serviceUsage))
	if len(args) < 2 {
		return usage
	}
	action := args[1]
	if action != "install" && action != "uninstall" && action != "status" {
		return usage
	}
	session := cfg.SessionName
	write := false
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--write":
			write = true
		case "--session":
			if i+1 >= len(args) {
				return usage
			}
			i++
			session = args[i]
			if session == "" {
				return errors.New("service: --session needs a name")
			}
		default:
			return usage
		}
	}
	if session == "" {
		session = "default"
	}
	if !serviceNameRe.MatchString(session) {
		return fmt.Errorf("service: session name %q must be letters, digits, '_' or '-' (not starting with '-')", session)
	}
	path := serviceUnitPath(env.goos, env.home, session)
	if path == "" {
		return fmt.Errorf("service: %s has no supported unit (macOS launchd, Linux systemd); run `%s daemon %s` under your own supervisor",
			env.goos, progname.Rewrite("rysh"), session)
	}
	label := serviceLabel(env.goos, session)
	out := env.out

	switch action {
	case "status":
		b, err := os.ReadFile(path)
		switch {
		case os.IsNotExist(err):
			fmt.Fprintf(out, "%s: not installed (%s)\n", label, path)
		case err != nil:
			return fmt.Errorf("service: read %s: %w", path, err)
		case !bytes.Contains(b, []byte(serviceMarker)):
			fmt.Fprintf(out, "%s: %s exists but was not written by rysh\n", label, path)
		default:
			fmt.Fprintf(out, "%s: installed (%s)\n", label, path)
			fmt.Fprintf(out, "whether it is loaded: %s\n", serviceStatusHint(env, label))
		}
		return nil

	case "install":
		var unit string
		if env.goos == "darwin" {
			unit = serviceLaunchdPlist(env.exe, session, env.workDir)
		} else {
			unit = serviceSystemdUnit(env.exe, session, env.workDir)
		}
		if b, err := os.ReadFile(path); err == nil && string(b) != unit {
			if !bytes.Contains(b, []byte(serviceMarker)) {
				return fmt.Errorf("service: %s exists and was not written by rysh; move it away first", path)
			}
			if write {
				fmt.Fprintf(out, "replacing the rysh unit at %s\n", path)
			}
		}
		if !write {
			fmt.Fprintf(out, "DRY RUN: nothing written. Re-run with --write to write %s:\n\n%s\n", path, unit)
		} else {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("service: create %s: %w", filepath.Dir(path), err)
			}
			if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
				return fmt.Errorf("service: write %s: %w", path, err)
			}
			fmt.Fprintf(out, "wrote %s\n", path)
		}
		fmt.Fprintf(out, "it runs: %s daemon %s   (in %s)\n", env.exe, session, env.workDir)
		fmt.Fprintln(out, "stop the session first if it is running, so the unit owns the daemon.")
		fmt.Fprintln(out, "then load it yourself (rysh does not):")
		if env.goos == "darwin" {
			fmt.Fprintf(out, "  launchctl bootstrap gui/$(id -u) %s\n", path)
		} else {
			fmt.Fprintf(out, "  systemctl --user daemon-reload && systemctl --user enable --now %s.service\n", label)
			fmt.Fprintln(out, "  sudo loginctl enable-linger \"$USER\"   # keep it running after logout")
		}
		return nil

	default: // uninstall
		b, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			fmt.Fprintf(out, "%s: not installed (%s)\n", label, path)
			return nil
		}
		if err != nil {
			return fmt.Errorf("service: read %s: %w", path, err)
		}
		if !bytes.Contains(b, []byte(serviceMarker)) {
			return fmt.Errorf("service: %s was not written by rysh; not removing it", path)
		}
		fmt.Fprintln(out, "unload it first (rysh does not):")
		if env.goos == "darwin" {
			fmt.Fprintf(out, "  launchctl bootout gui/$(id -u)/%s\n", label)
		} else {
			fmt.Fprintf(out, "  systemctl --user disable --now %s.service\n", label)
		}
		if !write {
			fmt.Fprintf(out, "DRY RUN: %s not removed. Re-run with --write to remove it.\n", path)
			return nil
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("service: remove %s: %w", path, err)
		}
		fmt.Fprintf(out, "removed %s\n", path)
		return nil
	}
}

func serviceStatusHint(env serviceEnv, label string) string {
	if env.goos == "darwin" {
		return "launchctl print gui/$(id -u)/" + label
	}
	return "systemctl --user status " + label + ".service"
}
