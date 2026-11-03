// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// TestMacOSNetworkProxyE2E (slice 2, R9) under the REAL sandbox-exec, same gate
// as the other macOS E2E tests. Every listener is the test's own; nothing
// leaves the host. network: proxy reaches the governance proxy's port and
// nothing else, and the agent's env holds only the proxy placeholder key.
func TestMacOSNetworkProxyE2E(t *testing.T) {
	gateMacE2E(t)
	l := newLayout(t)
	if err := PrepareHome(l.sbxHome, filepath.Join(l.home, ".claude"), defaultCopyConfig); err != nil {
		t.Fatal(err)
	}
	listen := func(network, addr string) net.Listener {
		ln, err := net.Listen(network, addr)
		if err != nil {
			t.Fatalf("listen %s %s: %v", network, addr, err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		return ln
	}
	proxy := listen("tcp", "127.0.0.1:0")
	control := listen("tcp", "127.0.0.1:0")
	control6 := listen("tcp6", "[::1]:0")
	var lan net.Listener
	if ip := hostLANAddr(); ip != "" {
		lan = listen("tcp4", ip+":0")
	}
	sockDir, err := os.MkdirTemp("/tmp", "g1p")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "s.sock")
	listen("unix", sock)

	port := func(ln net.Listener) string { return strconv.Itoa(ln.Addr().(*net.TCPAddr).Port) }
	proxyMode := func(s *Spec) {
		s.Profile.Network, s.Profile.Proxy = false, true
		s.Env = append(s.Env, "ANTHROPIC_BASE_URL=http://127.0.0.1:"+port(proxy), "ANTHROPIC_API_KEY=synthetic-not-a-key")
		s.ProxyPort, _ = strconv.Atoi(port(proxy)) // the session record's proxy port (R12)
	}
	py := "/usr/bin/python3 -I -c 'import socket,sys; socket.create_connection((sys.argv[1], int(sys.argv[2])), 2).close()' "
	if err := macRun(t, l, proxyMode, py+"127.0.0.1 "+port(proxy)); err != nil {
		t.Fatalf("the governance proxy port must be reachable: %v", err)
	}
	if err := macRun(t, l, proxyMode, "test \"$ANTHROPIC_API_KEY\" = "+config.ProxyDummyKey); err != nil {
		t.Fatalf("the agent's key is not the proxy placeholder: %v", err)
	}
	probes := map[string]string{
		"another loopback port": py + "127.0.0.1 " + port(control),
		"IPv6 loopback":         py + "::1 " + port(control6),
		"a unix socket":         "/usr/bin/python3 -I -c 'import socket,sys; s=socket.socket(socket.AF_UNIX); s.settimeout(2); s.connect(sys.argv[1])' " + sock,
	}
	if lan != nil {
		probes["the host's LAN address"] = py + strings.Split(lan.Addr().String(), ":")[0] + " " + port(lan)
	}
	for name, sc := range probes {
		if err := macRun(t, l, proxyMode, sc); err == nil {
			t.Errorf("%s: REACHABLE in network: proxy", name)
		}
	}
}
