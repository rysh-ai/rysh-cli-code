package web

// Username/password credentials for the web UI, and where they live.
//
// This login is the only way into the web UI (auth.go); the ?token= bearer
// secret it replaced is gone. Credentials are set with
// `##rysh web auth username=<u> password=<p>` (or `##rysh web start
// --username <u> --password <p>`) and persist in the project's rysh dir — rysh
// state is always project-local (see internal/config: there is no ~/.rysh), so
// the credentials that guard a workspace's web UI are stored beside that
// workspace's sessions.
//
// The file holds a bcrypt hash, never the password, plus the HS256 signing key
// that mints access tokens. The key is regenerated every time credentials are
// set, so changing the password logs every browser out — no revocation list is
// needed to make a password change mean something.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// credentialsFileName is the per-workspace credentials file inside the rysh dir.
const credentialsFileName = "web-auth.json"

// Credentials is the on-disk shape of web-auth.json.
type Credentials struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	// Secret is the base64url HS256 signing key for issued access tokens.
	Secret    string `json:"secret"`
	UpdatedAt string `json:"updated_at"`
}

// CredentialsPath returns the credentials file for a rysh dir.
func CredentialsPath(ryshDir string) string {
	return filepath.Join(ryshDir, credentialsFileName)
}

// LoadCredentials reads the credentials for a rysh dir. A missing file is not
// an error — it means username/password auth is simply not configured, and
// (nil, nil) is returned. A file that exists but is unusable IS an error: a
// corrupt credentials file must not silently downgrade to "no auth".
func LoadCredentials(ryshDir string) (*Credentials, error) {
	if ryshDir == "" {
		return nil, nil
	}
	data, err := os.ReadFile(CredentialsPath(ryshDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read web credentials: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", CredentialsPath(ryshDir), err)
	}
	if c.Username == "" || c.PasswordHash == "" || c.Secret == "" {
		return nil, fmt.Errorf("%s is incomplete — set credentials again with `##rysh web auth username=<u> password=<p>`", CredentialsPath(ryshDir))
	}
	return &c, nil
}

// SaveCredentials hashes password, mints a fresh signing key, and writes the
// credentials file (0600 in a 0700 dir). Returns what was written.
func SaveCredentials(ryshDir, username, password string) (*Credentials, error) {
	if ryshDir == "" {
		return nil, errors.New("no rysh dir to store web credentials in")
	}
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, errors.New("username must not be empty")
	}
	if password == "" {
		return nil, errors.New("password must not be empty")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	secret, err := newSigningKey()
	if err != nil {
		return nil, err
	}
	c := &Credentials{
		Username:     username,
		PasswordHash: string(hash),
		Secret:       secret,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(ryshDir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", ryshDir, err)
	}
	if err := os.WriteFile(CredentialsPath(ryshDir), append(data, '\n'), 0o600); err != nil {
		return nil, fmt.Errorf("write web credentials: %w", err)
	}
	return c, nil
}

// ClearCredentials removes the credentials file, disabling username/password
// auth. Reports whether a file was actually there to remove.
func ClearCredentials(ryshDir string) (bool, error) {
	if ryshDir == "" {
		return false, nil
	}
	err := os.Remove(CredentialsPath(ryshDir))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("remove web credentials: %w", err)
}

// Verify reports whether a login attempt matches. bcrypt runs even when the
// username is wrong so a bad username and a bad password cost the same time,
// and neither answer is distinguishable from the outside.
func (c *Credentials) Verify(username, password string) bool {
	if c == nil {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(c.Username)) == 1
	passOK := bcrypt.CompareHashAndPassword([]byte(c.PasswordHash), []byte(password)) == nil
	return userOK && passOK
}

// SigningKey decodes the HS256 key used to sign and verify access tokens.
// Returns nil when the stored key is unusable — parseJWT rejects an empty key,
// so a broken file fails every token rather than accepting every token.
func (c *Credentials) SigningKey() []byte {
	if c == nil {
		return nil
	}
	key, err := base64.RawURLEncoding.DecodeString(c.Secret)
	if err != nil {
		return nil
	}
	return key
}

// newSigningKey returns a fresh 256-bit HS256 key, base64url-encoded.
func newSigningKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate signing key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
