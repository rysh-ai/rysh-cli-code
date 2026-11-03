// SPDX-License-Identifier: Apache-2.0

package web

// The control door's per-launch secret.
//
// Control mode is the desktop app's own connection to the daemon it runs. Being
// on loopback is not enough to identify that client: anything else on this
// machine can reach a loopback port, including web content in a browser. So the
// control door opens only for a request that also carries the secret the app
// made for this daemon. Nothing else changes for the app: it presents the secret
// on every request and on the /ws handshake.
//
// How the secret reaches the daemon, never through argv or a URL:
//
//   - a daemon the app spawns reads RYSH_WEB_CONTROL_SECRET_FILE once at start
//     (TakeControlSecretHandover), drops the name from its own environment
//     before it starts any child, and deletes the file;
//   - `##rysh web start --control --control-secret-file <path>` (the app asking
//     a live daemon to open a door) reads and deletes the file the same way;
//   - a control-mode server started with neither makes its own secret and
//     writes it, mode 0600, under the user's own config dir (never the project
//     tree), so the app can still adopt it; the path goes on the session
//     record and the file is removed when the server stops.
//
// The secret's value is never logged, not even its length.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	// EnvControlSecretFile names the handover file for a daemon the app spawns.
	EnvControlSecretFile = "RYSH_WEB_CONTROL_SECRET_FILE"
	// ControlSecretHeader carries the secret on HTTP requests and on the /ws
	// handshake when the client can set headers.
	ControlSecretHeader = "X-Rysh-Control-Secret"
	// ControlSubprotocol is the WebSocket subprotocol the server selects when a
	// browser client presents the secret as a subprotocol (a browser WebSocket
	// cannot set headers). The client offers it together with
	// ControlSecretSubprotocolPrefix+<secret>; only this plain name is echoed.
	ControlSubprotocol = "rysh-control"
	// ControlSecretSubprotocolPrefix prefixes the secret in Sec-WebSocket-Protocol.
	ControlSecretSubprotocolPrefix = "rysh-control."

	// controlSecretBytes is the size of a secret this package makes, and the
	// minimum decoded size it accepts.
	controlSecretBytes = 32
	// maxControlSecretFile bounds what a handover read takes in.
	maxControlSecretFile = 1024
)

// handover holds the secret a spawned daemon was given at start, for every
// control-mode server this process runs.
var handover struct {
	mu     sync.Mutex
	secret string
}

// TakeControlSecretHandover reads the secret handed over in
// RYSH_WEB_CONTROL_SECRET_FILE, if the name is set. It removes the name from
// this process's environment first, whatever happens next, so no child the
// daemon starts can see it; then it reads and deletes the file. A bad file
// leaves the control door closed (requests fall through to the login). Call it
// before the daemon starts any child process.
func TakeControlSecretHandover() error {
	path, set := os.LookupEnv(EnvControlSecretFile)
	if !set {
		return nil
	}
	_ = os.Unsetenv(EnvControlSecretFile)
	if strings.TrimSpace(path) == "" {
		return errors.New("control secret handover: empty path")
	}
	secret, err := ReadControlSecretFile(path)
	if err != nil {
		return err
	}
	handover.mu.Lock()
	handover.secret = secret
	handover.mu.Unlock()
	return nil
}

func handoverSecret() string {
	handover.mu.Lock()
	defer handover.mu.Unlock()
	return handover.secret
}

// ReadControlSecretFile reads a secret handover file and deletes it. The file
// must be a regular file (not a link), owned by this user, with no group or
// other permission bits, holding one line of base64url (or hex) that decodes
// to at least 32 bytes. The file is deleted even when it is refused, so a bad
// handover does not linger on disk.
func ReadControlSecretFile(path string) (string, error) {
	path = filepath.Clean(path)
	fi, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("control secret file: %w", err)
	}
	defer func() { _ = os.Remove(path) }()
	if !fi.Mode().IsRegular() {
		return "", errors.New("control secret file: not a regular file")
	}
	if err := privateFileOK(fi); err != nil {
		return "", fmt.Errorf("control secret file: %w", err)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("control secret file: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxControlSecretFile+1))
	_ = f.Close()
	if err != nil {
		return "", fmt.Errorf("control secret file: %w", err)
	}
	if len(data) > maxControlSecretFile {
		return "", errors.New("control secret file: too large")
	}
	secret := strings.TrimRight(string(data), "\r\n")
	if !validControlSecret(secret) {
		return "", errors.New("control secret file: not a 32-byte base64url or hex secret on one line")
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("control secret file: delete: %w", err)
	}
	return secret, nil
}

// validControlSecret reports whether s is subprotocol-safe text that decodes
// to at least controlSecretBytes bytes, as base64url (padded or not) or hex.
func validControlSecret(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		ok := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '='
		if !ok {
			return false
		}
	}
	if b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "=")); err == nil && len(b) >= controlSecretBytes {
		return true
	}
	if len(s)%2 == 0 && len(s)/2 >= controlSecretBytes {
		hexOK := true
		for _, c := range s {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				hexOK = false
				break
			}
		}
		if hexOK {
			return true
		}
	}
	return false
}

// NewControlSecret makes a fresh secret: 32 random bytes, base64url, no padding.
func NewControlSecret() (string, error) {
	b := make([]byte, controlSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// controlSecretDir is where a server that made its own secret writes it: the
// user's own config dir, never the project tree (a project .rysh/ can be
// committed). A var so tests can point it at a temp dir.
var controlSecretDir = func() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "rysh", "web-control"), nil
}

// writeOwnControlSecret writes secret to a new 0600 file with a random name in
// controlSecretDir (made 0700) and returns its path.
func writeOwnControlSecret(secret string) (string, error) {
	dir, err := controlSecretDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	name := make([]byte, 12)
	if _, err := rand.Read(name); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "ctl-"+base64.RawURLEncoding.EncodeToString(name))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(secret + "\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// SetControlSecretFile reads (and deletes) a handover file for this server's
// control door. Call before Start. On error the server keeps no secret from
// it, so the control door stays closed unless another source supplies one.
func (s *Server) SetControlSecretFile(path string) error {
	secret, err := ReadControlSecretFile(path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.controlSecret = secret
	s.mu.Unlock()
	return nil
}

// ControlSecretFile is the path of the secret file this server wrote for
// itself (a control-mode server that was handed none), or "" when it was
// handed one. The desktop app reads it from the session record to adopt the
// daemon.
func (s *Server) ControlSecretFile() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ownSecretFile
}

// prepareControlSecret runs at Start, under no lock: a control-mode server
// uses the secret it was given, else the daemon's handover, else makes and
// writes its own. A failure leaves no secret, so the control door stays shut.
func (s *Server) prepareControlSecret() {
	s.mu.Lock()
	control, have := s.control, s.controlSecret != ""
	s.mu.Unlock()
	if !control || have {
		return
	}
	if h := handoverSecret(); h != "" {
		s.mu.Lock()
		s.controlSecret = h
		s.mu.Unlock()
		return
	}
	secret, err := NewControlSecret()
	if err != nil {
		s.logger.Error("web control door: could not make a secret; the door stays closed", "err", err)
		return
	}
	path, err := writeOwnControlSecret(secret)
	if err != nil {
		s.logger.Error("web control door: could not write the secret file; the door stays closed", "err", err)
		return
	}
	s.mu.Lock()
	s.controlSecret, s.ownSecretFile = secret, path
	s.mu.Unlock()
}

// dropControlSecret runs at Stop: forget the secret and remove a file this
// server wrote for itself. A handed-over secret stays with the process.
func (s *Server) dropControlSecret() {
	s.mu.Lock()
	path := s.ownSecretFile
	if path != "" {
		s.controlSecret, s.ownSecretFile = "", ""
	}
	s.mu.Unlock()
	if path != "" {
		_ = os.Remove(path)
	}
}

// presentedControlSecrets returns what r offers as the secret: the header, and
// any ControlSecretSubprotocolPrefix entry in Sec-WebSocket-Protocol.
func presentedControlSecrets(r *http.Request) []string {
	var out []string
	if v := r.Header.Get(ControlSecretHeader); v != "" {
		out = append(out, v)
	}
	for _, line := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(line, ",") {
			p = strings.TrimSpace(p)
			if strings.HasPrefix(p, ControlSecretSubprotocolPrefix) {
				out = append(out, strings.TrimPrefix(p, ControlSecretSubprotocolPrefix))
			}
		}
	}
	return out
}

// controlSecretOK reports whether r presents this server's control secret.
// Every presented value is compared in full and in constant time.
func (s *Server) controlSecretOK(r *http.Request) bool {
	s.mu.Lock()
	want := s.controlSecret
	s.mu.Unlock()
	if want == "" {
		return false
	}
	ok := 0
	for _, got := range presentedControlSecrets(r) {
		ok |= subtle.ConstantTimeCompare([]byte(got), []byte(want))
	}
	return ok == 1
}

// privateHostOK is the Host check for a request on the loopback private
// listener that no proxy forwarded: Host must name this listener by a loopback
// name. It stops a page whose hostname resolves to 127.0.0.1 (DNS rebinding)
// from talking to the listener as if it were same-origin. A listener not bound
// to loopback, the shared listener and forwarded requests are not checked here
// (the shared and forwarded paths always need the login).
func (s *Server) privateHostOK(r *http.Request) bool {
	if arrivedShared(r) || forwarded(r) {
		return true
	}
	s.mu.Lock()
	loop, port := s.privateLoopback, s.privatePort
	s.mu.Unlock()
	if !loop {
		return true
	}
	return loopbackHostPort(r.Host, port)
}

// loopbackHostPort reports whether hostport names a loopback address with
// exactly port: "localhost", or a loopback IP literal (127.0.0.0/8 — the bind
// may be an alias such as 127.0.100.1 — or ::1). A rebinding page always has a
// hostname, never an IP literal, so literals are safe to accept.
func loopbackHostPort(hostport string, port int) bool {
	h, p, err := net.SplitHostPort(hostport)
	if err != nil || p != strconv.Itoa(port) {
		return false
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// wsOriginAllowed is the /ws CheckOrigin.
//
//   - No Origin: a non-browser client (the CLI, tests). Allowed; the door or
//     the login still decides.
//   - The request opens the control door (it carries the secret): the app's
//     renderer may be a file:// page (Origin "file://" or "null") or, in a dev
//     build, an http://localhost:<port> page; those are allowed. The secret is
//     what decides here, never the Origin alone.
//   - Otherwise the Origin must be same-origin: its host:port equals Host, or
//     X-Forwarded-Host for a forwarded request.
func (s *Server) wsOriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if s.privateDoorOpen(r) {
		if origin == "null" || origin == "file://" {
			return true
		}
		if u, err := url.Parse(origin); err == nil && u.Scheme == "http" {
			h := u.Hostname()
			if ip := net.ParseIP(h); strings.EqualFold(h, "localhost") || (ip != nil && ip.IsLoopback()) {
				return true
			}
		}
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	if forwarded(r) {
		if xf := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); xf != "" && strings.EqualFold(u.Host, xf) {
			return true
		}
	}
	return false
}

// logRefused notes a refused control-door request: time is in the log line,
// plus the remote address. Never the presented value.
func (s *Server) logRefused(r *http.Request, why string) {
	s.logger.Warn("web: request refused", "why", why, "remote", r.RemoteAddr, "path", r.URL.Path)
}
