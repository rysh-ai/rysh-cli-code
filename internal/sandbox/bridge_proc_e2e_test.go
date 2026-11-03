// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"bufio"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestBridgeProcessesRealE2E runs the REAL `rysh sandbox-bridge` and
// `rysh sandbox-forward` processes from a built binary (RYSH_SANDBOX_BRIDGE_E2E=1,
// RYSH_BIN=<absolute path to a built rysh>). No namespace is involved — that
// part needs Linux (TestBwrapRealE2E) — but the processes, the socket modes,
// the fixed upstream, the agent-as-child and the bridge's exit when its parent
// goes are all real. Every listener is the test's own, on loopback.
func TestBridgeProcessesRealE2E(t *testing.T) {
	bin := os.Getenv("RYSH_BIN")
	if os.Getenv("RYSH_SANDBOX_BRIDGE_E2E") != "1" || !filepath.IsAbs(bin) {
		t.Skip("gated: set RYSH_SANDBOX_BRIDGE_E2E=1 and RYSH_BIN=<absolute path to a built rysh>")
	}
	up, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				c.Write([]byte("upstream:" + line))
			}(c)
		}
	}()
	dir := filepath.Join(shortTempDir(t), "run", "e2e")
	// The bridge's parent: a short-lived sh that the test can end on purpose.
	parent := exec.Command("/bin/sh", "-c", "\""+bin+"\" sandbox-bridge --dir "+dir+" --upstream "+up.Addr().String()+" --parent $$ & wait")
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "bridge.sock")
	for i := 0; i < 120; i++ {
		if fi, err := os.Lstat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	fi, err := os.Lstat(sock)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("bridge socket: %v mode %v", err, fi)
	}
	// The forwarder on another loopback port, running a one-shot "agent" that
	// talks through it.
	fl, _ := net.Listen("tcp", "127.0.0.1:0")
	fport := strconv.Itoa(fl.Addr().(*net.TCPAddr).Port)
	fl.Close()
	agent := "sleep 0.3; exec 3<>/dev/tcp/127.0.0.1/" + fport + " 2>/dev/null || exit 9; echo hello >&3; read -r r <&3; test \"$r\" = upstream:hello"
	out, err := exec.Command(bin, "sandbox-forward", "--listen", "127.0.0.1:"+fport, "--socket", sock, "--", "/bin/bash", "-c", agent).CombinedOutput()
	if err != nil {
		t.Fatalf("agent through the forwarder: %v %s", err, out)
	}
	// End the parent: the bridge must exit and remove its dir.
	_ = parent.Process.Kill()
	_ = parent.Wait()
	for i := 0; i < 80; i++ {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the bridge outlived its parent (dir %s still there)", strings.TrimPrefix(dir, os.TempDir()))
}
