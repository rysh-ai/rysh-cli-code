// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// S2.5 (plan 0.2.16): `connect --file <path>` claims with the connect file the
// company page offers (rysh-connect-<slug>.ryshconnect; format agreed with
// desktop's app reader and founder-tools' S2.2 endpoint). The file carries a
// server and a one-time code and never a key: a file with anything else in it
// is refused before anything is sent, and the file is removed only after a
// good claim.

const connectFileCode = "rysh_ct_0123456789abcdef0123456789abcdef0123456789abcdef"

type connectFileServer struct {
	*httptest.Server
	claims []map[string]any
}

func newConnectFileServer(t *testing.T, accept bool) *connectFileServer {
	t.Helper()
	s := &connectFileServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		s.claims = append(s.claims, body)
		if r.URL.Path != "/api/company-connect/claim" || !accept || body["token"] != connectFileCode {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"connect code is invalid, expired or already used"}`))
			return
		}
		_, _ = w.Write([]byte(`{"server_url":"https://letchat.ai","api_key":"rysh_key","workspace_id":"11111111-1111-1111-1111-111111111111",
			"company":{"id":"22222222-2222-2222-2222-222222222222","workspace_id":"11111111-1111-1111-1111-111111111111","name":"Modern Bistro","slug":"firat","fqdn":"firat.dns.rysh.ai","status":"active","template_id":"restaurant"}}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func connectFileBody(server string, expires time.Time) string {
	return fmt.Sprintf(`{"rysh_connect":1,"server":%q,"code":%q,"company":{"name":"Modern Bistro","slug":"firat"},"expires_at":%q}`,
		server, connectFileCode, expires.UTC().Format(time.RFC3339))
}

func writeConnectFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rysh-connect-firat.ryshconnect")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runConnectFile(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Setenv("RYSH_SESSION", "")
	t.Setenv("RYSH_UPSTREAM_API_KEY", "")
	t.Setenv("RYSH_DIR", "")
	dir := filepath.Join(t.TempDir(), "firat")
	var err error
	out := captureStdout(t, func() {
		err = runConnect(config.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)),
			append(args, "--json", "--device", "test-box", "--dir", dir))
	})
	return out, err
}

func TestConnectFileClaimsAndRemovesTheFile(t *testing.T) {
	srv := newConnectFileServer(t, true)
	p := writeConnectFile(t, connectFileBody(srv.URL, time.Now().Add(20*time.Minute)))

	out, err := runConnectFile(t, "--file", p)
	if err != nil {
		t.Fatalf("connect --file: %v", err)
	}
	if len(srv.claims) != 1 {
		t.Fatalf("connect --file sent %d claim(s), want 1", len(srv.claims))
	}
	c := srv.claims[0]
	if len(c) != 3 || c["token"] != connectFileCode || c["vehicle"] != "terminal" || c["device"] != "test-box" {
		t.Fatalf("claim body must be exactly token, vehicle, device: %v", c)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("the connect file is still there after a good claim (stat err %v)", err)
	}
	if strings.Contains(out, "rysh_key") {
		t.Fatalf("--json output carries the key:\n%s", out)
	}

	// The desktop app names its vehicle the same way it does for a link.
	srv2 := newConnectFileServer(t, true)
	p2 := writeConnectFile(t, connectFileBody(srv2.URL, time.Now().Add(20*time.Minute)))
	if _, err := runConnectFile(t, "--file", p2, "--vehicle", "desktop"); err != nil {
		t.Fatalf("connect --file --vehicle desktop: %v", err)
	}
	if len(srv2.claims) != 1 || srv2.claims[0]["vehicle"] != "desktop" {
		t.Fatalf("--vehicle desktop with --file sent %v", srv2.claims)
	}
}

func TestConnectFileKeptWhenTheClaimFails(t *testing.T) {
	srv := newConnectFileServer(t, false)
	p := writeConnectFile(t, connectFileBody(srv.URL, time.Now().Add(20*time.Minute)))

	_, err := runConnectFile(t, "--file", p)
	if err == nil || !strings.Contains(err.Error(), "invalid, expired or already used") {
		t.Fatalf("a refused claim must relay the server's message, got %v", err)
	}
	if len(srv.claims) != 1 {
		t.Fatalf("sent %d claim(s), want 1", len(srv.claims))
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("the connect file was removed after a failed claim: %v", err)
	}
}

func TestConnectFileRefusesAnythingButServerAndCode(t *testing.T) {
	srv := newConnectFileServer(t, true)
	soon := time.Now().Add(20 * time.Minute).UTC().Format(time.RFC3339)
	good := func(extra string) string {
		return fmt.Sprintf(`{"rysh_connect":1,"server":%q,"code":%q,"company":{"name":"Modern Bistro","slug":"firat"},"expires_at":%q%s}`,
			srv.URL, connectFileCode, soon, extra)
	}
	cases := map[string]string{
		"a key field":                good(`,"api_key":"rysh_abcdEFGH1234abcdEFGH1234abcdEFGH"`),
		"a key named token":          good(`,"token":"x"`),
		"a key nested in company":    fmt.Sprintf(`{"rysh_connect":1,"server":%q,"code":%q,"company":{"name":"Modern Bistro","slug":"firat","key":"x"},"expires_at":%q}`, srv.URL, connectFileCode, soon),
		"a workspace key as a value": fmt.Sprintf(`{"rysh_connect":1,"server":%q,"code":%q,"company":{"name":"rysh_abcdEFGH1234abcdEFGH1234abcdEFGH","slug":"firat"},"expires_at":%q}`, srv.URL, connectFileCode, soon),
		"a second code before":       fmt.Sprintf(`{"rysh_connect":1,"code":"rysh_abcdEFGH1234abcdEFGH1234abcdEFGH","server":%q,"code":%q,"expires_at":%q}`, srv.URL, connectFileCode, soon),
		"an unknown field":           good(`,"note":"hi"`),
		"version 2":                  strings.Replace(good(""), `"rysh_connect":1`, `"rysh_connect":2`, 1),
		"version as a string":        strings.Replace(good(""), `"rysh_connect":1`, `"rysh_connect":"1"`, 1),
		"no version":                 strings.Replace(good(""), `"rysh_connect":1,`, ``, 1),
		"a code of the wrong shape":  strings.Replace(good(""), connectFileCode, "rysh_ct_good", 1),
		"an upper-case code":         strings.Replace(good(""), connectFileCode, strings.ToUpper(connectFileCode[:8])+connectFileCode[8:], 1),
		"no expiry":                  strings.Replace(good(""), fmt.Sprintf(`,"expires_at":%q`, soon), ``, 1),
		"an expired code":            strings.Replace(good(""), soon, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), 1),
		"a server with a path":       strings.Replace(good(""), srv.URL, srv.URL+"/x", 1),
		"a server with a user":       strings.Replace(good(""), "http://", "http://u:p@", 1),
		"a server with a query":      strings.Replace(good(""), srv.URL, srv.URL+"?a=b", 1),
		"plain http off loopback":    strings.Replace(good(""), srv.URL, "http://letchat.ai", 1),
		"not an object":              `["rysh_connect",1]`,
		"not JSON":                   `rysh connect`,
		"trailing data":              good("") + `{"api_key":"x"}`,
		"too big":                    good(`,"pad":"` + strings.Repeat("a", 5000) + `"`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			before := len(srv.claims)
			p := writeConnectFile(t, body)
			_, err := runConnectFile(t, "--file", p)
			if err == nil || !strings.Contains(err.Error(), "connect file") {
				t.Fatalf("want a connect file refusal, got %v", err)
			}
			if len(srv.claims) != before {
				t.Fatalf("a refused file still sent a claim")
			}
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("a refused file was removed: %v", err)
			}
		})
	}

	t.Run("an old file", func(t *testing.T) {
		p := writeConnectFile(t, good(""))
		old := time.Now().Add(-31 * time.Minute)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		if _, err := runConnectFile(t, "--file", p); err == nil || !strings.Contains(err.Error(), "connect file") {
			t.Fatalf("a file older than 30 minutes was accepted: %v", err)
		}
		if len(srv.claims) != 0 {
			t.Fatalf("an old file sent a claim")
		}
	})
	t.Run("a symlink", func(t *testing.T) {
		target := writeConnectFile(t, good(""))
		p := filepath.Join(t.TempDir(), "rysh-connect-firat.ryshconnect")
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
		if _, err := runConnectFile(t, "--file", p); err == nil || !strings.Contains(err.Error(), "connect file") {
			t.Fatalf("a symlinked file was accepted: %v", err)
		}
	})
	t.Run("positional args beside --file", func(t *testing.T) {
		p := writeConnectFile(t, good(""))
		if _, err := runConnectFile(t, "--file", p, srv.URL, connectFileCode); err == nil || !strings.Contains(err.Error(), "--file") {
			t.Fatalf("--file with a server and code was accepted: %v", err)
		}
	})
	t.Run("--file without a path", func(t *testing.T) {
		if _, err := runConnectFile(t, "--file"); err == nil || !strings.Contains(err.Error(), "--file") {
			t.Fatalf("--file with no path was accepted: %v", err)
		}
	})
	if len(srv.claims) != 0 {
		t.Fatalf("refused files sent %d claim(s)", len(srv.claims))
	}
}

// The two writers of the file (rysh-server's S2.2 json.Marshal and the web
// page's JSON.stringify, 2026-11-09) differ in small ways the reader must take:
// Go escapes &<> in names as &…, JavaScript writes milliseconds in ISO
// times, either may end with a newline. A UTF-8 BOM is not valid JSON and is
// refused like any other non-JSON file.
func TestConnectFileAcceptsBothWritersVariants(t *testing.T) {
	soon := time.Now().Add(20 * time.Minute).UTC()
	cases := map[string]func(server string) string{
		"go json.Marshal escapes": func(server string) string {
			return fmt.Sprintf(`{"rysh_connect":1,"server":%q,"code":%q,"company":{"name":"Fish \u0026 Chips \u003cLtd\u003e","slug":"firat"},"expires_at":%q}`,
				server, connectFileCode, soon.Format(time.RFC3339))
		},
		"js milliseconds": func(server string) string {
			return fmt.Sprintf(`{"rysh_connect":1,"server":%q,"code":%q,"company":{"name":"Modern Bistro","slug":"firat"},"expires_at":%q}`,
				server, connectFileCode, soon.Format("2006-01-02T15:04:05.000Z"))
		},
		"trailing newline": func(server string) string {
			return connectFileBody(server, soon) + "\n"
		},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newConnectFileServer(t, true)
			p := writeConnectFile(t, body(srv.URL))
			if _, err := runConnectFile(t, "--file", p); err != nil {
				t.Fatalf("a %s file was refused: %v", name, err)
			}
			if len(srv.claims) != 1 {
				t.Fatalf("sent %d claim(s), want 1", len(srv.claims))
			}
		})
	}
	t.Run("a BOM", func(t *testing.T) {
		srv := newConnectFileServer(t, true)
		p := writeConnectFile(t, "\ufeff"+connectFileBody(srv.URL, soon))
		if _, err := runConnectFile(t, "--file", p); err == nil || !strings.Contains(err.Error(), "connect file") {
			t.Fatalf("a file with a BOM was accepted: %v", err)
		}
		if len(srv.claims) != 0 {
			t.Fatal("a refused file sent a claim")
		}
	})
}

// desktop (2026-11-09): a browser saves the download 0644, and that must be
// accepted; a file that another user can write (group/other-writable) could
// have been rewritten to name another server, so it is refused, as the app's
// reader does. Windows has no such mode bits.
func TestConnectFileModeRules(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix mode bits")
	}
	soon := time.Now().Add(20 * time.Minute)
	for mode, ok := range map[os.FileMode]bool{0o600: true, 0o644: true, 0o664: false, 0o646: false, 0o666: false} {
		t.Run(mode.String(), func(t *testing.T) {
			srv := newConnectFileServer(t, true)
			p := writeConnectFile(t, connectFileBody(srv.URL, soon))
			if err := os.Chmod(p, mode); err != nil {
				t.Fatal(err)
			}
			_, err := runConnectFile(t, "--file", p)
			if ok && err != nil {
				t.Fatalf("a %v file was refused: %v", mode, err)
			}
			if !ok && (err == nil || !strings.Contains(err.Error(), "connect file")) {
				t.Fatalf("a %v (writable by others) file was accepted: %v", mode, err)
			}
			if !ok && len(srv.claims) != 0 {
				t.Fatal("a refused file sent a claim")
			}
		})
	}
}
