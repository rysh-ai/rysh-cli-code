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
	"time"
)

// TestContainerUnixSocketCrossesE2E is slice 2's proof-first step (R9): can a
// unix socket created on the HOST be reached from inside a `--network none`
// container through a bind mount? On a VM-backed docker (colima, Docker
// Desktop) the socket file crosses the VM boundary through a shared-folder
// filesystem, which may not carry unix sockets at all.
//
// ENV-GATED like the other real-docker tests (RYSH_SANDBOX_DOCKER_E2E=1,
// RYSH_SANDBOX_E2E_PROXY_IMAGE = a LOCAL image with the Go toolchain; run with
// --pull never --network none). The client is compiled INSIDE the container
// from the standard library only (GOPROXY=off, GOFLAGS=-mod=mod is not needed:
// no module), so nothing is fetched. The listener is this test's own.
func TestContainerUnixSocketCrossesE2E(t *testing.T) {
	image := os.Getenv("RYSH_SANDBOX_E2E_PROXY_IMAGE")
	if os.Getenv("RYSH_SANDBOX_DOCKER_E2E") != "1" || image == "" {
		t.Skip("gated: set RYSH_SANDBOX_DOCKER_E2E=1 and RYSH_SANDBOX_E2E_PROXY_IMAGE=<local image with go>")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatalf("gate set but no docker: %v", err)
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// Short path: a unix socket path is limited to ~104 bytes on macOS.
	base, err := os.MkdirTemp(realHome, ".g1x-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	base, _ = filepath.EvalSymlinks(base)
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(base, "b.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen %s: %v", sock, err)
	}
	t.Cleanup(func() { ln.Close() })
	if err := os.Chmod(sock, 0o600); err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(20 * time.Second))
		line, _ := bufio.NewReader(c).ReadString('\n')
		got <- strings.TrimSpace(line)
		_, _ = c.Write([]byte("pong\n"))
	}()

	client := `package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
)

func main() {
	c, err := net.Dial("unix", os.Args[1])
	if err != nil {
		fmt.Println("DIAL-ERR", err)
		os.Exit(2)
	}
	fmt.Fprintln(c, "ping")
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		fmt.Println("READ-ERR", err)
		os.Exit(3)
	}
	fmt.Print("GOT ", line)
}
`
	if err := os.WriteFile(filepath.Join(base, "client.go"), []byte(client), 0o644); err != nil {
		t.Fatal(err)
	}
	// Two shapes: the socket file itself, or the directory holding it.
	inner := "/sbx/b.sock"
	if os.Getenv("RYSH_SANDBOX_E2E_SOCK_SHAPE") == "dir" {
		inner = "/sbxdir/b.sock"
	}
	// Named like the other E2E containers (e2eID), so a runner removes only its own.
	name := "rysh-sbx-" + e2eID(t, "g1x"+strings.ToLower(filepath.Base(base)[len(".g1x-"):]))
	cmd := exec.Command(docker, "run", "--rm", "--name", name, "--pull", "never", "--network", "none",
		"--user", strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()),
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--mount", sockMount(base, sock, inner),
		"--mount", "type=bind,source="+filepath.Join(base, "client.go")+",target=/src/client.go,readonly",
		"-e", "GOPROXY=off", "-e", "GOTOOLCHAIN=local", "-e", "GOCACHE=/tmp/gocache", "-e", "HOME=/tmp",
		image, "sh", "-c", "cd /tmp && go run /src/client.go "+inner)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	t.Logf("container: err=%v out=%s", err, strings.TrimSpace(string(out)))
	crossed := false
	select {
	case line := <-got:
		t.Logf("host listener received %q", line)
		crossed = line == "ping" && strings.Contains(string(out), "GOT pong")
	case <-time.After(5 * time.Second):
	}
	// The premise slice 2 rests on, per OS: on macOS every docker is VM-backed
	// and its shared folder does not carry unix sockets (found 2026-10-21 on
	// colima: the file bind fails "stat: operation not supported", the
	// directory bind fails "connect: operation not supported"), so
	// network: proxy is refused for the container backend there. On Linux the
	// socket is the same kernel's, and must cross.
	switch {
	case runtime.GOOS == "darwin" && crossed:
		t.Fatal("a host unix socket CROSSED into the container on macOS: the darwin refusal of container + network: proxy can be revisited")
	case runtime.GOOS == "darwin":
		t.Log("RESULT (darwin): the host unix socket does NOT cross the VM boundary; container + network: proxy stays refused")
	case !crossed:
		t.Fatalf("RESULT (%s): the host unix socket did NOT cross into the container: %s", runtime.GOOS, strings.TrimSpace(string(out)))
	default:
		t.Logf("RESULT (%s): the host unix socket crosses into the container", runtime.GOOS)
	}
}

func sockMount(base, sock, inner string) string {
	if strings.HasPrefix(inner, "/sbxdir/") {
		return "type=bind,source=" + base + ",target=/sbxdir"
	}
	return "type=bind,source=" + sock + ",target=" + inner
}
