// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
)

// ryshSelfCmd runs one ##rysh invocation against a bare workspace and returns
// the rendered output. See paneCmd in workspace_pane_commands_test.go for why
// this only became possible once the case left handleRyshCommand's switch.
func ryshSelfCmd(t *testing.T, w *WorkspaceActor, args ...string) string {
	t.Helper()
	var out strings.Builder
	w.handleRyshSelfCommand(nil, &out, "", args)
	return out.String()
}

// TestRyshSelfCommand_BareIsHelp pins that `##rysh` with no subcommand prints
// the help rather than doing anything: the case rewrites an empty sub to
// "help", which no branch handles, so it falls to default.
func TestRyshSelfCommand_BareIsHelp(t *testing.T) {
	out := ryshSelfCmd(t, &WorkspaceActor{})

	if !strings.Contains(out, `unknown subcommand for ##rysh: "help"`) {
		t.Errorf("bare ##rysh should fall through to the help/default branch, got:\n%s", out)
	}
	for _, want := range []string{
		"##rysh new tab",
		"##rysh new lane",
		"##rysh new pane",
		"##rysh tab name",
		"##rysh lane name",
		"##rysh web start",
		"##rysh web stop",
		"##rysh web status",
		"##rysh web auth",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q:\n%s", want, out)
		}
	}
}

func TestRyshSelfCommand_UnknownSubcommand(t *testing.T) {
	out := ryshSelfCmd(t, &WorkspaceActor{}, "wibble")
	if !strings.Contains(out, `unknown subcommand for ##rysh: "wibble"`) {
		t.Errorf("expected the unknown-subcommand line, got:\n%s", out)
	}
}

// TestRyshSelfCommand_WebUsage pins that `##rysh web` with no action, and with
// an unrecognised one, both render the web usage block.
func TestRyshSelfCommand_WebUsage(t *testing.T) {
	for _, args := range [][]string{
		{"web"},
		{"web", "sideways"},
	} {
		out := ryshSelfCmd(t, &WorkspaceActor{}, args...)
		if !strings.Contains(out, "##rysh web start") || !strings.Contains(out, "##rysh web status") {
			t.Errorf("##rysh %v did not render the web usage:\n%s", args, out)
		}
	}
}

// TestRyshSelfCommand_WebStopWhenNotRunning pins the not-running report. This
// is the one web branch reachable with no server object at all, and it is the
// message a user sees most often after a failed start.
func TestRyshSelfCommand_WebStopWhenNotRunning(t *testing.T) {
	out := ryshSelfCmd(t, &WorkspaceActor{}, "web", "stop")
	if !strings.Contains(out, "web server is not running") {
		t.Errorf("expected the not-running report, got:\n%s", out)
	}
}

// TestRyshSelfCommand_WebStartRefusesWithoutLogin is the point of the whole
// change: with no login stored and none passed, the UI is not served at all
// rather than falling back to a URL-borne access token.
func TestRyshSelfCommand_WebStartRefusesWithoutLogin(t *testing.T) {
	w := &WorkspaceActor{}
	w.cfg.RyshDir = t.TempDir()

	out := ryshSelfCmd(t, w, "web", "start")
	if !strings.Contains(out, "no web login configured") {
		t.Errorf("expected a refusal explaining the missing login, got:\n%s", out)
	}
	if !strings.Contains(out, "--username") || !strings.Contains(out, "##rysh web auth") {
		t.Errorf("refusal should point at both ways to set a login, got:\n%s", out)
	}
	if w.webServer != nil {
		t.Error("a refused start must not leave a web server behind")
	}
}

// Half a login is a typo, and saying which half is missing is cheaper than
// letting it start under the wrong (or no) credentials.
func TestRyshSelfCommand_WebStartRejectsHalfALogin(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		missing string
	}{
		{[]string{"web", "start", "--username", "halil"}, "--password"},
		{[]string{"web", "start", "--password", "s3cret"}, "--username"},
	} {
		w := &WorkspaceActor{}
		w.cfg.RyshDir = t.TempDir()
		out := ryshSelfCmd(t, w, tc.args...)
		if !strings.Contains(out, tc.missing+" is missing") {
			t.Errorf("%v should name %s as missing, got:\n%s", tc.args, tc.missing, out)
		}
		if w.webServer != nil {
			t.Errorf("%v must not start a server", tc.args)
		}
	}
}

// `##rysh web token` is gone, but muscle memory is not: it must answer with
// what replaced it rather than the generic unknown-subcommand usage.
func TestRyshSelfCommand_WebTokenIsRetired(t *testing.T) {
	out := ryshSelfCmd(t, &WorkspaceActor{}, "web", "token")
	if !strings.Contains(out, "access tokens are gone") {
		t.Errorf("expected the retirement notice, got:\n%s", out)
	}
	if !strings.Contains(out, "##rysh web auth") {
		t.Errorf("retirement notice should point at the login, got:\n%s", out)
	}
}

// The stop guard asks "is anything connected", not "is this the app's daemon".
// A server nobody is attached to stops without argument — that is the ordinary
// case for a `bash` pane in a session no app window is showing — while a
// connected client makes --force the only way through.
func TestWebStopNeedsForce(t *testing.T) {
	cases := []struct {
		name    string
		clients int32
		args    []string
		want    bool
	}{
		{"nobody connected", 0, nil, false},
		{"nobody connected, control-mode daemon", 0, nil, false},
		{"one client connected", 1, nil, true},
		{"several connected", 3, nil, true},
		{"connected but forced", 2, []string{"--force"}, false},
		{"connected but forced short", 2, []string{"-f"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := webStopNeedsForce(tc.clients, tc.args); got != tc.want {
				t.Fatalf("webStopNeedsForce(%d, %v) = %v, want %v", tc.clients, tc.args, got, tc.want)
			}
		})
	}
}
