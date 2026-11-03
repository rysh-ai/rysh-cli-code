// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"strings"
	"testing"
)

// planWindowsShell is pure, so the Windows pane's argv and environment are
// tested here on any OS; only the ConPTY start itself needs Windows.
func TestPlanWindowsShell(t *testing.T) {
	base := []string{`Path=C:\w`, "rysh_web_port=7", "RYSH_WEB_HOST=x", "Rysh_Web_Control=1", "TERM=dumb", "=C:=C:\\w"}
	extras := []string{"TERM=xterm-256color", "RYSH_PANE=p1"}
	look := func(found ...string) func(string) (string, error) {
		return func(name string) (string, error) {
			for _, f := range found {
				if strings.EqualFold(f, name) {
					return `C:\bin\` + name, nil
				}
			}
			return "", errors.New("not found")
		}
	}
	noEnv := func(string) string { return "" }

	argv, env, err := planWindowsShell("/bin/bash", true, base, extras, noEnv, look("pwsh.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != `C:\bin\pwsh.exe` {
		t.Errorf("argv[0] = %q, want pwsh: a POSIX default names nothing on Windows", argv[0])
	}
	if !strings.Contains(strings.Join(argv, " "), "-EncodedCommand") {
		t.Errorf("pwsh argv %q lacks the OSC 7 prompt hook", argv)
	}
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, gone := range []string{"rysh_web_port", "RYSH_WEB_HOST", "Rysh_Web_Control", "TERM=dumb"} {
		if strings.Contains(joined, "\n"+gone) {
			t.Errorf("env still has %q (Windows names are case-insensitive):\n%s", gone, joined)
		}
	}
	for _, want := range []string{`Path=C:\w`, "TERM=xterm-256color", "RYSH_PANE=p1", "=C:=C:\\w"} {
		if !strings.Contains(joined, "\n"+want+"\n") {
			t.Errorf("env lacks %q:\n%s", want, joined)
		}
	}

	argv, env, err = planWindowsShell("cmd", true, base, extras, noEnv, look("cmd.exe"))
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) != 1 || argv[0] != `C:\bin\cmd.exe` {
		t.Errorf("cmd argv = %q", argv)
	}
	if !strings.Contains(strings.Join(env, "\n"), "PROMPT=$E]7;") {
		t.Errorf("cmd env lacks the OSC 7 PROMPT: %q", env)
	}

	if _, _, err := planWindowsShell("", true, base, extras, noEnv, look()); err == nil {
		t.Error("no shell anywhere: want an error, not an empty argv")
	}
}
