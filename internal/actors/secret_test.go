package actors

import (
	"os"
	"path/filepath"
	"testing"
)

const testScope = "tab-aaaa" // a stand-in tab-ID scope

// TestSecretStoreResolutionOrder verifies the config-over-env precedence and the
// environment fallback within a tab scope. The session (KV) and persist tiers are
// exercised elsewhere; here kv is nil and the CWD has no .rysh/secrets, so config
// is the top resolving tier.
func TestSecretStoreResolutionOrder(t *testing.T) {
	t.Chdir(t.TempDir()) // isolate from any project-local .rysh/secrets

	const both = "RYSH_TEST_SECRET_BOTH"
	const envOnly = "RYSH_TEST_SECRET_ENVONLY"

	os.Setenv(both, "from-env")
	os.Setenv(envOnly, "env-value")
	defer os.Unsetenv(both)
	defer os.Unsetenv(envOnly)

	s := newSecretStore(nil, map[string]map[string]string{
		testScope: {both: "from-config"},
	})

	// Config beats environment within the tab.
	if v, src, ok := s.Get(testScope, both); !ok || v != "from-config" || src != secretSourceConfig {
		t.Fatalf("config tier: got (%q,%q,%v), want (from-config,config,true)", v, src, ok)
	}
	// Falls back to the global environment when not defined in the tab.
	if v, src, ok := s.Get(testScope, envOnly); !ok || v != "env-value" || src != secretSourceEnv {
		t.Fatalf("env tier: got (%q,%q,%v), want (env-value,env,true)", v, src, ok)
	}
	// Missing everywhere.
	if _, _, ok := s.Get(testScope, "RYSH_TEST_SECRET_MISSING"); ok {
		t.Fatalf("expected missing secret to be not found")
	}
}

// TestSecretStoreTabIsolation verifies a secret defined in one tab is not visible
// from another tab (config tier), while the global env fallback is shared.
func TestSecretStoreTabIsolation(t *testing.T) {
	t.Chdir(t.TempDir())
	const a, b = "tab-a", "tab-b"
	s := newSecretStore(nil, map[string]map[string]string{
		a: {"DB_URL": "postgres://a"},
	})

	if v, src, ok := s.Get(a, "DB_URL"); !ok || v != "postgres://a" || src != secretSourceConfig {
		t.Fatalf("tab a: got (%q,%q,%v), want (postgres://a,config,true)", v, src, ok)
	}
	// Tab b must NOT see tab a's config secret.
	if _, _, ok := s.Get(b, "DB_URL"); ok {
		t.Fatalf("tab b should not see tab a's secret")
	}
	// List is per-tab.
	if len(s.List(b)) != 0 {
		t.Fatalf("tab b should list no secrets")
	}
	if got := s.List(a); len(got) != 1 || got[0].Name != "DB_URL" {
		t.Fatalf("tab a list: got %+v", got)
	}
}

// TestSecretStoreExpand verifies tab-scoped ${NAME} expansion, leaving
// unresolved references untouched.
func TestSecretStoreExpand(t *testing.T) {
	t.Chdir(t.TempDir())
	s := newSecretStore(nil, map[string]map[string]string{
		testScope: {"EMAIL_PASSWORD": "hunter2"},
	})
	got := s.Expand(testScope, "pass=${EMAIL_PASSWORD} keep=${NOT_A_SECRET}")
	want := "pass=hunter2 keep=${NOT_A_SECRET}"
	if got != want {
		t.Fatalf("Expand: got %q, want %q", got, want)
	}
	// A different tab does not resolve the secret.
	if got := s.Expand("other-tab", "pass=${EMAIL_PASSWORD}"); got != "pass=${EMAIL_PASSWORD}" {
		t.Fatalf("cross-tab Expand leaked: got %q", got)
	}
	// nil store is a no-op.
	var nilStore *namedStore
	if out := nilStore.Expand(testScope, "x=${EMAIL_PASSWORD}"); out != "x=${EMAIL_PASSWORD}" {
		t.Fatalf("nil Expand: got %q", out)
	}
}

// TestSecretStoreGetLayered verifies the layered tab → workspace → environment
// resolution: a tab secret overrides the workspace default, the workspace value
// is used when the tab has none, and the environment is the final global
// fallback. The resolved scope token is reported so callers can label the source.
func TestSecretStoreGetLayered(t *testing.T) {
	t.Chdir(t.TempDir())

	const ws, tab = "ws-main", "tab-build"
	const envOnly = "RYSH_TEST_LAYERED_ENVONLY"
	os.Setenv(envOnly, "from-env")
	defer os.Unsetenv(envOnly)

	s := newSecretStore(nil, map[string]map[string]string{
		ws:  {"SHARED": "ws-value", "ONLY_WS": "ws-only"},
		tab: {"SHARED": "tab-value"},
	})

	chain := []string{tab, ws} // most specific first

	// Tab overrides the workspace default.
	if v, _, from, ok := s.GetLayered(chain, "SHARED"); !ok || v != "tab-value" || from != tab {
		t.Fatalf("SHARED: got (%q, from=%q, %v), want (tab-value, %q, true)", v, from, ok, tab)
	}
	// Falls back to the workspace when the tab has no value.
	if v, _, from, ok := s.GetLayered(chain, "ONLY_WS"); !ok || v != "ws-only" || from != ws {
		t.Fatalf("ONLY_WS: got (%q, from=%q, %v), want (ws-only, %q, true)", v, from, ok, ws)
	}
	// Environment is the final global fallback (scope token is empty).
	if v, src, from, ok := s.GetLayered(chain, envOnly); !ok || v != "from-env" || src != secretSourceEnv || from != "" {
		t.Fatalf("env fallback: got (%q, %q, from=%q, %v), want (from-env, env, \"\", true)", v, src, from, ok)
	}
	// Workspace-only chain does not see the tab secret.
	if v, _, _, ok := s.GetLayered([]string{ws}, "SHARED"); !ok || v != "ws-value" {
		t.Fatalf("ws-only chain SHARED: got (%q, %v), want (ws-value, true)", v, ok)
	}
	// Missing everywhere.
	if _, _, _, ok := s.GetLayered(chain, "RYSH_TEST_LAYERED_MISSING"); ok {
		t.Fatalf("expected missing secret to be not found")
	}
}

// TestSecretStoreExpandLayered verifies ${NAME} expansion across the tab →
// workspace chain, with the tab overriding the workspace and unresolved
// references left untouched.
func TestSecretStoreExpandLayered(t *testing.T) {
	t.Chdir(t.TempDir())
	const ws, tab = "ws-main", "tab-build"
	s := newSecretStore(nil, map[string]map[string]string{
		ws:  {"HOST": "ws-host", "TOKEN": "ws-token"},
		tab: {"TOKEN": "tab-token"},
	})
	got := s.ExpandLayered([]string{tab, ws}, "host=${HOST} token=${TOKEN} keep=${NOPE}")
	want := "host=ws-host token=tab-token keep=${NOPE}"
	if got != want {
		t.Fatalf("ExpandLayered: got %q, want %q", got, want)
	}
	// nil store is a no-op.
	var nilStore *namedStore
	if out := nilStore.ExpandLayered([]string{tab, ws}, "x=${TOKEN}"); out != "x=${TOKEN}" {
		t.Fatalf("nil ExpandLayered: got %q", out)
	}
}

// TestSecretStoreSetRequiresBucket ensures a non-persisted Set fails cleanly
// without a session bucket and that names are validated.
func TestSecretStoreSetRequiresBucket(t *testing.T) {
	t.Chdir(t.TempDir())
	s := newSecretStore(nil, nil)
	if err := s.Set(testScope, "VALID_NAME", "v", false); err == nil {
		t.Fatalf("expected error when no session bucket is available and --persist is off")
	}
	if validSecretName("1bad") || validSecretName("has space") || validSecretName("") || validSecretName("a/b") {
		t.Fatalf("validSecretName accepted an invalid name")
	}
	if !validSecretName("EMAIL_PASSWORD") || !validSecretName("_x9") {
		t.Fatalf("validSecretName rejected a valid name")
	}
}

// TestSecretStorePersist covers the --persist round trip under a tab scope: Set
// writes a .rysh/secrets/<scope> file even without a session bucket, Get resolves
// it, the file is isolated per scope, and Delete removes it.
func TestSecretStorePersist(t *testing.T) {
	t.Chdir(t.TempDir())
	s := newSecretStore(nil, nil)

	if err := s.Set(testScope, "PERSISTED_TOKEN", "abc123", true); err != nil {
		t.Fatalf("persist Set: %v", err)
	}

	path := filepath.Join(".rysh", "secrets", testScope, "PERSISTED_TOKEN")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "abc123" {
		t.Fatalf("persisted file: got (%q, %v), want abc123", string(data), err)
	}

	if v, src, ok := s.Get(testScope, "PERSISTED_TOKEN"); !ok || v != "abc123" || src != secretSourcePersist {
		t.Fatalf("persist Get: got (%q,%q,%v), want (abc123,persist,true)", v, src, ok)
	}
	// Another tab scope cannot read it.
	if _, _, ok := s.Get("tab-other", "PERSISTED_TOKEN"); ok {
		t.Fatalf("persisted secret leaked across tab scopes")
	}
	// A protective .gitignore is dropped at the secrets root.
	if _, err := os.Stat(filepath.Join(".rysh", "secrets", ".gitignore")); err != nil {
		t.Fatalf("expected .rysh/secrets/.gitignore to be created: %v", err)
	}
	// List tags the row with every tier it exists in; here that is persist only
	// (no session bucket), and the resolving tier is Sources[0].
	if got := s.List(testScope); len(got) != 1 || got[0].Name != "PERSISTED_TOKEN" ||
		got[0].Source() != secretSourcePersist || got[0].sourcesLabel() != "persist" {
		t.Fatalf("List after persist Set: got %+v, want one persist-tier row", got)
	}

	rs, rp, err := s.Delete(testScope, "PERSISTED_TOKEN")
	if err != nil || rs || !rp {
		t.Fatalf("Delete: got (session=%v, persist=%v, err=%v), want (false,true,nil)", rs, rp, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("persisted file was not removed: %v", err)
	}
}

// TestMaskSecretCommandEcho verifies that only the value of a secret-setting
// command is masked, --persist stays visible, and other commands pass through.
func TestMaskSecretCommandEcho(t *testing.T) {
	cases := []struct{ in, want string }{
		{"##secret new EMAIL_PASSWORD app-pass-1234", "##secret new EMAIL_PASSWORD ****"},
		{"##secret set TOKEN a b c", "##secret set TOKEN ****"},
		{"##secret new EMAIL_PASSWORD app-pass --persist", "##secret new EMAIL_PASSWORD **** --persist"},
		{"##secret new --persist TOKEN xyz", "##secret new TOKEN **** --persist"},
		{"##secret new EMAIL_PASSWORD pw --tab work --persist", "##secret new EMAIL_PASSWORD **** --tab work --persist"},
		// --no-persist (persistence is on by default) stays visible like --persist.
		{"##secret new TOKEN xyz --no-persist", "##secret new TOKEN **** --no-persist"},
		{"##secret new --no-persist TOKEN xyz --tab work", "##secret new TOKEN **** --no-persist --tab work"},
		{"##secret new --tab work TOKEN secret-val", "##secret new TOKEN **** --tab work"},
		{"##secret list", "##secret list"},
		{"##secret get EMAIL_PASSWORD", "##secret get EMAIL_PASSWORD"},
		// The ##secrets (plural) alias is masked the same way and keeps its verb.
		{"##secrets new SLACK_BOT_TOKEN xoxb-abc-123", "##secrets new SLACK_BOT_TOKEN ****"},
		{"##secrets set TOKEN a b c --tab work", "##secrets set TOKEN **** --tab work"},
		{"##secrets list", "##secrets list"},
		{"##pane info", "##pane info"},
	}
	for _, c := range cases {
		if got := maskSecretCommandEcho(c.in); got != c.want {
			t.Errorf("maskSecretCommandEcho(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestSplitTabFlag verifies the --tab flag is extracted (in all forms) and
// stripped from the remaining arguments.
func TestSplitTabFlag(t *testing.T) {
	cases := []struct {
		in      []string
		wantTab string
		wantRst []string
	}{
		{[]string{"new", "FOO", "bar"}, "", []string{"new", "FOO", "bar"}},
		{[]string{"new", "FOO", "bar", "--tab", "work"}, "work", []string{"new", "FOO", "bar"}},
		{[]string{"new", "--tab", "work", "FOO", "bar"}, "work", []string{"new", "FOO", "bar"}},
		{[]string{"list", "--tab=work"}, "work", []string{"list"}},
		{[]string{"get", "FOO", "-t", "2"}, "2", []string{"get", "FOO"}},
		{[]string{"new", "FOO", "bar", "--persist", "--tab", "work"}, "work", []string{"new", "FOO", "bar", "--persist"}},
	}
	for _, c := range cases {
		gotTab, gotRst := splitTabFlag(c.in)
		if gotTab != c.wantTab {
			t.Errorf("splitTabFlag(%v) tab = %q, want %q", c.in, gotTab, c.wantTab)
		}
		if len(gotRst) != len(c.wantRst) {
			t.Errorf("splitTabFlag(%v) rest = %v, want %v", c.in, gotRst, c.wantRst)
			continue
		}
		for i := range gotRst {
			if gotRst[i] != c.wantRst[i] {
				t.Errorf("splitTabFlag(%v) rest = %v, want %v", c.in, gotRst, c.wantRst)
				break
			}
		}
	}
}

// TestSecretWorkspaceScopeAndChain verifies the WorkspaceActor scope helpers:
// the workspace scope derives from the workspace name (falling back to session),
// and the skill-resolution chain is [tab, workspace] — collapsing to a single
// element when there is no distinct tab scope.
func TestSecretWorkspaceScopeAndChain(t *testing.T) {
	// Workspace name drives the scope token and label.
	w := &WorkspaceActor{
		workspaceName: "ws-main",
		sessionName:   "sess",
		tabs:          []*tabInfo{{id: "tab-1111", title: "Build"}},
		activeTabIdx:  0,
	}
	if sc, label := w.secretWorkspaceScope(); sc != "ws-main" || label != "ws-main" {
		t.Fatalf("workspace scope: got (%q,%q), want (ws-main,ws-main)", sc, label)
	}
	// Empty paneID falls back to the active tab; chain is [tab, workspace].
	chain := w.secretScopeChain("")
	if len(chain) != 2 || chain[0] != "tab-1111" || chain[1] != "ws-main" {
		t.Fatalf("scope chain: got %v, want [tab-1111 ws-main]", chain)
	}

	// Falls back to the session name when the workspace name is empty.
	w2 := &WorkspaceActor{sessionName: "sess-only"}
	if sc, _ := w2.secretWorkspaceScope(); sc != "sess-only" {
		t.Fatalf("session fallback scope: got %q, want sess-only", sc)
	}
	// With no tabs there is no tab scope, so the chain is workspace-only.
	if chain := w2.secretScopeChain(""); len(chain) != 1 || chain[0] != "sess-only" {
		t.Fatalf("workspace-only chain: got %v, want [sess-only]", chain)
	}
}

// TestSanitizeScope checks UUID pass-through and fallback behaviour.
func TestSanitizeScope(t *testing.T) {
	if got := sanitizeScope("3f2a9c1e-1234-5678-9abc-def012345678"); got != "3f2a9c1e-1234-5678-9abc-def012345678" {
		t.Fatalf("uuid should pass through, got %q", got)
	}
	if got := sanitizeScope(""); got != "default" {
		t.Fatalf("empty scope: got %q, want default", got)
	}
	if got := sanitizeScope("a/b c"); got != "a-b-c" {
		t.Fatalf("unsafe scope: got %q, want a-b-c", got)
	}
}

// TestSecretStoreTrimsSurroundingWhitespace verifies that every resolution tier
// strips leading/trailing spaces and newlines while preserving internal spaces,
// so a Gmail app password ("abcd efgh ijkl mnop") written with a trailing
// newline loads intact.
func TestSecretStoreTrimsSurroundingWhitespace(t *testing.T) {
	t.Chdir(t.TempDir()) // isolate; writes land in CWD/.rysh/secrets

	const gmailPass = "abcd efgh ijkl mnop" // internal spaces must survive

	// Persist tier: file written with surrounding whitespace + newlines.
	if _, err := writePersistedSecret(testScope, "GMAIL_PASS", "  \n"+gmailPass+" \n"); err != nil {
		t.Fatalf("writePersistedSecret: %v", err)
	}
	// Config tier value with surrounding whitespace.
	// Env tier value with surrounding whitespace.
	const envName = "RYSH_TEST_TRIM_ENV"
	os.Setenv(envName, "\t env-secret \n")
	defer os.Unsetenv(envName)

	s := newSecretStore(nil, map[string]map[string]string{
		testScope: {"CFG_KEY": " \tconfig-secret\n"},
	})

	if v, src, ok := s.Get(testScope, "GMAIL_PASS"); !ok || v != gmailPass || src != secretSourcePersist {
		t.Fatalf("persist tier: got (%q,%q,%v), want (%q,persist,true)", v, src, ok, gmailPass)
	}
	if v, src, ok := s.Get(testScope, "CFG_KEY"); !ok || v != "config-secret" || src != secretSourceConfig {
		t.Fatalf("config tier: got (%q,%q,%v), want (config-secret,config,true)", v, src, ok)
	}
	if v, src, ok := s.Get(testScope, envName); !ok || v != "env-secret" || src != secretSourceEnv {
		t.Fatalf("env tier: got (%q,%q,%v), want (env-secret,env,true)", v, src, ok)
	}
}
