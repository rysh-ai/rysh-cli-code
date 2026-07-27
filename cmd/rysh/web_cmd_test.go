package main

import (
	"strings"
	"testing"
)

// TestBuildWebStartCommand pins the composed in-daemon command: explicit
// client-side token by default (so the URL is printable for detached
// sessions), --no-token suppressing it, and --control passed through.
func TestBuildWebStartCommand(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			"token + control",
			buildWebStartCommand(23299, "", "tok123", true, false),
			"##rysh web start --port 23299 --token tok123 --control",
		},
		{
			"no-token wins over token",
			buildWebStartCommand(23232, "", "tok123", false, true),
			"##rysh web start --port 23232 --no-token",
		},
		{
			"host passthrough",
			buildWebStartCommand(8080, "0.0.0.0", "", false, true),
			"##rysh web start --port 8080 --host 0.0.0.0 --no-token",
		},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestWebCmdURL: the printed URL must reflect the daemon's actual behaviour —
// control mode forces loopback regardless of --host, and the token rides the
// query string.
func TestWebCmdURL(t *testing.T) {
	if got := webCmdURL(23299, "0.0.0.0", "tok", true); got != "http://127.0.0.1:23299/?token=tok" {
		t.Fatalf("control-mode URL = %q — must show the loopback the daemon actually binds", got)
	}
	if got := webCmdURL(23232, "", "", false); got != "http://127.0.0.1:23232/" {
		t.Fatalf("default URL = %q", got)
	}
	if got := webCmdURL(8080, "192.168.1.5", "t", false); !strings.HasPrefix(got, "http://192.168.1.5:8080/") {
		t.Fatalf("explicit host URL = %q", got)
	}
}
