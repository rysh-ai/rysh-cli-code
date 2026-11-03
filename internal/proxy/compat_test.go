// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProfileFor_TolerantLookup(t *testing.T) {
	// A process table reports whatever was exec'd: a path, a case variant, or
	// an .exe. All of them must resolve to the same profile.
	for _, in := range []string{"claude", "CLAUDE", "/usr/local/bin/claude", "claude.exe", " claude "} {
		p, ok := ProfileFor(in)
		if !ok || p.Binary != "claude" {
			t.Errorf("ProfileFor(%q) = %+v, %v; want the claude profile", in, p, ok)
		}
	}
	if _, ok := ProfileFor("definitely-not-an-agent"); ok {
		t.Error("an unknown binary must not resolve to a profile")
	}
}

// TestCodexProfile_PinsProviderNotJustBaseURL is the anti-drift assertion for
// the knowledge that used to live only in cmd/wire-harness. Exporting
// OPENAI_BASE_URL does NOT govern codex — it can fall back to ChatGPT auth and
// egress directly — so its probe must pin a model_provider at the proxy.
func TestCodexProfile_PinsProviderNotJustBaseURL(t *testing.T) {
	p, ok := ProfileFor("codex")
	if !ok {
		t.Fatal("codex profile missing")
	}
	if p.EnvIsEnough {
		t.Error("codex must be marked as NOT governed by base-URL env alone")
	}
	if p.Note == "" {
		t.Error("a CLI that env injection cannot govern must explain why")
	}

	scratch := t.TempDir()
	argv, env, err := p.ProbeCommand("codex", "http://127.0.0.1:9/openai/pane", "hi", scratch, "dummy")
	if err != nil {
		t.Fatalf("ProbeCommand: %v", err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "model_provider=rysh") {
		t.Errorf("probe argv must pin a provider, got: %s", joined)
	}
	if !strings.Contains(joined, "model_providers.rysh.base_url=http://127.0.0.1:9/openai/pane/v1") {
		t.Errorf("probe argv must point the pinned provider at the proxy, got: %s", joined)
	}
	if !hasEnvPrefix(env, "CODEX_HOME=") {
		t.Errorf("probe env must redirect CODEX_HOME away from the user's own, got: %v", env)
	}
	if !hasEnvPrefix(env, "OPENAI_BASE_URL=") || !hasEnvPrefix(env, "OPENAI_API_KEY=") {
		t.Errorf("probe env missing base URL or dummy key: %v", env)
	}
}

// TestCodexAdapter_WritesNoAuth is the security property of the adapter: the
// rysh-owned CODEX_HOME must carry a provider pin and NO auth.json. A ChatGPT
// session is exactly what lets codex egress around the proxy.
func TestCodexAdapter_WritesNoAuth(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "codex")
	env, err := codexAdaptEnv("http://127.0.0.1:5555/openai/p1", dir)
	if err != nil {
		t.Fatalf("codexAdaptEnv: %v", err)
	}
	if len(env) != 1 || !strings.HasPrefix(env[0], "CODEX_HOME="+dir) {
		t.Fatalf("env = %v, want CODEX_HOME pointing at the rysh dir", env)
	}

	body, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatalf("config.toml not written: %v", err)
	}
	if !strings.Contains(string(body), `model_provider = "rysh"`) {
		t.Errorf("config.toml does not pin the provider:\n%s", body)
	}
	if !strings.Contains(string(body), "http://127.0.0.1:5555/openai/p1/v1") {
		t.Errorf("config.toml does not point at the proxy:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(dir, "auth.json")); !os.IsNotExist(err) {
		t.Error("the adapter home must contain NO auth.json — a ChatGPT session " +
			"is what lets codex egress around the proxy")
	}
}

func hasEnvPrefix(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// ---- governance observation ----

func TestGovWatch_WarnsOnceAfterGrace(t *testing.T) {
	g := newGovWatch()
	t0 := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	g.noteForeground("p1", "aider", t0)

	if _, ok := g.dueWarning("p1", t0.Add(GraceWindow-time.Second)); ok {
		t.Fatal("must not accuse a CLI that is still inside the grace window")
	}

	bin, ok := g.dueWarning("p1", t0.Add(GraceWindow+time.Second))
	if !ok || bin != "aider" {
		t.Fatalf("dueWarning = %q, %v; want aider, true", bin, ok)
	}
	if _, ok := g.dueWarning("p1", t0.Add(time.Hour)); ok {
		t.Fatal("the warning must latch — one per run, not one per poll")
	}
}

// TestGovWatch_GovernedTrafficClearsSuspicion: positive evidence wins. Once a
// pane's request reaches the proxy there is nothing to warn about.
func TestGovWatch_GovernedTrafficClearsSuspicion(t *testing.T) {
	g := newGovWatch()
	t0 := time.Now()
	g.noteForeground("p1", "claude", t0)
	g.noteGoverned("p1")

	if _, ok := g.dueWarning("p1", t0.Add(time.Hour)); ok {
		t.Fatal("a pane that produced governed traffic must never be warned about")
	}
	if got := g.state("p1", t0); got != "governed" {
		t.Errorf("state = %q, want governed", got)
	}
}

// TestGovWatch_NewProgramIsJudgedAfresh: each run of a CLI gets its own
// verdict, so a governed claude run does not vouch for an aider run after it.
func TestGovWatch_NewProgramIsJudgedAfresh(t *testing.T) {
	g := newGovWatch()
	t0 := time.Now()

	g.noteForeground("p1", "claude", t0)
	g.noteGoverned("p1")
	if _, ok := g.dueWarning("p1", t0.Add(time.Minute)); ok {
		t.Fatal("the governed run must not warn")
	}

	// A different program starts later, and produces nothing.
	t1 := t0.Add(time.Minute)
	g.noteForeground("p1", "aider", t1)
	bin, ok := g.dueWarning("p1", t1.Add(GraceWindow+time.Second))
	if !ok || bin != "aider" {
		t.Fatalf("dueWarning = %q, %v; want aider, true — the earlier governed "+
			"run must not vouch for a later program", bin, ok)
	}
}

func TestGovWatch_ShellForegroundClearsTheWatch(t *testing.T) {
	g := newGovWatch()
	t0 := time.Now()
	g.noteForeground("p1", "aider", t0)
	g.noteForeground("p1", "", t0.Add(time.Second)) // back to the shell

	if _, ok := g.dueWarning("p1", t0.Add(time.Hour)); ok {
		t.Fatal("a pane sitting at its shell must not be warned about")
	}
	if got := g.state("p1", t0.Add(time.Hour)); got != "idle" {
		t.Errorf("state = %q, want idle", got)
	}
}

func TestGovWatch_StateProgression(t *testing.T) {
	g := newGovWatch()
	t0 := time.Now()
	if got := g.state("p1", t0); got != "idle" {
		t.Errorf("unknown pane state = %q, want idle", got)
	}
	g.noteForeground("p1", "codex", t0)
	if got := g.state("p1", t0.Add(time.Second)); got != "starting" {
		t.Errorf("state = %q, want starting", got)
	}
	if got := g.state("p1", t0.Add(GraceWindow+time.Second)); got != "unproxied?" {
		t.Errorf("state = %q, want unproxied?", got)
	}
}

// TestUnproxiedWarning_IsActionable: the line must name the env var that was
// probably ignored and the command that answers the question for certain.
func TestUnproxiedWarning_IsActionable(t *testing.T) {
	w := UnproxiedWarning("codex")
	for _, want := range []string{"codex", "OPENAI_BASE_URL", "##proxy check codex", "ChatGPT"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning missing %q:\n%s", want, w)
		}
	}
	// An unknown binary must still produce a usable line rather than panicking.
	if got := UnproxiedWarning("mystery-cli"); !strings.Contains(got, "mystery-cli") {
		t.Errorf("unknown binary warning is unusable: %s", got)
	}
}

// TestNoteGoverned_FlowsFromServeHTTP wires the observation to real traffic:
// one proxied request must mark the pane governed.
func TestNoteGoverned_FlowsFromServeHTTP(t *testing.T) {
	_, upstream := newRecordingUpstream(func(w http.ResponseWriter, _ *http.Request, _ int) {
		_, _ = w.Write([]byte(`{}`))
	})
	defer upstream.Close()

	s := New(nil, nil, nil, map[string]string{"anthropic": upstream.URL}, false)
	s.NoteForeground("p1", "claude")
	if got := s.PaneGovernanceState("p1"); got != "starting" {
		t.Fatalf("state before traffic = %q, want starting", got)
	}

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/anthropic/p1/v1/messages",
		strings.NewReader(`{}`)))

	if got := s.PaneGovernanceState("p1"); got != "governed" {
		t.Fatalf("state after a proxied request = %q, want governed", got)
	}
	if _, ok := s.DueWarning("p1"); ok {
		t.Error("a governed pane must never come due for a warning")
	}
}

// identityHome lays out a fake home with the files IdentifyCLI resolves, and
// points $HOME at it so the profile table's "~/" install locations land there.
// Each path is created as an empty file; a value starting with "->" makes a
// symlink to that home-relative target instead.
func identityHome(t *testing.T, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for rel, target := range files {
		path := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if to, ok := strings.CutPrefix(target, "->"); ok {
			if err := os.Symlink(filepath.Join(home, to), path); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(path, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

// TestIdentifyCLI_ByRealExecutable is the decision function under WO-7: the
// real executable decides, at the CLI's known install locations, whatever the
// process calls itself. It covers F-15 too: the native-installer claude, whose
// kernel name is its version, is identified by the version file it runs.
func TestIdentifyCLI_ByRealExecutable(t *testing.T) {
	home := identityHome(t, map[string]string{
		".local/share/claude/versions/2.1.288":       "",
		".local/bin/claude":                          "->.local/share/claude/versions/2.1.288",
		"py/bin/python3.12":                          "",
		".local/share/uv/tools/aider-chat/bin/aider": "",
	})
	versioned := filepath.Join(home, ".local/share/claude/versions/2.1.288")
	python := filepath.Join(home, "py/bin/python3.12")
	cases := []struct {
		name, exe string
		argv      []string
		want      string
	}{
		{"genuine native claude", versioned, []string{"claude", "--print", "hi"}, "claude"},
		{"argv unreadable", versioned, nil, "claude"},
		{"real claude under another name (exec -a)", versioned, []string{"ryshother"}, "claude"},
		{"real claude through its install symlink", filepath.Join(home, ".local/bin/claude"), nil, "claude"},
		{"interpreter running the real script", python, []string{python, filepath.Join(home, ".local/share/uv/tools/aider-chat/bin/aider")}, "aider"},
	}
	for _, c := range cases {
		got, ok := IdentifyCLI(c.exe, c.argv)
		if !ok || got != c.want {
			t.Errorf("%s: identified as %q, %v; want %q, true", c.name, got, ok, c.want)
		}
	}
}

// TestIdentifyCLI_NotIdentified is the other half: nothing a launcher chooses
// (argv[0], a symlink's name, a script's name) makes a process an allowed CLI,
// and an unreadable executable is unknown (F-13), so it is not watched.
func TestIdentifyCLI_NotIdentified(t *testing.T) {
	home := identityHome(t, map[string]string{
		"bin/ryshdecoy":                        "",
		"bin/ryshother":                        "",
		"bin/claude":                           "->bin/ryshother",
		"elsewhere/claude/versions/2.1.288":    "",
		"elsewhere/aider":                      "",
		"opt/tool/versions/1.2.3":              "",
		"py/bin/python3.12":                    "",
		"usr/bin/vim":                          "",
		".local/share/claude/versions/2.1.288": "",
	})
	at := func(rel string) string { return filepath.Join(home, rel) }
	python := at("py/bin/python3.12")
	cases := []struct {
		name, exe string
		argv      []string
	}{
		{"exec -a decoy", at("bin/ryshdecoy"), []string{"claude", "--print"}},
		{"symlink named claude to another binary", at("bin/claude"), []string{"claude"}},
		{"copy outside the install locations", at("elsewhere/claude/versions/2.1.288"), []string{"claude"}},
		{"a versions/ directory that is not a known CLI's", at("opt/tool/versions/1.2.3"), nil},
		{"interpreter running a look-alike script", python, []string{python, at("elsewhere/aider")}},
		{"interpreter running a relative script", python, []string{python, "aider"}},
		{"interpreter whose script is missing", python, []string{python, at(".local/share/uv/tools/aider-chat/bin/aider")}},
		{"not an interpreter: argv[1] is just an argument", at("usr/bin/vim"), []string{"vim", at(".local/share/claude/versions/2.1.288")}},
		{"interpreter with only argv[0]", python, []string{"claude"}},
		{"unreadable executable", "", []string{"claude"}},
		{"relative executable", "claude", []string{"claude"}},
		{"executable that does not exist", at("bin/missing"), []string{"claude"}},
	}
	for _, c := range cases {
		if got, ok := IdentifyCLI(c.exe, c.argv); ok {
			t.Errorf("%s: identified as %q; want not identified", c.name, got)
		}
	}
}

// TestGovWatch_IgnoresAProgramThatIsNotAKnownCLI is F-13 at the watch: design
// 022 §4.4 scopes the detector to a known agent CLI, so a long-running build or
// `sleep` must never become "unproxied?", never be warned about, and so never
// be stopped by strict.
func TestGovWatch_IgnoresAProgramThatIsNotAKnownCLI(t *testing.T) {
	g := newGovWatch()
	t0 := time.Now()
	g.noteForeground("p1", "sleep", t0)
	if bin, ok := g.dueWarning("p1", t0.Add(time.Hour)); ok {
		t.Fatalf("dueWarning = %q, true for a program with no CLI profile — "+
			"under strict that is a SIGTERM to a build or an editor", bin)
	}
	if got := g.state("p1", t0.Add(time.Hour)); got != "idle" {
		t.Errorf("state = %q, want idle", got)
	}

	// An unknown program replacing a watched CLI ends that CLI's watch.
	g.noteForeground("p2", "claude", t0)
	g.noteForeground("p2", "make", t0.Add(time.Second))
	if bin, ok := g.dueWarning("p2", t0.Add(time.Hour)); ok {
		t.Fatalf("dueWarning = %q, true after the CLI gave way to make", bin)
	}
}

// TestGovWatch_NamesTheCLIByItsProfile: a CLI reported by path is warned about
// by the name `##proxy check` takes.
func TestGovWatch_NamesTheCLIByItsProfile(t *testing.T) {
	g := newGovWatch()
	t0 := time.Now()
	g.noteForeground("p1", "/opt/homebrew/bin/codex", t0)
	bin, ok := g.dueWarning("p1", t0.Add(GraceWindow+time.Second))
	if !ok || bin != "codex" {
		t.Fatalf("dueWarning = %q, %v; want codex, true", bin, ok)
	}
}
