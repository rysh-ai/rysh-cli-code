// SPDX-License-Identifier: Apache-2.0

package web

// Middleware tests for the login gate. The login endpoints themselves, and the
// JWT they issue, are covered in login_test.go; this file pins the two shapes
// the gate has — no credentials (control mode: everything open) and credentials
// (everything but /health and the login bundle closed).

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// router builds a gin engine with the auth middleware and a couple of stub
// routes, mirroring server.go's setup, for isolated middleware testing.
// creds nil ⇒ the ungated control-mode posture.
func router(creds *Credentials) *gin.Engine {
	gin.SetMode(gin.TestMode)
	s := &Server{creds: creds}
	r := gin.New()
	r.Use(s.authMiddleware())
	s.registerAuthAPI(r)
	r.GET("/", func(c *gin.Context) { c.String(http.StatusOK, "index") })
	r.GET("/assets/app.js", func(c *gin.Context) { c.String(http.StatusOK, "js") })
	r.GET("/mobile", func(c *gin.Context) { c.String(http.StatusOK, "mobile") })
	r.GET("/health", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/ws", func(c *gin.Context) { c.String(http.StatusOK, "ws") })
	r.GET("/api/env", func(c *gin.Context) { c.String(http.StatusOK, "env") })
	return r
}

func do(r *gin.Engine, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// testCreds returns credentials backed by a temp dir, for a gated router.
func testCreds(t *testing.T) *Credentials {
	t.Helper()
	creds, err := SaveCredentials(t.TempDir(), "halil", "s3cret")
	if err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	return creds
}

// No credentials is the control-mode posture: the desktop app spawns the daemon
// on loopback and is its only client, so nothing is gated.
func TestAuth_NoCredentials_AllowsEverything(t *testing.T) {
	r := router(nil)
	for _, p := range []string{"/", "/assets/app.js", "/ws", "/health", "/api/env"} {
		if w := do(r, http.MethodGet, p); w.Code != http.StatusOK {
			t.Fatalf("ungated %s = %d, want 200", p, w.Code)
		}
	}
}

// Anything carrying session data is closed without a login.
func TestAuth_Login_DeniesSessionData(t *testing.T) {
	r := router(testCreds(t))
	for _, p := range []string{"/ws", "/api/env"} {
		if w := do(r, http.MethodGet, p); w.Code != http.StatusUnauthorized {
			t.Fatalf("gated %s without a login = %d, want 401", p, w.Code)
		}
	}
}

// The retired access token must not open anything: a ?token= that is not a
// valid JWT is just a wrong credential now, and there is no cookie to set.
func TestAuth_Login_RejectsAccessTokenStyleQuery(t *testing.T) {
	r := router(testCreds(t))
	w := do(r, http.MethodGet, "/ws?token=secret")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("?token=secret on /ws = %d, want 401", w.Code)
	}
	if sc := w.Header().Get("Set-Cookie"); sc != "" {
		t.Fatalf("Set-Cookie = %q, want none — the token cookie is gone", sc)
	}
}

func TestAuth_HealthAlwaysPublic(t *testing.T) {
	r := router(testCreds(t))
	if w := do(r, http.MethodGet, "/health"); w.Code != http.StatusOK {
		t.Fatalf("/health gated = %d, want 200 (public)", w.Code)
	}
}

// The login form ships inside the app bundle, so the bundle cannot be behind
// the login it is there to collect.
func TestAuth_Login_AppShellStaysPublic(t *testing.T) {
	r := router(testCreds(t))
	for _, p := range []string{"/", "/assets/app.js", "/mobile", "/api/auth/status"} {
		if w := do(r, http.MethodGet, p); w.Code != http.StatusOK {
			t.Fatalf("app shell %s = %d, want 200", p, w.Code)
		}
	}
}
