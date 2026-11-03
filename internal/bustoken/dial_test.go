// SPDX-License-Identifier: Apache-2.0

package bustoken

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
)

// tokenServer starts a NATS server that requires a fresh token and writes the
// token where Read finds it (the test's HOME).
func tokenServer(t *testing.T) int {
	t.Helper()
	tempHome(t)
	tok, _ := New()
	ns, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: server.RANDOM_PORT, Authorization: tok, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(ns.Shutdown)
	port := ns.Addr().(*net.TCPAddr).Port
	if err := Write(port, tok); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestDialPresentsTheToken(t *testing.T) {
	port := tokenServer(t)
	nc, err := Dial(port)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	nc.Close()
	// Without the file the same server refuses, as an authorization error.
	p, _ := Path(port)
	os.Remove(p)
	if nc, err := Dial(port); err == nil {
		nc.Close()
		t.Fatal("dial without the token file was accepted")
	} else if !IsAuthError(err) {
		t.Fatalf("want an authorization error, got %v", err)
	}
}

// Every rysh dial of the session bus goes through this package. A raw
// nats.Connect is allowed only for an in-process server, a remote websocket
// URL (its own auth), or the operator's external NATS (cfg.URL).
func TestNoTokenlessSessionDials(t *testing.T) {
	root := filepath.Join("..", "..")
	call := regexp.MustCompile(`nats\.Connect\(`)
	allowed := []string{"bustoken.", "tokOpt", "InProcessServer", "wsURL", "WebSocketURL", "cfg.URL"}
	var bad []string
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if fi.IsDir() && (fi.Name() == "vendor" || fi.Name() == "testdata" || strings.HasPrefix(fi.Name(), ".")) && p != root {
			return filepath.SkipDir
		}
		if fi.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || filepath.Base(filepath.Dir(p)) == "bustoken" {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		src := string(data)
		for _, loc := range call.FindAllStringIndex(src, -1) {
			end := loc[1] + 400
			if end > len(src) {
				end = len(src)
			}
			// The call's own argument list: up to the first line that closes it.
			args := src[loc[1]:end]
			if i := strings.Index(args, ")\n"); i >= 0 {
				args = args[:i]
			}
			ok := false
			for _, a := range allowed {
				ok = ok || strings.Contains(args, a)
			}
			if !ok {
				line := strings.Count(src[:loc[0]], "\n") + 1
				bad = append(bad, p+":"+strconv.Itoa(line))
			}
		}
		return nil
	})
	if len(bad) > 0 {
		t.Errorf("session-bus dials without the bus token (use bustoken.Dial / bustoken.Option):\n%s", strings.Join(bad, "\n"))
	}
}
