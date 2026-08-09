package actors

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The OSS on-ramp. Before `##upstream connect`, the path from `brew install
// rysh` to a live shared session was: find the dashboard, sign up, find the
// workspace page, copy a snippet, hand-edit rysh.config.yaml, restart — and
// the `workspace` key had to be the workspace UUID, not its name, a trap the
// struct comment at config.go warns about but the user could not see.
//
// These tests pin the whole command against a stand-in /api/server-info: the
// happy path derives the UUID and persists it, every failure mode is one human
// line, a failure never touches the config on disk, and connecting never turns
// on `governance` (a separate data-egress opt-in).

// wsInfoServer stands in for the upstream server's /api/server-info endpoint.
// It records the API key it was presented with, so a test can prove the key
// actually travels.
type wsInfoServer struct {
	*httptest.Server
	gotAuth   string
	gotAPIKey string
}

func newWSInfoServer(t *testing.T, status int, body string) *wsInfoServer {
	t.Helper()
	s := &wsInfoServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/server-info" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.gotAuth = r.Header.Get("Authorization")
		s.gotAPIKey = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

// seedConfig writes a config file carrying a comment and unrelated settings.
// Both must survive a connect: a hand-rolled YAML rewrite that drops the user's
// other settings or their comments is a worse bug than the one being fixed.
const seedConfigYAML = `# my rysh config — keep this comment
session:
  name: "sunflower"
rysh:
  initial_panes: 3
`

func seedConfigFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rysh.config.yaml")
	if err := os.WriteFile(path, []byte(seedConfigYAML), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// connectActor builds a bare WorkspaceActor pointed at a config file, the same
// way the ##workspace cwd persistence path reads w.cfg.ConfigFile.
func connectActor(path string) *WorkspaceActor {
	w := &WorkspaceActor{}
	w.cfg.ConfigFile = path
	return w
}

const testWorkspaceUUID = "3f2b1c4d-5e6f-4a8b-9c0d-1e2f3a4b5c6d"

// TestUpstreamConnect_HappyPathPersistsUUID is the core of the on-ramp: two
// arguments in, and the workspace UUID — which the user never types and cannot
// be expected to know — is derived from the API key and written to the config.
func TestUpstreamConnect_HappyPathPersistsUUID(t *testing.T) {
	srv := newWSInfoServer(t, http.StatusOK,
		`{"workspace":"acme","workspace_id":"`+testWorkspaceUUID+`"}`)
	path := seedConfigFile(t)
	w := connectActor(path)

	out := upstreamCmd(t, w, "connect", srv.URL, "rysh_sk_test_key")

	if err := w.takeRyshFailure(); err != nil {
		t.Fatalf("connect recorded a failure on the happy path: %v", err)
	}
	// The key must actually reach the server — otherwise the id resolves for
	// the server's default workspace, not the user's.
	if srv.gotAuth != "Bearer rysh_sk_test_key" && srv.gotAPIKey != "rysh_sk_test_key" {
		t.Errorf("api key not presented to the server: auth=%q x-api-key=%q", srv.gotAuth, srv.gotAPIKey)
	}

	// The report names both halves: the name is what the user recognises, the
	// id is what actually namespaces the wire.
	for _, want := range []string{"acme", testWorkspaceUUID, srv.URL} {
		if !strings.Contains(out, want) {
			t.Errorf("connect output missing %q:\n%s", want, out)
		}
	}

	got := readFile(t, path)
	for _, want := range []string{
		"# my rysh config — keep this comment", // comments survive
		`name: "sunflower"`,                    // unrelated settings survive
		"initial_panes: 3",
		"enabled: true",
		"url: " + srv.URL,
		"api_key: rysh_sk_test_key",
		"workspace: " + testWorkspaceUUID,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("config file missing %q after connect:\n%s", want, got)
		}
	}
	// The name is NOT the wire namespace. Writing it there is the exact trap
	// the UpstreamConfig.Workspace comment warns about.
	if strings.Contains(got, "workspace: acme") {
		t.Errorf("connect wrote the workspace NAME as the namespace; it must be the UUID:\n%s", got)
	}

	// And the running session reflects it immediately, so ##upstream status
	// does not lie until the next restart.
	if w.cfg.Upstream.Workspace != testWorkspaceUUID {
		t.Errorf("in-memory upstream workspace = %q, want the resolved UUID", w.cfg.Upstream.Workspace)
	}
	if !w.cfg.Upstream.Enabled {
		t.Error("in-memory upstream still disabled after connect")
	}
}

// TestUpstreamConnect_GovernanceStaysOff pins the data-egress boundary.
// `governance` opts the daemon into reporting local spend to a server; it is
// deliberately separate from `enabled`, and connecting must not smuggle it on.
func TestUpstreamConnect_GovernanceStaysOff(t *testing.T) {
	srv := newWSInfoServer(t, http.StatusOK,
		`{"workspace":"acme","workspace_id":"`+testWorkspaceUUID+`"}`)
	path := seedConfigFile(t)
	w := connectActor(path)

	out := upstreamCmd(t, w, "connect", srv.URL, "rysh_sk_test_key")

	if w.cfg.Upstream.Governance {
		t.Error("connect enabled upstream.governance in the running session")
	}
	if got := readFile(t, path); strings.Contains(got, "governance: true") {
		t.Errorf("connect wrote governance: true:\n%s", got)
	}
	// Silence about it is not good enough: the user must be told what was NOT
	// turned on, or "connected" reads as "everything is on".
	if !strings.Contains(out, "governance") {
		t.Errorf("connect output must say governance was left off:\n%s", out)
	}
}

// TestUpstreamConnect_BadKey pins that a 401 is one human line, not a Go error
// string with an HTTP body glued to it — and that nothing is written.
func TestUpstreamConnect_BadKey(t *testing.T) {
	srv := newWSInfoServer(t, http.StatusUnauthorized, `{"error":"invalid api key"}`)
	path := seedConfigFile(t)
	w := connectActor(path)

	out := upstreamCmd(t, w, "connect", srv.URL, "wrong-key")

	if w.takeRyshFailure() == nil {
		t.Error("a rejected API key must record a command failure")
	}
	low := strings.ToLower(out)
	if !strings.Contains(low, "api key") {
		t.Errorf("401 must be reported as a rejected API key, got:\n%s", out)
	}
	if strings.Contains(out, "panic") || strings.Contains(out, "goroutine") {
		t.Errorf("connect leaked a stack trace:\n%s", out)
	}
	if got := readFile(t, path); got != seedConfigYAML {
		t.Errorf("a failed connect rewrote the config:\n%s", got)
	}
}

// TestUpstreamConnect_Unreachable pins that a dead URL leaves the config
// byte-identical. A half-written [upstream] block would be worse than no
// command at all: the session would start "enabled" against nothing.
func TestUpstreamConnect_Unreachable(t *testing.T) {
	srv := newWSInfoServer(t, http.StatusOK, `{}`)
	dead := srv.URL
	srv.Close() // nothing is listening on that port any more

	path := seedConfigFile(t)
	w := connectActor(path)

	out := upstreamCmd(t, w, "connect", dead, "rysh_sk_test_key")

	if w.takeRyshFailure() == nil {
		t.Error("an unreachable server must record a command failure")
	}
	low := strings.ToLower(out)
	if !strings.Contains(low, "could not reach") && !strings.Contains(low, "unreachable") {
		t.Errorf("an unreachable server must say so plainly, got:\n%s", out)
	}
	if got := readFile(t, path); got != seedConfigYAML {
		t.Errorf("an unreachable server corrupted the config:\n%s", got)
	}
}

// TestUpstreamConnect_NoWorkspaceID pins the third failure mode: a 200 from a
// server that predates per-user workspace ids. Guessing a namespace here would
// produce a session that connects and silently sees no shares.
func TestUpstreamConnect_NoWorkspaceID(t *testing.T) {
	srv := newWSInfoServer(t, http.StatusOK, `{"workspace":"default","workspace_id":""}`)
	path := seedConfigFile(t)
	w := connectActor(path)

	out := upstreamCmd(t, w, "connect", srv.URL, "rysh_sk_test_key")

	if w.takeRyshFailure() == nil {
		t.Error("a missing workspace_id must record a command failure")
	}
	if !strings.Contains(out, "workspace_id") {
		t.Errorf("the missing-workspace_id message must name it:\n%s", out)
	}
	if got := readFile(t, path); got != seedConfigYAML {
		t.Errorf("a workspace_id-less response wrote to the config:\n%s", got)
	}
}

// TestUpstreamConnect_Usage pins that the command states its own shape rather
// than failing obscurely on a missing argument.
func TestUpstreamConnect_Usage(t *testing.T) {
	for _, args := range [][]string{
		{"connect"},
		{"connect", "https://rysh.example"},
	} {
		w := connectActor(seedConfigFile(t))
		out := upstreamCmd(t, w, args...)
		if !strings.Contains(out, "##upstream connect <url> <api-key>") {
			t.Errorf("args %v: expected the usage line, got:\n%s", args, out)
		}
		if w.takeRyshFailure() == nil {
			t.Errorf("args %v: a missing argument must record a usage failure", args)
		}
	}
}

// TestUpstreamConnect_Discoverable pins that the on-ramp is findable. A command
// nobody can discover converts nobody, so it must appear both in ##upstream's
// own subcommand list and in the ##help registry.
func TestUpstreamConnect_Discoverable(t *testing.T) {
	out := upstreamCmd(t, &WorkspaceActor{}, "wibble")
	if !strings.Contains(out, "##upstream connect") {
		t.Errorf("##upstream's subcommand list does not mention connect:\n%s", out)
	}

	var help strings.Builder
	for _, c := range ryshCommands {
		if c.name != "upstream" {
			continue
		}
		for _, line := range c.help {
			help.WriteString(line)
		}
	}
	if !strings.Contains(help.String(), "##upstream connect") {
		t.Errorf("the ##help registry does not list ##upstream connect:\n%s", help.String())
	}
}
