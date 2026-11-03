// SPDX-License-Identifier: Apache-2.0

// Package bustoken holds the per-server token of the session NATS (gap01
// slice 3). The owner of the machine's embedded server generates it and writes
// it to ~/.rysh-run/nats-<port>.token; every rysh process that dials the bus by
// port reads it from there. The dir is per user, not per workspace, because
// guests of the shared server come from other workspaces. The token never goes
// into env, argv, a session record, a log or the bus itself, and every sandbox
// denies the dir. It stops sandboxed processes and other users; it does not
// stop an unsandboxed process running as the same user, which can read the file.
package bustoken

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/nats-io/nats.go"
)

const dirName = ".rysh-run"

// Dir is the token dir, ~/.rysh-run. It does not depend on XDG_RUNTIME_DIR or
// any other env: the owner and a guest started with different envs must agree.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("bus token: no home dir: %v", err)
	}
	return filepath.Join(home, dirName), nil
}

// Path is the token file for the server on port.
func Path(port int) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "nats-"+strconv.Itoa(port)+".token"), nil
}

// SandboxDenyDirs are every place a token could be looked for, for a sandbox
// to deny whatever the agent's env says: the token dir, and the per-user
// runtime dirs an older or other build might use.
func SandboxDenyDirs() []string {
	var out []string
	if d, err := Dir(); err == nil {
		out = append(out, d)
	}
	if x := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(x) {
		out = append(out, filepath.Join(x, "rysh"))
	}
	if runtime.GOOS == "linux" {
		out = append(out, filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "rysh"))
	}
	return out
}

// New returns a fresh token: 32 random bytes, hex.
func New() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("bus token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func validToken(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// checkPrivate refuses a symlink, a path of the wrong kind, a path another
// user owns, or one that group or others can reach.
func checkPrivate(p string, wantDir bool) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("bus token: %s is a symlink", p)
	case wantDir && !fi.IsDir(), !wantDir && !fi.Mode().IsRegular():
		return fmt.Errorf("bus token: %s is not a %s", p, map[bool]string{true: "directory", false: "regular file"}[wantDir])
	}
	if runtime.GOOS == "windows" {
		return nil // no unix modes or uids; the dir lives in the user's profile
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("bus token: %s is reachable by other users (mode %v); it must be %s", p, fi.Mode().Perm(), map[bool]string{true: "0700", false: "0600"}[wantDir])
	}
	if !ownedByMe(fi) {
		return fmt.Errorf("bus token: %s is not owned by this user", p)
	}
	return nil
}

// Read returns the token of the server on port. No dir or no file means the
// owner runs without auth (an older rysh): "", nil. A file that is not private
// to this user, or not a token, is an error, never a token.
func Read(port int) (string, error) {
	p, err := Path(port)
	if err != nil {
		return "", err
	}
	if err := checkPrivate(filepath.Dir(p), true); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	if err := checkPrivate(p, false); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("bus token: %w", err)
	}
	tok := strings.TrimSpace(string(data))
	if !validToken(tok) {
		return "", fmt.Errorf("bus token: %s does not hold a token", p)
	}
	return tok, nil
}

// Write stores tok for the server on port: the dir is created 0700 (an
// existing one must already be private), the file is written to an O_EXCL
// temp file 0600 and renamed into place.
func Write(port int, tok string) error {
	if !validToken(tok) {
		return errors.New("bus token: refusing to write a malformed token")
	}
	p, err := Path(port)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("bus token: %w", err)
	}
	if err := checkPrivate(dir, true); err != nil {
		return err
	}
	suffix, err := New()
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".nats-"+strconv.Itoa(port)+"."+suffix[:16]+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("bus token: %w", err)
	}
	_, werr := f.WriteString(tok + "\n")
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, p)
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("bus token: %w", werr)
	}
	return nil
}

// Remove deletes the token file for port if it still holds tok, so an owner
// that shuts down never removes the token of a newer owner on the same port.
func Remove(port int, tok string) {
	if got, err := Read(port); err == nil && got != "" && got == tok {
		if p, err := Path(port); err == nil {
			_ = os.Remove(p)
		}
	}
}

// Option is the nats.Option every rysh dial to the session bus on port uses.
// It reads the file once now, so an unsafe file is an error before dialling,
// and again on every (re)connect, so a client survives an owner restart that
// wrote a new token.
func Option(port int) (nats.Option, error) {
	if _, err := Read(port); err != nil {
		return nil, err
	}
	return nats.TokenHandler(func() string {
		tok, _ := Read(port)
		return tok
	}), nil
}

// loopbackPort returns the port of a nats URL whose host is loopback
// (127.0.0.1, ::1, localhost); false for any other host or no port.
func loopbackPort(raw string) (int, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Port() == "" {
		return 0, false
	}
	h := u.Hostname()
	if ip := net.ParseIP(h); !(h == "localhost" || ip != nil && ip.IsLoopback()) {
		return 0, false
	}
	port, err := strconv.Atoi(u.Port())
	return port, err == nil
}

// OptionForURL is Option for a nats://host:port URL. The token goes only to a
// loopback host: a URL naming any other host, or one without a port, gets no
// token, so it never leaves the machine.
func OptionForURL(raw string) (nats.Option, error) {
	port, ok := loopbackPort(raw)
	if !ok {
		return func(*nats.Options) error { return nil }, nil
	}
	return Option(port)
}

// PathForURL is the token file for a loopback nats URL, for a process rysh
// starts that dials the bus itself (a nats-transport plugin): it gets the
// PATH, never the token, and reads the file. false for a non-loopback URL.
func PathForURL(raw string) (string, bool) {
	port, ok := loopbackPort(raw)
	if !ok {
		return "", false
	}
	p, err := Path(port)
	return p, err == nil
}

// IsAuthError reports whether err is the server refusing our credentials: a
// server IS listening, so a probe must not read it as a free port.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, nats.ErrAuthorization) || strings.Contains(strings.ToLower(err.Error()), "authorization violation")
}

// Missing explains a refused join to the operator (T3: fail loudly, name the fix).
func Missing(port int, err error) error {
	p, _ := Path(port)
	return fmt.Errorf("the rysh NATS server on port %d requires a token this process cannot use (%s): %v — if that server is an older rysh, restart it; otherwise check that %s is yours and mode 0600", port, p, err, p)
}

// Dial connects to the session NATS on 127.0.0.1:port with the server's
// token. extra options follow the token (timeouts, reconnect policy, name).
func Dial(port int, extra ...nats.Option) (*nats.Conn, error) {
	opt, err := Option(port)
	if err != nil {
		return nil, err
	}
	return nats.Connect("nats://127.0.0.1:"+strconv.Itoa(port), append([]nats.Option{opt}, extra...)...)
}
