// SPDX-License-Identifier: Apache-2.0

package familyclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TokenStore is the file `rysh family login` writes: one entry per server,
// plus the server the last login used. It lives under the rysh dir the CLI
// already uses (<rysh dir>/family/auth.json), mode 0600 in a 0700 directory.
//
// rysh-server's existing upstream credential (`[upstream] api_key`, a
// workspace key) is not accepted by the JWT-only /api/family routes, so the
// family commands keep their own user session here.
type TokenStore struct {
	Path string
}

// StoredSession is one server's saved login.
type StoredSession struct {
	Email        string    `json:"email,omitempty"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	SavedAt      time.Time `json:"saved_at"`
}

type storeFile struct {
	DefaultServer string                    `json:"default_server,omitempty"`
	Servers       map[string]*StoredSession `json:"servers"`
}

// DefaultTokenStore is the store under ryshDir.
func DefaultTokenStore(ryshDir string) *TokenStore {
	return &TokenStore{Path: filepath.Join(ryshDir, "family", "auth.json")}
}

// NormalizeServer trims a server URL to the form used as a store key.
func NormalizeServer(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/")
}

func (s *TokenStore) load() (storeFile, error) {
	f := storeFile{Servers: map[string]*StoredSession{}}
	b, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("%s is not valid JSON: %w", s.Path, err)
	}
	if f.Servers == nil {
		f.Servers = map[string]*StoredSession{}
	}
	return f, nil
}

func (s *TokenStore) save(f storeFile) error {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(dir, 0o700)
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".auth-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after the rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.Path)
}

// DefaultServer is the server the last login used ("" if none).
func (s *TokenStore) DefaultServer() (string, error) {
	f, err := s.load()
	return f.DefaultServer, err
}

// Get returns the saved session for server, or nil.
func (s *TokenStore) Get(server string) (*StoredSession, error) {
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	return f.Servers[NormalizeServer(server)], nil
}

// Put saves a session for server and makes it the default server.
func (s *TokenStore) Put(server string, sess StoredSession) error {
	f, err := s.load()
	if err != nil {
		return err
	}
	key := NormalizeServer(server)
	if sess.SavedAt.IsZero() {
		sess.SavedAt = time.Now().UTC()
	}
	f.Servers[key] = &sess
	f.DefaultServer = key
	return s.save(f)
}

// UpdateTokens replaces server's tokens after a refresh, keeping its email.
func (s *TokenStore) UpdateTokens(server string, t Tokens) error {
	f, err := s.load()
	if err != nil {
		return err
	}
	key := NormalizeServer(server)
	cur := f.Servers[key]
	if cur == nil {
		cur = &StoredSession{}
		f.Servers[key] = cur
	}
	cur.AccessToken = t.AccessToken
	if t.RefreshToken != "" {
		cur.RefreshToken = t.RefreshToken
	}
	cur.SavedAt = time.Now().UTC()
	return s.save(f)
}

// Delete forgets server's session. It reports whether one existed.
func (s *TokenStore) Delete(server string) (bool, error) {
	f, err := s.load()
	if err != nil {
		return false, err
	}
	key := NormalizeServer(server)
	if _, ok := f.Servers[key]; !ok {
		return false, nil
	}
	delete(f.Servers, key)
	if f.DefaultServer == key {
		f.DefaultServer = ""
	}
	return true, s.save(f)
}
