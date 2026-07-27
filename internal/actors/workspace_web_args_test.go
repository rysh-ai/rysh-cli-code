package actors

import (
	"strings"
	"testing"
)

// TestParseWebStartArgs covers the three `##rysh web start` parameters — bind
// address, port and token — across flag, bare-positional and config-default
// forms.
func TestParseWebStartArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     string
		defHost  string
		defPort  int
		defToken string
		want     webStartOpts
		warns    int
	}{
		// --- defaults ---
		{
			name: "no args falls back to built-in port",
			want: webStartOpts{Host: "", Port: 23232},
		},
		{
			name: "config defaults apply", defHost: "127.0.0.1", defPort: 9000, defToken: "cfg-tok",
			want: webStartOpts{Host: "127.0.0.1", Port: 9000, Token: "cfg-tok"},
		},

		// --- bind address ---
		{
			name: "bind flag", args: "--bind 127.0.0.1",
			want: webStartOpts{Host: "127.0.0.1", Port: 23232},
		},
		{
			name: "bind equals form", args: "--bind=0.0.0.0",
			want: webStartOpts{Host: "0.0.0.0", Port: 23232},
		},
		{
			name: "host alias", args: "--host 192.168.1.10",
			want: webStartOpts{Host: "192.168.1.10", Port: 23232},
		},
		{
			name: "bind with port sets both", args: "--bind 127.0.0.1:8080",
			want: webStartOpts{Host: "127.0.0.1", Port: 8080},
		},
		{
			name: "bare host:port positional", args: "0.0.0.0:8080",
			want: webStartOpts{Host: "0.0.0.0", Port: 8080},
		},
		{
			name: "bare host positional", args: "localhost",
			want: webStartOpts{Host: "localhost", Port: 23232},
		},
		{
			name: "colon-port only sets port, keeps default host", args: "--bind :8080",
			want: webStartOpts{Host: "", Port: 8080},
		},
		{
			name: "ipv6 bracketed with port", args: "--bind [::1]:8080",
			want: webStartOpts{Host: "::1", Port: 8080},
		},
		{
			name: "ipv6 bare", args: "--bind ::1",
			want: webStartOpts{Host: "::1", Port: 23232},
		},
		{
			name: "bind flag overrides config host", args: "--bind 0.0.0.0", defHost: "127.0.0.1",
			want: webStartOpts{Host: "0.0.0.0", Port: 23232},
		},

		// --- port ---
		{
			name: "bare port stays backward compatible", args: "8080",
			want: webStartOpts{Host: "", Port: 8080},
		},
		{
			name: "port flag", args: "--port 8080",
			want: webStartOpts{Host: "", Port: 8080},
		},
		{
			name: "port equals form", args: "--port=8080",
			want: webStartOpts{Host: "", Port: 8080},
		},
		{
			name: "explicit port after bind wins", args: "--bind 127.0.0.1:8080 --port 9999",
			want: webStartOpts{Host: "127.0.0.1", Port: 9999},
		},

		// --- token ---
		{
			name: "token flag", args: "--token s3cret",
			want: webStartOpts{Host: "", Port: 23232, Token: "s3cret"},
		},
		{
			name: "token equals form", args: "--token=s3cret",
			want: webStartOpts{Host: "", Port: 23232, Token: "s3cret"},
		},
		{
			name: "no-token clears config token", args: "--no-token", defToken: "cfg-tok",
			want: webStartOpts{Host: "", Port: 23232, Token: "", NoToken: true},
		},
		{
			name: "token flag overrides config token", args: "--token cli", defToken: "cfg",
			want: webStartOpts{Host: "", Port: 23232, Token: "cli"},
		},
		{
			name: "auth flag is a no-op", args: "--auth",
			want: webStartOpts{Host: "", Port: 23232},
		},

		// --- all three together, in either order ---
		{
			name: "all three flags", args: "--bind 127.0.0.1 --port 8080 --token s3cret",
			want: webStartOpts{Host: "127.0.0.1", Port: 8080, Token: "s3cret"},
		},
		{
			name: "all three, reordered", args: "--token s3cret --port 8080 --bind 127.0.0.1",
			want: webStartOpts{Host: "127.0.0.1", Port: 8080, Token: "s3cret"},
		},
		{
			name: "positional bind plus positional port", args: "127.0.0.1 8080",
			want: webStartOpts{Host: "127.0.0.1", Port: 8080},
		},

		// --- warnings (never fatal) ---
		{
			name: "unknown flag warns and is ignored", args: "--nope 8080",
			want: webStartOpts{Host: "", Port: 8080}, warns: 1,
		},
		{
			name: "out-of-range port warns and keeps default", args: "70000",
			want: webStartOpts{Host: "", Port: 23232}, warns: 1,
		},
		{
			name: "bad bind port warns and keeps defaults", args: "--bind 127.0.0.1:abc",
			want: webStartOpts{Host: "", Port: 23232}, warns: 1,
		},
		{
			name: "missing flag value warns", args: "--bind --token t",
			want: webStartOpts{Host: "", Port: 23232, Token: "t"}, warns: 1,
		},
		{
			name: "garbage host warns", args: "--bind foo/bar",
			want: webStartOpts{Host: "", Port: 23232}, warns: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var args []string
			if tc.args != "" {
				args = strings.Fields(tc.args)
			}
			got, warns := parseWebStartArgs(args, tc.defHost, tc.defPort, tc.defToken)
			if got != tc.want {
				t.Fatalf("parseWebStartArgs(%q) = %+v, want %+v", tc.args, got, tc.want)
			}
			if len(warns) != tc.warns {
				t.Fatalf("parseWebStartArgs(%q) warnings = %d %v, want %d", tc.args, len(warns), warns, tc.warns)
			}
		})
	}
}

// TestSplitBindSpec covers the address forms accepted by --bind.
func TestSplitBindSpec(t *testing.T) {
	cases := []struct {
		spec    string
		host    string
		port    int
		wantErr bool
	}{
		{spec: "", host: "", port: 0},
		{spec: "127.0.0.1", host: "127.0.0.1"},
		{spec: "0.0.0.0", host: "0.0.0.0"},
		{spec: "localhost", host: "localhost"},
		{spec: "::1", host: "::1"},
		{spec: "[::1]", host: "::1"},
		{spec: "::", host: "::"},
		{spec: "127.0.0.1:8080", host: "127.0.0.1", port: 8080},
		{spec: ":8080", host: "", port: 8080},
		{spec: "[::1]:8080", host: "::1", port: 8080},
		{spec: "rysh.internal:8080", host: "rysh.internal", port: 8080},
		{spec: "127.0.0.1:abc", wantErr: true},
		{spec: "127.0.0.1:99999", wantErr: true},
		{spec: "foo/bar", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			host, port, err := splitBindSpec(tc.spec)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("splitBindSpec(%q) = (%q, %d, nil), want error", tc.spec, host, port)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitBindSpec(%q) unexpected error: %v", tc.spec, err)
			}
			if host != tc.host || port != tc.port {
				t.Fatalf("splitBindSpec(%q) = (%q, %d), want (%q, %d)", tc.spec, host, port, tc.host, tc.port)
			}
		})
	}
}

// TestWebURLAndLabel covers the display helpers: a wildcard bind renders as a
// clickable localhost URL while status output still names the real bind, and
// the default (no bind given) reads as loopback rather than all interfaces.
func TestWebURLAndLabel(t *testing.T) {
	urlCases := map[string]string{
		"":            "127.0.0.1",
		"0.0.0.0":     "localhost",
		"::":          "localhost",
		"127.0.0.1":   "127.0.0.1",
		"localhost":   "localhost",
		"::1":         "[::1]",
		"192.168.1.9": "192.168.1.9",
	}
	for host, want := range urlCases {
		if got := webURLHost(host); got != want {
			t.Errorf("webURLHost(%q) = %q, want %q", host, got, want)
		}
	}

	labelCases := map[string]string{
		"":            "127.0.0.1 (loopback — this machine only)",
		"127.0.0.1":   "127.0.0.1 (loopback — this machine only)",
		"::1":         "::1 (loopback — this machine only)",
		"0.0.0.0":     "0.0.0.0 (all interfaces)",
		"::":          ":: (all interfaces)",
		"192.168.1.9": "192.168.1.9",
	}
	for host, want := range labelCases {
		if got := webBindLabel(host); got != want {
			t.Errorf("webBindLabel(%q) = %q, want %q", host, got, want)
		}
	}

	loopbackCases := map[string]bool{
		"":            true, // default bind is loopback
		"127.0.0.1":   true,
		"127.0.100.1": true,
		"::1":         true,
		"[::1]":       true,
		"localhost":   true,
		"0.0.0.0":     false,
		"::":          false,
		"192.168.1.9": false,
	}
	for host, want := range loopbackCases {
		if got := webBindIsLoopback(host); got != want {
			t.Errorf("webBindIsLoopback(%q) = %v, want %v", host, got, want)
		}
	}

	if got := webBaseURL("::1", 8080); got != "http://[::1]:8080" {
		t.Errorf("webBaseURL ipv6 = %q", got)
	}
	if got := webBaseURL("", 23232); got != "http://127.0.0.1:23232" {
		t.Errorf("webBaseURL default = %q", got)
	}
	if got := webBaseURL("0.0.0.0", 23232); got != "http://localhost:23232" {
		t.Errorf("webBaseURL wildcard = %q", got)
	}
}

// Auto-start token resolution: configured token wins; no token → generated
// (never an unprotected auto-started UI); control mode (Electron sidecar,
// loopback-forced) stays token-less.
func TestAutoStartWebToken(t *testing.T) {
	if got := autoStartWebToken("s3cret", false); got != "s3cret" {
		t.Errorf("configured token: got %q, want s3cret", got)
	}
	if got := autoStartWebToken("s3cret", true); got != "s3cret" {
		t.Errorf("configured token in control mode: got %q, want s3cret", got)
	}
	if got := autoStartWebToken("", true); got != "" {
		t.Errorf("control mode without token: got %q, want empty (sidecar connects token-less)", got)
	}
	gen := autoStartWebToken("", false)
	if gen == "" {
		t.Fatal("no token + no control: expected a generated token, got empty")
	}
	if gen2 := autoStartWebToken("", false); gen2 == gen {
		t.Errorf("generated tokens should be random, got %q twice", gen)
	}
	if got := autoStartWebToken("  \t ", false); got == "" || strings.TrimSpace(got) != got {
		t.Errorf("whitespace-only config should generate a clean token, got %q", got)
	}
}
