// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// BridgeMain is `rysh sandbox-bridge --dir <d> --upstream 127.0.0.1:<p>
// --parent <pid> [--bus-ports a,b]`: the host end of the proxy bridge. It
// listens on <d>/bridge.sock (0600, dir 0700), copies every connection to the
// one upstream, and exits — removing <d> — when its parent (the sandbox
// launcher, which became the backend tool by exec) is gone.
func BridgeMain(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("sandbox-bridge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "")
	upstream := fs.String("upstream", "", "")
	parent := fs.Int("parent", 0, "")
	bus := fs.String("bus-ports", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "rysh sandbox-bridge: %v\n", err)
		return 2
	}
	if *dir == "" || !filepath.IsAbs(*dir) || filepath.Clean(*dir) != *dir {
		return fail(errors.New("--dir must be an absolute, clean path"))
	}
	if *parent <= 1 {
		return fail(errors.New("--parent must name the launcher's pid"))
	}
	var busPorts []int
	for _, f := range strings.Split(*bus, ",") {
		if p, err := strconv.Atoi(strings.TrimSpace(f)); err == nil {
			busPorts = append(busPorts, p)
		}
	}
	busPorts = append(busPorts, defaultBusPort)
	if err := CheckUpstream(*upstream, busPorts); err != nil {
		return fail(err)
	}
	ln, _, err := ListenBridgeSocket(*dir)
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(*dir)
	stop := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-sig:
				close(stop)
				return
			case <-t.C:
				if os.Getppid() != *parent {
					close(stop) // the sandbox is gone
					return
				}
			}
		}
	}()
	if err := ServeBridge(ln, *upstream, stop); err != nil {
		return fail(err)
	}
	return 0
}

// ForwardMain is `rysh sandbox-forward --listen 127.0.0.1:<p> --socket <s> --
// <agent> [args…]`, run INSIDE the sandbox: it serves the agent's loopback
// port (in the sandbox's own network namespace) onto the bridge socket and
// runs the agent as its child on the same terminal, returning its exit code.
func ForwardMain(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("sandbox-forward", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "", "")
	socket := fs.String("socket", "", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "rysh sandbox-forward: %v\n", err)
		return 125
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || host != "127.0.0.1" {
		return fail(fmt.Errorf("--listen %q: must be 127.0.0.1:<port>", *listen))
	}
	argv := fs.Args()
	if len(argv) == 0 || *socket == "" {
		return fail(errors.New("usage: sandbox-forward --listen 127.0.0.1:<port> --socket <path> -- <agent> [args…]"))
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fail(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() { _ = ServeForward(ln, *socket, stop) }()

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	// The terminal's own signals (^C, resize) reach the agent directly: it is
	// in this process group. Termination requests are passed on.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for s := range sig {
			_ = cmd.Process.Signal(s)
		}
	}()
	err = cmd.Wait()
	signal.Stop(sig)
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	default:
		return fail(err)
	}
}

// StartBridge launches `<self> sandbox-bridge` for one sandbox and waits for
// its socket. The bridge outlives this process's exec (it becomes the backend
// tool, same pid) and exits when that pid is gone.
func StartBridge(dir, upstream string, busPorts []int) (*Bridge, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("sandbox bridge: %w", err)
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return nil, fmt.Errorf("sandbox bridge: %w", err)
	}
	if err := CheckUpstream(upstream, busPorts); err != nil {
		return nil, err
	}
	_, port, _ := net.SplitHostPort(upstream)
	p, _ := strconv.Atoi(port)
	bus := make([]string, 0, len(busPorts))
	for _, b := range busPorts {
		bus = append(bus, strconv.Itoa(b))
	}
	cmd := exec.Command(self, "sandbox-bridge", "--dir", dir, "--upstream", upstream,
		"--parent", strconv.Itoa(os.Getpid()), "--bus-ports", strings.Join(bus, ","))
	cmd.Env = []string{"PATH=/usr/bin:/bin"} // it needs nothing from the agent env
	cmd.SysProcAttr = bridgeProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("sandbox bridge: start: %w", err)
	}
	sock := filepath.Join(dir, bridgeSocketName)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fi, err := os.Lstat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			go func() { _ = cmd.Wait() }()
			return &Bridge{Dir: dir, Socket: sock, Binary: self, Port: p}, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return nil, fmt.Errorf("sandbox bridge: no socket at %s after 3s", sock)
}
