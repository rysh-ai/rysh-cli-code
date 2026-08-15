// SPDX-License-Identifier: Apache-2.0

// Package daemontest boots a real rysh daemon for tests.
//
// Almost every test in this tree exercises a handler by calling it directly,
// which is fast and right for logic but blind to a whole class of defect: the
// path from `rysh <verb>` through NATS into a live WorkspaceActor and back.
// Design 021's exit-status work found five handlers that reported success when
// they should have failed, and NOT ONE of them was catchable that way — they
// were paths where a named target does not exist, or where a handler delegates
// to a `handleCLI*`, prints its error and drops it. They were found by a human
// running ~50 commands against a daemon by hand, which is not a regression test.
//
// This package is that sweep, automated.
//
// # Isolation
//
// A test daemon must never touch the sessions a developer is actually using.
// Three things guarantee that, and all three matter:
//
//   - Every session gets its own temp directory holding its own
//     rysh.config.yaml. rysh state is project-local and derived from the config
//     file's location, so the session registry, the KV buckets and the
//     JetStream data all land inside that directory and vanish with it.
//   - The NATS port is 0, i.e. kernel-assigned. Two test daemons — or a test
//     daemon and a developer's — can never collide on a fixed port.
//   - Session names carry the pid and a counter, so a crashed run cannot leave
//     a record that a later run adopts.
//
// # Cost
//
// The package runs in about 40 seconds: ~8s to build the binary once, ~12s
// booting daemons, and ~14s of CLI round-trips at roughly 150ms each. That is
// in line with internal/actors and is the price of testing the real path, so it
// runs in the default `go test ./...` rather than hiding behind a build tag —
// a suite nobody runs catches nothing. `go test -short` skips it for quick
// local iteration.
//
// Booting a daemon costs a second or two, so the default is one shared session
// per test binary (Shared), reused by every test that only needs to ASK the
// daemon things. A test that mutates the workspace in a way others would notice
// takes its own with Fresh.
//
// # What this does NOT cover
//
// The daemon is a separate process, so `go test -race` instruments the test
// binary and not the daemon. A data race inside the WorkspaceActor is invisible
// here — that is what the in-process tests in internal/actors are for. These
// tests check behaviour across the wire, not memory safety behind it.
package daemontest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// bootTimeout bounds daemon spawn plus registry readiness.
const bootTimeout = 30 * time.Second

// execTimeout bounds a single CLI invocation against a running daemon.
const execTimeout = 30 * time.Second

// Session is a running rysh daemon and the CLI binary that drives it.
type Session struct {
	// Name is the session name, unique to this test process.
	Name string
	// Dir is the temp directory holding the session's config and all its state.
	Dir string
	// Config is the absolute path of the session's rysh.config.yaml.
	Config string
	// Bin is the rysh binary under test.
	Bin string
}

var (
	binOnce sync.Once
	binPath string
	binErr  error

	sharedOnce sync.Once
	shared     *Session
	sharedErr  error

	nameCounter atomic64
)

// Binary builds cmd/rysh once per test process and returns the path.
//
// It builds rather than reusing an installed rysh on purpose: the point is to
// test THIS tree, and a stale binary on PATH would quietly test something else.
func Binary(t *testing.T) string {
	t.Helper()
	binOnce.Do(func() {
		if _, err := exec.LookPath("go"); err != nil {
			binErr = fmt.Errorf("no go toolchain to build the daemon with: %w", err)
			return
		}
		// Built into the OS temp dir, not t.TempDir(): the binary outlives the
		// test that happened to trigger the build.
		dir, err := os.MkdirTemp("", "rysh-daemontest-bin-")
		if err != nil {
			binErr = err
			return
		}
		out := filepath.Join(dir, "rysh")
		if runtime.GOOS == "windows" {
			out += ".exe"
		}
		cmd := exec.Command("go", "build", "-o", out, "./cmd/rysh")
		cmd.Dir = moduleRoot()
		// GOWORK=off: rysh-cli is deliberately outside go.work (see CLAUDE.md),
		// so a workspace-aware build resolves the wrong rysh-shared.
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if combined, err := cmd.CombinedOutput(); err != nil {
			binErr = fmt.Errorf("build cmd/rysh: %w\n%s", err, combined)
			return
		}
		binPath = out
	})
	if binErr != nil {
		t.Skipf("daemontest: %v", binErr)
	}
	return binPath
}

// Main runs a package's tests and then stops the shared session, if one was
// booted. A package that uses Shared must call it:
//
//	func TestMain(m *testing.M) { os.Exit(daemontest.Main(m)) }
//
// Without it the shared daemon outlives the test binary. Nothing would break —
// its name and port are unique and its state is in a temp directory — but a
// stray daemon per test run is exactly the kind of litter that makes people
// stop running a suite.
func Main(m *testing.M) int {
	code := m.Run()
	if shared != nil {
		shared.stop()
	}
	if binPath != "" {
		os.RemoveAll(filepath.Dir(binPath))
	}
	return code
}

// Shared returns the session shared by every test in this binary, booting it on
// first use. Use it for anything that only reads.
func Shared(t *testing.T) *Session {
	t.Helper()
	requireSupported(t)
	bin := Binary(t)
	sharedOnce.Do(func() {
		shared, sharedErr = start(bin, "shared")
	})
	if sharedErr != nil {
		t.Fatalf("daemontest: boot shared session: %v", sharedErr)
	}
	return shared
}

// Fresh boots a session used only by this test and stops it when the test ends.
// Use it when the test mutates the workspace in a way another test would see.
func Fresh(t *testing.T) *Session {
	t.Helper()
	requireSupported(t)
	bin := Binary(t)
	s, err := start(bin, strings.ToLower(sanitize(t.Name())))
	if err != nil {
		t.Fatalf("daemontest: boot session: %v", err)
	}
	t.Cleanup(func() { s.stop() })
	return s
}

// requireSupported skips where a rysh daemon cannot run, or where the caller
// asked for a quick pass.
func requireSupported(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("daemontest: -short skips the live-daemon suite (~40s)")
	}
	if runtime.GOOS == "windows" {
		// Design 011: native Windows is CLI-only until ConPTY exists, so a
		// daemon there cannot create the pane these tests talk to.
		t.Skip("daemontest: rysh cannot open panes on native Windows (design 011)")
	}
}

// start writes an isolated config, spawns a detached daemon and waits for it to
// register.
func start(bin, label string) (*Session, error) {
	dir, err := os.MkdirTemp("", "rysh-daemontest-")
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("dt-%d-%s-%d", os.Getpid(), label, nameCounter.next())

	cfgPath := filepath.Join(dir, "rysh.config.yaml")
	// port 0 asks the kernel for a free port, which is what makes concurrent
	// test daemons (and a developer's own) safe from each other. data_dir is
	// left empty so JetStream storage lands under this directory's .rysh.
	cfg := fmt.Sprintf(`rysh:
  session_name: %q
nats:
  mode: "embedded"
  port: 0
provider:
  name: "claude"
  api_key: ""
`, name)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}

	s := &Session{Name: name, Dir: dir, Config: cfgPath, Bin: bin}

	cmd := exec.Command(bin, "--config", cfgPath, "create", name, "--detached")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("create session: %w\n%s", err, out)
	}

	if err := s.waitReady(); err != nil {
		s.stop()
		return nil, err
	}
	return s, nil
}

// waitReady blocks until the daemon answers a command, so a test never races
// the boot. `##tab list` is the cheapest question that proves the whole chain —
// CLI, NATS, WorkspaceActor — is up.
func (s *Session) waitReady() error {
	deadline := time.Now().Add(bootTimeout)
	var last string
	for time.Now().Before(deadline) {
		out, code, err := s.run("exec", "--session", s.Name, "--", "##tab list")
		if err == nil && code == 0 {
			return nil
		}
		last = out
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("session %q did not become ready within %s; last output:\n%s", s.Name, bootTimeout, last)
}

// stop tears the session down: stop the daemon, delete the record, remove the
// directory. Best-effort by design — a test failure must not be compounded by a
// cleanup failure, but a leaked daemon would poison later runs, so the process
// is killed if the graceful path does not take.
func (s *Session) stop() {
	_, _, _ = s.run("stop", s.Name)
	_, _, _ = s.run("delete-session", s.Name)
	os.RemoveAll(s.Dir)
}

// run invokes the rysh binary against this session's config.
func (s *Session) run(args ...string) (string, int, error) {
	full := append([]string{"--config", s.Config}, args...)
	cmd := exec.Command(s.Bin, full...)
	cmd.Dir = s.Dir

	done := make(chan struct{})
	var out []byte
	var err error
	go func() {
		out, err = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(execTimeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
		return string(out), -1, fmt.Errorf("timed out after %s: rysh %s", execTimeout, strings.Join(args, " "))
	}

	code := 0
	if err != nil {
		var ee *exec.ExitError
		if asExit(err, &ee) {
			code = ee.ExitCode()
		} else {
			return string(out), -1, err
		}
	}
	return string(out), code, nil
}

// Exec runs a "##" command and returns its output and exit code.
func (s *Session) Exec(t *testing.T, command string) (string, int) {
	t.Helper()
	out, code, err := s.run("exec", "--session", s.Name, "--", command)
	if err != nil {
		t.Fatalf("rysh exec %q: %v\n%s", command, err, out)
	}
	return out, code
}

// MustSucceed asserts a command exits 0, and returns its output.
func (s *Session) MustSucceed(t *testing.T, command string) string {
	t.Helper()
	out, code := s.Exec(t, command)
	if code != 0 {
		t.Errorf("%q exited %d, want 0 — a script would abort here under `set -e`.\n%s",
			command, code, indent(out))
	}
	return out
}

// MustFail asserts a command exits non-zero, and returns its output.
func (s *Session) MustFail(t *testing.T, command string) string {
	t.Helper()
	out, code := s.Exec(t, command)
	if code == 0 {
		t.Errorf("%q exited 0, want non-zero — a script would not notice this failed.\n%s",
			command, indent(out))
	}
	return out
}

// Script writes src to a .rysh file and runs it against this session, returning
// the combined output and exit code.
func (s *Session) Script(t *testing.T, src string, args ...string) (string, int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.rysh")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	// No --allow-attached: a test session is detached, so this exercises the
	// same guard a real script hits rather than routing around it.
	full := append([]string{"script", path, "--session", s.Name, "--"}, args...)
	out, code, err := s.run(full...)
	if err != nil {
		t.Fatalf("rysh script: %v\n%s", err, out)
	}
	return out, code
}

// CLI runs an arbitrary rysh subcommand against this session's config, for
// tests that drive a verb other than exec/script.
func (s *Session) CLI(t *testing.T, args ...string) (string, int) {
	t.Helper()
	out, code, err := s.run(args...)
	if err != nil {
		t.Fatalf("rysh %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out, code
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// moduleRoot walks up from this source file to the directory holding go.mod.
// Using the source path rather than the working directory means the fixture
// works no matter which package's tests are running.
func moduleRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	dir := filepath.Dir(file)
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "."
}

// sanitize reduces a test name to something usable in a session name.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 24 {
		out = out[:24]
	}
	if out == "" {
		out = "test"
	}
	return out
}

// indent shifts command output right so it reads as evidence under a failure.
func indent(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return "    (no output)"
	}
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}

func asExit(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

// atomic64 is a tiny counter; sync/atomic would do but this keeps the
// dependency surface of a test fixture at zero.
type atomic64 struct {
	mu sync.Mutex
	n  int64
}

func (a *atomic64) next() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n++
	return a.n
}
