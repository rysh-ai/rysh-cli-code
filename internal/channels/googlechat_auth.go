// SPDX-License-Identifier: Apache-2.0

package channels

// Service-account authentication for the Google Chat adapter.
//
// Google does not hand out a long-lived bot token the way Slack, Telegram and
// Discord do. A Chat app authenticates as a SERVICE ACCOUNT: it signs a JWT
// with the account's private key, exchanges that assertion for an OAuth2
// access token, and uses the token as a Bearer for one hour. So the stored
// credential is an RSA private key, and minting is this file's job.
//
// Implemented on crypto/rsa + encoding/json rather than google.golang.org/api:
// the surface actually needed is one signing algorithm and one token exchange,
// which is smaller than the dependency tree would be — the same reasoning
// teams_auth.go and telegram.go already apply.
//
// The one token is used for BOTH directions, because both are Google APIs:
// pulling events from Pub/Sub and posting messages to Chat. They need different
// scopes, so the scope list is a parameter rather than a constant.

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// googleTokenEndpoint is where a signed assertion is exchanged for an
	// access token. Overridable per-instance in tests.
	googleTokenEndpoint = "https://oauth2.googleapis.com/token"
	// googleJWTGrantType is the RFC 7523 assertion grant.
	googleJWTGrantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	// googleAssertionLifetime is how long a signed assertion claims validity.
	// Google caps this at an hour; the assertion is single-use anyway.
	googleAssertionLifetime = time.Hour
	// googleTokenRefreshMargin refreshes ahead of expiry so a call in flight
	// never races the boundary (same discipline as teams/wechat/wecom).
	googleTokenRefreshMargin = 5 * time.Minute

	// scopeChatBot lets the app post to Chat as itself.
	scopeChatBot = "https://www.googleapis.com/auth/chat.bot"
	// scopePubSub lets the app pull and acknowledge its subscription.
	scopePubSub = "https://www.googleapis.com/auth/pubsub"
)

// googleServiceAccount is the subset of a service-account key file that matters
// here. The JSON is what the Google Cloud console downloads.
type googleServiceAccount struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// googleAuth mints and caches an access token for one service account. It is
// safe for concurrent use: the inbound pull loop and outbound sends share one.
type googleAuth struct {
	sa     *googleServiceAccount
	key    *rsa.PrivateKey
	scopes []string
	client *http.Client
	// tokenURI is resolved from the key file, falling back to the public
	// endpoint. Tests point it at an httptest server.
	tokenURI string

	mu    sync.Mutex
	token string
	exp   time.Time
}

// parseGoogleServiceAccount reads a service-account key JSON and prepares the
// signing key.
//
// Validation is strict and up front because every failure here is a
// configuration mistake that would otherwise surface as a confusing 401 on the
// first message, long after the operator stopped looking.
func parseGoogleServiceAccount(raw string) (*googleServiceAccount, *rsa.PrivateKey, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil, fmt.Errorf("service account JSON is empty")
	}
	var sa googleServiceAccount
	if err := json.Unmarshal([]byte(raw), &sa); err != nil {
		return nil, nil, fmt.Errorf("service account JSON is not valid JSON: %w", err)
	}
	if sa.Type != "" && sa.Type != "service_account" {
		return nil, nil, fmt.Errorf("credentials are of type %q, want \"service_account\" "+
			"(an OAuth client ID file will not work)", sa.Type)
	}
	if sa.ClientEmail == "" {
		return nil, nil, fmt.Errorf("service account JSON has no client_email")
	}
	if sa.PrivateKey == "" {
		return nil, nil, fmt.Errorf("service account JSON has no private_key")
	}
	key, err := parseRSAPrivateKey(sa.PrivateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("service account private_key: %w", err)
	}
	return &sa, key, nil
}

// parseRSAPrivateKey decodes a PEM private key, accepting both the PKCS#8 form
// Google emits and the older PKCS#1 form, since a key pasted from elsewhere may
// be either.
func parseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("not a PEM block (is the \\n escaping intact?)")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rsaKey, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("key is %T, want an RSA key", k)
		}
		return rsaKey, nil
	}
	k, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("unparseable as PKCS#8 or PKCS#1: %w", err)
	}
	return k, nil
}

// newGoogleAuth builds the auth helper for a service account key.
func newGoogleAuth(credentialsJSON string, scopes []string, client *http.Client) (*googleAuth, error) {
	sa, key, err := parseGoogleServiceAccount(credentialsJSON)
	if err != nil {
		return nil, err
	}
	tokenURI := sa.TokenURI
	if tokenURI == "" {
		tokenURI = googleTokenEndpoint
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &googleAuth{sa: sa, key: key, scopes: scopes, client: client, tokenURI: tokenURI}, nil
}

// projectID reports the project the key belongs to, used as a default when the
// channel config does not name one.
func (a *googleAuth) projectID() string { return a.sa.ProjectID }

// clientEmail is the service account's identity, surfaced in Status so an
// operator can check it against the Pub/Sub IAM binding without opening the key.
func (a *googleAuth) clientEmail() string { return a.sa.ClientEmail }

// accessToken returns a valid bearer token, minting one when the cache is empty
// or close to expiring.
func (a *googleAuth) accessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Until(a.exp) > googleTokenRefreshMargin {
		return a.token, nil
	}

	assertion, err := a.signAssertion()
	if err != nil {
		return "", fmt.Errorf("sign assertion: %w", err)
	}

	form := url.Values{}
	form.Set("grant_type", googleJWTGrantType)
	form.Set("assertion", assertion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tr.Error != "" || tr.AccessToken == "" {
		// The description is where Google says useful things like "Invalid JWT
		// Signature" or which API is disabled, so it is kept rather than
		// flattened into the code.
		detail := tr.Error
		if tr.ErrorDesc != "" {
			detail += ": " + tr.ErrorDesc
		}
		if detail == "" {
			detail = "no access_token in response"
		}
		return "", fmt.Errorf("token exchange refused (%s)", detail)
	}

	lifetime := time.Duration(tr.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = time.Hour
	}
	a.token = tr.AccessToken
	a.exp = time.Now().Add(lifetime)
	return a.token, nil
}

// invalidate drops the cached token so the next call remints. Used when the API
// reports a token problem, which can happen for reasons outside this process.
func (a *googleAuth) invalidate() {
	a.mu.Lock()
	a.token = ""
	a.exp = time.Time{}
	a.mu.Unlock()
}

// signAssertion builds and RS256-signs the JWT Google exchanges for a token.
func (a *googleAuth) signAssertion() (string, error) {
	now := time.Now()
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	claims := map[string]any{
		"iss":   a.sa.ClientEmail,
		"scope": strings.Join(a.scopes, " "),
		// The audience is the token endpoint itself, per RFC 7523 — NOT the API
		// being called. Using the API URL here yields "Invalid JWT" with no
		// hint as to why.
		"aud": a.tokenURI,
		"iat": now.Unix(),
		"exp": now.Add(googleAssertionLifetime).Unix(),
	}

	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signingInput := b64url(hb) + "." + b64url(cb)

	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + b64url(sig), nil
}

// b64url is base64url without padding, which is what JWT requires.
func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
