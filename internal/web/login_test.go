// SPDX-License-Identifier: Apache-2.0

package web

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// --- JWT ---------------------------------------------------------------

func testKey() []byte { return []byte("0123456789abcdef0123456789abcdef") }

func TestJWT_RoundTrip(t *testing.T) {
	now := time.Now()
	tok, err := signJWT(testKey(), "halil", now, AccessTokenTTL)
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	claims, err := parseJWT(testKey(), tok, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("parseJWT: %v", err)
	}
	if claims.Sub != "halil" {
		t.Fatalf("sub = %q, want halil", claims.Sub)
	}
	if got := time.Unix(claims.Exp, 0).Sub(time.Unix(claims.Iat, 0)); got != AccessTokenTTL {
		t.Fatalf("ttl = %v, want %v", got, AccessTokenTTL)
	}
}

// One month is the product promise ("the access token should live for a
// month"), so it is asserted rather than left to the constant's definition.
func TestJWT_TTLIsOneMonth(t *testing.T) {
	if AccessTokenTTL != 30*24*time.Hour {
		t.Fatalf("AccessTokenTTL = %v, want 720h", AccessTokenTTL)
	}
}

func TestJWT_Expired(t *testing.T) {
	now := time.Now()
	tok, err := signJWT(testKey(), "halil", now, time.Minute)
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	if _, err := parseJWT(testKey(), tok, now.Add(2*time.Minute)); err != errJWTExpired {
		t.Fatalf("err = %v, want %v", err, errJWTExpired)
	}
}

func TestJWT_WrongKey(t *testing.T) {
	tok, err := signJWT(testKey(), "halil", time.Now(), time.Hour)
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	other := []byte("ffffffffffffffffffffffffffffffff")
	if _, err := parseJWT(other, tok, time.Now()); err != errJWTSignature {
		t.Fatalf("err = %v, want %v", err, errJWTSignature)
	}
}

func TestJWT_EmptyKeyNeverValidates(t *testing.T) {
	tok, err := signJWT(testKey(), "halil", time.Now(), time.Hour)
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	if _, err := parseJWT(nil, tok, time.Now()); err != errJWTSignature {
		t.Fatalf("err = %v, want %v", err, errJWTSignature)
	}
}

// The classic JWT forgery: strip the signature and declare the token unsigned.
func TestJWT_RejectsAlgNone(t *testing.T) {
	head, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	body, _ := json.Marshal(jwtClaims{Sub: "halil", Exp: time.Now().Add(time.Hour).Unix()})
	forged := b64(head) + "." + b64(body) + "."
	if _, err := parseJWT(testKey(), forged, time.Now()); err != errJWTAlgorithm {
		t.Fatalf("err = %v, want %v", err, errJWTAlgorithm)
	}
}

func TestJWT_Malformed(t *testing.T) {
	for _, tok := range []string{"", "abc", "a.b", "a.b.c.d", "!!.??.$$"} {
		if _, err := parseJWT(testKey(), tok, time.Now()); err == nil {
			t.Fatalf("parseJWT(%q) accepted a malformed token", tok)
		}
	}
}

// --- Credentials -------------------------------------------------------

func TestCredentials_SaveLoadVerify(t *testing.T) {
	dir := t.TempDir()
	saved, err := SaveCredentials(dir, " halil ", "s3cret")
	if err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	if saved.Username != "halil" {
		t.Fatalf("username = %q, want halil (trimmed)", saved.Username)
	}
	if strings.Contains(saved.PasswordHash, "s3cret") {
		t.Fatal("password hash contains the plaintext password")
	}

	loaded, err := LoadCredentials(dir)
	if err != nil || loaded == nil {
		t.Fatalf("LoadCredentials: %v (creds %v)", err, loaded)
	}
	if !loaded.Verify("halil", "s3cret") {
		t.Fatal("correct credentials rejected")
	}
	if loaded.Verify("halil", "wrong") {
		t.Fatal("wrong password accepted")
	}
	if loaded.Verify("someone", "s3cret") {
		t.Fatal("wrong username accepted")
	}
	if len(loaded.SigningKey()) != 32 {
		t.Fatalf("signing key = %d bytes, want 32", len(loaded.SigningKey()))
	}
}

// The password file must not be world- or group-readable: it carries the
// token-signing key, which is as good as the password for issuing logins.
func TestCredentials_FileIsPrivate(t *testing.T) {
	dir := t.TempDir()
	if _, err := SaveCredentials(dir, "halil", "s3cret"); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	info, err := os.Stat(CredentialsPath(dir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 600", perm)
	}
}

// Re-setting credentials rotates the signing key, which is what makes a
// password change log every existing browser out.
func TestCredentials_SaveRotatesSigningKey(t *testing.T) {
	dir := t.TempDir()
	first, err := SaveCredentials(dir, "halil", "one")
	if err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	tok, err := signJWT(first.SigningKey(), "halil", time.Now(), time.Hour)
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	second, err := SaveCredentials(dir, "halil", "two")
	if err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	if second.Secret == first.Secret {
		t.Fatal("signing key survived a password change")
	}
	if _, err := parseJWT(second.SigningKey(), tok, time.Now()); err == nil {
		t.Fatal("token issued under the old password still validates")
	}
}

func TestCredentials_MissingIsNotAnError(t *testing.T) {
	creds, err := LoadCredentials(t.TempDir())
	if err != nil || creds != nil {
		t.Fatalf("LoadCredentials = (%v, %v), want (nil, nil)", creds, err)
	}
}

// A corrupt file must fail loudly rather than quietly disabling the login.
func TestCredentials_CorruptIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, credentialsFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadCredentials(dir); err == nil {
		t.Fatal("corrupt credentials file loaded without error")
	}
}

func TestCredentials_Clear(t *testing.T) {
	dir := t.TempDir()
	if _, err := SaveCredentials(dir, "halil", "s3cret"); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	removed, err := ClearCredentials(dir)
	if err != nil || !removed {
		t.Fatalf("ClearCredentials = (%v, %v), want (true, nil)", removed, err)
	}
	if removed, _ := ClearCredentials(dir); removed {
		t.Fatal("second ClearCredentials reported a removal")
	}
	creds, err := LoadCredentials(dir)
	if err != nil || creds != nil {
		t.Fatalf("credentials survived the clear: %v %v", creds, err)
	}
}

// --- Login endpoints + middleware --------------------------------------

// loginRouter mirrors server.go's setup for a server with a password login
// configured.
func loginRouter(t *testing.T) (*gin.Engine, *Server) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	creds, err := SaveCredentials(t.TempDir(), "halil", "s3cret")
	if err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	s := &Server{creds: creds}
	r := gin.New()
	r.Use(s.authMiddleware())
	s.registerAuthAPI(r)
	r.GET("/", func(c *gin.Context) { c.String(http.StatusOK, "index") })
	r.GET("/assets/app.js", func(c *gin.Context) { c.String(http.StatusOK, "js") })
	r.GET("/health", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	r.GET("/ws", func(c *gin.Context) { c.String(http.StatusOK, "ws") })
	r.GET("/api/env", func(c *gin.Context) { c.String(http.StatusOK, "env") })
	return r, s
}

func postLogin(r *gin.Engine, username, password string) *httptest.ResponseRecorder {
	body := `{"username":` + jsonQuote(username) + `,"password":` + jsonQuote(password) + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// jsonQuote JSON-quotes a string for the hand-built request bodies above.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func getWithBearer(r *gin.Engine, target, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestLogin_IssuesUsableToken(t *testing.T) {
	r, _ := loginRouter(t)
	w := postLogin(r, "halil", "s3cret")
	if w.Code != http.StatusOK {
		t.Fatalf("login = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	var reply struct {
		Token     string `json:"token"`
		Username  string `json:"username"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if reply.Token == "" || reply.Username != "halil" {
		t.Fatalf("reply = %+v", reply)
	}
	exp, err := time.Parse(time.RFC3339, reply.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q: %v", reply.ExpiresAt, err)
	}
	if d := time.Until(exp); d < AccessTokenTTL-time.Minute || d > AccessTokenTTL+time.Minute {
		t.Fatalf("expires in %v, want ~%v", d, AccessTokenTTL)
	}

	// The token opens the data routes.
	if got := getWithBearer(r, "/api/env", reply.Token); got.Code != http.StatusOK {
		t.Fatalf("/api/env with bearer = %d, want 200", got.Code)
	}
	// And on the WebSocket upgrade, which cannot carry a header.
	if got := getWithBearer(r, "/ws?stream=1&token="+reply.Token, ""); got.Code != http.StatusOK {
		t.Fatalf("/ws with ?token= = %d, want 200", got.Code)
	}
}

func TestLogin_RejectsBadCredentials(t *testing.T) {
	r, _ := loginRouter(t)
	for _, c := range []struct{ user, pass string }{
		{"halil", "wrong"},
		{"nobody", "s3cret"},
		{"", ""},
	} {
		if w := postLogin(r, c.user, c.pass); w.Code != http.StatusUnauthorized {
			t.Fatalf("login(%q,%q) = %d, want 401", c.user, c.pass, w.Code)
		}
	}
}

// The login form ships inside the app bundle, so the bundle has to be
// reachable before anyone has logged in — but nothing with session data in it.
func TestLogin_AppShellPublic_DataRoutesGated(t *testing.T) {
	r, _ := loginRouter(t)
	for _, p := range []string{"/", "/assets/app.js", "/health", "/api/auth/status"} {
		if w := getWithBearer(r, p, ""); w.Code != http.StatusOK {
			t.Fatalf("anonymous %s = %d, want 200", p, w.Code)
		}
	}
	for _, p := range []string{"/ws?stream=1", "/api/env"} {
		if w := getWithBearer(r, p, ""); w.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s = %d, want 401", p, w.Code)
		}
	}
}

func TestLogin_ExpiredTokenIsRefused(t *testing.T) {
	r, s := loginRouter(t)
	stale, err := signJWT(s.creds.SigningKey(), "halil", time.Now().Add(-2*AccessTokenTTL), AccessTokenTTL)
	if err != nil {
		t.Fatalf("signJWT: %v", err)
	}
	if w := getWithBearer(r, "/api/env", stale); w.Code != http.StatusUnauthorized {
		t.Fatalf("/api/env with an expired token = %d, want 401", w.Code)
	}
	// …and the status endpoint tells the UI to show the form again.
	w := getWithBearer(r, "/api/auth/status", stale)
	var st struct {
		LoginRequired bool `json:"login_required"`
		Authenticated bool `json:"authenticated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !st.LoginRequired || st.Authenticated {
		t.Fatalf("status = %+v, want login_required=true authenticated=false", st)
	}
}

func TestAuthStatus_ReportsSignedIn(t *testing.T) {
	r, _ := loginRouter(t)
	var reply struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(postLogin(r, "halil", "s3cret").Body.Bytes(), &reply)
	w := getWithBearer(r, "/api/auth/status", reply.Token)
	var st struct {
		LoginRequired bool   `json:"login_required"`
		Authenticated bool   `json:"authenticated"`
		Username      string `json:"username"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !st.LoginRequired || !st.Authenticated || st.Username != "halil" {
		t.Fatalf("status = %+v", st)
	}
}

// No login configured: the status endpoint must not make the UI ask for one.
func TestAuthStatus_NoLoginConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{}
	r := gin.New()
	r.Use(s.authMiddleware())
	s.registerAuthAPI(r)
	w := getWithBearer(r, "/api/auth/status", "")
	var st struct {
		LoginRequired bool `json:"login_required"`
		Authenticated bool `json:"authenticated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.LoginRequired || !st.Authenticated {
		t.Fatalf("status = %+v, want login_required=false authenticated=true", st)
	}
	// And POSTing a login to a server that has none is a 404, not a 500.
	if got := postLogin(r, "halil", "s3cret"); got.Code != http.StatusNotFound {
		t.Fatalf("login without credentials = %d, want 404", got.Code)
	}
}

// With the access token gone, the login JWT is the only thing that authorizes
// a request — an arbitrary ?token= must not.
func TestLogin_NonJWTTokenQueryDenied(t *testing.T) {
	r, _ := loginRouter(t)
	if w := getWithBearer(r, "/api/env?token=s3kritToken", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("/api/env with a non-JWT ?token= = %d, want 401", w.Code)
	}
}

// Nothing but a valid login makes the UI stop showing the form.
func TestAuthStatus_NonJWTTokenIsNotAuthenticated(t *testing.T) {
	r, _ := loginRouter(t)
	w := getWithBearer(r, "/api/auth/status?token=s3kritToken", "")
	var st struct {
		Authenticated bool `json:"authenticated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.Authenticated {
		t.Fatalf("status = %+v, want authenticated=false", st)
	}
}

// SetCredentials is called on a running server by `##rysh web auth`.
func TestSetCredentials_AppliesLive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{}
	r := gin.New()
	r.Use(s.authMiddleware())
	s.registerAuthAPI(r)
	r.GET("/api/env", func(c *gin.Context) { c.String(http.StatusOK, "env") })

	if w := getWithBearer(r, "/api/env", ""); w.Code != http.StatusOK {
		t.Fatalf("before credentials /api/env = %d, want 200", w.Code)
	}
	creds, err := SaveCredentials(t.TempDir(), "halil", "s3cret")
	if err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	s.SetCredentials(creds)
	if !s.LoginEnabled() || s.LoginUsername() != "halil" {
		t.Fatalf("LoginEnabled=%v username=%q", s.LoginEnabled(), s.LoginUsername())
	}
	if w := getWithBearer(r, "/api/env", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("after credentials /api/env = %d, want 401", w.Code)
	}
	s.SetCredentials(nil)
	if w := getWithBearer(r, "/api/env", ""); w.Code != http.StatusOK {
		t.Fatalf("after clearing /api/env = %d, want 200", w.Code)
	}
}

func TestSigningKey_UnusableSecretYieldsNil(t *testing.T) {
	c := &Credentials{Secret: "not valid base64!!"}
	if key := c.SigningKey(); key != nil {
		t.Fatalf("SigningKey = %q, want nil", base64.RawURLEncoding.EncodeToString(key))
	}
}
