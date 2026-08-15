// SPDX-License-Identifier: Apache-2.0

package runtime

// token.go implements the forged-API authentication/refresh layer (Phase A of
// the forged-API auth plan). A TokenManager acquires a bearer access token from
// a login/token endpoint and keeps it fresh: proactively (before it expires) and
// reactively (when the backend answers 401). It is wired into the HTTP/GraphQL
// executors via Options.TokenProvider / Options.TokenRefresher.
//
// Security invariants (see the plan's §8 review gate):
//   - Secrets are NEVER passed inline. AuthConfig names ENVIRONMENT VARIABLES;
//     the actual username/password/client-secret/token are read from the
//     environment at construction time only.
//   - Access and refresh tokens live IN MEMORY ONLY. They are never written to
//     the integration store, the pane KV, logs, or the shared-output stream.
//     A restart re-logs-in.
//   - Refresh is single-flight per manager (the mutex is held across the network
//     call) so a burst of concurrent invocations cannot trigger a refresh storm.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Auth grant types accepted by AuthConfig.Type.
const (
	AuthNone              = ""                          // no token flow (static apiKey/basic/bearer handled by InjectAuth)
	AuthStatic            = "static"                    // read token(s) from env; no login, still refreshable
	AuthJWTLogin          = "jwt_login"                 // custom JSON login -> {access,refresh,expires_in}
	AuthOAuth2Password    = "oauth2_password"           // OAuth2 Resource Owner Password Credentials
	AuthOAuth2ClientCreds = "oauth2_client_credentials" // OAuth2 Client Credentials
)

// defaultExpiryMargin is subtracted from a token's lifetime so a request never
// races the exact expiry instant.
const defaultExpiryMargin = 30 * time.Second

// AuthConfig declares how an integration (owner, Model A) or a subscription
// (subscriber, Model B) acquires and refreshes a bearer token. It is persisted
// in the integration store and the YAML config, so it holds ONLY env-var names
// and URLs — never secret values.
type AuthConfig struct {
	Type        string `json:"type" yaml:"type"`                                     // see Auth* constants
	TokenURL    string `json:"token_url,omitempty" yaml:"token_url,omitempty"`       // login / token endpoint
	RefreshURL  string `json:"refresh_url,omitempty" yaml:"refresh_url,omitempty"`   // defaults to TokenURL
	ContentType string `json:"content_type,omitempty" yaml:"content_type,omitempty"` // "form" | "json"

	// Secrets are ENV-VAR NAMES only — resolved from the environment at construction.
	UsernameEnv     string `json:"username_env,omitempty" yaml:"username_env,omitempty"`
	PasswordEnv     string `json:"password_env,omitempty" yaml:"password_env,omitempty"`
	ClientIDEnv     string `json:"client_id_env,omitempty" yaml:"client_id_env,omitempty"`
	ClientSecretEnv string `json:"client_secret_env,omitempty" yaml:"client_secret_env,omitempty"`
	AccessTokenEnv  string `json:"access_token_env,omitempty" yaml:"access_token_env,omitempty"`   // static
	RefreshTokenEnv string `json:"refresh_token_env,omitempty" yaml:"refresh_token_env,omitempty"` // static (optional)

	Scope string `json:"scope,omitempty" yaml:"scope,omitempty"`

	// Response field paths (dotted) for jwt_login / non-standard token responses.
	AccessTokenField  string `json:"access_token_field,omitempty" yaml:"access_token_field,omitempty"`
	RefreshTokenField string `json:"refresh_token_field,omitempty" yaml:"refresh_token_field,omitempty"`
	ExpiresInField    string `json:"expires_in_field,omitempty" yaml:"expires_in_field,omitempty"`

	// Injection target. Defaults: header "Authorization", scheme "Bearer".
	Header string `json:"header,omitempty" yaml:"header,omitempty"`
	Scheme string `json:"scheme,omitempty" yaml:"scheme,omitempty"`

	ExpiryMargin string `json:"expiry_margin,omitempty" yaml:"expiry_margin,omitempty"` // e.g. "30s"
}

// IsTokenFlow reports whether this config drives a TokenManager (a login/refresh
// flow) rather than the static apiKey/basic/fixed-bearer path handled by
// InjectAuth. An empty/unknown type, "apikey", "basic" or "bearer" return false.
func (c AuthConfig) IsTokenFlow() bool {
	switch c.Type {
	case AuthStatic, AuthJWTLogin, AuthOAuth2Password, AuthOAuth2ClientCreds:
		return true
	default:
		return false
	}
}

// TokenManager returns valid bearer tokens, refreshing as needed. Both methods
// are safe for concurrent use; refresh is single-flight.
type TokenManager interface {
	// Token returns a valid bearer, refreshing proactively if it is at/near expiry.
	Token(ctx context.Context) (string, error)
	// Refresh forces a refresh (used reactively on a 401) and returns the new bearer.
	Refresh(ctx context.Context) (string, error)
}

// tokenManager is the single concrete TokenManager; the grant type selects the
// acquire/refresh behavior. Construction resolves all secrets from the
// environment so the struct never references env-var names again.
type tokenManager struct {
	cfg    AuthConfig
	client *http.Client
	margin time.Duration

	// Resolved secrets (read once, at construction).
	username, password          string
	clientID, clientSecret      string
	staticAccess, staticRefresh string

	mu      sync.Mutex // serializes acquire/refresh: single-flight, no refresh storm
	access  string
	refresh string
	expiry  time.Time // zero ⇒ no known expiry (treated as non-expiring)
}

// NewTokenManager builds a TokenManager from cfg, or returns (nil, nil) when cfg
// does not describe a token flow (the caller then relies on InjectAuth's static
// path). client may be nil (a default client with a sane timeout is used).
func NewTokenManager(cfg AuthConfig, client *http.Client) (TokenManager, error) {
	if !cfg.IsTokenFlow() {
		return nil, nil
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	margin := defaultExpiryMargin
	if cfg.ExpiryMargin != "" {
		d, err := time.ParseDuration(cfg.ExpiryMargin)
		if err != nil {
			return nil, fmt.Errorf("auth: invalid expiry_margin %q: %w", cfg.ExpiryMargin, err)
		}
		if d > 0 {
			margin = d
		}
	}
	tm := &tokenManager{
		cfg:           cfg,
		client:        client,
		margin:        margin,
		username:      os.Getenv(cfg.UsernameEnv),
		password:      os.Getenv(cfg.PasswordEnv),
		clientID:      os.Getenv(cfg.ClientIDEnv),
		clientSecret:  os.Getenv(cfg.ClientSecretEnv),
		staticAccess:  os.Getenv(cfg.AccessTokenEnv),
		staticRefresh: os.Getenv(cfg.RefreshTokenEnv),
	}
	switch cfg.Type {
	case AuthStatic:
		if tm.staticAccess == "" {
			return nil, fmt.Errorf("auth: static type needs access_token_env (%q is empty)", cfg.AccessTokenEnv)
		}
	case AuthJWTLogin, AuthOAuth2Password:
		if cfg.TokenURL == "" {
			return nil, fmt.Errorf("auth: %s needs a token_url", cfg.Type)
		}
	case AuthOAuth2ClientCreds:
		if cfg.TokenURL == "" {
			return nil, fmt.Errorf("auth: %s needs a token_url", cfg.Type)
		}
	}
	return tm, nil
}

// Token returns a valid bearer, acquiring or refreshing if necessary.
func (t *tokenManager) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.access != "" && t.valid() {
		return t.access, nil
	}
	return t.ensureLocked(ctx)
}

// Refresh forces a token refresh and returns the new bearer. Used reactively
// when a request gets a 401 (the cached token was rejected early).
func (t *tokenManager) Refresh(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// Prefer a refresh-token exchange; fall back to a fresh login.
	if t.refresh != "" {
		if err := t.doRefreshLocked(ctx); err == nil {
			return t.access, nil
		}
	}
	if err := t.acquireLocked(ctx); err != nil {
		return "", err
	}
	return t.access, nil
}

// valid reports whether the cached access token is still within its lifetime
// (minus the safety margin). A zero expiry means "no known expiry" → valid.
func (t *tokenManager) valid() bool {
	if t.expiry.IsZero() {
		return true
	}
	return time.Now().Before(t.expiry.Add(-t.margin))
}

// ensureLocked obtains a usable token assuming the lock is held: refresh if a
// refresh token is available, otherwise (re-)login.
func (t *tokenManager) ensureLocked(ctx context.Context) (string, error) {
	if t.refresh != "" {
		if err := t.doRefreshLocked(ctx); err == nil {
			return t.access, nil
		}
		// Refresh token rejected/expired → fall through to a fresh login.
	}
	if err := t.acquireLocked(ctx); err != nil {
		return "", err
	}
	return t.access, nil
}

// acquireLocked performs the initial token acquisition for the grant type.
func (t *tokenManager) acquireLocked(ctx context.Context) error {
	switch t.cfg.Type {
	case AuthStatic:
		if t.staticAccess == "" {
			return fmt.Errorf("auth: re-authentication required (static access token unavailable)")
		}
		t.access = t.staticAccess
		t.refresh = t.staticRefresh
		t.expiry = time.Time{}
		return nil

	case AuthOAuth2Password:
		if t.username == "" {
			return fmt.Errorf("auth: re-authentication required (username env %q empty)", t.cfg.UsernameEnv)
		}
		form := url.Values{}
		form.Set("grant_type", "password")
		form.Set("username", t.username)
		form.Set("password", t.password)
		t.addClientAndScope(form)
		return t.postToken(ctx, t.cfg.TokenURL, form)

	case AuthOAuth2ClientCreds:
		if t.clientID == "" {
			return fmt.Errorf("auth: re-authentication required (client_id env %q empty)", t.cfg.ClientIDEnv)
		}
		form := url.Values{}
		form.Set("grant_type", "client_credentials")
		t.addClientAndScope(form)
		return t.postToken(ctx, t.cfg.TokenURL, form)

	case AuthJWTLogin:
		if t.username == "" {
			return fmt.Errorf("auth: re-authentication required (username env %q empty)", t.cfg.UsernameEnv)
		}
		body, _ := json.Marshal(map[string]string{"username": t.username, "password": t.password})
		return t.postJSON(ctx, t.cfg.TokenURL, body)

	default:
		return fmt.Errorf("auth: unsupported grant type %q", t.cfg.Type)
	}
}

// doRefreshLocked exchanges the refresh token for a new access token.
func (t *tokenManager) doRefreshLocked(ctx context.Context) error {
	if t.refresh == "" {
		return fmt.Errorf("auth: no refresh token")
	}
	refreshURL := t.cfg.RefreshURL
	if refreshURL == "" {
		refreshURL = t.cfg.TokenURL
	}
	if refreshURL == "" {
		return fmt.Errorf("auth: no refresh/token URL")
	}
	switch t.cfg.Type {
	case AuthJWTLogin:
		body, _ := json.Marshal(map[string]string{"refresh_token": t.refresh})
		return t.postJSON(ctx, refreshURL, body)
	default: // oauth2 password / client-credentials
		form := url.Values{}
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", t.refresh)
		t.addClientAndScope(form)
		return t.postToken(ctx, refreshURL, form)
	}
}

// addClientAndScope adds optional client_id/secret and scope to an OAuth2 form.
func (t *tokenManager) addClientAndScope(form url.Values) {
	if t.clientID != "" {
		form.Set("client_id", t.clientID)
	}
	if t.clientSecret != "" {
		form.Set("client_secret", t.clientSecret)
	}
	if t.cfg.Scope != "" {
		form.Set("scope", t.cfg.Scope)
	}
}

// postToken POSTs an OAuth2 form (or JSON, when content_type=json) to endpoint
// and stores the parsed tokens. Assumes the lock is held.
func (t *tokenManager) postToken(ctx context.Context, endpoint string, form url.Values) error {
	if t.cfg.ContentType == "json" {
		body, _ := json.Marshal(formToMap(form))
		return t.postJSON(ctx, endpoint, body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// OAuth2 also allows client creds via HTTP Basic; send both forms is harmless
	// but we only set Basic when an id is present and not already in the body.
	if t.clientID != "" && form.Get("client_id") == "" {
		req.SetBasicAuth(t.clientID, t.clientSecret)
	}
	return t.do(req)
}

// postJSON POSTs a JSON body (custom login / refresh) and stores parsed tokens.
func (t *tokenManager) postJSON(ctx context.Context, endpoint string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return t.do(req)
}

func (t *tokenManager) do(req *http.Request) error {
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("auth: token request failed: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Do NOT echo the response body verbatim into the error: it may contain
		// token material. Surface status only.
		return fmt.Errorf("auth: token endpoint returned HTTP %d", resp.StatusCode)
	}
	return t.store(data)
}

// store parses a token response (JSON, or form-encoded fallback) and updates the
// cached access/refresh/expiry. A response that omits a refresh token keeps the
// previous one (refresh responses commonly do this).
func (t *tokenManager) store(data []byte) error {
	access, refresh, expiresIn, ok := t.parseTokenResponse(data)
	if !ok || access == "" {
		return fmt.Errorf("auth: token response missing access token")
	}
	t.access = access
	if refresh != "" {
		t.refresh = refresh
	}
	switch {
	case expiresIn > 0:
		t.expiry = time.Now().Add(time.Duration(expiresIn) * time.Second)
	case jwtExp(access) > 0:
		t.expiry = time.Unix(jwtExp(access), 0)
	default:
		t.expiry = time.Time{} // unknown → rely on reactive 401 refresh
	}
	return nil
}

// parseTokenResponse extracts access/refresh/expires_in from a token response,
// honoring configured field paths and falling back to the OAuth2 standard names.
// It accepts JSON objects and, failing that, x-www-form-urlencoded bodies.
func (t *tokenManager) parseTokenResponse(data []byte) (access, refresh string, expiresIn int64, ok bool) {
	accField := orDefault(t.cfg.AccessTokenField, "access_token")
	refField := orDefault(t.cfg.RefreshTokenField, "refresh_token")
	expField := orDefault(t.cfg.ExpiresInField, "expires_in")

	var obj map[string]any
	if json.Unmarshal(data, &obj) == nil && obj != nil {
		access = asString(getPath(obj, accField))
		refresh = asString(getPath(obj, refField))
		expiresIn = asInt64(getPath(obj, expField))
		return access, refresh, expiresIn, true
	}
	// Form-encoded fallback (some legacy OAuth2 servers).
	if vals, err := url.ParseQuery(string(data)); err == nil {
		access = vals.Get(accField)
		refresh = vals.Get(refField)
		if n, e := strconv.ParseInt(vals.Get(expField), 10, 64); e == nil {
			expiresIn = n
		}
		if access != "" {
			return access, refresh, expiresIn, true
		}
	}
	return "", "", 0, false
}

// ---- helpers ----

func formToMap(form url.Values) map[string]string {
	m := make(map[string]string, len(form))
	for k := range form {
		m[k] = form.Get(k)
	}
	return m
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// getPath resolves a dotted path ("a.b.c") within a decoded JSON object.
func getPath(obj map[string]any, path string) any {
	parts := strings.Split(path, ".")
	var cur any = obj
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	default:
		return 0
	}
}

// jwtExp returns the "exp" (seconds since epoch) claim of a JWT, or 0 if the
// token is not a parseable JWT or has no exp. Signature is NOT verified — exp is
// used only as a refresh hint.
func jwtExp(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some encoders pad; try standard URL encoding too.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return 0
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return 0
	}
	return claims.Exp
}
