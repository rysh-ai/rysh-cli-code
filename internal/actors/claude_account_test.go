// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/agentenv"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const fixtureAccountA = "fixture-account-a"

func accountHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	prev := claudeAccountHome
	claudeAccountHome = func() string { return home }
	t.Cleanup(func() { claudeAccountHome = prev })
	return home
}

func accountWorkspace(secrets map[string]string) *WorkspaceActor {
	return &WorkspaceActor{
		workspaceName: "ws-main",
		sessionName:   "sess",
		secrets:       newSecretStore(nil, map[string]map[string]string{"ws-main": secrets}),
	}
}

func TestClaudeAccountSecretNames(t *testing.T) {
	if s, ok := claudeAccountSecret("Main"); !ok || s != "CLAUDE_MAIN_OAUTH_TOKEN" {
		t.Fatalf("Main -> %q %v", s, ok)
	}
	for _, bad := range []string{"", "code", "a-b", "_x", "a b"} {
		if _, ok := claudeAccountSecret(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if a, ok := claudeAccountFromSecret("CLAUDE_SECOND_OAUTH_TOKEN"); !ok || a != "second" {
		t.Fatalf("from secret -> %q %v", a, ok)
	}
	for _, not := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE__OAUTH_TOKEN", "OPENAI_API_KEY", "CLAUDE_X_TOKEN"} {
		if _, ok := claudeAccountFromSecret(not); ok {
			t.Errorf("%q read as an account", not)
		}
	}
}

func TestClaudeAccountUseListOff(t *testing.T) {
	home := accountHome(t)
	w := accountWorkspace(map[string]string{
		"CLAUDE_MAIN_OAUTH_TOKEN":   fixtureAccountA,
		"CLAUDE_SECOND_OAUTH_TOKEN": "fixture-account-b",
		"OPENAI_API_KEY":            "unrelated",
	})

	var out strings.Builder
	if err := w.handleNativeAgentCommand(&out, "pane-1", nativeAgentClaude, []string{"account"}); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "main") || !strings.Contains(s, "second") || strings.Contains(s, "OPENAI") || !strings.Contains(s, "active: none") {
		t.Fatalf("list:\n%s", s)
	}

	out.Reset()
	if err := w.handleClaudeAccount(&out, "pane-1", []string{"use", "MAIN"}); err != nil {
		t.Fatalf("use: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), fixtureAccountA) {
		t.Fatal("the token was printed")
	}
	dir := agentenv.ClaudeAccountDir(home)
	tok := agentenv.ClaudeAccountTokenFile(dir)
	data, err := os.ReadFile(tok)
	if err != nil || strings.TrimSpace(string(data)) != fixtureAccountA {
		t.Fatalf("token file not written: %v", err)
	}
	for path, want := range map[string]os.FileMode{tok: 0o600, dir: 0o700} {
		if fi, _ := os.Stat(path); fi == nil || fi.Mode().Perm() != want {
			t.Fatalf("%s mode wrong", path)
		}
	}
	if got := claudeAccountActive(); got != "main" {
		t.Fatalf("active = %q", got)
	}

	out.Reset()
	_ = w.handleClaudeAccount(&out, "pane-1", nil)
	if !strings.Contains(out.String(), "* main") {
		t.Fatalf("active not marked:\n%s", out.String())
	}

	out.Reset()
	if err := w.handleClaudeAccount(&out, "pane-1", []string{"off"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tok); !os.IsNotExist(err) {
		t.Fatal("off left the token file behind")
	}
	if claudeAccountActive() != "" {
		t.Fatal("still active after off")
	}
}

func TestClaudeAccountUseRefusesMissingSecret(t *testing.T) {
	home := accountHome(t)
	w := accountWorkspace(nil)
	var out strings.Builder
	if err := w.handleClaudeAccount(&out, "pane-1", []string{"use", "ghost"}); err == nil {
		t.Fatal("use of a missing secret succeeded")
	}
	if !strings.Contains(out.String(), "##secret set CLAUDE_GHOST_OAUTH_TOKEN") {
		t.Fatalf("no guidance:\n%s", out.String())
	}
	if _, err := os.Stat(agentenv.ClaudeAccountTokenFile(agentenv.ClaudeAccountDir(home))); !os.IsNotExist(err) {
		t.Fatal("a refused use wrote a token file")
	}
}

// A secret replaced after `use` is picked up by the next launch.
func TestClaudeAccountRefreshRecopiesTheSecret(t *testing.T) {
	home := accountHome(t)
	w := accountWorkspace(map[string]string{"CLAUDE_MAIN_OAUTH_TOKEN": "fixture-old"})
	var out strings.Builder
	if err := w.handleClaudeAccount(&out, "p", []string{"use", "main"}); err != nil {
		t.Fatal(err)
	}
	w.secrets = newSecretStore(nil, map[string]map[string]string{"ws-main": {"CLAUDE_MAIN_OAUTH_TOKEN": "fixture-new"}})
	if acct, note := w.claudeAccountRefresh("p"); acct != "main" || note != "" {
		t.Fatalf("refresh = %q %q", acct, note)
	}
	data, _ := os.ReadFile(agentenv.ClaudeAccountTokenFile(agentenv.ClaudeAccountDir(home)))
	if strings.TrimSpace(string(data)) != "fixture-new" {
		t.Fatal("refresh did not re-copy the secret")
	}
}

func claudePane(id string, meta map[string]string) ansaPane {
	m := map[string]string{}
	for k, v := range meta {
		m[k] = v
	}
	return ansaPane{ID: id, GivenName: id, Meta: m}
}

// --restart stops and resumes rysh-launched claudes, caller last, and leaves
// hand-started ones alone.
func TestClaudeAccountRestartOrderAndScope(t *testing.T) {
	prevPause := ansaKillVerifyPause
	ansaKillVerifyPause = time.Millisecond
	t.Cleanup(func() { ansaKillVerifyPause = prevPause })

	native := map[string]string{metaAgentNative: "1", metaAgentKind: "claude"}
	tr := &fakeAnsaTransport{program: "claude", panes: []ansaPane{
		claudePane("caller", native),
		claudePane("worker", native),
		claudePane("by-hand", nil),
		claudePane("codexer", map[string]string{metaAgentNative: "1", metaAgentKind: "codex"}),
	}}
	var out strings.Builder
	claudeAccountRestartVia(&out, &ansaRouter{tr: tr}, "caller", false)

	var resumed []string
	for _, d := range tr.delivered {
		if d.Mode != msg.AnsaModeShell || !strings.Contains(d.Text, "claude --resume "+d.PaneID) {
			t.Fatalf("unexpected delivery %+v", d)
		}
		resumed = append(resumed, d.PaneID)
	}
	if strings.Join(resumed, ",") != "worker,caller" {
		t.Fatalf("resumed %v, want worker then caller\n%s", resumed, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "by-hand") || !strings.Contains(s, "started by hand") || strings.Contains(s, "codexer") {
		t.Fatalf("scope wrong:\n%s", s)
	}
	if !strings.Contains(s, "2 claude panes restarted") || !strings.Contains(s, "0 busy") {
		t.Fatalf("summary wrong:\n%s", s)
	}
}

// C-1: the account is read from the WORKSPACE scope only. A same-name tab
// secret (the calling pane's tab is active) must not reach the file, on use or
// on refresh; nor may a process environment variable stand in for a missing
// workspace secret. Compared by equality on synthetic fixtures, nothing printed.
func TestClaudeAccountIgnoresTabAndEnvScopes(t *testing.T) {
	home := accountHome(t)
	w := &WorkspaceActor{
		workspaceName: "ws-main",
		sessionName:   "sess",
		tabs:          []*tabInfo{{id: "tab-1111", title: "Build"}},
		secrets: newSecretStore(nil, map[string]map[string]string{
			"ws-main":  {"CLAUDE_MAIN_OAUTH_TOKEN": "fixture-workspace"},
			"tab-1111": {"CLAUDE_MAIN_OAUTH_TOKEN": "fixture-tab", "CLAUDE_TABONLY_OAUTH_TOKEN": "fixture-tab-only"},
		}),
	}
	// The tab scope holds a different value under the same name; with the old
	// pane-chain lookup (tab first) it is the one that would have won.
	tok := agentenv.ClaudeAccountTokenFile(agentenv.ClaudeAccountDir(home))
	fileIs := func(want string) bool {
		data, _ := os.ReadFile(tok)
		return strings.TrimSpace(string(data)) == want
	}

	var out strings.Builder
	if err := w.handleClaudeAccount(&out, "pane-1", []string{"use", "main"}); err != nil {
		t.Fatal(err)
	}
	if !fileIs("fixture-workspace") {
		t.Fatal("use did not take the workspace value")
	}
	if acct, _ := w.claudeAccountRefresh("pane-1"); acct != "main" || !fileIs("fixture-workspace") {
		t.Fatal("refresh from a pane in the tab changed the file")
	}
	if got := claudeAccountActiveScope(); got != "ws-main" {
		t.Fatalf("scope recorded as %q", got)
	}

	out.Reset()
	if err := w.handleClaudeAccount(&out, "pane-1", []string{"use", "tabonly"}); err == nil {
		t.Fatal("a tab-only secret was accepted")
	}
	out.Reset()
	_ = w.handleClaudeAccount(&out, "pane-1", nil)
	if strings.Contains(out.String(), "tabonly") {
		t.Fatalf("a tab-only secret was listed:\n%s", out.String())
	}

	t.Setenv("CLAUDE_ENVONLY_OAUTH_TOKEN", "fixture-env")
	out.Reset()
	if err := w.handleClaudeAccount(&out, "pane-1", []string{"use", "envonly"}); err == nil {
		t.Fatal("a process-environment value was accepted")
	}
	if !fileIs("fixture-workspace") {
		t.Fatal("a refused use changed the file")
	}
}

// A refresh run by a daemon of another workspace leaves the file alone.
func TestClaudeAccountRefreshSkipsOtherWorkspace(t *testing.T) {
	home := accountHome(t)
	a := accountWorkspace(map[string]string{"CLAUDE_MAIN_OAUTH_TOKEN": "fixture-a"})
	var out strings.Builder
	if err := a.handleClaudeAccount(&out, "p", []string{"use", "main"}); err != nil {
		t.Fatal(err)
	}
	b := &WorkspaceActor{workspaceName: "ws-other", secrets: newSecretStore(nil, map[string]map[string]string{
		"ws-other": {"CLAUDE_MAIN_OAUTH_TOKEN": "fixture-b"},
	})}
	if _, note := b.claudeAccountRefresh("p"); !strings.Contains(note, "not refreshed") {
		t.Fatalf("note = %q", note)
	}
	data, _ := os.ReadFile(agentenv.ClaudeAccountTokenFile(agentenv.ClaudeAccountDir(home)))
	if strings.TrimSpace(string(data)) != "fixture-a" {
		t.Fatal("another workspace's refresh rewrote the file")
	}
}

// Q4: without --now a busy pane is left running; with it, it is restarted.
func TestClaudeAccountRestartSkipsBusyUnlessNow(t *testing.T) {
	prevPause := ansaKillVerifyPause
	ansaKillVerifyPause = time.Millisecond
	t.Cleanup(func() { ansaKillVerifyPause = prevPause })
	native := map[string]string{metaAgentNative: "1", metaAgentKind: "claude"}
	working := [][]string{{"✻ Thinking… (esc to interrupt)"}}

	tr := &fakeAnsaTransport{program: "claude", screens: working, panes: []ansaPane{claudePane("worker", native)}}
	var out strings.Builder
	claudeAccountRestartVia(&out, &ansaRouter{tr: tr}, "caller", false)
	if len(tr.delivered) != 0 || len(tr.killed) != 0 || !strings.Contains(out.String(), "busy, left running worker") {
		t.Fatalf("busy pane touched without --now (killed %v):\n%s", tr.killed, out.String())
	}

	tr = &fakeAnsaTransport{program: "claude", screenErr: errors.New("unreadable"), panes: []ansaPane{claudePane("worker", native)}}
	out.Reset()
	claudeAccountRestartVia(&out, &ansaRouter{tr: tr}, "caller", false)
	if len(tr.killed) != 0 {
		t.Fatal("an unreadable screen was treated as idle")
	}

	tr = &fakeAnsaTransport{program: "claude", screens: working, panes: []ansaPane{claudePane("worker", native)}}
	out.Reset()
	claudeAccountRestartVia(&out, &ansaRouter{tr: tr}, "caller", true)
	if len(tr.delivered) != 1 || tr.delivered[0].PaneID != "worker" {
		t.Fatalf("--now did not restart the busy pane:\n%s", out.String())
	}
}
