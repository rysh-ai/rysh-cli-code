// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/familyclient"
	"github.com/rysh-ai/rysh-cli-code/internal/familyclient/familytest"
)

// `rysh heymates` (design 093 §6) against an in-process fake of rysh-server's
// /api/family. No network, no session, no daemon.

// lockedBuffer is a bytes.Buffer safe for the socket goroutine and the test.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type familyHarness struct {
	srv    *familytest.Server
	cli    *familyCLI
	out    *lockedBuffer
	errOut *lockedBuffer
	env    map[string]string
}

// newFamilyHarness returns a CLI whose rysh dir holds a saved login for the
// fake server.
func newFamilyHarness(t *testing.T) *familyHarness {
	t.Helper()
	srv := familytest.New()
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	h := &familyHarness{srv: srv, out: &lockedBuffer{}, errOut: &lockedBuffer{}, env: map[string]string{}}
	h.cli = &familyCLI{
		cfg:    config.Config{RyshDir: dir},
		stdin:  strings.NewReader(""),
		stdout: h.out,
		stderr: h.errOut,
		getenv: func(k string) string { return h.env[k] },
		follow: familyclient.FollowOptions{MinBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond},
	}
	if err := familyclient.DefaultTokenStore(dir).Put(srv.URL, familyclient.StoredSession{
		Email: "u@example.test", AccessToken: "acc-1", RefreshToken: "ref-1",
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *familyHarness) run(args ...string) error {
	return h.cli.run(append([]string{"family"}, args...))
}

func TestFamilyLsUsesTheSavedLogin(t *testing.T) {
	h := newFamilyHarness(t)
	if err := h.run("ls"); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	for _, want := range []string{"Home  (fam-aaaa, cozy, 2 avatars)", "Mia  partner   thinking 40%  looking up concerts", "Holiday Crew"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls missing %q:\n%s", want, out)
		}
	}
}

func TestFamilyShowByNameAndPrefix(t *testing.T) {
	h := newFamilyHarness(t)
	if err := h.run("show", "HOME"); err != nil {
		t.Fatal(err)
	}
	out := h.out.String()
	for _, want := range []string{"Home  id fam-aaaa-1111", "open tasks: 2", "Find a concert Saturday"} {
		if !strings.Contains(out, want) {
			t.Errorf("show missing %q:\n%s", want, out)
		}
	}
	err := h.run("show", "fam-a")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "Holiday Crew (fam-aabb-2222)") {
		t.Fatalf("ambiguous prefix: %v", err)
	}
}

func TestFamilyShowJSON(t *testing.T) {
	h := newFamilyHarness(t)
	if err := h.run("show", "fam-aab", "--json"); err != nil {
		t.Fatal(err)
	}
	if out := h.out.String(); !strings.Contains(out, `"name": "Holiday Crew"`) || !strings.Contains(out, `"tasks": []`) {
		t.Errorf("json output:\n%s", out)
	}
}

func TestFamilyChatOneShotStreamsTheReply(t *testing.T) {
	h := newFamilyHarness(t)
	if err := h.run("chat", "home", "mia", "--", "find", "a", "concert"); err != nil {
		t.Fatal(err)
	}
	if got := h.out.String(); got != "Hello there\n" {
		t.Errorf("stdout %q, want exactly the reply", got)
	}
	if p := h.srv.PostedBodies(); len(p) != 1 || p[0] != "find a concert" {
		t.Errorf("posted %q", p)
	}
}

func TestFamilyChatTimeout(t *testing.T) {
	h := newFamilyHarness(t)
	h.srv.Mu.Lock()
	h.srv.NoReply = true
	h.srv.Mu.Unlock()
	start := time.Now()
	err := h.run("chat", "home", "rex", "--timeout", "300ms", "--", "hello?")
	if err == nil || !strings.Contains(err.Error(), "no reply from Rex within 300ms") {
		t.Fatalf("got %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s", d)
	}
}

func TestFamilyChatInteractiveUntilEOF(t *testing.T) {
	h := newFamilyHarness(t)
	h.cli.stdin = strings.NewReader("first\n\nsecond\n")
	if err := h.run("chat", "home", "mia"); err != nil {
		t.Fatal(err)
	}
	if got := h.out.String(); got != "Hello there\nHello there\n" {
		t.Errorf("stdout %q", got)
	}
	if p := h.srv.PostedBodies(); len(p) != 2 || p[0] != "first" || p[1] != "second" {
		t.Errorf("posted %q (the blank line must be skipped)", p)
	}
}

func TestFamilyRefreshesAndSavesTokens(t *testing.T) {
	h := newFamilyHarness(t)
	h.srv.Expire()
	if err := h.run("ls"); err != nil {
		t.Fatal(err)
	}
	if h.srv.Refreshes() != 1 {
		t.Fatalf("refresh calls %d", h.srv.Refreshes())
	}
	sess, err := familyclient.DefaultTokenStore(h.cli.cfg.RyshDir).Get(h.srv.URL)
	if err != nil || sess == nil || sess.AccessToken != "acc-2" || sess.RefreshToken != "ref-2" {
		t.Fatalf("refreshed tokens not saved: %+v %v", sess, err)
	}
}

func TestFamilyTokenEnvAndServerFlag(t *testing.T) {
	h := newFamilyHarness(t)
	// Another rysh dir with no saved login: only --server + --token-env work.
	h.cli.cfg.RyshDir = t.TempDir()
	if err := h.run("ls"); err == nil || !strings.Contains(err.Error(), "no rysh server") {
		t.Fatalf("no server: %v", err)
	}
	if err := h.run("ls", "--server", h.srv.URL); err == nil || !strings.Contains(err.Error(), "not signed in to") {
		t.Fatalf("no login: %v", err)
	}
	if err := h.run("ls", "--server", h.srv.URL, "--token-env", "FAM_TOKEN"); err == nil || !strings.Contains(err.Error(), "FAM_TOKEN: that environment variable is not set") {
		t.Fatalf("unset env: %v", err)
	}
	h.env["FAM_TOKEN"] = h.srv.AccessToken()
	if err := h.run("ls", "--server="+h.srv.URL, "--token-env", "FAM_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.out.String(), "Home") {
		t.Errorf("ls with --token-env:\n%s", h.out.String())
	}
	// RYSH_SERVER_URL is the next fallback after --server.
	h.env["RYSH_SERVER_URL"] = h.srv.URL
	if err := h.run("ls", "--token-env", "FAM_TOKEN"); err != nil {
		t.Fatal(err)
	}
	// The upstream url is the last fallback.
	delete(h.env, "RYSH_SERVER_URL")
	h.cli.cfg.Upstream.URL = h.srv.URL + "/"
	if err := h.run("ls", "--token-env", "FAM_TOKEN"); err != nil {
		t.Fatal(err)
	}
}

func TestFamilyRefusesALiteralToken(t *testing.T) {
	h := newFamilyHarness(t)
	for _, args := range [][]string{{"ls", "--token", "abc"}, {"ls", "--token=abc"}, {"login", "--password", "x"}} {
		err := h.run(args...)
		if err == nil || !strings.Contains(err.Error(), "--token-env") {
			t.Errorf("%v: got %v", args, err)
		}
	}
}

func TestFamilyLoginSavesPrivateTokens(t *testing.T) {
	h := newFamilyHarness(t)
	h.cli.cfg.RyshDir = t.TempDir()
	h.cli.stdin = strings.NewReader("u@example.test\npw\n")
	if err := h.run("login", "--server", h.srv.URL); err != nil {
		t.Fatal(err)
	}
	st := familyclient.DefaultTokenStore(h.cli.cfg.RyshDir)
	fi, err := os.Stat(st.Path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	if strings.Contains(h.out.String()+h.errOut.String(), "acc-1") {
		t.Error("a token was printed")
	}
	// The login's server is now the default: no --server needed.
	h.out = &lockedBuffer{}
	h.cli.stdout = h.out
	if err := h.run("ls"); err != nil {
		t.Fatal(err)
	}
	h.cli.stdin = strings.NewReader("u@example.test\nwrong\n")
	if err := h.run("login"); err == nil || !strings.Contains(err.Error(), "wrong email or password") {
		t.Fatalf("bad password: %v", err)
	}
	if err := h.run("logout"); err != nil || !strings.Contains(h.out.String(), "signed out of") {
		t.Fatalf("logout: %v %q", err, h.out.String())
	}
}

func TestFamilyWatchPrintsOneLinePerEvent(t *testing.T) {
	h := newFamilyHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := parseFamilyArgs([]string{"home"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := h.cli.client(a)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- h.cli.watch(ctx, c, a) }()

	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(h.out.String(), want) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("watch never printed %q:\n%s", want, h.out.String())
	}
	waitFor("hello    connected to Home (2 avatars)")
	pct := 55.0
	h.srv.Broadcast("fam-aaaa-1111", familyclient.Event{Type: "status", AvatarID: "av-rex-0002", Status: "working", Pct: &pct, Note: "flights"})
	h.srv.Broadcast("fam-aaaa-1111", familyclient.Event{Type: "note", FromAvatarID: "av-mia-0001", ToAvatarID: "av-rex-0002", Text: "any gigs?"})
	h.srv.Broadcast("fam-aaaa-1111", familyclient.Event{Type: "delta", AvatarID: "av-mia-0001", Text: "SHOULD-NOT-PRINT"})
	h.srv.Broadcast("fam-aaaa-1111", familyclient.Event{Type: "task", Task: &familyclient.Task{AvatarID: "av-rex-0002", Title: "Compare flights", State: "doing"}})
	waitFor("task     Rex [doing] Compare flights")
	out := h.out.String()
	for _, want := range []string{"status   Rex → working 55% (flights)", "note     Mia → Rex: any gigs?"} {
		if !strings.Contains(out, want) {
			t.Errorf("watch missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SHOULD-NOT-PRINT") {
		t.Error("watch printed a delta chunk")
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if len(line) < 9 || line[2] != ':' || line[5] != ':' {
			t.Errorf("line without a time prefix: %q", line)
		}
	}

	// A dropped socket reconnects and says so on stderr.
	h.srv.DropSockets()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(h.errOut.String(), "reconnected") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(h.errOut.String(), "[family socket reconnected]") {
		t.Errorf("no reconnect notice: %q", h.errOut.String())
	}

	cancel() // Ctrl-C
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch returned %v on cancel, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not stop on cancel")
	}
}

func TestFamilyUsage(t *testing.T) {
	h := newFamilyHarness(t)
	if err := h.run(); err == nil || !strings.Contains(err.Error(), "heymates chat <family> <avatar>") {
		t.Errorf("no-arg usage: %v", err)
	}
	if err := h.run("bogus"); err == nil || !strings.Contains(err.Error(), `unknown heymates command "bogus"`) {
		t.Errorf("unknown sub: %v", err)
	}
	if err := h.run("chat", "home"); err == nil || !strings.Contains(err.Error(), "usage: ") {
		t.Errorf("chat without avatar: %v", err)
	}
	if err := h.run("chat", "home", "mia", "--timeout", "soon"); err == nil || !strings.Contains(err.Error(), "--timeout") {
		t.Errorf("bad timeout: %v", err)
	}
}

// `rysh heymates serve` has its own flags and never touches the family API.
func TestFamilyServeArgs(t *testing.T) {
	h := newFamilyHarness(t)
	err := h.run("serve", "--bogus")
	if err == nil || !strings.Contains(err.Error(), "unknown argument") || !strings.Contains(err.Error(), "heymates serve") {
		t.Fatalf("err = %v", err)
	}
	if err := h.run("serve", "--session"); err == nil || !strings.Contains(err.Error(), "--session needs a value") {
		t.Fatalf("err = %v", err)
	}
	if err := h.run("help"); err != nil || !strings.Contains(h.out.String(), "rysh heymates serve") {
		t.Fatalf("help does not list serve: %v\n%s", err, h.out.String())
	}
}

// F1 (e20): a plain-http family server off loopback is refused before anything
// is sent to it, whichever way it was chosen; loopback (the fake) still works.
func TestFamilyRefusesAPlainHTTPServer(t *testing.T) {
	h := newFamilyHarness(t)
	h.env["FAM_TOKEN"] = h.srv.AccessToken()
	for _, server := range []string{"http://family.example.test", "ws://family.example.test"} {
		err := h.run("ls", "--server", server, "--token-env", "FAM_TOKEN")
		if err == nil || !strings.Contains(err.Error(), "https") {
			t.Errorf("--server %s: got %v, want a refusal naming https", server, err)
		}
	}
	h.env["RYSH_SERVER_URL"] = "http://family.example.test"
	if err := h.run("ls", "--token-env", "FAM_TOKEN"); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("RYSH_SERVER_URL http: got %v, want a refusal", err)
	}
	delete(h.env, "RYSH_SERVER_URL")
	h.cli.cfg.RyshDir = t.TempDir()
	h.cli.stdin = strings.NewReader("u@example.test\npw\n")
	if err := h.run("login", "--server", "http://family.example.test"); err == nil {
		t.Errorf("login to a plain-http server was not refused")
	}
	if err := h.run("ls", "--server", h.srv.URL, "--token-env", "FAM_TOKEN"); err != nil {
		t.Errorf("loopback http (the fake) refused: %v", err)
	}
}

// F3 (e20): before a password or an environment token is sent, the command
// names the server that gets it.
func TestFamilyNamesTheServerBeforeSendingASecret(t *testing.T) {
	h := newFamilyHarness(t)
	host := strings.TrimPrefix(h.srv.URL, "http://")
	h.env["FAM_TOKEN"] = h.srv.AccessToken()
	if err := h.run("ls", "--server", h.srv.URL, "--token-env", "FAM_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.errOut.String(), host) {
		t.Errorf("--token-env: stderr %q does not name %s", h.errOut.String(), host)
	}
	h.errOut = &lockedBuffer{}
	h.cli.stderr = h.errOut
	h.cli.cfg.RyshDir = t.TempDir()
	h.cli.stdin = strings.NewReader("pw\n")
	if err := h.run("login", "--server", h.srv.URL, "--email", "u@example.test"); err != nil {
		t.Fatal(err)
	}
	if e := h.errOut.String(); !strings.Contains(e, "password for "+host+": ") {
		t.Errorf("login with --email: stderr %q must ask for the password for %s", e, host)
	}
}

// heymates is the product's name (founder, 2026-11-01); `family` stays an alias
// for one release because the hosted runner execs `rysh_local family apply|serve`
// from a digest-pinned image. Both names must dispatch alike and skip the PTY
// preflight alike.
func TestHeymatesCommandAndFamilyAlias(t *testing.T) {
	for _, name := range []string{"heymates", "family"} {
		if !ptylessCommands[name] {
			t.Errorf("%q is not in the PTY-free command list", name)
		}
	}
	names := map[string]bool{}
	for _, n := range dispatchedCommands(t) {
		names[n] = true
	}
	if !names["heymates"] || !names["family"] {
		t.Fatalf("run() must dispatch both heymates and family, got heymates=%v family=%v", names["heymates"], names["family"])
	}
	if !strings.Contains(familyUsage, "rysh heymates ") || strings.Contains(familyUsage, "rysh family ") {
		t.Errorf("usage must name rysh heymates only:\n%s", familyUsage)
	}
}
