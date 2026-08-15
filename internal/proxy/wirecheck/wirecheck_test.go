// SPDX-License-Identifier: Apache-2.0

package wirecheck

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/proxy"
)

// The fake CLI is this test binary re-exec'd under a symlink named after a real
// agent CLI. That keeps the test hermetic — no curl, no network, no vendor
// binary — while still exercising the genuine path: PATH lookup, argv from the
// profile table, injected env, and a real HTTP request through a real proxy.
const fakeModeEnv = "RYSH_WIRECHECK_FAKE"

func TestMain(m *testing.M) {
	switch os.Getenv(fakeModeEnv) {
	case "honor":
		// A well-behaved CLI: read the injected base URL and use it.
		base := os.Getenv("ANTHROPIC_BASE_URL")
		prompt := ""
		if len(os.Args) > 2 {
			prompt = os.Args[len(os.Args)-1]
		}
		body := `{"model":"m","messages":[{"role":"user","content":` +
			quote(prompt) + `}]}`
		resp, err := http.Post(base+"/v1/messages", "application/json",
			bytes.NewReader([]byte(body)))
		if err == nil {
			_ = resp.Body.Close()
		}
		os.Exit(0)
	case "ignore":
		// A CLI that ignores the base URL entirely — the case design 001 §2
		// listed as a non-goal and this check exists to expose.
		os.Exit(0)
	case "crash":
		// A CLI that falls over before it can call anything (a broken install).
		fmt.Fprintln(os.Stderr, "SyntaxError: Invalid regular expression flags")
		os.Exit(1)
	case "leak":
		// Proxied, but the secret rides the query string — where SNAT's JSON
		// body sanitizer never looks. Those bytes still left the machine.
		base := os.Getenv("ANTHROPIC_BASE_URL")
		prompt := os.Args[len(os.Args)-1]
		resp, err := http.Post(base+"/v1/messages?note="+url.QueryEscape(prompt),
			"application/json", bytes.NewReader([]byte(`{"model":"m"}`)))
		if err == nil {
			_ = resp.Body.Close()
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// fakeCLI puts a symlink named `binary` (pointing at this test binary) at the
// front of PATH, and selects the fake's behaviour.
func fakeCLI(t *testing.T, binary, mode string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	dir := t.TempDir()
	link := filepath.Join(dir, binary)
	if err := os.Symlink(self, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(fakeModeEnv, mode)
}

func TestRun_GovernedCLI(t *testing.T) {
	fakeCLI(t, "claude", "honor")

	res, err := Run(context.Background(), "claude", 30*time.Second, t.TempDir())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Governed {
		t.Fatalf("want GOVERNED, got %+v", res)
	}
	if res.Requests == 0 {
		t.Error("a governed verdict with zero requests is incoherent")
	}
	if res.Planted != 0 {
		t.Errorf("planted secret reached the wire %dx", res.Planted)
	}
	if res.Tokens == 0 {
		t.Error("a governed verdict requires the SNAT stand-in on the wire")
	}
	if !strings.Contains(res.String(), "GOVERNED") {
		t.Errorf("verdict line reads wrong: %s", res.String())
	}
}

// TestRun_UngovernedCLI is the case the whole feature exists for: the CLI runs
// happily and exits 0, and its traffic never came near rysh.
func TestRun_UngovernedCLI(t *testing.T) {
	fakeCLI(t, "claude", "ignore")

	res, err := Run(context.Background(), "claude", 30*time.Second, t.TempDir())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Governed {
		t.Fatal("a CLI that made no request must never be reported as governed")
	}
	if res.Requests != 0 {
		t.Errorf("requests = %d, want 0", res.Requests)
	}
	if !strings.Contains(res.Reason, "ANTHROPIC_BASE_URL") {
		t.Errorf("the reason must name the variable that was ignored: %s", res.Reason)
	}
	if !strings.Contains(res.String(), "NOT GOVERNED") {
		t.Errorf("verdict line reads wrong: %s", res.String())
	}
}

// TestRun_ProxiedButLeaking: reaching the proxy is not the same as being
// governed. A secret that rides a path SNAT does not sanitize must not be
// scored as a pass.
func TestRun_ProxiedButLeaking(t *testing.T) {
	fakeCLI(t, "claude", "leak")

	res, err := Run(context.Background(), "claude", 30*time.Second, t.TempDir())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Governed {
		t.Fatalf("a request that carried the raw secret must not pass: %+v", res)
	}
	if res.Requests == 0 {
		t.Fatal("the fake did reach the proxy; the probe should have counted it")
	}
	// Non-vacuous: the verdict must fail BECAUSE the raw secret was seen, not
	// merely because no SNAT token turned up.
	if res.Planted == 0 {
		t.Fatal("the probe did not see the secret on the wire — query strings " +
			"are being missed, so a CLI could smuggle context past this check")
	}
	if !strings.Contains(res.Reason, "SECURITY FAILURE") {
		t.Errorf("reason should name the leak: %s", res.Reason)
	}
}

// TestRun_LeavesTheLiveEndpointAlone is the probe-isolation regression.
//
// `##proxy check` runs INSIDE the session daemon, next to the real proxy. The
// probe's own proxy used to publish itself through proxy.Start and clear the
// globals on its deferred Stop, so after one check proxy.Endpoint() was "" and
// proxy.Current() nil for the rest of the daemon's life: every pane spawned
// afterwards got no base-URL injection and ran ungoverned, while ##proxy status
// still reported ON.
func TestRun_LeavesTheLiveEndpointAlone(t *testing.T) {
	fakeCLI(t, "claude", "honor")

	live := proxy.New(nil, nil, nil, nil, false)
	if _, err := live.Start(""); err != nil {
		t.Fatalf("start live proxy: %v", err)
	}
	defer live.Stop()
	wantEndpoint := live.BaseURL()

	res, err := Run(context.Background(), "claude", 30*time.Second, t.TempDir())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Non-vacuous: the probe really did run its own proxy.
	if !res.Governed {
		t.Fatalf("probe did not run to a governed verdict: %+v", res)
	}

	if got := proxy.Endpoint(); got != wantEndpoint {
		t.Fatalf("Endpoint() = %q after the probe, want the live proxy's %q — "+
			"panes spawned from here would get no injection", got, wantEndpoint)
	}
	if got := proxy.Current(); got != live {
		t.Fatalf("Current() = %v after the probe, want the live server — "+
			"pane-side governance lookups would see no proxy", got)
	}
}

func TestRun_UnknownCLI(t *testing.T) {
	_, err := Run(context.Background(), "not-an-agent-cli", time.Second, t.TempDir())
	if err == nil {
		t.Fatal("an unknown CLI must be refused, not probed")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("the error should list what rysh does know: %v", err)
	}
}

func TestRun_BinaryNotOnPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Run(context.Background(), "claude", time.Second, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("want a PATH error, got %v", err)
	}
}

// TestMakeScratch_HonoursBase pins the fix for the codex /tmp warning: the
// probe's working directory must land under the project-local base it is given,
// not under the system temp dir.
func TestMakeScratch_HonoursBase(t *testing.T) {
	base := filepath.Join(t.TempDir(), "nested", "proxy-check")
	dir, err := makeScratch(base)
	if err != nil {
		t.Fatalf("makeScratch: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	if filepath.Dir(dir) != base {
		t.Fatalf("scratch = %q, want a child of %q", dir, base)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("scratch dir not created: %v", err)
	}
	// The base is created on demand, several levels deep if need be.
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("base not created: %v", err)
	}
}

// TestMakeScratch_EmptyBaseFallsBackToTemp keeps a caller with no project
// directory working — tests, and a session with no RyshDir.
func TestMakeScratch_EmptyBaseFallsBackToTemp(t *testing.T) {
	dir, err := makeScratch("")
	if err != nil {
		t.Fatalf("makeScratch: %v", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// filepath.Clean, because macOS reports TMPDIR with a trailing slash and a
	// raw string compare against filepath.Dir fails there for no real reason.
	if filepath.Dir(dir) != filepath.Clean(os.TempDir()) {
		t.Fatalf("scratch = %q, want a child of the system temp dir %q", dir, os.TempDir())
	}
}

// TestRun_CrashedCLIIsInconclusiveNotUngoverned is the lesson from verifying
// gemini-cli: it crashed on Node 18 before making any request. Reporting that
// as NOT GOVERNED would have recorded a broken install as a governance failure
// and put a false ❌ in the compatibility matrix.
func TestRun_CrashedCLIIsInconclusiveNotUngoverned(t *testing.T) {
	fakeCLI(t, "claude", "crash")

	res, err := Run(context.Background(), "claude", 30*time.Second, t.TempDir())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Governed {
		t.Fatal("a CLI that made no request must never be reported as governed")
	}
	if !res.Inconclusive {
		t.Fatal("a CLI that died before making a request proves nothing either " +
			"way — it must not be scored as NOT GOVERNED")
	}
	if !strings.Contains(res.String(), "INCONCLUSIVE") {
		t.Errorf("verdict line should say INCONCLUSIVE: %s", res.String())
	}
	if !strings.Contains(res.Reason, "proves nothing") {
		t.Errorf("reason should be explicit that this is not evidence: %s", res.Reason)
	}
}
