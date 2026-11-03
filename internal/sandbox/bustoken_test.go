// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// gap01 slice 3 (e20 T1 c): every dir a bus token could live in is always
// hidden, whatever the agent's env says.
func TestBusTokenDirsAreAlwaysHidden(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", xdg)
	home := "/home/u"
	sp := strings.Join(SensitivePaths(home), "\n")
	for _, want := range []string{filepath.Join(home, ".rysh-run"), filepath.Join(xdg, "rysh")} {
		if !strings.Contains(sp, want) {
			t.Errorf("SensitivePaths misses %s", want)
		}
	}
	if runtime.GOOS == "linux" && !strings.Contains(sp, "/run/user/") {
		t.Error("SensitivePaths misses /run/user/<uid>/rysh")
	}
}

func TestMacOSDeniesTheBusTokenDir(t *testing.T) {
	l := newLayout(t)
	agent := installAgent(t, filepath.Join(l.root, "opt", "bin"))
	plan, err := macPlan(t, l, agent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.SBPL, filepath.Join(l.home, ".rysh-run")) {
		t.Errorf("profile does not name %s", filepath.Join(l.home, ".rysh-run"))
	}
}
