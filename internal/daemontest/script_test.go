// SPDX-License-Identifier: Apache-2.0

package daemontest_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/daemontest"
)

// TestScriptRunsAgainstALiveSession is design 021 §5's "end-to-end" row, which
// shipped as a manual check because nothing could boot a daemon.
//
// Every property the language claims is exercised here against a real daemon in
// one script: a ## command running, its output captured into $RYSH_OUT, a bash
// conditional branching on that, a ## command inside a block via the colon form,
// a pipe from rysh into bash, a bash loop driving rysh, and script arguments.
func TestScriptRunsAgainstALiveSession(t *testing.T) {
	s := daemontest.Fresh(t)

	src := `set -euo pipefail

##new tab
##tab list

if [[ "${RYSH_OUT:-}" == *"tabs (2 total)"* ]]; then
  : ##variable new SCRIPT_MARKER yes
  echo "BRANCH_TAKEN"
else
  echo "UNEXPECTED_TAB_COUNT"; exit 1
fi

##variable list | grep -c SCRIPT_MARKER

for n in 1 2; do
  : ##variable new "LOOP_$n" "v$n"
done
##variable list
echo "LOOP_COUNT=$(echo "$RYSH_OUT" | grep -c LOOP_)"

echo "ARGS=$*"
`
	out, code := s.Script(t, src, "alpha", "beta")
	if code != 0 {
		t.Fatalf("script exited %d:\n%s", code, out)
	}
	for _, want := range []string{"BRANCH_TAKEN", "LOOP_COUNT=2", "ARGS=alpha beta"} {
		if !strings.Contains(out, want) {
			t.Errorf("script output missing %q:\n%s", want, out)
		}
	}
}

// TestScriptExitStatusReachesBash is the property the whole exit-status
// migration exists to serve, checked where it actually matters: inside bash,
// against a real daemon.
func TestScriptExitStatusReachesBash(t *testing.T) {
	s := daemontest.Shared(t)

	t.Run("set -e aborts on a failed command", func(t *testing.T) {
		out, code := s.Script(t, "set -e\n##tab zzznotasubcommand\necho NOT_REACHED\n")
		if code == 0 {
			t.Errorf("script exited 0 despite a failed ## command:\n%s", out)
		}
		if strings.Contains(out, "NOT_REACHED") {
			t.Errorf("execution continued past the failure:\n%s", out)
		}
	})

	t.Run("|| catches a failure", func(t *testing.T) {
		out, code := s.Script(t, "set -e\n: ##tab zzznotasubcommand || echo CAUGHT\necho AFTER\n")
		if code != 0 {
			t.Fatalf("|| did not catch the failure, exited %d:\n%s", code, out)
		}
		if !strings.Contains(out, "CAUGHT") || !strings.Contains(out, "AFTER") {
			t.Errorf("expected CAUGHT and AFTER:\n%s", out)
		}
	})

	t.Run("RYSH_STATUS matches", func(t *testing.T) {
		out, _ := s.Script(t, "##tab zzznotasubcommand\necho \"STATUS=$RYSH_STATUS\"\n")
		if !strings.Contains(out, "STATUS=1") {
			t.Errorf("$RYSH_STATUS did not reflect the failure:\n%s", out)
		}
	})

	t.Run("a successful command leaves status 0", func(t *testing.T) {
		out, code := s.Script(t, "set -e\n##tab list\necho \"STATUS=$RYSH_STATUS\"\n")
		if code != 0 {
			t.Fatalf("script exited %d on a successful command:\n%s", code, out)
		}
		if !strings.Contains(out, "STATUS=0") {
			t.Errorf("expected STATUS=0:\n%s", out)
		}
	})
}

// TestScriptIsAlsoAValidBashScript is the polyglot claim, checked end to end:
// the SAME file that just drove a daemon must also run under plain bash with
// its rysh lines inert. A unit test can check that it parses; only running it
// proves it behaves.
func TestScriptIsAlsoAValidBashScript(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	// Shared: this asserts on the SHAPE of the output, not on a tab count, so
	// it does not need a pristine workspace — and a daemon boot is ~2s.
	s := daemontest.Shared(t)

	src := `set -euo pipefail
echo "BASH_RAN"
##new tab
##tab list
if [[ "${RYSH_OUT:-}" == *"tabs"* ]]; then
  : ##variable new POLYGLOT yes
  echo "RYSH_RAN"
fi
echo "DONE"
`
	// Under rysh: both halves run.
	out, code := s.Script(t, src)
	if code != 0 {
		t.Fatalf("under rysh, exited %d:\n%s", code, out)
	}
	for _, want := range []string{"BASH_RAN", "RYSH_RAN", "DONE"} {
		if !strings.Contains(out, want) {
			t.Errorf("under rysh, missing %q:\n%s", want, out)
		}
	}

	// Under plain bash: the bash half runs, the rysh half is comments. The
	// branch must NOT be taken, because $RYSH_OUT is never set.
	path := filepath.Join(t.TempDir(), "polyglot.rysh")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", path)
	cmd.Dir = t.TempDir()
	bashOut, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("under plain bash, failed: %v\n%s", err, bashOut)
	}
	got := string(bashOut)
	if !strings.Contains(got, "BASH_RAN") || !strings.Contains(got, "DONE") {
		t.Errorf("under plain bash, the bash half did not run:\n%s", got)
	}
	if strings.Contains(got, "RYSH_RAN") {
		t.Errorf("under plain bash, a rysh-gated branch was taken:\n%s", got)
	}
}

// TestScriptEphemeralBootsAndTearsDown covers the CI mode: --ephemeral must
// create a session, run against it, and leave nothing behind. The leak it
// guards against is invisible to the script's own output — you only see it in
// the session list afterwards.
func TestScriptEphemeralBootsAndTearsDown(t *testing.T) {
	// Shared only supplies the config and registry to inspect afterwards; the
	// script under test boots its own session.
	s := daemontest.Shared(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "eph.rysh")
	src := "set -euo pipefail\n##new tab\n##tab list\necho \"SAW=$(echo \"$RYSH_OUT\" | grep -c 'tab-')\"\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	out, code := s.CLI(t, "script", path, "--ephemeral")
	if code != 0 {
		t.Fatalf("--ephemeral exited %d:\n%s", code, out)
	}
	if !strings.Contains(out, "SAW=2") {
		t.Errorf("the ephemeral session did not start clean and gain a tab:\n%s", out)
	}

	// Nothing left behind: the only session in this registry is the fixture's.
	list, _ := s.CLI(t, "list-sessions")
	for _, line := range strings.Split(list, "\n") {
		if strings.Contains(line, "script-") {
			t.Errorf("--ephemeral leaked a session:\n%s", list)
		}
	}
}

// TestScriptCheckVerifiesThePolyglotProperty covers --check, including the one
// construct that breaks a .rysh file under bash: a ## line as a block's only
// statement (design 021 §3.1a).
func TestScriptCheckVerifiesThePolyglotProperty(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	s := daemontest.Shared(t)
	dir := t.TempDir()

	good := filepath.Join(dir, "good.rysh")
	if err := os.WriteFile(good, []byte("if true; then\n  : ##tab list\nfi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := s.CLI(t, "script", good, "--check")
	if code != 0 {
		t.Errorf("--check rejected a valid file (exit %d):\n%s", code, out)
	}
	if !strings.Contains(out, "polyglot: OK") {
		t.Errorf("--check did not confirm the polyglot property:\n%s", out)
	}

	bad := filepath.Join(dir, "bad.rysh")
	if err := os.WriteFile(bad, []byte("if true; then\n  ##tab list\nfi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code = s.CLI(t, "script", bad, "--check")
	if code == 0 {
		t.Errorf("--check accepted a file bash cannot parse:\n%s", out)
	}
	if !strings.Contains(out, ": ##command") {
		t.Errorf("--check did not point at the colon form as the fix:\n%s", out)
	}
}
