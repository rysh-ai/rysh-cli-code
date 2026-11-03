// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func loadUI(t *testing.T, ui string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rysh.config.yaml")
	if err := os.WriteFile(path, []byte("ui:\n"+ui), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadFrom(path)
}

// root-order-044 #3 P2: the terminal notification for a "needs you" pane is
// opt-in. Unset, empty and unknown values all mean off.
func TestNotifyTerminalDefaultsOff(t *testing.T) {
	t.Setenv("RYSH_NOTIFY_TERMINAL", "")
	if got := loadUI(t, "  shell: bash\n").NotifyTerminal; got != NotifyTerminalOff {
		t.Fatalf("default NotifyTerminal = %q, want off", got)
	}
}

func TestNotifyTerminalAcceptsOnlyKnownValues(t *testing.T) {
	t.Setenv("RYSH_NOTIFY_TERMINAL", "")
	for in, want := range map[string]string{
		"bell":    NotifyTerminalBell,
		"osc9":    NotifyTerminalOSC9,
		"OSC9":    NotifyTerminalOSC9,
		"off":     NotifyTerminalOff,
		"loud":    NotifyTerminalOff,
		"":        NotifyTerminalOff,
		" bell  ": NotifyTerminalBell,
	} {
		if got := loadUI(t, "  notify_terminal: \""+in+"\"\n").NotifyTerminal; got != want {
			t.Errorf("notify_terminal %q -> %q, want %q", in, got, want)
		}
	}
}

func TestNotifyTerminalEnvOverride(t *testing.T) {
	t.Setenv("RYSH_NOTIFY_TERMINAL", "osc9")
	if got := loadUI(t, "  notify_terminal: bell\n").NotifyTerminal; got != NotifyTerminalOSC9 {
		t.Fatalf("env override: NotifyTerminal = %q, want osc9", got)
	}
	t.Setenv("RYSH_NOTIFY_TERMINAL", "nonsense")
	if got := loadUI(t, "  notify_terminal: bell\n").NotifyTerminal; got != NotifyTerminalOff {
		t.Fatalf("unknown env value: NotifyTerminal = %q, want off", got)
	}
}
