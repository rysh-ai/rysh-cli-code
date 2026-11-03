// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestBwrapRealE2E runs the REAL bwrap with a generated argv. ENV-GATED and
// Linux-only: RYSH_SANDBOX_BWRAP_E2E=1 (a skip in the default suite, and NOT
// RUN on the macOS build host). Throwaway dirs only; the "agent" is /bin/sh;
// network OFF, so nothing in here can reach a network at all.
func TestBwrapRealE2E(t *testing.T) {
	if runtime.GOOS != "linux" || os.Getenv("RYSH_SANDBOX_BWRAP_E2E") != "1" {
		t.Skip("gated: set RYSH_SANDBOX_BWRAP_E2E=1 on Linux with bwrap installed")
	}
	tool, err := exec.LookPath("bwrap")
	if err != nil {
		t.Fatalf("gate set but bwrap is not installed: %v", err)
	}
	l := newLayout(t)
	if err := PrepareHome(l.sbxHome, filepath.Join(l.home, ".claude"), defaultCopyConfig); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(l.rysh, "memory", "entries", "e1.md"), "synthetic memory\n")
	if out, err := exec.Command("git", "-C", l.ws, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	run := func(script string) error {
		p := builtinDefault()
		p.Backend = BackendBwrap
		p.Network = false
		s := l.spec(p)
		s.Argv = []string{"/bin/sh", "-c", script}
		s.Env = []string{"PATH=/usr/bin:/bin"}
		plan, err := (&Bwrap{LookPath: func(string) (string, error) { return tool, nil }}).Prepare(s)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(plan.Argv[0], plan.Argv[1:]...)
		cmd.Env = plan.Env
		out, err := cmd.CombinedOutput()
		t.Logf("%q -> err=%v out=%s", script, err, strings.TrimSpace(string(out)))
		return err
	}
	for name, sc := range map[string]string{
		"write in workspace":    "echo ok > " + filepath.Join(l.ws, "w.txt"),
		"write in sandbox home": "echo ok > \"$HOME/h.txt\"",
	} {
		if err := run(sc); err != nil {
			t.Fatalf("%s: refused: %v", name, err)
		}
	}
	for name, sc := range map[string]string{
		"read ~/.ssh":          "cat " + filepath.Join(l.home, ".ssh", "id_synthetic"),
		"read ~/.aws":          "cat " + filepath.Join(l.home, ".aws", "credentials"),
		"read rysh secrets":    "cat " + filepath.Join(l.rysh, "secrets", "x"),
		"read a memory entry":  "cat " + filepath.Join(l.rysh, "memory", "entries", "e1.md"),
		"write a memory entry": "echo x > " + filepath.Join(l.rysh, "memory", "entries", "e2.md"),
		"write a git hook":     "echo x > " + filepath.Join(l.ws, ".git", "hooks", "post-commit"),
		"write .git/config":    "echo x >> " + filepath.Join(l.ws, ".git", "config"),
		"rename .git":          "mv " + filepath.Join(l.ws, ".git") + " " + filepath.Join(l.ws, "g"),
		// /proc/net/dev: two header lines, then one per interface; lo only = 3.
		"a non-lo interface": "test $(wc -l < /proc/net/dev) -gt 3",
		"write the system":   "echo x > /etc/rysh-gap01",
	} {
		if err := run(sc); err == nil {
			t.Errorf("%s: ALLOWED inside the sandbox", name)
		}
	}
}

// TestBwrapProxyRealE2E: network: proxy under the REAL bwrap (Linux only;
// RYSH_SANDBOX_BWRAP_E2E=1 and RYSH_BIN=<absolute path to a built rysh>;
// NOT RUN on the macOS build host). The agent inside reaches the test's own
// "proxy" listener through the bridge, and nothing else: its namespace has
// only lo, and the host's other loopback listener does not exist there.
func TestBwrapProxyRealE2E(t *testing.T) {
	bin := os.Getenv("RYSH_BIN")
	if runtime.GOOS != "linux" || os.Getenv("RYSH_SANDBOX_BWRAP_E2E") != "1" || !filepath.IsAbs(bin) {
		t.Skip("gated: Linux, RYSH_SANDBOX_BWRAP_E2E=1 and RYSH_BIN=<absolute path to a built rysh>")
	}
	tool, err := exec.LookPath("bwrap")
	if err != nil {
		t.Fatalf("gate set but bwrap is not installed: %v", err)
	}
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	go func() {
		for {
			c, err := proxy.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				c.Write([]byte("proxy:" + line))
			}(c)
		}
	}()
	other, _ := net.Listen("tcp", "127.0.0.1:0")
	defer other.Close()
	pport := proxy.Addr().(*net.TCPAddr).Port
	l := newLayout(t)
	if err := PrepareHome(l.sbxHome, filepath.Join(l.home, ".claude"), defaultCopyConfig); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(l.rysh, "sandbox", "run", "t1")
	ln, sock, err := ListenBridgeSocket(dir)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go ServeBridge(ln, proxy.Addr().String(), stop)
	run := func(script string) error {
		p := builtinDefault()
		p.Backend, p.Network, p.Proxy = BackendBwrap, false, true
		s := l.spec(p)
		s.Argv = []string{"/bin/bash", "-c", script}
		s.Env = []string{"PATH=/usr/bin:/bin", "ANTHROPIC_BASE_URL=http://127.0.0.1:" + strconv.Itoa(pport)}
		s.Bridge = &Bridge{Dir: dir, Socket: sock, Binary: bin, Port: pport}
		s.ProxyPort = pport
		plan, err := (&Bwrap{LookPath: func(string) (string, error) { return tool, nil }}).Prepare(s)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(plan.Argv[0], plan.Argv[1:]...)
		cmd.Env = plan.Env
		out, err := cmd.CombinedOutput()
		t.Logf("%q -> err=%v out=%s", script, err, strings.TrimSpace(string(out)))
		return err
	}
	talk := "exec 3<>/dev/tcp/127.0.0.1/" + strconv.Itoa(pport) + " && echo hi >&3 && read -r r <&3 && test \"$r\" = proxy:hi"
	if err := run("sleep 0.3; " + talk); err != nil {
		t.Fatalf("the agent cannot reach the proxy through the bridge: %v", err)
	}
	if err := run("exec 3<>/dev/tcp/127.0.0.1/" + strconv.Itoa(other.Addr().(*net.TCPAddr).Port)); err == nil {
		t.Error("the host's other loopback listener is reachable from the sandbox's namespace")
	}
	if err := run("test \"$ANTHROPIC_API_KEY\" = " + "rysh-proxy-dummy"); err != nil {
		t.Error("the agent's key is not the proxy placeholder")
	}
}
