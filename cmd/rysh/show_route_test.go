// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// E17 (o4), design 020 §5.9: `show` was missing from the CLI usage banner,
// though `##agent show` / `##humanoid show` work in-session, and the
// `rysh --agent show <name>` flag route had only a static-dispatch test.

// captureStdout runs f and returns what it printed.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	f()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

func TestUsageBannerListsShow(t *testing.T) {
	out := captureStdout(t, printUsage)
	block := func(start string) string {
		i := strings.Index(out, start)
		if i < 0 {
			t.Fatalf("usage has no %q line", start)
		}
		rest := out[i:]
		// The block runs until the next "rysh --" entry.
		if j := strings.Index(rest[len(start):], "rysh --"); j >= 0 {
			rest = rest[:len(start)+j]
		}
		return rest
	}
	for _, flag := range []string{"rysh --agent", "rysh --humanoid"} {
		if b := block(flag); !strings.Contains(b, "show") {
			t.Errorf("usage for %s does not list show:\n%s", flag, b)
		}
	}
}

// TestShowFlagRouteEndToEnd drives `rysh --agent show <name>` and `rysh
// --humanoid show <name>` through run(), cmd/rysh's real argument dispatch,
// into a fake daemon on a real NATS port, and checks the "##" command that
// arrives and the output printed back.
func TestShowFlagRouteEndToEnd(t *testing.T) {
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(ns.Shutdown)
	port := ns.Addr().(*net.TCPAddr).Port

	const sess = "o4-show"
	dir := t.TempDir()
	cfg := config.Config{SessionDir: dir, SessionName: sess}
	store, err := session.NewStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(session.Record{Name: sess, State: "detached", NATSPort: port, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	codecs := msg.DefaultCodecRegistry()
	got := make(chan string, 4)
	_, err = nc.Subscribe(sess+".ws.inbox", func(m *nats.Msg) {
		var env msg.NATSEnvelope
		if json.Unmarshal(m.Data, &env) != nil {
			return
		}
		d, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		c, ok := d.(*msg.MsgCLIRyshCommand)
		if !ok {
			return
		}
		got <- c.Command
		re := &msg.RequestEnvelope{ReplyTo: env.ReplyTo, NC: nc, Codecs: codecs}
		_ = re.Reply(&msg.MsgCLIResponse{OK: true, Output: "recipe for " + c.Command})
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = nc.Flush()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct{ flag, want string }{
		{"--agent", "agent show scribe"},
		{"--humanoid", "humanoid show mailbot"},
	} {
		name := strings.Fields(tc.want)[2]
		var runErr error
		out := captureStdout(t, func() {
			runErr = run(cfg, logger, []string{tc.flag, "show", name, "--session", sess}, "", false, false, false)
		})
		if runErr != nil {
			t.Fatalf("rysh %s show %s: %v", tc.flag, name, runErr)
		}
		select {
		case cmd := <-got:
			if cmd != tc.want {
				t.Errorf("rysh %s show %s reached the daemon as %q, want %q", tc.flag, name, cmd, tc.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("rysh %s show %s never reached the daemon", tc.flag, name)
		}
		if !strings.Contains(out, "recipe for "+tc.want) {
			t.Errorf("rysh %s show %s printed %q, want the daemon's output", tc.flag, name, out)
		}
	}
}
