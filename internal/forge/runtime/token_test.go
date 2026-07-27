package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// authServer is a fake token + protected-resource server used across the auth
// tests. It mints short-lived access tokens, accepts refresh-token exchanges and
// a custom JSON login, and serves a protected endpoint that 401s without a valid
// bearer.
type authServer struct {
	mu           sync.Mutex
	logins       int32
	refreshes    int32
	accessSeq    int
	valid        map[string]bool // currently-accepted access tokens
	validRefresh map[string]bool // currently-accepted refresh tokens
	expiresIn    int             // expires_in to report (0 ⇒ omit)
}

func newAuthServer() *authServer {
	return &authServer{valid: map[string]bool{}, validRefresh: map[string]bool{}, expiresIn: 3600}
}

// mint creates a new access (and refresh) token, invalidating prior ones.
func (s *authServer) mint() (access, refresh string) {
	s.accessSeq++
	access = "acc-" + itoa(s.accessSeq)
	refresh = "ref-" + itoa(s.accessSeq)
	s.valid = map[string]bool{access: true}
	s.validRefresh[refresh] = true
	return access, refresh
}

func (s *authServer) tokenResponse(access, refresh string) map[string]any {
	m := map[string]any{"access_token": access, "refresh_token": refresh}
	if s.expiresIn > 0 {
		m["expires_in"] = s.expiresIn
	}
	return m
}

func (s *authServer) handler() http.Handler {
	mux := http.NewServeMux()

	// OAuth2 token endpoint (password / client_credentials / refresh_token).
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.Form.Get("grant_type") {
		case "password", "client_credentials":
			atomic.AddInt32(&s.logins, 1)
			a, rf := s.mint()
			writeJSON(w, s.tokenResponse(a, rf))
		case "refresh_token":
			rt := r.Form.Get("refresh_token")
			if !s.validRefresh[rt] {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			atomic.AddInt32(&s.refreshes, 1)
			delete(s.validRefresh, rt)
			a, rf := s.mint()
			writeJSON(w, s.tokenResponse(a, rf))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	// Custom JSON login endpoint: {username,password} or {refresh_token}.
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		defer s.mu.Unlock()
		if rt := body["refresh_token"]; rt != "" {
			if !s.validRefresh[rt] {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			atomic.AddInt32(&s.refreshes, 1)
			delete(s.validRefresh, rt)
			a, rf := s.mint()
			writeJSON(w, map[string]any{"data": map[string]any{"jwt": a, "renew": rf, "ttl": s.expiresIn}})
			return
		}
		atomic.AddInt32(&s.logins, 1)
		a, rf := s.mint()
		// Nested fields to exercise dotted field paths.
		writeJSON(w, map[string]any{"data": map[string]any{"jwt": a, "renew": rf, "ttl": s.expiresIn}})
	})

	// Protected resource: 200 with a valid bearer, else 401.
	mux.HandleFunc("/protected", func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		ok := s.valid[tok]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "bearer": tok})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestTokenManager_PasswordGrant(t *testing.T) {
	s := newAuthServer()
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	t.Setenv("TM_USER", "alice")
	t.Setenv("TM_PASS", "secret")

	tm, err := NewTokenManager(AuthConfig{
		Type:        AuthOAuth2Password,
		TokenURL:    srv.URL + "/oauth/token",
		UsernameEnv: "TM_USER",
		PasswordEnv: "TM_PASS",
	}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	tok, err := tm.Token(context.Background())
	if err != nil || tok == "" {
		t.Fatalf("Token() = %q, %v", tok, err)
	}
	// A second call within the lifetime should NOT re-login (cached).
	tok2, _ := tm.Token(context.Background())
	if tok2 != tok {
		t.Fatalf("token changed without expiry: %q -> %q", tok, tok2)
	}
	if got := atomic.LoadInt32(&s.logins); got != 1 {
		t.Fatalf("logins = %d, want 1 (cached)", got)
	}
}

func TestTokenManager_ClientCredentials(t *testing.T) {
	s := newAuthServer()
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	t.Setenv("TM_CID", "client-1")
	t.Setenv("TM_CSEC", "shh")

	tm, err := NewTokenManager(AuthConfig{
		Type:            AuthOAuth2ClientCreds,
		TokenURL:        srv.URL + "/oauth/token",
		ClientIDEnv:     "TM_CID",
		ClientSecretEnv: "TM_CSEC",
		Scope:           "read",
	}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if tok, err := tm.Token(context.Background()); err != nil || tok == "" {
		t.Fatalf("Token() = %q, %v", tok, err)
	}
}

func TestTokenManager_CustomLoginDottedFields(t *testing.T) {
	s := newAuthServer()
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	t.Setenv("TM_USER", "bob")
	t.Setenv("TM_PASS", "pw")

	tm, err := NewTokenManager(AuthConfig{
		Type:              AuthJWTLogin,
		TokenURL:          srv.URL + "/login",
		RefreshURL:        srv.URL + "/login",
		UsernameEnv:       "TM_USER",
		PasswordEnv:       "TM_PASS",
		AccessTokenField:  "data.jwt",
		RefreshTokenField: "data.renew",
		ExpiresInField:    "data.ttl",
	}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	tok, err := tm.Token(context.Background())
	if err != nil || tok == "" {
		t.Fatalf("custom login Token() = %q, %v", tok, err)
	}
	// Force a refresh: should use the refresh token and yield a new access token.
	tok2, err := tm.Refresh(context.Background())
	if err != nil || tok2 == "" || tok2 == tok {
		t.Fatalf("Refresh() = %q, %v (prev %q)", tok2, err, tok)
	}
	if got := atomic.LoadInt32(&s.refreshes); got != 1 {
		t.Fatalf("refreshes = %d, want 1", got)
	}
}

func TestTokenManager_ProactiveRefresh(t *testing.T) {
	s := newAuthServer()
	s.expiresIn = 1 // 1s lifetime; with the 30s margin, the token is "stale" immediately
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	t.Setenv("TM_USER", "a")
	t.Setenv("TM_PASS", "b")

	tm, err := NewTokenManager(AuthConfig{
		Type:        AuthOAuth2Password,
		TokenURL:    srv.URL + "/oauth/token",
		UsernameEnv: "TM_USER",
		PasswordEnv: "TM_PASS",
	}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tm.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Because expires_in(1s) < margin(30s), the next Token() must refresh first.
	if _, err := tm.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&s.refreshes); got < 1 {
		t.Fatalf("expected a proactive refresh, refreshes=%d", got)
	}
}

func TestTokenManager_ReLoginWhenRefreshRejected(t *testing.T) {
	s := newAuthServer()
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	t.Setenv("TM_USER", "a")
	t.Setenv("TM_PASS", "b")

	tm, err := NewTokenManager(AuthConfig{
		Type:        AuthOAuth2Password,
		TokenURL:    srv.URL + "/oauth/token",
		UsernameEnv: "TM_USER",
		PasswordEnv: "TM_PASS",
	}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tm.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Invalidate ALL refresh tokens on the server: Refresh must fall back to a
	// fresh login rather than failing.
	s.mu.Lock()
	s.validRefresh = map[string]bool{}
	s.mu.Unlock()
	loginsBefore := atomic.LoadInt32(&s.logins)
	tok, err := tm.Refresh(context.Background())
	if err != nil || tok == "" {
		t.Fatalf("Refresh() after refresh-token rejection = %q, %v", tok, err)
	}
	if atomic.LoadInt32(&s.logins) != loginsBefore+1 {
		t.Fatalf("expected a re-login after refresh rejection")
	}
}

func TestTokenManager_SingleFlight(t *testing.T) {
	s := newAuthServer()
	// Slow the login so concurrent callers overlap.
	slow := http.NewServeMux()
	slow.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(40 * time.Millisecond)
		s.mu.Lock()
		defer s.mu.Unlock()
		atomic.AddInt32(&s.logins, 1)
		a, rf := s.mint()
		writeJSON(w, s.tokenResponse(a, rf))
	})
	srv := httptest.NewServer(slow)
	defer srv.Close()
	t.Setenv("TM_USER", "a")
	t.Setenv("TM_PASS", "b")

	tm, err := NewTokenManager(AuthConfig{
		Type:        AuthOAuth2Password,
		TokenURL:    srv.URL + "/oauth/token",
		UsernameEnv: "TM_USER",
		PasswordEnv: "TM_PASS",
	}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = tm.Token(context.Background())
		}()
	}
	wg.Wait()
	// Serialized acquisition: the first caller logs in, the rest see the cache.
	if got := atomic.LoadInt32(&s.logins); got != 1 {
		t.Fatalf("logins = %d, want 1 (single-flight)", got)
	}
}

func TestTokenManager_MissingCredentials(t *testing.T) {
	// Construction fails fast when a required env is empty.
	if _, err := NewTokenManager(AuthConfig{Type: AuthStatic, AccessTokenEnv: "TM_ABSENT"}, nil); err == nil {
		t.Fatal("expected error for static auth with empty access_token_env")
	}
	// A token URL is required for login grants.
	if _, err := NewTokenManager(AuthConfig{Type: AuthOAuth2Password, UsernameEnv: "X"}, nil); err == nil {
		t.Fatal("expected error for password grant without token_url")
	}
}

func TestTokenManager_StaticNoFlow(t *testing.T) {
	// apikey/basic/none are NOT token flows → no manager.
	for _, ty := range []string{AuthNone, "apikey", "basic", "bearer"} {
		tm, err := NewTokenManager(AuthConfig{Type: ty}, nil)
		if err != nil || tm != nil {
			t.Fatalf("type %q: got (%v,%v), want (nil,nil)", ty, tm, err)
		}
	}
}

func TestJWTExp(t *testing.T) {
	// header.payload.signature with {"exp":1234567890}
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1234567890}`))
	tok := "x." + payload + ".y"
	if got := jwtExp(tok); got != 1234567890 {
		t.Fatalf("jwtExp = %d, want 1234567890", got)
	}
	if got := jwtExp("not-a-jwt"); got != 0 {
		t.Fatalf("jwtExp(non-jwt) = %d, want 0", got)
	}
}
