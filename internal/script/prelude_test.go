// SPDX-License-Identifier: Apache-2.0

package script

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runScript transpiles src, writes the prelude + body the way `rysh script`
// does, and runs it with a stub standing in for the rysh binary.
//
// The stub is what makes these tests hermetic: the contract being checked is
// between the prelude and bash ($RYSH_OUT, $RYSH_STATUS, exit codes), not
// anything about a live session.
func runScript(t *testing.T, src string) (stdout string, exitCode int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	dir := t.TempDir()

	// The stub echoes the command it was given and derives an exit status from
	// it, so a test can ask for a failure on demand.
	stub := filepath.Join(dir, "rysh-stub")
	stubSrc := `#!/usr/bin/env bash
# args: exec [--session s] [--tab-id t] [--pane-id p] -- <command>
cmd=""
while [ $# -gt 0 ]; do
  case "$1" in
    --) shift; cmd="$*"; break ;;
    *) shift ;;
  esac
done
echo "ran:${cmd}"
case "$cmd" in
  *fail*) exit 1 ;;
  *) exit 0 ;;
esac
`
	if err := os.WriteFile(stub, []byte(stubSrc), 0o700); err != nil {
		t.Fatal(err)
	}

	result, err := Transpile(src)
	if err != nil {
		t.Fatalf("transpile: %v", err)
	}
	body := filepath.Join(dir, "test.rysh")
	if err := os.WriteFile(body, []byte(result.Bash), 0o600); err != nil {
		t.Fatal(err)
	}
	prelude := filepath.Join(dir, "prelude.sh")
	if err := os.WriteFile(prelude, []byte(Prelude(body)), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", prelude)
	cmd.Env = append(os.Environ(), "RYSH_BIN="+stub, "RYSH_SESSION=", "RYSH_TAB=", "RYSH_PANE=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("run: %v\n%s", err, out)
	}
	return string(out), 0
}

// TestPrelude_CapturesOutput checks the $RYSH_OUT half of the contract: the
// command's output is both printed and captured.
func TestPrelude_CapturesOutput(t *testing.T) {
	out, code := runScript(t, "##pane info\necho \"captured:[$RYSH_OUT]\"\n")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "ran:##pane info") {
		t.Errorf("command output was not printed:\n%s", out)
	}
	if !strings.Contains(out, "captured:[ran:##pane info]") {
		t.Errorf("$RYSH_OUT was not populated:\n%s", out)
	}
}

// TestPrelude_ExitStatus is the property that makes this a language rather
// than a macro expander: a failed ## command must be visible to bash.
func TestPrelude_ExitStatus(t *testing.T) {
	t.Run("status is propagated", func(t *testing.T) {
		out, code := runScript(t, "##pane fail\necho \"status=$RYSH_STATUS\"\n")
		if code != 0 {
			t.Fatalf("script should not have aborted without set -e:\n%s", out)
		}
		if !strings.Contains(out, "status=1") {
			t.Errorf("$RYSH_STATUS not set from the command:\n%s", out)
		}
	})

	t.Run("dollar-question matches", func(t *testing.T) {
		out, _ := runScript(t, "##pane fail\necho \"q=$?\"\n")
		if !strings.Contains(out, "q=1") {
			t.Errorf("$? did not reflect the command's failure:\n%s", out)
		}
	})

	t.Run("set -e aborts", func(t *testing.T) {
		out, code := runScript(t, "set -e\n##pane fail\necho NOT_REACHED\n")
		if code == 0 {
			t.Errorf("set -e did not abort on a failed ## command:\n%s", out)
		}
		if strings.Contains(out, "NOT_REACHED") {
			t.Errorf("execution continued past a failed command under set -e:\n%s", out)
		}
	})

	t.Run("or-else catches", func(t *testing.T) {
		out, code := runScript(t, "set -e\n##pane fail || echo CAUGHT\necho AFTER\n")
		if code != 0 {
			t.Fatalf("|| should have caught the failure:\n%s", out)
		}
		if !strings.Contains(out, "CAUGHT") || !strings.Contains(out, "AFTER") {
			t.Errorf("|| did not catch the failure:\n%s", out)
		}
	})

	// `if ##pane ok; then ...` is deliberately NOT supported: the ## would not
	// be in statement position, and the line would not be valid bash either
	// (the comment swallows the condition, leaving `if` with nothing). These
	// are the two forms that branch on a command AND stay bash-valid.
	t.Run("and-then branches on success", func(t *testing.T) {
		out, code := runScript(t, ": ##pane ok && echo YES\n")
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if !strings.Contains(out, "YES") {
			t.Errorf("&& did not fire on a successful command:\n%s", out)
		}
	})

	t.Run("status variable branches", func(t *testing.T) {
		src := "##pane fail\nif [ \"$RYSH_STATUS\" -ne 0 ]; then echo FAILED; else echo OK; fi\n"
		out, _ := runScript(t, src)
		if !strings.Contains(out, "FAILED") {
			t.Errorf("branching on $RYSH_STATUS did not see the failure:\n%s", out)
		}
	})
}

// TestPrelude_PipesAndRedirects checks that a ## command composes with bash's
// own plumbing, which is the difference between augmenting bash and merely
// living next to it.
func TestPrelude_PipesAndRedirects(t *testing.T) {
	out, code := runScript(t, "##pane info | tr a-z A-Z\n")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "RAN:##PANE INFO") {
		t.Errorf("output did not flow through the pipe:\n%s", out)
	}
}

// TestPrelude_LineNumbersSurvive is the reason the prelude sources the body
// instead of being prepended to it. A syntax error must name the author's line.
func TestPrelude_LineNumbersSurvive(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	// Line 3 is a deliberate syntax error.
	src := "echo one\necho two\nthis is broken (\n"
	result, err := Transpile(src)
	if err != nil {
		t.Fatal(err)
	}
	body := filepath.Join(dir, "deploy.rysh")
	if err := os.WriteFile(body, []byte(result.Bash), 0o600); err != nil {
		t.Fatal(err)
	}
	prelude := filepath.Join(dir, "prelude.sh")
	if err := os.WriteFile(prelude, []byte(Prelude(body)), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", prelude)
	cmd.Env = append(os.Environ(), "RYSH_BIN=/bin/true")
	out, _ := cmd.CombinedOutput()

	if !strings.Contains(string(out), "deploy.rysh") {
		t.Errorf("diagnostic does not name the source file:\n%s", out)
	}
	if !strings.Contains(string(out), "line 3") {
		t.Errorf("diagnostic does not name the source line (prelude shifted it?):\n%s", out)
	}
}

// TestPrelude_UnsetBeforeFirstCommand keeps a .rysh file honest under plain
// bash: the variables start empty, exactly as they would if bash ran the file
// with every ## line commented out.
func TestPrelude_UnsetBeforeFirstCommand(t *testing.T) {
	out, code := runScript(t, "echo \"before:[$RYSH_OUT][$RYSH_STATUS]\"\n")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "before:[][0]") {
		t.Errorf("RYSH_OUT/RYSH_STATUS not initialised:\n%s", out)
	}
}
