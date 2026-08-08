package main

import (
	"strings"
	"testing"
)

// TestBuildWebStartCommand pins the composed in-daemon command: the login flags
// forwarded verbatim (they are how a session with no stored login gets one),
// and --control passed through.
func TestBuildWebStartCommand(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			"control, stored login",
			buildWebStartCommand(23299, "", "", "", true),
			"##rysh web start --port 23299 --control",
		},
		{
			"login passthrough",
			buildWebStartCommand(23232, "", "halil", "s3cret", false),
			"##rysh web start --port 23232 --username halil --password s3cret",
		},
		{
			"host passthrough",
			buildWebStartCommand(8080, "0.0.0.0", "halil", "s3cret", false),
			"##rysh web start --port 8080 --host 0.0.0.0 --username halil --password s3cret",
		},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestWebCmdURL: the printed URL must reflect the daemon's actual behaviour —
// control mode forces loopback regardless of --host. No credential rides the
// query string any more; the browser is asked to sign in.
func TestWebCmdURL(t *testing.T) {
	if got := webCmdURL(23299, "0.0.0.0", true); got != "http://127.0.0.1:23299/" {
		t.Fatalf("control-mode URL = %q — must show the loopback the daemon actually binds", got)
	}
	if got := webCmdURL(23232, "", false); got != "http://127.0.0.1:23232/" {
		t.Fatalf("default URL = %q", got)
	}
	if got := webCmdURL(8080, "192.168.1.5", false); !strings.HasPrefix(got, "http://192.168.1.5:8080/") {
		t.Fatalf("explicit host URL = %q", got)
	}
}

// firstFlagVal picks whichever alias the caller used, and returns "" when the
// flag is absent — the "use the stored login" signal.
func TestFirstFlagVal(t *testing.T) {
	args := []string{"--port", "8080", "--user", "halil"}
	if got := firstFlagVal(args, "--username", "--user"); got != "halil" {
		t.Fatalf("alias lookup = %q, want halil", got)
	}
	if got := firstFlagVal(args, "--password", "--pass"); got != "" {
		t.Fatalf("absent flag = %q, want empty", got)
	}
}
