package channels

// Bot Framework authentication for the Teams adapter, both directions:
//
//   - OUTBOUND: an Entra (Azure AD) client-credentials token, cached until it
//     is close to expiry, sent as the Bearer on Connector API calls.
//   - INBOUND: verification of the JWT Microsoft signs every activity with.
//     Signature (RS256 against the Bot Framework JWKS), issuer, audience and
//     validity window are all checked, plus the serviceurl claim when present.
//
// Inbound verification is not optional and has no config switch. The endpoint
// is reachable from the public internet through the operator's tunnel, and the
// only thing separating a real Teams activity from a forged POST is this token
// — WhatsApp's app_secret is optional because Meta's console makes it so, but
// Bot Framework always sends the token, so accepting an unsigned one would be
// choosing to be spoofable.
//
// Implemented on crypto/rsa + encoding/json rather than a JWT library: the
// verification surface is one algorithm and four claims, which is smaller than
// the dependency would be (same reasoning as telegram.go's raw Bot API calls).

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// teamsLoginBase is the Entra token authority.
	teamsLoginBase = "https://login.microsoftonline.com"
	// teamsMultiTenantAuthority is the tenant segment used by multi-tenant
	// bots; single-tenant bots substitute their own tenant id.
	teamsMultiTenantAuthority = "botframework.com"
	// teamsTokenScope is the Connector API scope requested for outbound calls.
	teamsTokenScope = "https://api.botframework.com/.default"
	// teamsOpenIDConfigURL advertises the JWKS that signs inbound activities
	// delivered by the Bot Connector.
	teamsOpenIDConfigURL = "https://login.botframework.com/v1/.well-known/openidconfiguration"
	// teamsConnectorIssuer is the issuer on those inbound tokens.
	teamsConnectorIssuer = "https://api.botframework.com"
	// teamsClockSkew is the tolerance applied to exp/nbf, covering ordinary
	// clock drift between Microsoft's signer and this host.
	teamsClockSkew = 5 * time.Minute
	// teamsTokenRefreshMargin refreshes the outbound token this long before it
	// actually expires, so a send never races the expiry.
	teamsTokenRefreshMargin = 5 * time.Minute
	// teamsKeyCacheTTL is how long the signing keys are reused before a
	// scheduled refetch. Microsoft rotates them slowly; an unknown kid forces
	// an immediate refresh regardless (rate-limited by teamsKeyRefetchMin).
	teamsKeyCacheTTL = 24 * time.Hour
	// teamsKeyRefetchMin is the minimum gap between JWKS fetches triggered by
	// an unknown kid, so a stream of forged tokens cannot turn this adapter
	// into a request amplifier against Microsoft.
	teamsKeyRefetchMin = 5 * time.Minute
)

// teamsAuth holds the bot credentials and both token caches. It is safe for
// concurrent use: the inbound webhook handler and outbound senders share one.
type teamsAuth struct {
	appID     string
	appSecret string
	tenantID  string
	client    *http.Client

	// tokenURL and openIDConfigURL are the live Microsoft endpoints, overridden
	// in tests so no test reaches the network.
	tokenURL        string
	openIDConfigURL string

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
	keys        map[string]*rsa.PublicKey
	keysFetched time.Time
}

// newTeamsAuth builds the auth helper for a bot registration.
func newTeamsAuth(appID, appSecret, tenantID string, client *http.Client) *teamsAuth {
	authority := strings.TrimSpace(tenantID)
	if authority == "" {
		authority = teamsMultiTenantAuthority
	}
	return &teamsAuth{
		appID:           appID,
		appSecret:       appSecret,
		tenantID:        strings.TrimSpace(tenantID),
		client:          client,
		tokenURL:        fmt.Sprintf("%s/%s/oauth2/v2.0/token", teamsLoginBase, authority),
		openIDConfigURL: teamsOpenIDConfigURL,
	}
}

// ---------------------------------------------------------------------------
// Outbound: client-credentials access token.
// ---------------------------------------------------------------------------

// teamsTokenResponse is the subset of the Entra token response we consume.
type teamsTokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

// accessToken returns a valid Connector API bearer token, fetching a new one
// when the cache is empty or close to expiring.
func (a *teamsAuth) accessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	if a.token != "" && time.Now().Add(teamsTokenRefreshMargin).Before(a.tokenExpiry) {
		tok := a.token
		a.mu.Unlock()
		return tok, nil
	}
	a.mu.Unlock()

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", a.appID)
	form.Set("client_secret", a.appSecret)
	form.Set("scope", teamsTokenScope)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("teams: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("teams: token endpoint unreachable: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	var tr teamsTokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", fmt.Errorf("teams: decode token response (HTTP %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || tr.AccessToken == "" {
		detail := tr.ErrorDesc
		if detail == "" {
			detail = tr.Error
		}
		if detail == "" {
			detail = strings.TrimSpace(string(body))
		}
		return "", fmt.Errorf("teams: could not get an access token (HTTP %d): %s — check app_id, app_secret%s",
			resp.StatusCode, detail, a.tenantHint())
	}

	// A missing expires_in would otherwise cache a token as already expired,
	// re-fetching on every send; an hour is Entra's standard lifetime.
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	a.mu.Lock()
	a.token = tr.AccessToken
	a.tokenExpiry = time.Now().Add(ttl)
	a.mu.Unlock()
	return tr.AccessToken, nil
}

// tenantHint adds the tenant to credential errors when one is configured,
// because "wrong tenant" and "wrong secret" look identical otherwise.
func (a *teamsAuth) tenantHint() string {
	if a.tenantID == "" {
		return ""
	}
	return " and tenant_id " + a.tenantID
}

// invalidateToken drops the cached token so the next call fetches a fresh one.
// Called after a 401 from the Connector API: the token may have been revoked
// before its stated expiry.
func (a *teamsAuth) invalidateToken() {
	a.mu.Lock()
	a.token = ""
	a.tokenExpiry = time.Time{}
	a.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Inbound: activity JWT verification.
// ---------------------------------------------------------------------------

// teamsClaims is the subset of an inbound activity token we validate.
type teamsClaims struct {
	Issuer     string `json:"iss"`
	Audience   string `json:"aud"`
	Expiry     int64  `json:"exp"`
	NotBefore  int64  `json:"nbf"`
	ServiceURL string `json:"serviceurl"`
}

// verifyActivityToken authenticates one inbound activity. serviceURL is the
// value from the activity body; when the token carries a serviceurl claim the
// two must agree, which is what stops a replayed token from redirecting our
// replies to an attacker's endpoint.
func (a *teamsAuth) verifyActivityToken(ctx context.Context, authHeader, serviceURL string) error {
	raw := strings.TrimSpace(authHeader)
	if raw == "" {
		return fmt.Errorf("no Authorization header (Bot Framework always signs activities)")
	}
	if !strings.HasPrefix(strings.ToLower(raw), "bearer ") {
		return fmt.Errorf("Authorization header is not a Bearer token")
	}
	raw = strings.TrimSpace(raw[len("bearer "):])

	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return fmt.Errorf("malformed JWT (%d segments, want 3)", len(parts))
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("decode JWT header: %w", err)
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &hdr); err != nil {
		return fmt.Errorf("parse JWT header: %w", err)
	}
	// Pinning the algorithm is what closes the "alg: none" and
	// HMAC-with-the-public-key substitution attacks.
	if hdr.Alg != "RS256" {
		return fmt.Errorf("unexpected signing algorithm %q (want RS256)", hdr.Alg)
	}

	key, err := a.signingKey(ctx, hdr.Kid)
	if err != nil {
		return err
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("decode JWT signature: %w", err)
	}
	signed := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, signed[:], sig); err != nil {
		return fmt.Errorf("JWT signature does not verify against key %q: %w", hdr.Kid, err)
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("decode JWT claims: %w", err)
	}
	var claims teamsClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return fmt.Errorf("parse JWT claims: %w", err)
	}
	return a.checkClaims(claims, serviceURL)
}

// checkClaims validates issuer, audience, validity window and serviceurl.
func (a *teamsAuth) checkClaims(claims teamsClaims, serviceURL string) error {
	if !a.issuerAllowed(claims.Issuer) {
		return fmt.Errorf("unexpected token issuer %q", claims.Issuer)
	}
	// The audience is this bot's app id: a token minted for a different bot is
	// perfectly valid and still must not be accepted here.
	if claims.Audience != a.appID {
		return fmt.Errorf("token audience %q is not this bot's app_id", claims.Audience)
	}
	now := time.Now()
	if claims.Expiry > 0 && now.After(time.Unix(claims.Expiry, 0).Add(teamsClockSkew)) {
		return fmt.Errorf("token expired at %s", time.Unix(claims.Expiry, 0).UTC().Format(time.RFC3339))
	}
	if claims.NotBefore > 0 && now.Add(teamsClockSkew).Before(time.Unix(claims.NotBefore, 0)) {
		return fmt.Errorf("token not valid until %s", time.Unix(claims.NotBefore, 0).UTC().Format(time.RFC3339))
	}
	if claims.ServiceURL != "" && serviceURL != "" &&
		strings.TrimRight(claims.ServiceURL, "/") != strings.TrimRight(serviceURL, "/") {
		return fmt.Errorf("token serviceurl %q does not match the activity's %q",
			claims.ServiceURL, serviceURL)
	}
	return nil
}

// issuerAllowed reports whether the issuer is one this bot accepts: the Bot
// Connector always, plus the configured tenant's own issuer for single-tenant
// registrations.
func (a *teamsAuth) issuerAllowed(iss string) bool {
	if iss == teamsConnectorIssuer {
		return true
	}
	if a.tenantID != "" {
		return iss == fmt.Sprintf("%s/%s/v2.0", teamsLoginBase, a.tenantID)
	}
	return false
}

// signingKey resolves a kid to its public key, fetching the JWKS on a cache
// miss or when the cache has aged out.
func (a *teamsAuth) signingKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if kid == "" {
		return nil, fmt.Errorf("JWT header has no kid")
	}

	a.mu.Lock()
	key, ok := a.keys[kid]
	fresh := time.Since(a.keysFetched) < teamsKeyCacheTTL
	canRefetch := time.Since(a.keysFetched) >= teamsKeyRefetchMin
	a.mu.Unlock()

	if ok && fresh {
		return key, nil
	}
	if !ok && !canRefetch {
		// Refusing rather than refetching keeps a flood of unknown-kid tokens
		// from turning into a flood of requests to Microsoft.
		return nil, fmt.Errorf("unknown signing key %q (refetch rate-limited)", kid)
	}

	if err := a.refreshKeys(ctx); err != nil {
		// A stale copy of a key we already trust is better than dropping a real
		// activity because Microsoft's metadata endpoint had a bad minute.
		if ok {
			return key, nil
		}
		return nil, err
	}

	a.mu.Lock()
	key, ok = a.keys[kid]
	a.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("signing key %q is not in the Bot Framework JWKS", kid)
	}
	return key, nil
}

// teamsJWK is one key from the JWKS document.
type teamsJWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// refreshKeys fetches the OpenID metadata, then the JWKS it points at, and
// replaces the key cache.
func (a *teamsAuth) refreshKeys(ctx context.Context) error {
	// Record the attempt up front so a failing endpoint is retried on the
	// teamsKeyRefetchMin schedule rather than on every inbound request.
	a.mu.Lock()
	a.keysFetched = time.Now()
	a.mu.Unlock()

	var meta struct {
		JwksURI string `json:"jwks_uri"`
	}
	if err := a.getJSON(ctx, a.openIDConfigURL, &meta); err != nil {
		return fmt.Errorf("teams: fetch OpenID metadata: %w", err)
	}
	if meta.JwksURI == "" {
		return fmt.Errorf("teams: OpenID metadata has no jwks_uri")
	}

	var doc struct {
		Keys []teamsJWK `json:"keys"`
	}
	if err := a.getJSON(ctx, meta.JwksURI, &doc); err != nil {
		return fmt.Errorf("teams: fetch JWKS: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		pub, err := teamsRSAKey(k)
		if err != nil {
			// One malformed entry must not discard the rest of the document.
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return fmt.Errorf("teams: JWKS contained no usable RSA keys")
	}

	a.mu.Lock()
	a.keys = keys
	a.mu.Unlock()
	return nil
}

// getJSON performs a GET and decodes the body into v.
func (a *teamsAuth) getJSON(ctx context.Context, endpoint string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, endpoint)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// teamsRSAKey rebuilds an RSA public key from a JWK's base64url modulus and
// exponent.
func teamsRSAKey(k teamsJWK) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.N, "="))
	if err != nil {
		return nil, fmt.Errorf("decode modulus: %w", err)
	}
	eb, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(k.E, "="))
	if err != nil {
		return nil, fmt.Errorf("decode exponent: %w", err)
	}
	if len(nb) == 0 || len(eb) == 0 || len(eb) > 8 {
		return nil, fmt.Errorf("implausible key material")
	}
	// The exponent is a big-endian integer of whatever length the issuer chose
	// (almost always 3 bytes, 65537); left-pad it into a uint64.
	padded := make([]byte, 8)
	copy(padded[8-len(eb):], eb)
	e := binary.BigEndian.Uint64(padded)
	if e == 0 || e > 1<<31 {
		return nil, fmt.Errorf("implausible exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(e)}, nil
}
