package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"

	"github.com/gin-gonic/gin"
)

// authCookieName carries the access token across the asset + WebSocket requests
// that follow the initial ?token= page load, so the token only needs to appear
// in the URL once.
const authCookieName = "rysh_web_token"

// SetAuthToken sets the access token required to reach the web UI. When set,
// every request except /health must present the token — via the ?token= query
// on first load, after which a cookie authorizes the follow-up asset and
// WebSocket requests. Empty ⇒ no auth (loopback-only default). Call before Start.
func (s *Server) SetAuthToken(token string) { s.authToken = token }

// AuthEnabled reports whether an access token is required.
func (s *Server) AuthEnabled() bool { return s.authToken != "" }

// AuthToken returns the configured access token, or "" when auth is disabled.
// Used by ##rysh web status / ##rysh web token to surface the token on request.
func (s *Server) AuthToken() string { return s.authToken }

// GenerateToken returns a URL-safe random access token (192 bits).
func GenerateToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		// rand.Read failing is effectively impossible; fall back to a fixed-length
		// non-empty token rather than an empty (auth-disabling) string.
		return "rysh-token-unavailable"
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// tokenEqual compares two tokens in constant time (ConstantTimeCompare already
// returns 0 for length mismatches, so this is safe for arbitrary inputs).
func tokenEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// authMiddleware gates every route when an access token is configured. Order:
//  1. no token configured           → allow (backward-compatible loopback mode)
//  2. path is /health               → allow (liveness probes stay public)
//  3. valid auth cookie             → allow
//  4. valid ?token= query           → set the cookie; for a page navigation,
//     redirect to a clean URL (strip the token) then continue
//  5. otherwise                     → 401 Unauthorized
func (s *Server) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.authToken == "" {
			c.Next()
			return
		}
		if c.Request.URL.Path == "/health" {
			c.Next()
			return
		}
		if ck, err := c.Cookie(authCookieName); err == nil && tokenEqual(ck, s.authToken) {
			c.Next()
			return
		}
		if q := c.Query("token"); q != "" && tokenEqual(q, s.authToken) {
			// Loopback HTTP: no Secure flag (would drop the cookie over plain
			// http). HttpOnly + SameSite=Lax keep it off cross-site requests.
			http.SetCookie(c.Writer, &http.Cookie{
				Name:     authCookieName,
				Value:    s.authToken,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
			// On a top-level page load, redirect to a clean URL so the token
			// is not left sitting in the address bar / history. Covers both the
			// desktop ("/") and mobile ("/mobile[/]") entry points. Every other
			// query parameter is preserved — only the token is stripped — so UI
			// selectors like ?ui=mobile survive the redirect. Asset and /ws
			// requests just proceed (the cookie is now set).
			if p := c.Request.URL.Path; c.Request.Method == http.MethodGet &&
				(p == "/" || p == "/mobile" || p == "/mobile/") {
				q := c.Request.URL.Query()
				q.Del("token")
				clean := p
				if enc := q.Encode(); enc != "" {
					clean += "?" + enc
				}
				c.Redirect(http.StatusFound, clean)
				c.Abort()
				return
			}
			c.Next()
			return
		}
		c.String(http.StatusUnauthorized, progname.Rewrite("unauthorized: a valid ?token= is required to access this rysh web session"))
		c.Abort()
	}
}
