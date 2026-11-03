// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package actors

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/proxy"
)

// Strict mode identifies an allowed CLI by the process's REAL executable, at
// the CLI's known install locations (proxy.IdentifyCLI). Everything else the
// OS reports about a process (its kernel name, the path it was exec'd through,
// argv) is the launcher's choice, so it can neither make a process a CLI nor
// stop a real one being identified.
//
// These tests use stand-in binaries and scripts in temp dirs, never a vendor
// CLI. The fake home's .local/share/claude/versions is the native installer's
// install location; HOME is pointed at it only after the stand-ins are built,
// so `go build` keeps the real cache. Every process a test starts is its own
// and is stopped by its own pid/group in t.Cleanup.

// installedClaude builds a stand-in claude at the native installer's install
// location under a fresh fake home, and returns the home and the binary.
func installedClaude(t *testing.T, version string) (home, real string) {
	t.Helper()
	home = t.TempDir()
	versions := filepath.Join(home, ".local", "share", "claude", "versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	real = filepath.Join(versions, version)
	buildSleeper(t, real)
	return home, real
}

// startExe execs exe with argv (see startOwnChild) and waits until the kernel
// names it wantComm. It returns the pid.
func startExe(t *testing.T, exe string, argv []string, wantComm string) int {
	t.Helper()
	pid := startOwnChild(t, exe, argv)
	var comm string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if comm = processName(pid); comm == wantComm {
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("processName(%d) = %q, want %q: the fixture did not start as laid out", pid, comm, wantComm)
	return 0
}

// startOwnChild starts exe with argv as this test's own child, preferably as
// the leader of a new process group, and stops it in t.Cleanup.
//
// A pane may refuse a new process group ("child setpgid ... Operation not
// permitted", rr-msg-332). Go reports that as a cmd.Start error, so the child
// is then started again without one, and stopOwnChild signals only its pid.
func startOwnChild(t *testing.T, exe string, argv []string) int {
	t.Helper()
	cmd := &exec.Cmd{Path: exe, Args: argv}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Logf("start %s in its own group failed (%v); starting it without one", filepath.Base(exe), err)
		cmd = &exec.Cmd{Path: exe, Args: argv}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start %s: %v", filepath.Base(exe), err)
		}
	}
	t.Cleanup(func() { stopOwnChild(t, cmd, exe) })
	return cmd.Process.Pid
}

// stopOwnChild stops a child this test started, and reaps it.
//
// The group is signalled only when the child provably leads it
// (Getpgid(pid) == pid). Otherwise only the single pid is signalled, and only
// after its real executable is checked against what was started. The child
// is not yet reaped, so its pid cannot have been reused. A group this test
// did not create is never signalled: it could be the pane's own.
func stopOwnChild(t *testing.T, cmd *exec.Cmd, exe string) {
	t.Helper()
	pid := cmd.Process.Pid
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	} else if want, err := filepath.EvalSymlinks(exe); err == nil && processRealExe(pid) == want {
		_ = cmd.Process.Signal(syscall.SIGKILL)
	} else {
		t.Logf("pid %d is not in its own group and its executable does not match %s; not signalled", pid, filepath.Base(exe))
	}
	_ = cmd.Wait()
}

// The fallback path: a child that is NOT a group leader (started without
// Setpgid, so it shares this test's group) is stopped by its pid alone, and
// the test's own group, which a group kill would hit, survives.
func TestStopOwnChildSignalsOnlyThePidWithoutAnOwnGroup(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "ryshnogroup")
	buildSleeper(t, exe)
	cmd := &exec.Cmd{Path: exe, Args: []string{"ryshnogroup", "60"}}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("getpgid: %v", err)
	}
	if pgid == pid {
		t.Fatalf("the child leads its own group (%d); this test needs one that does not", pgid)
	}
	if own := syscall.Getpgrp(); pgid != own {
		t.Logf("child pgid %d differs from the test's group %d", pgid, own)
	}
	done := make(chan struct{})
	go func() { stopOwnChild(t, cmd, exe); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the child was not stopped by its pid")
	}
	if cmd.ProcessState == nil || cmd.ProcessState.Success() {
		t.Fatalf("the child did not die by signal: %v", cmd.ProcessState)
	}
	// Reaching here at all is the other half: a kill of the shared group
	// would have taken this test process with it.
}

// Records which path this pane takes: does cmd.Start with Setpgid succeed,
// and does the child then lead its own group?
func TestOwnProcessGroupIsAvailableHere(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "ryshpgidprobe")
	buildSleeper(t, exe)
	cmd := &exec.Cmd{Path: exe, Args: []string{"ryshpgidprobe", "60"}}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Logf("cmd.Start with Setpgid FAILED here: %v (tests fall back to a single pid)", err)
		return
	}
	defer stopOwnChild(t, cmd, exe)
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	t.Logf("cmd.Start with Setpgid succeeded; Getpgid(pid)==pid: %v (err %v)", err == nil && pgid == cmd.Process.Pid, err)
}

func isKnownCLI(name string) bool {
	_, ok := proxy.ProfileFor(name)
	return ok
}

// An `exec -a claude` decoy: argv[0] names an allowed CLI, the real binary is
// something else. Not identified.
func TestIdentityArgv0DecoyIsNotIdentified(t *testing.T) {
	const name = "ryshdecoy"
	pid := startProgramAs(t, filepath.Join(t.TempDir(), name), "", "claude", name)

	if got := foregroundCLI(pid); isKnownCLI(got) {
		t.Fatalf("foregroundCLI = %q for a %s binary whose argv[0] is \"claude\": "+
			"argv[0] decided the identity", got, name)
	}
}

// A symlink NAMED like an allowed CLI that points at another binary. Not
// identified: the as-invoked path is the launcher's choice too.
func TestIdentityCLINamedSymlinkToAnotherBinaryIsNotIdentified(t *testing.T) {
	const name = "ryshother"
	root := t.TempDir()
	pid := startProgramAs(t, filepath.Join(root, name), filepath.Join(root, "claude"), "claude", name)

	if got := foregroundCLI(pid); isKnownCLI(got) {
		t.Fatalf("foregroundCLI = %q for a %s binary reached through a symlink "+
			"named claude: the as-invoked path decided the identity", got, name)
	}
}

// The genuine CLI at its install location. Identified.
func TestIdentityGenuineCLIIsIdentified(t *testing.T) {
	const version = "9.9.993"
	home, real := installedClaude(t, version)
	pid := startExe(t, real, []string{"claude", "60"}, version)
	t.Setenv("HOME", home)

	if got := foregroundCLI(pid); got != "claude" {
		t.Fatalf("foregroundCLI = %q for the stand-in claude at its install location, want \"claude\"", got)
	}
}

// The real CLI reached through another NAME, `exec -a ryshother` of the real
// binary. Identified, by its real executable.
func TestIdentityRealCLIUnderAnotherNameIsIdentified(t *testing.T) {
	const version = "9.9.995"
	home, real := installedClaude(t, version)
	pid := startExe(t, real, []string{"ryshother", "60"}, version)
	t.Setenv("HOME", home)

	if got := foregroundCLI(pid); got != "claude" {
		t.Fatalf("foregroundCLI = %q for the real claude exec'd as \"ryshother\", want \"claude\"", got)
	}
}

// The real CLI reached through a symlink with another name that resolves to
// the binary at its install location. Identified, by its real executable.
func TestIdentityRealCLIThroughAnOtherNamedSymlinkIsIdentified(t *testing.T) {
	const version = "9.9.994"
	home, real := installedClaude(t, version)
	link := filepath.Join(t.TempDir(), "ryshinnocent")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	pid := startExe(t, link, []string{"ryshinnocent", "60"}, version)
	t.Setenv("HOME", home)

	if got := foregroundCLI(pid); got != "claude" {
		t.Fatalf("foregroundCLI = %q for the real claude reached through \"ryshinnocent\", want \"claude\"", got)
	}
}

// A COPY of the real binary placed in an unlisted location. Not identified:
// a copy outside the known install locations is unknown, so it is not watched
// (F-13). This is the limit the strict-mode docs state.
func TestIdentityCopyOutsideInstallLocationsIsNotIdentified(t *testing.T) {
	const version = "9.9.992"
	home, real := installedClaude(t, version)
	elsewhere := filepath.Join(t.TempDir(), "claude", "versions")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	copied := filepath.Join(elsewhere, version)
	copyExecutable(t, real, copied)
	pid := startExe(t, copied, []string{"claude", "60"}, version)
	t.Setenv("HOME", home)

	if got := foregroundCLI(pid); isKnownCLI(got) {
		t.Fatalf("foregroundCLI = %q for a copy of claude outside its install locations, "+
			"want not identified", got)
	}
}

// Interpreter: the real script at its install location (aider in its uv tool
// environment, run by python). Identified.
func TestIdentityInterpreterRunningTheInstalledScriptIsIdentified(t *testing.T) {
	home, python := interpreterHome(t)
	script := filepath.Join(home, ".local", "share", "uv", "tools", "aider-chat", "bin", "aider")
	writeScript(t, script)
	pid := startExe(t, python, []string{python, script, "60"}, filepath.Base(python))
	t.Setenv("HOME", home)

	if got := foregroundCLI(pid); got != "aider" {
		t.Fatalf("foregroundCLI = %q for python running the installed aider script, want \"aider\"", got)
	}
}

// Interpreter: a look-alike script of the same name elsewhere. Not identified.
func TestIdentityInterpreterRunningALookAlikeScriptIsNotIdentified(t *testing.T) {
	home, python := interpreterHome(t)
	script := filepath.Join(home, "elsewhere", "aider")
	writeScript(t, script)
	pid := startExe(t, python, []string{python, script, "60"}, filepath.Base(python))
	t.Setenv("HOME", home)

	if got := foregroundCLI(pid); isKnownCLI(got) {
		t.Fatalf("foregroundCLI = %q for python running a look-alike aider script, want not identified", got)
	}
}

// interpreterHome builds a stand-in interpreter (named like python, sleeping
// for its LAST argument) under a fresh fake home.
func interpreterHome(t *testing.T) (home, python string) {
	t.Helper()
	home = t.TempDir()
	python = filepath.Join(home, "py", "bin", "python3.12")
	if err := os.MkdirAll(filepath.Dir(python), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	buildLastArgSleeper(t, python)
	return home, python
}

func writeScript(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("# stand-in script\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// copyExecutable copies src to dst byte for byte, as an executable.
func copyExecutable(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// buildLastArgSleeper compiles a program that sleeps for its LAST argument's
// seconds, so it can stand in for an interpreter whose argv[1] is a script.
func buildLastArgSleeper(t *testing.T, dst string) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module sleeper\n\ngo 1.21\n",
		"main.go": "package main\n\nimport (\n\t\"os\"\n\t\"strconv\"\n\t\"time\"\n)\n\n" +
			"func main() {\n\tn, _ := strconv.Atoi(os.Args[len(os.Args)-1])\n\ttime.Sleep(time.Duration(n) * time.Second)\n}\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	cmd := exec.Command("go", "build", "-o", dst, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build the stand-in interpreter: %v\n%s", err, out)
	}
}
