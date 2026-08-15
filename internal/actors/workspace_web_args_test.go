// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"strings"
	"testing"
)

// TestParseWebStartArgs covers the `##rysh web start` parameters — bind
// address, port and the username/password login — across flag, bare-positional
// and config-default forms.
func TestParseWebStartArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		defHost string
		defPort int
		want    webStartOpts
		warns   int
	}{
		// --- defaults ---
		{
			name: "no args falls back to built-in port",
			want: webStartOpts{Host: "", Port: 23232},
		},
		{
			name: "config defaults apply", defHost: "127.0.0.1", defPort: 9000,
			want: webStartOpts{Host: "127.0.0.1", Port: 9000},
		},

		// --- bind address ---
		{
			name: "bind flag", args: "--bind 127.0.0.1",
			want: webStartOpts{Host: "127.0.0.1", Port: 23232, Explicit: true},
		},
		{
			name: "bind equals form", args: "--bind=0.0.0.0",
			want: webStartOpts{Host: "0.0.0.0", Port: 23232, Explicit: true},
		},
		{
			name: "host alias", args: "--host 192.168.1.10",
			want: webStartOpts{Host: "192.168.1.10", Port: 23232, Explicit: true},
		},
		{
			name: "bind with port sets both", args: "--bind 127.0.0.1:8080",
			want: webStartOpts{Host: "127.0.0.1", Port: 8080, Explicit: true},
		},
		{
			name: "bare host:port positional", args: "0.0.0.0:8080",
			want: webStartOpts{Host: "0.0.0.0", Port: 8080, Explicit: true},
		},
		{
			name: "bare host positional", args: "localhost",
			want: webStartOpts{Host: "localhost", Port: 23232, Explicit: true},
		},
		{
			name: "colon-port only sets port, keeps default host", args: "--bind :8080",
			want: webStartOpts{Host: "", Port: 8080, Explicit: true},
		},
		{
			name: "ipv6 bracketed with port", args: "--bind [::1]:8080",
			want: webStartOpts{Host: "::1", Port: 8080, Explicit: true},
		},
		{
			name: "ipv6 bare", args: "--bind ::1",
			want: webStartOpts{Host: "::1", Port: 23232, Explicit: true},
		},
		{
			name: "bind flag overrides config host", args: "--bind 0.0.0.0", defHost: "127.0.0.1",
			want: webStartOpts{Host: "0.0.0.0", Port: 23232, Explicit: true},
		},

		// --- port ---
		{
			name: "bare port stays backward compatible", args: "8080",
			want: webStartOpts{Host: "", Port: 8080, Explicit: true},
		},
		{
			name: "port flag", args: "--port 8080",
			want: webStartOpts{Host: "", Port: 8080, Explicit: true},
		},
		{
			name: "port equals form", args: "--port=8080",
			want: webStartOpts{Host: "", Port: 8080, Explicit: true},
		},
		{
			name: "explicit port after bind wins", args: "--bind 127.0.0.1:8080 --port 9999",
			want: webStartOpts{Host: "127.0.0.1", Port: 9999, Explicit: true},
		},

		// --- login ---
		{
			name: "username and password flags", args: "--username halil --password s3cret",
			want: webStartOpts{Host: "", Port: 23232, Username: "halil", Password: "s3cret"},
		},
		{
			name: "equals form", args: "--username=halil --password=s3cret",
			want: webStartOpts{Host: "", Port: 23232, Username: "halil", Password: "s3cret"},
		},
		{
			name: "short aliases", args: "--user halil --pass s3cret",
			want: webStartOpts{Host: "", Port: 23232, Username: "halil", Password: "s3cret"},
		},
		{
			name: "key=value form matches ##rysh web auth", args: "username=halil password=s3cret",
			want: webStartOpts{Host: "", Port: 23232, Username: "halil", Password: "s3cret"},
		},
		{
			name: "a password may contain anything but whitespace", args: "--password =p@ss=w0rd!",
			want: webStartOpts{Host: "", Port: 23232, Password: "=p@ss=w0rd!"},
		},

		// --- retired token flags: warned about by name, never applied ---
		{
			name: "token flag warns", args: "--token s3cret",
			want: webStartOpts{Host: "", Port: 23232}, warns: 1,
		},
		{
			name: "token equals form warns", args: "--token=s3cret",
			want: webStartOpts{Host: "", Port: 23232}, warns: 1,
		},
		{
			name: "no-token warns", args: "--no-token",
			want: webStartOpts{Host: "", Port: 23232}, warns: 1,
		},
		{
			name: "token value is not re-read as a bind address", args: "--token 0.0.0.0",
			want: webStartOpts{Host: "", Port: 23232}, warns: 1,
		},

		// --- everything together, in either order ---
		{
			name: "all flags", args: "--bind 127.0.0.1 --port 8080 --username halil --password s3cret",
			want: webStartOpts{Host: "127.0.0.1", Port: 8080, Username: "halil", Password: "s3cret", Explicit: true},
		},
		{
			name: "all flags, reordered", args: "--password s3cret --username halil --port 8080 --bind 127.0.0.1",
			want: webStartOpts{Host: "127.0.0.1", Port: 8080, Username: "halil", Password: "s3cret", Explicit: true},
		},
		{
			name: "positional bind plus positional port", args: "127.0.0.1 8080",
			want: webStartOpts{Host: "127.0.0.1", Port: 8080, Explicit: true},
		},

		// --- warnings (never fatal) ---
		{
			name: "unknown flag warns and is ignored", args: "--nope 8080",
			want: webStartOpts{Host: "", Port: 8080, Explicit: true}, warns: 1,
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
			name: "missing flag value warns", args: "--bind --username halil",
			want: webStartOpts{Host: "", Port: 23232, Username: "halil"}, warns: 1,
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
			got, warns := parseWebStartArgs(args, tc.defHost, tc.defPort)
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

// A login is required everywhere except control mode, which is the desktop
// app's own loopback-forced sidecar — demanding a password there would lock the
// app out of the daemon it just spawned.
func TestWebLoginRequired(t *testing.T) {
	if !webLoginRequired(false) {
		t.Error("a normal web start must require a login")
	}
	if webLoginRequired(true) {
		t.Error("control mode must not require a login")
	}
}

// --force is the opt-out for stopping the desktop app's own web server.
func TestHasWebForce(t *testing.T) {
	for _, args := range [][]string{{"--force"}, {"-f"}, {"--force", "extra"}} {
		if !hasWebForce(args) {
			t.Errorf("hasWebForce(%v) = false, want true", args)
		}
	}
	for _, args := range [][]string{nil, {}, {"--forced"}, {"force"}} {
		if hasWebForce(args) {
			t.Errorf("hasWebForce(%v) = true, want false", args)
		}
	}
}

// The tunnel and persistence flags parse in every form the rest of the command
// line accepts, and --ngrok-domain implies --ngrok (naming a domain to publish
// at and not publishing would be a silent no-op).
func TestParseWebStartArgsTunnelFlags(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantNgrok  bool
		wantDomain string
		wantNoSave bool
	}{
		{"plain", []string{"--ngrok"}, true, "", false},
		{"publish alias", []string{"--publish"}, true, "", false},
		{"domain space", []string{"--ngrok-domain", "rysh.ngrok.app"}, true, "rysh.ngrok.app", false},
		{"domain equals", []string{"--ngrok-domain=rysh.ngrok.app"}, true, "rysh.ngrok.app", false},
		{"domain alias", []string{"--domain", "rysh.ngrok.app"}, true, "rysh.ngrok.app", false},
		{"no-save", []string{"--no-save"}, false, "", true},
		{"once alias", []string{"--once"}, false, "", true},
		{"none", []string{"--port", "23001"}, false, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts, warnings := parseWebStartArgs(tc.args, "", 0)
			if len(warnings) != 0 {
				t.Fatalf("warnings = %v, want none", warnings)
			}
			if opts.Ngrok != tc.wantNgrok {
				t.Errorf("Ngrok = %v, want %v", opts.Ngrok, tc.wantNgrok)
			}
			if opts.NgrokDomain != tc.wantDomain {
				t.Errorf("NgrokDomain = %q, want %q", opts.NgrokDomain, tc.wantDomain)
			}
			if opts.NoSave != tc.wantNoSave {
				t.Errorf("NoSave = %v, want %v", opts.NoSave, tc.wantNoSave)
			}
		})
	}
}

// The full command the feature exists for, parsed as one line.
func TestParseWebStartArgsFullPublishedStart(t *testing.T) {
	opts, warnings := parseWebStartArgs([]string{
		"--bind", "0.0.0.0", "--port", "23001",
		"--username", "alice", "--password", "s3cret pass",
		"--ngrok-domain", "rysh-web.ngrok.app",
	}, "127.0.0.1", 23232)
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if opts.Host != "0.0.0.0" || opts.Port != 23001 || !opts.Explicit {
		t.Fatalf("address = %s:%d explicit=%v", opts.Host, opts.Port, opts.Explicit)
	}
	if opts.Username != "alice" || opts.Password != "s3cret pass" {
		t.Fatalf("login = %q/%q", opts.Username, opts.Password)
	}
	if !opts.Ngrok || opts.NgrokDomain != "rysh-web.ngrok.app" {
		t.Fatalf("tunnel = %v/%q", opts.Ngrok, opts.NgrokDomain)
	}
	if opts.NoSave {
		t.Fatal("NoSave set without --no-save — this start must be persisted")
	}
}
