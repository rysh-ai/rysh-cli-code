package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// The web UI has one way in: a USERNAME/PASSWORD LOGIN (SetCredentials, set via
// `##rysh web auth` or `##rysh web start --username/--password`).
// POST /api/auth/login returns a one-month JWT the browser keeps in
// localStorage and presents as `Authorization: Bearer …` (and as ?token= on the
// WebSocket upgrade, which cannot carry headers). When it expires the UI shows
// the login form again — there is no refresh token.
//
// There used to be a second door: a bearer access token pasted once as ?token=
// and thereafter carried by a cookie. It is gone. A secret in a URL survives in
// shell history, scrollback and browser history, never expires, and cannot be
// revoked short of restarting the server — everything the login was built to
// replace. `##rysh web start` now refuses to start without a login rather than
// falling back to one.
//
// With credentials configured the app shell (index.html, /assets) is served
// unauthenticated, because the login form is part of that bundle: it cannot ask
// for a password if the page holding it is behind the password. Everything with
// session data in it — /ws, /fs/*, /api/* — requires the login.
//
// No credentials at all means no gate, and that is reachable only in control
// mode: the desktop app spawns its own daemon, which forces a loopback bind and
// serves exactly one client — the app that started it.

// SetCredentials installs (or, with nil, removes) the username/password login.
// Safe to call while the server is running: `##rysh web auth` applies new
// credentials to a live server, and because saving credentials mints a fresh
// signing key, every token issued under the old password stops verifying.
func (s *Server) SetCredentials(c *Credentials) {
	s.credsMu.Lock()
	s.creds = c
	s.credsMu.Unlock()
}

// LoginEnabled reports whether a username/password login is configured.
func (s *Server) LoginEnabled() bool { return s.credentials() != nil }

// LoginUsername returns the configured username, or "" when login is off.
func (s *Server) LoginUsername() string {
	if c := s.credentials(); c != nil {
		return c.Username
	}
	return ""
}

// credentials snapshots the current credentials under the read lock.
func (s *Server) credentials() *Credentials {
	s.credsMu.RLock()
	defer s.credsMu.RUnlock()
	return s.creds
}

// bearerToken extracts the JWT from an `Authorization: Bearer …` header.
func bearerToken(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// jwtSubject verifies a login JWT presented on this request — from the
// Authorization header, or from ?token= (the WebSocket upgrade cannot set
// headers) — and returns the username it was issued to.
func (s *Server) jwtSubject(c *gin.Context) (string, bool) {
	creds := s.credentials()
	if creds == nil {
		return "", false
	}
	key := creds.SigningKey()
	for _, tok := range []string{bearerToken(c), c.Query("token")} {
		if tok == "" {
			continue
		}
		if claims, err := parseJWT(key, tok, time.Now()); err == nil {
			return claims.Sub, true
		}
	}
	return "", false
}

// isAppShell reports whether path serves the login-capable SPA bundle rather
// than session data. These stay public once a login is configured.
func isAppShell(path string) bool {
	switch path {
	case "/", "/mobile", "/mobile/":
		return true
	}
	return strings.HasPrefix(path, "/assets/") || strings.HasPrefix(path, "/static/")
}

// authMiddleware gates a request according to the DOOR it arrived through.
//
// The shared listener (server_shared.go) always demands the login — it is the
// address handed to a browser or a phone. The private listener keeps the
// posture that lets the desktop app work at all: in control mode it is the
// app's own loopback connection, presenting no credential, so it is not gated.
// A server started from the command line is not in control mode, so its
// private door is gated by the same login as the shared one.
//
// Order:
//
//	1. request came through the shared door → the login is required, skip to 3
//	2. private door, and either no credentials or control mode → allow
//	3. path is /health           → allow (liveness probes stay public)
//	4. valid login JWT           → allow (Bearer header, or ?token= on /ws)
//	5. the request is the app shell or an /api/auth/* endpoint → allow, so the
//	   login page can load and submit
//	6. otherwise                 → 401 Unauthorized
func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !arrivedShared(c.Request) && (s.credentials() == nil || s.ControlEnabled()) {
			c.Next()
			return
		}
		if c.Request.URL.Path == "/health" {
			c.Next()
			return
		}
		if _, ok := s.jwtSubject(c); ok {
			c.Next()
			return
		}
		if strings.HasPrefix(c.Request.URL.Path, "/api/auth/") || isAppShell(c.Request.URL.Path) {
			c.Next()
			return
		}
		// Deliberately opaque: the response must not disclose which credential
		// unlocks the session. No product name left to rewrite, hence no
		// progname.Rewrite here.
		c.String(http.StatusUnauthorized, "unauthorized")
		c.Abort()
	}
}

// registerAuthAPI wires the login endpoints. Both are reachable without
// credentials (the middleware lets /api/auth/* through) — they are how a
// browser goes from having nothing to having a token.
func (s *Server) registerAuthAPI(r gin.IRoutes) {
	r.POST("/api/auth/login", s.handleLogin)
	r.GET("/api/auth/status", s.handleAuthStatus)
}

// loginRequest is the POST /api/auth/login body.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleLogin exchanges a username and password for a one-month access token.
// The failure reply never says which half was wrong.
func (s *Server) handleLogin(c *gin.Context) {
	creds := s.credentials()
	if creds == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "password login is not configured"})
		return
	}
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if !creds.Verify(req.Username, req.Password) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid username or password"})
		return
	}
	now := time.Now()
	token, err := signJWT(creds.SigningKey(), creds.Username, now, AccessTokenTTL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not issue a token"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"token":      token,
		"username":   creds.Username,
		"expires_at": now.Add(AccessTokenTTL).UTC().Format(time.RFC3339),
	})
}

// handleAuthStatus tells the UI which of the three states it is in: no login
// needed, logged in, or show the form. The browser re-asks after a failed
// connect, which is how an expired token turns back into a login page.
func (s *Server) handleAuthStatus(c *gin.Context) {
	creds := s.credentials()
	// Answer for the door this request came through, not for the server as a
	// whole: the private listener in control mode is ungated even when a login
	// exists for the shared one, and a UI told otherwise would show a password
	// form to a client that needs none.
	if creds == nil || (!arrivedShared(c.Request) && s.ControlEnabled()) {
		c.JSON(http.StatusOK, gin.H{"login_required": false, "authenticated": true})
		return
	}
	sub, ok := s.jwtSubject(c)
	c.JSON(http.StatusOK, gin.H{
		"login_required": true,
		"authenticated":  ok,
		"username":       sub,
	})
}
