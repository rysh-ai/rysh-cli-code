// SPDX-License-Identifier: Apache-2.0

package web

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/rysh-ai/rysh-cli-code/internal/progname"
)

// The web UI has one way in: a USERNAME/PASSWORD LOGIN (SetCredentials, set via
// `##rysh web auth` or `##rysh web start --username/--password`).
// POST /api/auth/login returns a one-month JWT the browser keeps in
// localStorage and presents as `Authorization: Bearer …` or TokenHeader (and on
// the WebSocket upgrade, which cannot carry headers, as a TokenSubprotocolPrefix
// subprotocol entry; ?token= only from older bundles). When it expires the UI shows
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
// No login is needed in exactly one case: control mode, where the desktop app
// spawns its own daemon, which forces a loopback bind and serves exactly one
// client — the app that started it. Outside control mode a server with no login
// refuses session data: removing the login, or losing its file, closes the UI
// rather than opening it (privateDoorOpen).

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

// credsFileStamp is the cheap identity of web-auth.json — enough to notice a
// rewrite without reading and parsing the file on every request.
type credsFileStamp struct {
	mod  time.Time
	size int64
}

// TrackCredentialsFile tells the server which workspace rysh dir holds its
// web-auth.json, making that FILE the source of truth rather than whatever
// snapshot was installed at start.
//
// This exists because credentials are per-workspace state shared by every
// daemon rooted there, while `creds` is per-process. A second daemon running
// `##rysh web auth` rewrites the file and mints a fresh signing key — correctly,
// that is how changing the password logs old browsers out — and this process,
// still verifying against the superseded key, then rejects every client with a
// 401 that no user can diagnose or fix (F-9). Restarting the listener "fixed"
// it precisely because a restart is the only thing that re-read the file.
//
// Not a watcher: a stat per auth decision costs a syscall, needs no goroutine
// and no lifecycle, and cannot leak a watch when the server stops.
func (s *Server) TrackCredentialsFile(ryshDir string) {
	s.TrackCredentialsRef(ProjectCredentials(ryshDir))
}

// TrackCredentialsRef is TrackCredentialsFile for a session-scoped login: the
// server follows that session's own file, falling back to the project's while
// the session has none. The ref (not a resolved path) is what is held, so a
// session that sets its own login mid-run is picked up on the next auth
// decision rather than at the next restart.
func (s *Server) TrackCredentialsRef(ref CredentialsRef) {
	s.credsMu.Lock()
	s.credsRef = ref
	s.credsStat = credsFileStamp{}
	s.credsMu.Unlock()
}

// credentials returns the credentials in force, re-reading web-auth.json first
// if it has changed since we last looked.
func (s *Server) credentials() *Credentials {
	s.credsMu.RLock()
	ref := s.credsRef
	s.credsMu.RUnlock()
	if ref.RyshDir != "" {
		s.refreshCredentials(ref)
	}

	s.credsMu.RLock()
	defer s.credsMu.RUnlock()
	return s.creds
}

// refreshCredentials reloads web-auth.json if its mtime or size moved.
//
// Three failure modes are deliberately NOT treated the same, because collapsing
// them is how a credentials bug becomes a security bug:
//
//   - the file is GONE: login is not configured, which is what LoadCredentials
//     already means by (nil, nil). Every door then refuses session data except
//     control mode's private loopback door (privateDoorOpen).
//   - the file is UNREADABLE OR CORRUPT: keep what we have. A half-written file
//     (SaveCredentials truncates in place) must never be read as "no login" —
//     that would downgrade the gate to nothing at exactly the wrong moment.
//   - the file is FINE: adopt it, key and password hash together.
func (s *Server) refreshCredentials(ref CredentialsRef) {
	// Resolved every time: a session that gains its own file must start being
	// read from it, and one whose file is removed must fall back to the
	// project's.
	path := ref.Path()
	var stamp credsFileStamp
	if info, err := os.Stat(path); err == nil {
		stamp = credsFileStamp{mod: info.ModTime(), size: info.Size()}
	} else if !errors.Is(err, os.ErrNotExist) {
		return // cannot tell; keep what we have rather than guess
	}

	s.credsMu.RLock()
	unchanged := s.credsStat == stamp
	s.credsMu.RUnlock()
	if unchanged {
		return
	}

	creds, err := loadCredentialsFile(path)
	if err != nil {
		// Corrupt or half-written. Do not adopt it, and do not re-read it on
		// every request either — the next real change moves the stamp again.
		s.credsMu.Lock()
		s.credsStat = stamp
		s.credsMu.Unlock()
		return
	}
	s.credsMu.Lock()
	s.creds = creds
	s.credsStat = stamp
	s.credsMu.Unlock()
}

// Where else the login JWT may travel (security-lead ledger 86). nginx Basic
// auth in front of /ryshweb owns the Authorization header, so the web client
// sends the JWT as TokenHeader, and on the WebSocket upgrade (a browser cannot
// set headers there) as a TokenSubprotocolPrefix subprotocol entry next to the
// plain TokenSubprotocol, which keeps it out of URLs. Only the plain name is
// ever echoed back.
const (
	TokenHeader            = "X-Rysh-Token"
	TokenSubprotocol       = "rysh-auth"
	TokenSubprotocolPrefix = "rysh-auth."
)

// bearerToken extracts the JWT from an `Authorization: Bearer …` header.
func bearerToken(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// tokenSubprotocolEntries returns every TokenSubprotocolPrefix entry r offers
// in Sec-WebSocket-Protocol, across all header lines, prefix removed.
func tokenSubprotocolEntries(r *http.Request) []string {
	var out []string
	for _, line := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(line, ",") {
			p = strings.TrimSpace(p)
			if strings.HasPrefix(p, TokenSubprotocolPrefix) {
				out = append(out, strings.TrimPrefix(p, TokenSubprotocolPrefix))
			}
		}
	}
	return out
}

// presentedTokens returns the login JWTs this request offers: Authorization:
// Bearer, TokenHeader, a TokenSubprotocolPrefix subprotocol entry, and ?token=.
// ?token= is kept only for web bundles built before the subprotocol; it puts
// the JWT in a URL and goes in the release after the client ships (S2).
// An Authorization header that is not Bearer (nginx's Basic) is not ours and
// is ignored. More than one TokenHeader value or subprotocol entry is
// ambiguous and refuses the request (ok false), whatever else it carries.
func presentedTokens(c *gin.Context) (toks []string, ok bool) {
	hdr := c.Request.Header.Values(TokenHeader)
	sub := tokenSubprotocolEntries(c.Request)
	if len(hdr) > 1 || len(sub) > 1 {
		return nil, false
	}
	toks = append(toks, bearerToken(c))
	if len(hdr) == 1 {
		toks = append(toks, strings.TrimSpace(hdr[0]))
	}
	toks = append(toks, sub...)
	return append(toks, c.Query("token")), true
}

// jwtSubject verifies a login JWT presented on this request (presentedTokens)
// and returns the username it was issued to.
func (s *Server) jwtSubject(c *gin.Context) (string, bool) {
	creds := s.credentials()
	if creds == nil {
		return "", false
	}
	toks, ok := presentedTokens(c)
	if !ok {
		return "", false
	}
	key := creds.SigningKey()
	for _, tok := range toks {
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

// forwardedHeaders mark a request that reached this listener through a proxy
// or tunnel. The desktop app talks to its daemon directly and never sends them.
var forwardedHeaders = []string{"X-Forwarded-For", "X-Real-IP", "Forwarded"}

// forwarded reports whether r carries any proxy forwarding header. Presence
// counts, not content: an empty value still means something forwarded it.
func forwarded(r *http.Request) bool {
	for _, h := range forwardedHeaders {
		if len(r.Header.Values(h)) > 0 {
			return true
		}
	}
	return false
}

// privateDoorOpen reports whether r may use the session without the login: it
// came through the private listener, that listener is bound to loopback, the
// server is in control mode, nothing proxied the request, and it presents the
// control door's per-launch secret (control_secret.go). Control mode on
// loopback alone is not enough: other local software, web pages included, can
// reach a loopback port. A forwarded request counts as shared, so a tunnel or
// reverse proxy onto the private port fails closed. No login configured is
// NOT an exemption.
func (s *Server) privateDoorOpen(r *http.Request) bool {
	if arrivedShared(r) || forwarded(r) {
		return false
	}
	s.mu.Lock()
	open := s.control && s.privateLoopback
	s.mu.Unlock()
	return open && s.privateHostOK(r) && s.controlSecretOK(r)
}

// hostGuard refuses, with 403, a request on the loopback private listener whose
// Host does not name that listener (privateHostOK). It runs before the login
// gate and applies to every route there, /health included.
func (s *Server) hostGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.privateHostOK(c.Request) {
			s.logRefused(c.Request, "host")
			c.String(http.StatusForbidden, "forbidden")
			c.Abort()
			return
		}
		c.Next()
	}
}

// authMiddleware gates a request according to the DOOR it arrived through.
//
// The shared listener (server_shared.go) always demands the login — it is the
// address handed to a browser or a phone. The private listener keeps the
// posture that lets the desktop app work at all: in control mode it is the
// app's own loopback connection, presenting no credential, so it is not gated.
// A server started from the command line is not in control mode, so its
// private door is gated by the same login as the shared one — and with no
// login configured it refuses session data outright.
//
// Order:
//
//  1. privateDoorOpen (control mode, loopback, not forwarded, the per-launch
//     secret) → allow
//  2. otherwise the login is required:
//  3. path is /health           → allow (liveness probes stay public)
//  4. valid login JWT           → allow (presentedTokens: Bearer, TokenHeader,
//     the token subprotocol on /ws, or ?token= from older bundles)
//  5. the request is the app shell or an /api/auth/* endpoint → allow, so the
//     login page can load and submit
//  6. otherwise                 → 401 Unauthorized
func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.privateDoorOpen(c.Request) {
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
		// unlocks the session. With no login at all there is none to disclose,
		// and the operator needs to know why everything is refused.
		if s.credentials() == nil {
			c.String(http.StatusUnauthorized, progname.Rewrite(
				"unauthorized: no web login is configured — set one with ##rysh web auth"))
		} else {
			c.String(http.StatusUnauthorized, "unauthorized")
		}
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
	// Checked before the password, so past the limit the right password gets
	// the same answer as a wrong one and guessing learns nothing.
	if wait := s.loginLimit.blocked(); wait > 0 {
		secs := int(wait.Round(time.Second) / time.Second)
		if secs < 1 {
			secs = 1
		}
		c.Header("Retry-After", strconv.Itoa(secs))
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many failed logins; try again later"})
		return
	}
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if !creds.Verify(req.Username, req.Password) {
		s.loginLimit.fail()
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
	// Answer for the door this request came through, not for the server as a
	// whole: the private listener in control mode is ungated even when a login
	// exists for the shared one, and a UI told otherwise would show a password
	// form to a client that needs none. Everywhere else a login is required,
	// configured or not — the gate refuses session data either way.
	if s.privateDoorOpen(c.Request) {
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
