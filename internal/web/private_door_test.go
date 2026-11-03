// SPDX-License-Identifier: Apache-2.0

package web

// The private door is open to a client presenting nothing in exactly one case:
// control mode (the desktop app's own daemon) on a loopback-bound listener, for
// a request that did not come through a proxy. Every other way to reach it —
// no login configured, the login removed or its file deleted while running, a
// forwarded request, a listener bound off loopback — demands the login.

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// doorRouter is router() with the door posture set explicitly: control mode
// and whether the private listener is bound to loopback.
func doorRouter(creds *Credentials, control, loopback bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	s := &Server{creds: creds, control: control, privateLoopback: loopback,
		privatePort: doorPort, controlSecret: handoverSecret()}
	r := gin.New()
	r.Use(s.authMiddleware())
	s.registerAuthAPI(r)
	r.GET("/", func(c *gin.Context) { c.String(http.StatusOK, "index") })
	r.GET("/health", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/ws", func(c *gin.Context) { c.String(http.StatusOK, "ws") })
	r.GET("/api/env", func(c *gin.Context) { c.String(http.StatusOK, "env") })
	return r
}

// doorPort is doorRouter's private port; doWith sends a loopback Host for it.
const doorPort = 23232

// doWith sends the run's control secret, as the desktop app does, unless
// header maps ControlSecretHeader to "" (no secret).
func doWith(r *gin.Engine, target string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = fmt.Sprintf("127.0.0.1:%d", doorPort)
	req.Header = controlSecretHeaders()
	for k, v := range header {
		if v == "" && k == ControlSecretHeader {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// No login and not control mode: session data is closed, and the 401 says how
// to open it. The app shell and /health stay public as on any gated door.
func TestPrivateDoor_NoLoginOutsideControlModeIsClosed(t *testing.T) {
	r := doorRouter(nil, false, true)
	for _, p := range []string{"/ws", "/api/env"} {
		w := doWith(r, p, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("no login, not control: %s = %d, want 401", p, w.Code)
		}
		if !strings.Contains(w.Body.String(), "no web login is configured") {
			t.Errorf("%s 401 body = %q, want the set-a-login hint", p, w.Body.String())
		}
	}
	for _, p := range []string{"/", "/health"} {
		if w := doWith(r, p, nil); w.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200 (public)", p, w.Code)
		}
	}
}

// Control mode on a loopback listener, with the per-launch secret, is the
// desktop app's posture: open. Without the secret it is not.
func TestPrivateDoor_ControlModeOnLoopbackIsOpen(t *testing.T) {
	for _, creds := range []*Credentials{nil, testCreds(t)} {
		r := doorRouter(creds, true, true)
		for _, p := range []string{"/ws", "/api/env"} {
			if w := doWith(r, p, nil); w.Code != http.StatusOK {
				t.Fatalf("control+loopback+secret (login set: %v): %s = %d, want 200", creds != nil, p, w.Code)
			}
			if w := doWith(r, p, map[string]string{ControlSecretHeader: ""}); w.Code != http.StatusUnauthorized {
				t.Fatalf("control+loopback, no secret (login set: %v): %s = %d, want 401", creds != nil, p, w.Code)
			}
		}
	}
}

// Control mode is loopback-only: a private listener that is not bound to
// loopback gets no exemption, whatever control says.
func TestPrivateDoor_ControlModeOffLoopbackIsGated(t *testing.T) {
	for _, creds := range []*Credentials{nil, testCreds(t)} {
		r := doorRouter(creds, true, false)
		if w := doWith(r, "/api/env", nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("control but bound off loopback (login set: %v): /api/env = %d, want 401", creds != nil, w.Code)
		}
	}
}

// A forwarded request reached the private door through a proxy or tunnel: it
// is treated as shared, so it needs the login even in control mode.
func TestPrivateDoor_ForwardedRequestNeedsTheLogin(t *testing.T) {
	creds := testCreds(t)
	tok, err := signJWT(creds.SigningKey(), creds.Username, time.Now(), AccessTokenTTL)
	if err != nil {
		t.Fatal(err)
	}
	r := doorRouter(creds, true, true)
	for _, h := range []map[string]string{
		{"X-Forwarded-For": "203.0.113.7"},
		{"X-Real-IP": "203.0.113.7"},
		{"Forwarded": "for=203.0.113.7"},
	} {
		if w := doWith(r, "/api/env", h); w.Code != http.StatusUnauthorized {
			t.Errorf("control+loopback with %v and no login = %d, want 401", h, w.Code)
		}
		for k := range h { // present but empty still counts as forwarded
			req := httptest.NewRequest(http.MethodGet, "/api/env", nil)
			req.Header[http.CanonicalHeaderKey(k)] = []string{""}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("control+loopback with an empty %s header = %d, want 401", k, w.Code)
			}
		}
		h["Authorization"] = "Bearer " + tok
		if w := doWith(r, "/api/env", h); w.Code != http.StatusOK {
			t.Errorf("forwarded with a valid login = %d, want 200", w.Code)
		}
	}
}

// /api/auth/status must agree with the gate, or the UI skips the login form on
// a door that will then refuse everything.
func TestPrivateDoor_AuthStatusFollowsTheGate(t *testing.T) {
	cases := []struct {
		name     string
		r        *gin.Engine
		header   map[string]string
		required bool
	}{
		{"no login, not control", doorRouter(nil, false, true), nil, true},
		{"control, loopback", doorRouter(nil, true, true), nil, false},
		{"control, loopback, no secret", doorRouter(nil, true, true), map[string]string{ControlSecretHeader: ""}, true},
		{"control, off loopback", doorRouter(nil, true, false), nil, true},
		{"control, loopback, forwarded", doorRouter(testCreds(t), true, true), map[string]string{"X-Real-IP": "203.0.113.7"}, true},
	}
	for _, c := range cases {
		w := doWith(c.r, "/api/auth/status", c.header)
		want := fmt.Sprintf(`"login_required":%v`, c.required)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), want) {
			t.Errorf("%s: status = %d %s, want %s", c.name, w.Code, w.Body.String(), want)
		}
	}
}

// A running server outside control mode whose web-auth.json disappears must
// close, not open: the file going away is not permission to serve everyone.
func TestPrivateDoor_DeletedCredentialsFileClosesTheDoor(t *testing.T) {
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	dir := t.TempDir()
	creds, err := SaveCredentials(dir, "halil", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	s := NewServer(port, "door", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	s.SetControl(false)
	s.SetCredentials(creds)
	s.TrackCredentialsFile(dir)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	url := fmt.Sprintf("http://127.0.0.1:%d/api/env", port)
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/health", port))

	if code := get(t, url, ""); code != http.StatusUnauthorized {
		t.Fatalf("with a login, no token = %d, want 401", code)
	}
	if err := os.Remove(ProjectCredentials(dir).Path()); err != nil {
		t.Fatal(err)
	}
	if code := get(t, url, ""); code != http.StatusUnauthorized {
		t.Fatalf("after web-auth.json was deleted = %d, want 401 (never open)", code)
	}
}

// Failed logins are rate limited: past the limit even the right password waits
// (it must not become an oracle), and the limit lifts when the window passes.
func TestLogin_FailedAttemptsAreRateLimited(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	s := &Server{creds: testCreds(t)}
	s.loginLimit.now = func() time.Time { return now }
	r := gin.New()
	s.registerAuthAPI(r)
	login := func(pw string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"username":"halil","password":%q}`, pw)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	for i := 0; i < loginFailMax; i++ {
		if w := login("wrong"); w.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d = %d, want 401", i+1, w.Code)
		}
	}
	w := login("s3cret")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("right password past the limit = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Errorf("429 without Retry-After")
	}
	now = now.Add(loginFailWindow + time.Second)
	if w := login("s3cret"); w.Code != http.StatusOK {
		t.Fatalf("after the window = %d, want 200", w.Code)
	}
}
