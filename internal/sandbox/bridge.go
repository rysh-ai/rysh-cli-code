// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// bridge.go — gap01 slice 2 P2 (R9): `network: proxy` for the backends that
// give the agent its OWN network namespace (bwrap, Linux docker). That
// namespace's loopback is not the host's: nothing the host listens on exists
// in it. The agent reaches its model like this:
//
//	agent ──tcp 127.0.0.1:<p> (its own netns)──▶ rysh sandbox-forward
//	      ──unix <BridgeMount>/bridge.sock (bind-mounted)──▶ host: rysh sandbox-bridge
//	      ──tcp 127.0.0.1:<p> (the host's)──▶ the governance proxy
//
// The host end dials ONE fixed upstream chosen before the sandbox starts —
// the governance proxy's loopback port — and copies bytes. It parses nothing,
// so a request cannot name another destination (no CONNECT), and it never
// dials a session bus port (CheckUpstream). The proxy, on the host, swaps the
// placeholder key for the real one, so the key never enters the sandbox.

// BridgeMount is where the bridge dir is mounted inside the sandbox; the rysh
// binary is mounted beside it as <BridgeMount>/rysh.
const BridgeMount = "/run/rysh-proxy"

// bridgeSocketName is the socket's name inside the per-sandbox bridge dir.
const bridgeSocketName = "bridge.sock"

// Bridge is a prepared host end for one sandbox instance.
type Bridge struct {
	// Dir is the per-sandbox dir (0700) holding Socket; it is bind-mounted.
	Dir    string
	Socket string
	// Binary is the static rysh binary run inside as the forwarder.
	Binary string
	// Port is the governance proxy's port: the upstream on the host and the
	// forwarder's port inside (the agent's ANTHROPIC_BASE_URL is unchanged).
	Port int
}

// CheckUpstream accepts only 127.0.0.1:<port> with a real port that is not a
// session bus port. Nothing else may be a bridge upstream.
func CheckUpstream(upstream string, busPorts []int) error {
	host, port, err := net.SplitHostPort(upstream)
	if err != nil {
		return fmt.Errorf("sandbox bridge: upstream %q: %w", upstream, err)
	}
	if host != "127.0.0.1" {
		return fmt.Errorf("sandbox bridge: upstream %q is not 127.0.0.1", upstream)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 {
		return fmt.Errorf("sandbox bridge: upstream %q has no valid port", upstream)
	}
	for _, b := range busPorts {
		if p == b {
			return fmt.Errorf("sandbox bridge: upstream port %d is the rysh session bus", p)
		}
	}
	return nil
}

// ListenBridgeSocket creates dir (0700) and listens on <dir>/bridge.sock
// (0600). A leftover socket from a crashed run is replaced; anything else at
// that path is an error.
func ListenBridgeSocket(dir string) (net.Listener, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("sandbox bridge: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("sandbox bridge: %w", err)
	}
	sock := filepath.Join(dir, bridgeSocketName)
	if fi, err := os.Lstat(sock); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, "", fmt.Errorf("sandbox bridge: %s exists and is not a socket", sock)
		}
		_ = os.Remove(sock)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, "", fmt.Errorf("sandbox bridge: listen %s: %w", sock, err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return nil, "", fmt.Errorf("sandbox bridge: %w", err)
	}
	return ln, sock, nil
}

// ServeBridge is the host end: every connection on ln is copied to upstream,
// which is checked first (loopback only; bus ports are checked by the caller
// through CheckUpstream with the real list). It returns when stop closes.
func ServeBridge(ln net.Listener, upstream string, stop <-chan struct{}) error {
	if err := CheckUpstream(upstream, nil); err != nil {
		return err
	}
	return serve(ln, stop, func() (net.Conn, error) { return net.Dial("tcp", upstream) })
}

// ServeForward is the in-sandbox end: every connection on ln (the agent's
// loopback port in its own namespace) is copied to the bridge socket.
func ServeForward(ln net.Listener, socket string, stop <-chan struct{}) error {
	return serve(ln, stop, func() (net.Conn, error) { return net.Dial("unix", socket) })
}

func serve(ln net.Listener, stop <-chan struct{}, dial func() (net.Conn, error)) error {
	go func() {
		<-stop
		ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-stop:
				return nil
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go func(c net.Conn) {
			defer c.Close()
			u, err := dial()
			if err != nil {
				return
			}
			defer u.Close()
			pipe(c, u)
		}(c)
	}
}

// pipe copies both ways until either side ends.
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

// forwardArgv is the in-sandbox command that runs the forwarder and then the
// agent (as its child, sharing the terminal).
func forwardArgv(port int, agent string, args []string) []string {
	out := []string{BridgeMount + "/rysh", "sandbox-forward",
		"--listen", "127.0.0.1:" + strconv.Itoa(port),
		"--socket", BridgeMount + "/" + bridgeSocketName, "--", agent}
	return append(out, args...)
}

// checkBridge validates a proxy-mode spec's bridge for a bind-mount backend.
func checkBridge(s Spec) error {
	b := s.Bridge
	if b == nil {
		return fmt.Errorf("sandbox: profile %q is network: proxy, but no proxy bridge was started for it", s.Profile.Name)
	}
	ports := LoopbackProxyPorts(s.Env)
	if b.Port != s.ProxyPort || len(ports) != 1 || ports[0] != b.Port {
		return fmt.Errorf("sandbox: network: proxy needs the bridge on the session's governance proxy port (%s); bridge %d, provider ports %v", proxyPortText(s.ProxyPort), b.Port, ports)
	}
	if b.Socket != filepath.Join(b.Dir, bridgeSocketName) || !filepath.IsAbs(b.Dir) || !filepath.IsAbs(b.Binary) {
		return fmt.Errorf("sandbox: malformed proxy bridge %+v", *b)
	}
	return nil
}
