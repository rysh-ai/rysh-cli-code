// SPDX-License-Identifier: Apache-2.0

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

// credentialsFileName is the project-wide credentials file inside the rysh dir.
// It is the fallback, and what every install had before logins became
// per-session (see CredentialsRef).
const credentialsFileName = "web-auth.json"

// CredentialsRef says WHICH login a caller means: a named session's, or the
// project-wide one.
//
// Logins started out per project, on the reasoning that rysh state is
// project-local and sibling daemons rooted at one project should share a
// password (that sharing is what F-9 is about — auth.go). But a session is what
// gets served: two sessions in one project can be two different doors, on two
// ports, published at two URLs, shown to two different sets of people. Making
// them share one password meant the second session to set one silently changed
// the first session's, and logged its browsers out.
//
// So a session's login lives at <ryshDir>/web/<session>-auth.json, beside its
// web settings. Reads fall back to the project file when a session has none, so
// every existing install keeps working and a session inherits the project login
// until it sets its own.
type CredentialsRef struct {
	RyshDir string
	// Session names the session whose login is meant. Empty means the
	// project-wide file — which is what the fallback resolves to anyway.
	Session string
}

// ProjectCredentials refers to the project-wide login.
func ProjectCredentials(ryshDir string) CredentialsRef {
	return CredentialsRef{RyshDir: ryshDir}
}

// SessionCredentials refers to one session's login.
func SessionCredentials(ryshDir, session string) CredentialsRef {
	return CredentialsRef{RyshDir: ryshDir, Session: session}
}

// OwnPath is where this ref's credentials are WRITTEN: the session's own file,
// or the project file when no session is named.
func (r CredentialsRef) OwnPath() string {
	if r.RyshDir == "" {
		return ""
	}
	if session := sanitizeSessionName(r.Session); session != "" {
		return filepath.Join(r.RyshDir, "web", session+"-auth.json")
	}
	return CredentialsPath(r.RyshDir)
}

// Path is where this ref's credentials are READ from: its own file when that
// exists, else the project-wide one. The fallback is what lets a session
// inherit the project login until it sets one of its own.
func (r CredentialsRef) Path() string {
	own := r.OwnPath()
	if own == "" {
		return ""
	}
	if _, err := os.Stat(own); err == nil {
		return own
	}
	return CredentialsPath(r.RyshDir)
}

// HasOwn reports whether this ref has credentials of its own, as opposed to
// inheriting the project's. The difference matters: an inherited login is what
// a session serves only while it has nothing to say about the matter.
func (r CredentialsRef) HasOwn() bool {
	own := r.OwnPath()
	if own == "" || own == CredentialsPath(r.RyshDir) {
		return false
	}
	_, err := os.Stat(own)
	return err == nil
}

// sanitizeSessionName reduces a session name to a safe single filename segment.
// Anything outside [A-Za-z0-9._-] becomes '-', so a session name can never
// escape the directory or collide with the project file.
func sanitizeSessionName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" || out == "web-auth" {
		return ""
	}
	return out
}

// LoadCredentialsFor reads the credentials a ref resolves to (its own file, or
// the project one). Same contract as LoadCredentials.
func LoadCredentialsFor(ref CredentialsRef) (*Credentials, error) {
	return loadCredentialsFile(ref.Path())
}

// SaveCredentialsFor writes credentials to the ref's OWN file — a session that
// sets a login gets its own from that moment on, and stops following the
// project's.
func SaveCredentialsFor(ref CredentialsRef, username, password string) (*Credentials, error) {
	return saveCredentialsFile(ref.OwnPath(), username, password)
}

// ClearCredentialsFor removes the ref's own credentials file. The project login
// is left alone: clearing a session's login means "stop having your own", not
// "unlock every other session in this project".
func ClearCredentialsFor(ref CredentialsRef) (bool, error) {
	path := ref.OwnPath()
	if path == "" {
		return false, nil
	}
	err := os.Remove(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("remove web credentials: %w", err)
}

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
	return loadCredentialsFile(CredentialsPath(ryshDir))
}

// loadCredentialsFile is the path-based core of LoadCredentials.
func loadCredentialsFile(path string) (*Credentials, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read web credentials: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Username == "" || c.PasswordHash == "" || c.Secret == "" {
		return nil, fmt.Errorf("%s is incomplete — set credentials again with `##rysh web auth username=<u> password=<p>`", path)
	}
	return &c, nil
}

// SaveCredentials hashes password, mints a fresh signing key, and writes the
// credentials file (0600 in a 0700 dir). Returns what was written.
func SaveCredentials(ryshDir, username, password string) (*Credentials, error) {
	if ryshDir == "" {
		return nil, errors.New("no rysh dir to store web credentials in")
	}
	return saveCredentialsFile(CredentialsPath(ryshDir), username, password)
}

// saveCredentialsFile is the path-based core of SaveCredentials.
func saveCredentialsFile(path, username, password string) (*Credentials, error) {
	if path == "" {
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
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
		ensureCredentialsGitignore(dir)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
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

// ensureCredentialsGitignore drops a .gitignore beside per-session credentials
// so a project that tracks .rysh/ can never commit one.
//
// A credentials file is a password hash AND the HS256 signing key that mints
// access tokens; committing one publishes the key. The project-wide
// web-auth.json is covered by rysh's own .gitignore, but the per-session files
// live in a directory that did not exist when that rule was written — and the
// session SETTINGS beside them are ordinary state worth tracking, so the
// directory cannot simply be excluded wholesale.
//
// Best effort: a failure here must never stop a login from being set.
func ensureCredentialsGitignore(dir string) {
	gi := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gi); err == nil {
		return
	}
	_ = os.WriteFile(gi, []byte(
		"# rysh web logins — a password hash and a token signing key. Never commit these.\n"+
			"*-auth.json\n"), 0o600)
}
