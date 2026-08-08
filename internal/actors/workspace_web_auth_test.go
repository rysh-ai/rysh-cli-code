package actors

import (
	"strings"
	"testing"

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
