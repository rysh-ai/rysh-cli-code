// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/cron"
)

// MaxTriggers caps the number of triggers a session may hold.
const MaxTriggers = 100

// Trigger is a named, fixed input that a signed webhook may fire.
type Trigger struct {
	ID      string `json:"id"`
	Name    string `json:"name"`   // URL path segment: /t/<name>
	Target  string `json:"target"` // pane id/name, or "active"
	Mode    string `json:"mode"`   // prompt, or "" for an @agent input
	Input   string `json:"input"`  // @agent <prompt> or a prompt
	Enabled bool   `json:"enabled"`

	CreatedAt  time.Time  `json:"created_at"`
	LastRun    time.Time  `json:"last_run,omitempty"`
	LastStatus string     `json:"last_status,omitempty"`
	RunCount   int        `json:"run_count"`
	Runs       []cron.Run `json:"runs,omitempty"`
}

// RecordRun appends a run record, ring-buffered like a cron job's.
func (t *Trigger) RecordRun(r cron.Run) {
	t.LastRun = r.At
	t.LastStatus = r.Status
	t.RunCount++
	t.Runs = append(t.Runs, r)
	if len(t.Runs) > cron.MaxRunHistory {
		t.Runs = t.Runs[len(t.Runs)-cron.MaxRunHistory:]
	}
}

// ValidName reports whether name is a safe trigger name: letters, digits, -
// and _, at most 64 characters. It is a URL path segment and a file name, so
// nothing path-shaped may pass.
func ValidName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// Validate checks a trigger at creation time. A network event may start an
// agent or a prompt, never a shell line or a rysh command: shell and rysh
// modes are refused, and so is any input routeInput would read as a command
// (##, ####, a #user-command, or @@ control).
func Validate(name, mode, input string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid trigger name %q (letters, digits, - and _ only, ≤64 chars)", name)
	}
	in := strings.TrimSpace(input)
	if in == "" {
		return errors.New("empty input — a trigger must start an @agent or a prompt")
	}
	if mode != "" && mode != "prompt" {
		return fmt.Errorf("mode %q is not allowed for a trigger (prompt only; a webhook never runs a shell line or a rysh command)", mode)
	}
	switch {
	case strings.HasPrefix(in, "@@"):
		return errors.New("a trigger cannot run an @@ control command")
	case strings.HasPrefix(in, "#"):
		return errors.New("a trigger cannot run a # or ## command — use an @agent or a prompt")
	}
	return nil
}

// EffectiveMode is the input mode a trigger fires with: an @agent input is
// prefix-dispatched (mode "rysh", as cron infers it); anything else is a prompt.
func EffectiveMode(mode, input string) string {
	if strings.HasPrefix(strings.TrimSpace(input), "@") {
		return "rysh"
	}
	return "prompt"
}

// ---------------------------------------------------------------------------
// Signatures
// ---------------------------------------------------------------------------

const sigScheme = "sha256="

// NewSecret returns a fresh random secret (32 bytes, hex-encoded).
func NewSecret() ([]byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	out := make([]byte, hex.EncodedLen(len(raw)))
	hex.Encode(out, raw)
	return out, nil
}

// Sign returns the signature header value for body: "sha256=<hex HMAC>". It is
// the same scheme GitHub uses for X-Hub-Signature-256.
func Sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return sigScheme + hex.EncodeToString(m.Sum(nil))
}

// Verify reports whether header is a valid signature of body under secret.
// The comparison is constant-time; an empty secret never verifies.
func Verify(secret, body []byte, header string) bool {
	if len(secret) == 0 || !strings.HasPrefix(header, sigScheme) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, sigScheme))
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

// ---------------------------------------------------------------------------
// Secret files: <dir>/<name>.secret, dir 0700, file 0600
// ---------------------------------------------------------------------------

// SecretPath is where a trigger's secret lives.
func SecretPath(dir, name string) string {
	return filepath.Join(dir, name+".secret")
}

// WriteSecret creates (or rotates) a trigger's secret. The value is written
// and never returned: callers point the user at the file instead of printing
// it, because pane output is persisted.
func WriteSecret(dir, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid trigger name %q", name)
	}
	secret, err := NewSecret()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	ensureGitignore(dir)
	tmp, err := os.CreateTemp(dir, "."+name+".secret-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(secret, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), SecretPath(dir, name))
}

// ensureGitignore writes a .gitignore into the secrets dir (best effort) so a
// project that tracks .rysh/ never commits a trigger secret — the same file the
// ##secret / ##var stores write (internal/actors/store.go ensureGitignore).
func ensureGitignore(dir string) {
	gi := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gi); err == nil {
		return
	}
	_ = os.WriteFile(gi, []byte("# rysh triggers — managed by ##trigger; never commit these\n*\n!.gitignore\n"), 0o600)
}

// ReadSecret returns a trigger's secret; false when the name is invalid or no
// secret is stored.
func ReadSecret(dir, name string) ([]byte, bool) {
	if !ValidName(name) {
		return nil, false
	}
	b, err := os.ReadFile(SecretPath(dir, name))
	if err != nil {
		return nil, false
	}
	b = []byte(strings.TrimSpace(string(b)))
	return b, len(b) > 0
}

// RemoveSecret deletes a trigger's secret; a missing file is not an error.
func RemoveSecret(dir, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid trigger name %q", name)
	}
	if err := os.Remove(SecretPath(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Delivery dedupe
// ---------------------------------------------------------------------------

// maxDeliveries is how many recent delivery ids a trigger remembers.
const maxDeliveries = 64

// deliveryRing remembers the most recent delivery ids of one trigger.
type deliveryRing struct {
	ids []string
}

func (r *deliveryRing) seen(id string) bool {
	for _, x := range r.ids {
		if x == id {
			return true
		}
	}
	return false
}

func (r *deliveryRing) remove(id string) {
	for i, x := range r.ids {
		if x == id {
			r.ids = append(r.ids[:i], r.ids[i+1:]...)
			return
		}
	}
}

func (r *deliveryRing) add(id string) {
	r.ids = append(r.ids, id)
	if len(r.ids) > maxDeliveries {
		r.ids = r.ids[len(r.ids)-maxDeliveries:]
	}
}
