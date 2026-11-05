// SPDX-License-Identifier: Apache-2.0

package web

// The login JWT outside the Authorization header (security-lead ledger 86):
// nginx Basic auth in front of /ryshweb needs Authorization for itself, so the
// web client sends the JWT as X-Rysh-Token, and on the WebSocket upgrade (a
// browser cannot set headers there) as a TokenSubprotocolPrefix subprotocol,
// which keeps it out of URLs. An Authorization: Basic header belongs to the
// proxy and is ignored, never an error.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// loginToken logs in on r and returns the issued JWT.
func loginToken(t *testing.T, r http.Handler) string {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"username":"halil","password":"s3cret"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("login = %d (%s)", w.Code, w.Body.String())
	}
	var reply struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil || reply.Token == "" {
		t.Fatalf("login reply %q: %v", w.Body.String(), err)
	}
	return reply.Token
}

func getWithHeaders(r http.Handler, target string, h map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range h {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func TestToken_XRyshTokenHeaderOpensDataRoutes(t *testing.T) {
	r, _ := loginRouter(t)
	tok := loginToken(t, r)
	if w := getWithHeaders(r, "/api/env", map[string]string{TokenHeader: tok}); w.Code != http.StatusOK {
		t.Fatalf("/api/env with %s = %d, want 200", TokenHeader, w.Code)
	}
	w := getWithHeaders(r, "/api/auth/status", map[string]string{TokenHeader: tok})
	var st struct {
		Authenticated bool `json:"authenticated"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &st)
	if w.Code != http.StatusOK || !st.Authenticated {
		t.Fatalf("/api/auth/status with %s = %d %s, want signed in", TokenHeader, w.Code, w.Body.String())
	}
	// A wrong or non-JWT value opens nothing.
	for _, bad := range []string{"s3kritToken", tok + "x"} {
		if w := getWithHeaders(r, "/api/env", map[string]string{TokenHeader: bad}); w.Code != http.StatusUnauthorized {
			t.Fatalf("/api/env with a bad %s = %d, want 401", TokenHeader, w.Code)
		}
	}
}

// The shape nginx Basic auth produces: Authorization carries the proxy's
// Basic credentials and the JWT rides in X-Rysh-Token.
func TestToken_BasicAuthorizationIsIgnored(t *testing.T) {
	r, _ := loginRouter(t)
	tok := loginToken(t, r)
	basic := basicAuth("ryshweb", "proxy-pass")
	if w := getWithHeaders(r, "/api/env", map[string]string{"Authorization": basic, TokenHeader: tok}); w.Code != http.StatusOK {
		t.Fatalf("/api/env with Basic + %s = %d, want 200", TokenHeader, w.Code)
	}
	// Basic alone is no login: the same opaque 401 as no header, not a 400/500.
	w := getWithHeaders(r, "/api/env", map[string]string{"Authorization": basic})
	if w.Code != http.StatusUnauthorized || w.Body.String() != "unauthorized" {
		t.Fatalf("/api/env with Basic only = %d %q, want 401 \"unauthorized\"", w.Code, w.Body.String())
	}
	// Public paths stay public with a Basic header on them.
	for _, p := range []string{"/health", "/", "/assets/app.js", "/api/auth/status"} {
		if w := getWithHeaders(r, p, map[string]string{"Authorization": basic}); w.Code != http.StatusOK {
			t.Fatalf("%s with Basic = %d, want 200", p, w.Code)
		}
	}
	// And login works behind the proxy: POST /api/auth/login with Basic on it.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"username":"halil","password":"s3cret"}`))
	req.Header.Set("Authorization", basic)
	lw := httptest.NewRecorder()
	r.ServeHTTP(lw, req)
	if lw.Code != http.StatusOK {
		t.Fatalf("login with Basic on the request = %d, want 200", lw.Code)
	}
}

// Through the middleware: the subprotocol entry authenticates /ws.
func TestToken_SubprotocolAuthenticatesWS(t *testing.T) {
	r, _ := loginRouter(t)
	tok := loginToken(t, r)
	proto := TokenSubprotocol + ", " + TokenSubprotocolPrefix + tok
	if w := getWithHeaders(r, "/ws?stream=1", map[string]string{"Sec-WebSocket-Protocol": proto}); w.Code != http.StatusOK {
		t.Fatalf("/ws with the token subprotocol = %d, want 200", w.Code)
	}
	bad := TokenSubprotocol + ", " + TokenSubprotocolPrefix + "s3kritToken"
	if w := getWithHeaders(r, "/ws?stream=1", map[string]string{"Sec-WebSocket-Protocol": bad}); w.Code != http.StatusUnauthorized {
		t.Fatalf("/ws with a non-JWT token subprotocol = %d, want 401", w.Code)
	}
}

// A real upgrade on a login server: the JWT as a subprotocol (no ?token=, no
// header) is upgraded, and the server answers the plain TokenSubprotocol,
// never the token itself.
func TestToken_RealUpgradeAnswersPlainSubprotocol(t *testing.T) {
	creds := testCreds(t)
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	port := freePort(t)
	s := NewServer(port, "token-subprotocol", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	s.SetCredentials(creds)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/health", port))

	tok, err := signJWT(creds.SigningKey(), "halil", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	conn, resp, err := dialCtlWS(port, nil, TokenSubprotocol, TokenSubprotocolPrefix+tok)
	if err != nil {
		t.Fatalf("/ws with the token subprotocol: %v (%d), want 101", err, statusOf(resp))
	}
	conn.Close()
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != TokenSubprotocol {
		t.Fatalf("server answered subprotocol %q, want %q", got, TokenSubprotocol)
	}
	// Without the token entry: refused before the upgrade.
	if conn, resp, err := dialCtlWS(port, nil, TokenSubprotocol); err == nil {
		conn.Close()
		t.Fatal("/ws without a token was upgraded")
	} else if statusOf(resp) != http.StatusUnauthorized {
		t.Fatalf("/ws without a token = %d, want 401", statusOf(resp))
	}
}

// S3 (security-lead): what the token subprotocol must refuse, before any
// upgrade.
func TestToken_SubprotocolRefusals(t *testing.T) {
	r, s := loginRouter(t)
	tok := loginToken(t, r)
	expired, err := signJWT(s.credentials().SigningKey(), "halil", time.Now().Add(-2*AccessTokenTTL), AccessTokenTTL)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"expired JWT":             TokenSubprotocol + ", " + TokenSubprotocolPrefix + expired,
		"bad JWT":                 TokenSubprotocol + ", " + TokenSubprotocolPrefix + tok + "x",
		"two prefixed entries":    TokenSubprotocol + ", " + TokenSubprotocolPrefix + tok + ", " + TokenSubprotocolPrefix + tok,
		"two entries, one bad":    TokenSubprotocol + ", " + TokenSubprotocolPrefix + "s3kritToken, " + TokenSubprotocolPrefix + tok,
		"plain name, no token":    TokenSubprotocol,
		"empty prefixed entry":    TokenSubprotocol + ", " + TokenSubprotocolPrefix,
		"token as the plain name": tok,
	}
	for name, proto := range cases {
		w := getWithHeaders(r, "/ws?stream=1", map[string]string{"Sec-WebSocket-Protocol": proto})
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: /ws = %d, want 401", name, w.Code)
		}
		// S1: nothing of what was offered comes back.
		if strings.Contains(w.Body.String(), "rysh-auth") || strings.Contains(w.Body.String(), tok) ||
			w.Header().Get("Sec-WebSocket-Protocol") != "" {
			t.Errorf("%s: the refusal echoes the offered subprotocol (%q, %q)", name, w.Body.String(), w.Header().Get("Sec-WebSocket-Protocol"))
		}
	}
	// Two split across header lines count the same as two in one line.
	req := httptest.NewRequest(http.MethodGet, "/ws?stream=1", nil)
	req.Header.Add("Sec-WebSocket-Protocol", TokenSubprotocol+", "+TokenSubprotocolPrefix+tok)
	req.Header.Add("Sec-WebSocket-Protocol", TokenSubprotocolPrefix+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("two prefixed entries on two header lines: /ws = %d, want 401", w.Code)
	}
	// Two X-Rysh-Token values are refused the same way.
	req = httptest.NewRequest(http.MethodGet, "/api/env", nil)
	req.Header.Add(TokenHeader, tok)
	req.Header.Add(TokenHeader, tok)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("two %s values: /api/env = %d, want 401", TokenHeader, w.Code)
	}
}

// S2: the client that sends the token subprotocol / TokenHeader shipped in
// v0.2.14, so a valid JWT in ?token= no longer opens anything.
func TestToken_QueryTokenIsRefused(t *testing.T) {
	r, _ := loginRouter(t)
	tok := loginToken(t, r)
	for _, path := range []string{"/ws?stream=1&token=" + tok, "/api/env?token=" + tok} {
		if w := getWithHeaders(r, path, nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("%s with a valid JWT in ?token= = %d, want 401", strings.SplitN(path, "?", 2)[0], w.Code)
		}
	}
}

// The control path is unchanged: a control client still gets rysh-control.
func TestToken_ControlSubprotocolUnchanged(t *testing.T) {
	secret := mustSecret(t)
	_, port := startSecretServer(t, secret)
	conn, resp, err := dialCtlWS(port, nil, ControlSubprotocol, ControlSecretSubprotocolPrefix+secret)
	if err != nil {
		t.Fatalf("control /ws: %v (%d)", err, statusOf(resp))
	}
	conn.Close()
	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != ControlSubprotocol {
		t.Fatalf("control /ws answered %q, want %q", got, ControlSubprotocol)
	}
}
