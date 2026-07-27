package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// router builds a gin engine with the auth middleware and a couple of stub
// routes, mirroring server.go's setup, for isolated middleware testing.
func router(token string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	s := &Server{authToken: token}
	r := gin.New()
	r.Use(s.authMiddleware())
	r.GET("/", func(c *gin.Context) { c.String(http.StatusOK, "index") })
	r.GET("/assets/app.js", func(c *gin.Context) { c.String(http.StatusOK, "js") })
	r.GET("/health", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/ws", func(c *gin.Context) { c.String(http.StatusOK, "ws") })
	return r
}

func do(r *gin.Engine, method, target string, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	if cookie != "" {
		req.Header.Set("Cookie", authCookieName+"="+cookie)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAuth_Disabled_AllowsEverything(t *testing.T) {
	r := router("") // no token configured
	for _, p := range []string{"/", "/assets/app.js", "/ws", "/health"} {
		if w := do(r, http.MethodGet, p, ""); w.Code != http.StatusOK {
			t.Fatalf("no-auth %s = %d, want 200", p, w.Code)
		}
	}
}

func TestAuth_DeniesWithoutToken(t *testing.T) {
	r := router("secret")
	for _, p := range []string{"/", "/assets/app.js", "/ws"} {
		if w := do(r, http.MethodGet, p, ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("gated %s without token = %d, want 401", p, w.Code)
		}
	}
}

func TestAuth_HealthAlwaysPublic(t *testing.T) {
	r := router("secret")
	if w := do(r, http.MethodGet, "/health", ""); w.Code != http.StatusOK {
		t.Fatalf("/health gated = %d, want 200 (public)", w.Code)
	}
}

func TestAuth_WrongToken_Denied(t *testing.T) {
	r := router("secret")
	if w := do(r, http.MethodGet, "/?token=nope", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d, want 401", w.Code)
	}
}

func TestAuth_QueryToken_SetsCookie_AndRedirectsRoot(t *testing.T) {
	r := router("secret")
	w := do(r, http.MethodGet, "/?token=secret", "")
	// Page load with a valid token redirects to a clean "/" and sets the cookie.
	if w.Code != http.StatusFound {
		t.Fatalf("valid page token = %d, want 302", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Fatalf("redirect Location = %q, want /", loc)
	}
	setCookie := w.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, authCookieName+"=secret") || !strings.Contains(setCookie, "HttpOnly") {
		t.Fatalf("Set-Cookie = %q, want httpOnly %s=secret", setCookie, authCookieName)
	}
}

func TestAuth_QueryToken_OnWs_AllowsWithoutRedirect(t *testing.T) {
	r := router("secret")
	w := do(r, http.MethodGet, "/ws?stream=1&token=secret", "")
	if w.Code != http.StatusOK || w.Body.String() != "ws" {
		t.Fatalf("ws with query token = %d %q, want 200 ws", w.Code, w.Body.String())
	}
}

func TestAuth_ValidCookie_Allows(t *testing.T) {
	r := router("secret")
	for _, p := range []string{"/", "/assets/app.js", "/ws"} {
		if w := do(r, http.MethodGet, p, "secret"); w.Code != http.StatusOK {
			t.Fatalf("cookie-authed %s = %d, want 200", p, w.Code)
		}
	}
	// A wrong cookie is rejected.
	if w := do(r, http.MethodGet, "/", "wrong"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong cookie = %d, want 401", w.Code)
	}
}

func TestGenerateToken_UniqueNonEmpty(t *testing.T) {
	a, b := GenerateToken(), GenerateToken()
	if a == "" || b == "" {
		t.Fatal("empty token generated")
	}
	if a == b {
		t.Fatal("tokens not unique")
	}
	if len(a) < 20 {
		t.Fatalf("token too short: %q", a)
	}
}

func TestTokenEqual(t *testing.T) {
	if !tokenEqual("abc", "abc") || tokenEqual("abc", "abd") || tokenEqual("abc", "abcd") {
		t.Fatal("tokenEqual wrong")
	}
}

// TestAuthToken_Roundtrip verifies the getter surfaces exactly what was set, so
// ##rysh web status / ##rysh web token can list the live token on request.
func TestAuthToken_Roundtrip(t *testing.T) {
	s := &Server{}
	if s.AuthEnabled() {
		t.Fatal("fresh server should not be auth-enabled")
	}
	if got := s.AuthToken(); got != "" {
		t.Fatalf("AuthToken on fresh server = %q, want empty", got)
	}
	s.SetAuthToken("sekret-123")
	if !s.AuthEnabled() {
		t.Fatal("server should be auth-enabled after SetAuthToken")
	}
	if got := s.AuthToken(); got != "sekret-123" {
		t.Fatalf("AuthToken = %q, want %q", got, "sekret-123")
	}
	// Clearing the token disables auth again.
	s.SetAuthToken("")
	if s.AuthEnabled() || s.AuthToken() != "" {
		t.Fatal("clearing the token should disable auth")
	}
}
