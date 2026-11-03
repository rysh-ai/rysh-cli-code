// SPDX-License-Identifier: Apache-2.0

package channels

// gap05 (Teams level E), root ruling 2026-10-28: the Bot Framework token and
// OpenID endpoints may be redirected ONLY as a test seam — never from config
// or the environment, only to a loopback host, and any seam error rejects the
// token. These tests pin each of those properties.

import (
	"context"
	"encoding/base64"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestTeamsAuthSeamRefusesNonLoopback(t *testing.T) {
	const ok = "http://127.0.0.1:1/x"
	for _, bad := range []string{
		"",
		"https://login.botframework.com/v1/.well-known/openidconfiguration",
		"https://login.example.com/x",
		"http://localhost:1/x", // a name, not a loopback literal: /etc/hosts can move it
		"http://127.0.0.1.nip.io:1/x",
		"http://127.0.0.1@evil.example/x",
		"http://evil.example@127.0.0.1:1/x", // userinfo is refused outright
		"http://10.0.0.1/x",
		"http://[::ffff:8.8.8.8]/x",
		"ftp://127.0.0.1/x",
		"127.0.0.1:1/x",
	} {
		for _, pair := range [][2]string{{bad, ok}, {ok, bad}} {
			restore, err := SetTeamsAuthEndpointsForTest(pair[0], pair[1])
			if err == nil {
				restore()
				t.Errorf("SetTeamsAuthEndpointsForTest(%q, %q) accepted", pair[0], pair[1])
			}
			a := newTeamsAuth("app", "secret", "", http.DefaultClient)
			if a.openIDConfigURL != teamsOpenIDConfigURL || !strings.HasPrefix(a.tokenURL, teamsLoginBase+"/") {
				t.Fatalf("a refused override still changed the endpoints: %q %q", a.tokenURL, a.openIDConfigURL)
			}
		}
	}
	for _, good := range []string{"http://127.0.0.1:1/x", "https://127.0.0.1:1/x", "http://[::1]:1/x"} {
		restore, err := SetTeamsAuthEndpointsForTest(good, good)
		if err != nil {
			t.Errorf("loopback %q refused: %v", good, err)
			continue
		}
		if a := newTeamsAuth("app", "secret", "", http.DefaultClient); a.openIDConfigURL != good || a.tokenURL != good {
			t.Errorf("override not applied: %q %q", a.tokenURL, a.openIDConfigURL)
		}
		restore()
		if a := newTeamsAuth("app", "secret", "", http.DefaultClient); a.openIDConfigURL != teamsOpenIDConfigURL {
			t.Errorf("restore left the override in place: %q", a.openIDConfigURL)
		}
	}
}

// TestTeamsAuthEndpointsIgnoreConfigAndEnv: no ChannelConfig field and no
// environment variable moves the endpoints. Every string field of the config
// is set to a loopback URL, and plausible env names are set too.
func TestTeamsAuthEndpointsIgnoreConfigAndEnv(t *testing.T) {
	const fake = "http://127.0.0.1:1/x"
	for _, name := range []string{"RYSH_TEAMS_OPENID_URL", "RYSH_TEAMS_TOKEN_URL", "TEAMS_OPENID_CONFIG_URL",
		"TEAMS_LOGIN_BASE", "BOTFRAMEWORK_OPENID_URL", "RYSH_TEAMS_AUTH_BASE"} {
		t.Setenv(name, fake)
	}
	var cc msg.ChannelConfig
	v := reflect.ValueOf(&cc).Elem()
	for i := 0; i < v.NumField(); i++ {
		if f := v.Field(i); f.Kind() == reflect.String && f.CanSet() {
			f.SetString(fake)
		}
	}
	cc.AppID, cc.AppSecret, cc.TenantID, cc.WebhookPort = "app", "secret", "", "0"
	a := NewTeamsAdapter(cc)
	if a.auth.openIDConfigURL != teamsOpenIDConfigURL {
		t.Errorf("config/env moved the OpenID endpoint to %q", a.auth.openIDConfigURL)
	}
	if !strings.HasPrefix(a.auth.tokenURL, teamsLoginBase+"/") {
		t.Errorf("config/env moved the token endpoint to %q", a.auth.tokenURL)
	}
}

// failTransport fails the test if any request is made.
type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("a request was made to %s", r.URL.Host)
	return nil, http.ErrHandlerTimeout
}

// TestTeamsAuthFailsClosedOnBadEndpoint: an endpoint that is neither the
// production one nor loopback (however it got there) is refused before any
// request, so the token is rejected — and a loopback metadata document may
// not point the JWKS anywhere else.
func TestTeamsAuthFailsClosedOnBadEndpoint(t *testing.T) {
	client := &http.Client{Transport: failTransport{t}}
	a := newTeamsAuth("app", "secret", "", client)
	a.openIDConfigURL = "https://evil.example/openid"
	a.tokenURL = "https://evil.example/token"
	// A well-formed RS256 header, so verification reaches the key fetch.
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"k1"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"https://api.botframework.com","aud":"app"}`))
	if err := a.verifyActivityToken(context.Background(), "Bearer "+hdr+"."+body+".c2ln", ""); err == nil {
		t.Error("a token was accepted with a non-loopback OpenID endpoint")
	}
	if err := a.refreshKeys(context.Background()); err == nil {
		t.Error("keys were fetched from a non-loopback OpenID endpoint")
	}
	if _, err := a.accessToken(context.Background()); err == nil {
		t.Error("a token was requested from a non-loopback token endpoint")
	}
	if err := checkTeamsAuthEndpoint("https://evil.example/jwks"); err == nil {
		t.Error("a JWKS URI off loopback was accepted under the seam")
	}
	if err := checkTeamsAuthEndpoint(teamsOpenIDConfigURL); err != nil {
		t.Errorf("the production endpoint was refused: %v", err)
	}
}

// TestTeamsAuthSeamHasNoProductionCaller: the setter is called only from
// _test.go files anywhere in the module.
func TestTeamsAuthSeamHasNoProductionCaller(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root not found at %s", root)
	}
	scanned := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil // not ours to judge; the build would fail first
		}
		scanned++
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			}
			if name == "SetTeamsAuthEndpointsForTest" {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("non-test caller of SetTeamsAuthEndpointsForTest: %s", rel)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d non-test Go files; the walk is not covering the module", scanned)
	}
}

// metaTransport answers the production OpenID metadata with a jwks_uri on
// loopback, and fails the test on any other request.
type metaTransport struct{ t *testing.T }

func (m metaTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.String() == teamsOpenIDConfigURL {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"jwks_uri":"http://127.0.0.1:1/keys"}`)), Request: r}, nil
	}
	m.t.Errorf("a request was made to %s", r.URL.Host)
	return nil, http.ErrHandlerTimeout
}

// TestTeamsAuthLoopbackOnlyWhileSeamSet (e20 review): with the seam unset,
// production never talks to loopback — not a token URL, not an OpenID URL,
// and not a jwks_uri the metadata names — so a local process cannot serve
// signing keys to a released build.
func TestTeamsAuthLoopbackOnlyWhileSeamSet(t *testing.T) {
	const lo = "http://127.0.0.1:1/x"
	if err := checkTeamsAuthEndpoint(lo); err == nil {
		t.Error("loopback accepted with the seam unset")
	}
	a := newTeamsAuth("app", "secret", "", &http.Client{Transport: failTransport{t}})
	a.tokenURL, a.openIDConfigURL = lo, lo
	if _, err := a.accessToken(context.Background()); err == nil {
		t.Error("token requested from loopback with the seam unset")
	}
	if err := a.refreshKeys(context.Background()); err == nil {
		t.Error("OpenID metadata fetched from loopback with the seam unset")
	}
	b := newTeamsAuth("app", "secret", "", &http.Client{Transport: metaTransport{t}})
	if err := b.refreshKeys(context.Background()); err == nil {
		t.Error("JWKS fetched from a loopback jwks_uri with the seam unset")
	}
	restore, err := SetTeamsAuthEndpointsForTest(lo, lo)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if err := checkTeamsAuthEndpoint(lo); err != nil {
		t.Errorf("loopback refused while the seam is set: %v", err)
	}
}

// allowTeamsLoopbackSeam turns the seam on for this test, so the auth helper
// may talk to the test's own loopback servers (which the test then assigns to
// the auth fields directly); it is turned off again when the test ends.
func allowTeamsLoopbackSeam(t *testing.T) {
	t.Helper()
	restore, err := SetTeamsAuthEndpointsForTest("http://127.0.0.1:1/token", "http://127.0.0.1:1/openid")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
}
