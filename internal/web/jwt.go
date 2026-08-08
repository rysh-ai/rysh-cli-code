package web

// Minimal HS256 JSON Web Tokens for the web UI's username/password login.
//
// Hand-rolled on crypto/hmac + encoding/json for the same reason
// internal/channels/teams_auth.go is: one algorithm, one issuer, one audience,
// no key rotation — a JWT library would be more dependency than code. Only the
// three claims the login flow actually uses are emitted (sub/iat/exp), and
// verification rejects anything that is not HS256 (so the "alg": "none"
// downgrade cannot reach the signature check at all).
//
// There is deliberately no refresh token: the access token lives for a month
// (AccessTokenTTL) and when it expires the browser shows the login form again.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// AccessTokenTTL is how long a successful login stays valid: one month.
const AccessTokenTTL = 30 * 24 * time.Hour

// JWT verification failures. They are distinct so the middleware can tell an
// expired login (show the login page again) from a forged one, but the HTTP
// response never discloses which — see authMiddleware.
var (
	errJWTMalformed = errors.New("malformed token")
	errJWTAlgorithm = errors.New("unsupported token algorithm")
	errJWTSignature = errors.New("bad token signature")
	errJWTExpired   = errors.New("token expired")
)

// jwtHeader is the only header this server issues or accepts.
type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// jwtClaims is the payload: who logged in, when, and until when.
type jwtClaims struct {
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

// signJWT mints an HS256 token for subject, valid for ttl from issuedAt.
func signJWT(secret []byte, subject string, issuedAt time.Time, ttl time.Duration) (string, error) {
	head, err := json.Marshal(jwtHeader{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(jwtClaims{
		Sub: subject,
		Iat: issuedAt.Unix(),
		Exp: issuedAt.Add(ttl).Unix(),
	})
	if err != nil {
		return "", err
	}
	signing := b64(head) + "." + b64(body)
	return signing + "." + b64(jwtSignature(secret, signing)), nil
}

// parseJWT verifies a token's algorithm, signature and expiry against now, and
// returns its claims. An empty secret always fails: an unset signing key must
// never be a key that validates everything.
func parseJWT(secret []byte, token string, now time.Time) (jwtClaims, error) {
	var claims jwtClaims
	if len(secret) == 0 {
		return claims, errJWTSignature
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, errJWTMalformed
	}
	rawHead, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return claims, errJWTMalformed
	}
	var head jwtHeader
	if err := json.Unmarshal(rawHead, &head); err != nil {
		return claims, errJWTMalformed
	}
	// Checked before the signature so "alg": "none" is refused outright rather
	// than being handed to an HMAC that would compare against an empty MAC.
	if head.Alg != "HS256" {
		return claims, errJWTAlgorithm
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return claims, errJWTMalformed
	}
	if !hmac.Equal(sig, jwtSignature(secret, parts[0]+"."+parts[1])) {
		return claims, errJWTSignature
	}
	rawBody, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, errJWTMalformed
	}
	if err := json.Unmarshal(rawBody, &claims); err != nil {
		return claims, errJWTMalformed
	}
	// A token with no exp is not "valid forever" — it is not one of ours.
	if claims.Exp == 0 {
		return claims, errJWTMalformed
	}
	if now.Unix() >= claims.Exp {
		return claims, errJWTExpired
	}
	return claims, nil
}

// jwtSignature is the HMAC-SHA256 over "<header>.<payload>".
func jwtSignature(secret []byte, signingInput string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	return mac.Sum(nil)
}

// b64 is JWT's unpadded base64url.
func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
