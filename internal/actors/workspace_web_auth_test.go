// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/web"
)

func TestParseWebAuthArgs_SetsCredentials(t *testing.T) {
	got, warnings := parseWebAuthArgs([]string{"username=halil", "password=s3cret"})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if got.Username != "halil" || got.Password != "s3cret" {
		t.Fatalf("parsed = %+v", got)
	}
	if got.Show || got.Clear {
		t.Fatalf("parsed = %+v, want a plain set", got)
	}
}

// The aliases exist because "user=" and "pass=" are what people type.
func TestParseWebAuthArgs_Aliases(t *testing.T) {
	got, warnings := parseWebAuthArgs([]string{"--user=halil", "pass=s3cret"})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	if got.Username != "halil" || got.Password != "s3cret" {
		t.Fatalf("parsed = %+v", got)
	}
}

// A password is taken verbatim: '=' and punctuation are legal characters in
// one, and only the whitespace the command line already split on is not.
func TestParseWebAuthArgs_PasswordKeepsEquals(t *testing.T) {
	got, _ := parseWebAuthArgs([]string{"username=halil", "password=a=b=c!"})
	if got.Password != "a=b=c!" {
		t.Fatalf("password = %q, want a=b=c!", got.Password)
	}
}

func TestParseWebAuthArgs_BareShows(t *testing.T) {
	got, warnings := parseWebAuthArgs(nil)
	if !got.Show || len(warnings) != 0 {
		t.Fatalf("parsed = %+v warnings = %v", got, warnings)
	}
}

func TestParseWebAuthArgs_Clear(t *testing.T) {
	for _, arg := range []string{"clear", "--clear", "off", "REMOVE"} {
		got, warnings := parseWebAuthArgs([]string{arg})
		if !got.Clear || len(warnings) != 0 {
			t.Fatalf("%q: parsed = %+v warnings = %v", arg, got, warnings)
		}
	}
}

// A typo warns and is skipped rather than failing the command, matching
// parseWebStartArgs — the user still learns what was applied.
func TestParseWebAuthArgs_UnknownArgsWarn(t *testing.T) {
	got, warnings := parseWebAuthArgs([]string{"username=halil", "passwrod=s3cret", "loose"})
	if got.Username != "halil" || got.Password != "" {
		t.Fatalf("parsed = %+v", got)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want 2", warnings)
	}
}

// setWebCredentials now ALWAYS applies: the server enforces per door, so a
// control-mode server holds credentials without ever demanding them of the
// desktop app — and holding them is what lets its shared door check a
// browser's password.
func TestSetWebCredentials_AppliesEvenInControlMode(t *testing.T) {
	creds, err := web.SaveCredentials(t.TempDir(), "halil", "s3cret")
	if err != nil {
		t.Fatal(err)
	}

	control := web.NewServer(0, "s", nil, nil, nil)
	control.SetControl(true)
	var out strings.Builder
	if !setWebCredentials(control, creds, &out) {
		t.Error("credentials must be applied even in control mode")
	}
	if !control.LoginEnabled() {
		t.Error("control-mode server should hold the credentials for its shared door")
	}
	if !strings.Contains(out.String(), "stays open without it") {
		t.Errorf("expected a note that the app's own connection is unaffected, got:\n%s", out.String())
	}

	// Clearing still applies.
	if !setWebCredentials(control, nil, nil) {
		t.Error("clearing credentials must always apply")
	}
	if control.LoginEnabled() {
		t.Error("credentials should be cleared")
	}
}

// `##rysh web auth clear` must not leave a running UI outside control mode
// without its login: it refuses and says to stop the UI first. In control mode
// the desktop app's own door needs no login, so clearing applies there.
func TestWebAuthClear_RefusedWhileTheUIRunsOutsideControlMode(t *testing.T) {
	nc := newABLATestNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	for _, control := range []bool{false, true} {
		dir := t.TempDir()
		w := &WorkspaceActor{cfg: config.Config{RyshDir: dir}, sessionName: "s1"}
		if _, err := web.SaveCredentialsFor(w.credentialsRef(), "halil", "s3cret"); err != nil {
			t.Fatal(err)
		}
		srv := web.NewServer(freePort(t), "s1", pub, nc, pub.Codecs())
		srv.SetHost("127.0.0.1")
		srv.SetControl(control)
		srv.TrackCredentialsRef(w.credentialsRef()) // as ##rysh web start does
		if err := srv.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		t.Cleanup(func() { _ = srv.Stop() })
		w.webServer = srv

		var out strings.Builder
		w.handleWebAuth(&out, []string{"clear"})
		_, statErr := os.Stat(w.credentialsRef().OwnPath())
		if control {
			if statErr == nil {
				t.Errorf("control mode: clear did not remove the login:\n%s", out.String())
			}
			continue
		}
		if statErr != nil {
			t.Errorf("running, not control: the login was removed (%v):\n%s", statErr, out.String())
		}
		if !srv.LoginEnabled() {
			t.Errorf("running, not control: the live server lost its login")
		}
		if !strings.Contains(out.String(), "##rysh web stop") {
			t.Errorf("refusal must say to stop the UI first, got:\n%s", out.String())
		}
		if w.takeRyshFailure() == nil {
			t.Errorf("refusal must fail the command")
		}
	}
}
