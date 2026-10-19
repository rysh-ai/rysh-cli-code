// SPDX-License-Identifier: Apache-2.0

package courier

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func sockSet(paths ...string) func(string) bool {
	m := map[string]bool{}
	for _, p := range paths {
		m[p] = true
	}
	return func(p string) bool { return m[p] }
}

func TestPlan(t *testing.T) {
	cases := []struct {
		name      string
		t         Target
		socks     []string
		want      Route
		permanent bool
		errHas    string
	}{
		{"input-stream fleet is refused for good",
			Target{PaneID: "p", Program: "2.1.296", PGID: 42, Meta: map[string]string{MetaFleetComm: "input-stream"}},
			[]string{"/tmp/cc-socks/42.sock"}, Route{}, true, "input-stream"},
		{"stamped claude inbox wins",
			Target{PaneID: "p", Program: "2.1.296", PGID: 42, Meta: map[string]string{MetaClaudeInbox: "/x/inbox.sock"}},
			[]string{"/x/inbox.sock", "/tmp/cc-socks/42.sock"}, Route{RouteClaudeInbox, "/x/inbox.sock"}, false, ""},
		{"stale stamp falls back to the pid socket",
			Target{PaneID: "p", Program: "2.1.296", PGID: 42, Meta: map[string]string{MetaClaudeInbox: "/x/dead.sock"}},
			[]string{"/tmp/cc-socks/42.sock"}, Route{RouteClaudeInbox, "/tmp/cc-socks/42.sock"}, false, ""},
		{"claude found by pid, whatever its executable is called",
			Target{PaneID: "p", Program: "2.1.296", PGID: 42},
			[]string{"/tmp/cc-socks/42.sock"}, Route{RouteClaudeInbox, "/tmp/cc-socks/42.sock"}, false, ""},
		{"codex with a discovered thread",
			Target{PaneID: "p", Program: "codex", PGID: 7, Meta: map[string]string{MetaCodexSession: "01a1"}},
			nil, Route{RouteCodexQueue, "01a1"}, false, ""},
		{"a thread id alone does not make vim a codex",
			Target{PaneID: "p", Program: "vim", PGID: 7, Meta: map[string]string{MetaCodexSession: "01a1"}},
			nil, Route{}, false, "no inbox found"},
		{"agent running but no door yet is retried, not permanent",
			Target{PaneID: "p", Program: "2.1.296", PGID: 42},
			nil, Route{}, false, "no inbox found"},
		{"native rysh agent",
			Target{PaneID: "p", Meta: map[string]string{MetaFleetAgent: "rysh"}},
			nil, Route{RouteRyshNative, "p"}, false, ""},
		{"a plain shell is not an agent",
			Target{PaneID: "p"}, nil, Route{}, false, "shell prompt"},
		{"an exited codex is not queued to",
			Target{PaneID: "p", Meta: map[string]string{MetaCodexSession: "01a1", MetaFleetAgent: "codex"}},
			nil, Route{}, false, "shell prompt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Plan(tc.t, sockSet(tc.socks...))
			if tc.errHas != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errHas) {
					t.Fatalf("err = %v, want it to mention %q", err, tc.errHas)
				}
				if errors.Is(err, ErrPermanent) != tc.permanent {
					t.Errorf("permanent = %v, want %v", errors.Is(err, ErrPermanent), tc.permanent)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Plan = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func cmd(from, to, id string) *msg.MsgBoardPost {
	p := msg.NewBoardPost(from, "p-"+from, msg.BoardKindCmd, "do "+id, time.Now().UnixMilli())
	p.ToPaneID, p.ToPersona, p.CmdID, p.ThreadID = to, "p-"+to, id, msg.CmdThreadID(from, id)
	return p
}

func ev(boardID string, p *msg.MsgBoardPost) board.Event {
	return board.Event{Kind: board.EventPost, Board: boardID, Post: p}
}

func TestComposeTellsTheAgentHowToAnswer(t *testing.T) {
	p := cmd("ceo-pane-uuid", "wkr", "msg-0042")
	p.File = "/tmp/order.md"
	got := Compose("alpha", p, "/opt/rysh/bin/rysh", "my sess")
	for _, want := range []string{"rysh board alpha", "command msg-0042", "from p-ceo-pane-uuid (ceo-pane)",
		"do msg-0042", "File: /tmp/order.md",
		"/opt/rysh/bin/rysh board ack msg-0042 --as wkr --board alpha --session 'my sess'"} {
		if !strings.Contains(got, want) {
			t.Errorf("composed text missing %q:\n%s", want, got)
		}
	}
}

// harness: a courier whose deliveries are recorded, with scripted outcomes.
type harness struct {
	mu        sync.Mutex
	delivered []string // cmd ids in delivery order
	attempts  map[string]int
	notices   []*msg.MsgBoardPost
	fail      map[string]int // cmd id -> fail this many attempts first
	target    Target
}

func newHarness(t *testing.T, backoff []time.Duration) (*Courier, *harness) {
	h := &harness{attempts: map[string]int{}, fail: map[string]int{},
		target: Target{Program: "2.1.296", PGID: 42}}
	c := New(Deps{
		Snapshot: func(paneID string) (Target, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			tg := h.target
			tg.PaneID = paneID
			return tg, nil
		},
		IsSocket: sockSet("/tmp/cc-socks/42.sock"),
		Deliver: func(_ context.Context, r Route, _ Target, text string) error {
			id := strings.Fields(strings.SplitN(text, "command ", 2)[1])[0]
			h.mu.Lock()
			defer h.mu.Unlock()
			h.attempts[id]++
			if h.fail[id] > 0 {
				h.fail[id]--
				return errors.New("socket busy")
			}
			h.delivered = append(h.delivered, id)
			return nil
		},
		Notify: func(_ string, p *msg.MsgBoardPost) {
			h.mu.Lock()
			h.notices = append(h.notices, p)
			h.mu.Unlock()
		},
		Backoff: backoff,
	})
	t.Cleanup(c.Close)
	return c, h
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 3s")
}

func TestCourierDeliversInOrderOnceAndIgnoresStatus(t *testing.T) {
	c, h := newHarness(t, []time.Duration{20 * time.Millisecond})
	h.fail["c1"] = 1 // c1 fails once; c2 and c3 must still wait behind it
	c.Handle(ev("alpha", msg.NewBoardPost("ceo", "ceo", msg.BoardKindMilestone, "status, not work", 1)))
	for _, id := range []string{"c1", "c2", "c3"} {
		c.Handle(ev("alpha", cmd("ceo", "wkr", id)))
	}
	c.Handle(ev("alpha", cmd("ceo", "wkr", "c2"))) // a retried send: same (sender, id)

	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.delivered) == 3 })
	h.mu.Lock()
	defer h.mu.Unlock()
	if strings.Join(h.delivered, ",") != "c1,c2,c3" {
		t.Errorf("delivery order = %v, want c1,c2,c3 (per-recipient FIFO)", h.delivered)
	}
	if h.attempts["c1"] != 2 {
		t.Errorf("c1 attempts = %d, want 2 (one failure, one retry)", h.attempts["c1"])
	}
	if h.attempts["c2"] != 1 {
		t.Errorf("c2 delivered %d times, want once — a duplicate post is one command", h.attempts["c2"])
	}
	if len(h.notices) != 0 {
		t.Errorf("unexpected notices: %+v", h.notices)
	}
}

func TestCourierReportsUndeliveredOnTheCommandThread(t *testing.T) {
	c, h := newHarness(t, []time.Duration{time.Millisecond, time.Millisecond})
	h.mu.Lock()
	h.fail["c9"] = 99
	h.mu.Unlock()
	p := cmd("ceo", "wkr", "c9")
	c.Handle(ev("alpha", p))

	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.notices) == 1 })
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.attempts["c9"] != 3 {
		t.Errorf("attempts = %d, want 3 (1 + len(backoff))", h.attempts["c9"])
	}
	n := h.notices[0]
	if n.Kind != UndeliveredKind || n.ThreadID != p.ThreadID || n.CmdID != "c9" || n.PaneID != Identity {
		t.Errorf("notice = %+v; want an undelivered post from the courier on the command's thread", n)
	}
	if !strings.Contains(n.Text, "socket busy") || !strings.Contains(n.Text, "p-wkr") {
		t.Errorf("notice text %q should name the recipient and the reason", n.Text)
	}
}

func TestCourierDoesNotRetryAPermanentRefusal(t *testing.T) {
	c, h := newHarness(t, []time.Duration{time.Hour}) // a retry would hang the test
	h.mu.Lock()
	h.target.Meta = map[string]string{MetaFleetComm: CommInputStream}
	h.mu.Unlock()
	c.Handle(ev("alpha", cmd("ceo", "wkr", "c5")))
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.notices) == 1 })
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.attempts["c5"] != 0 || !strings.Contains(h.notices[0].Text, "input-stream") {
		t.Errorf("attempts=%d notice=%q; want no delivery and an input-stream notice", h.attempts["c5"], h.notices[0].Text)
	}
}

func TestSendClaudeInboxWritesTheWireFormat(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "crt") // unix socket paths are short on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "1.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		got <- line
	}()

	if !IsSocket(path) {
		t.Fatal("IsSocket says no for a listening socket")
	}
	if err := SendClaudeInbox(context.Background(), path, "hello \"agent\"\nline 2"); err != nil {
		t.Fatal(err)
	}
	var line string
	select {
	case line = <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("no line arrived")
	}
	var m struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
		t.Errorf("want exactly one newline-terminated line, got %q", line)
	}
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("not JSON: %q: %v", line, err)
	}
	if m.Type != "user" || m.Message.Role != "user" || m.Message.Content != "hello \"agent\"\nline 2" {
		t.Errorf("decoded %+v", m)
	}
	if err := SendClaudeInbox(context.Background(), filepath.Join(dir, "none.sock"), "x"); err == nil {
		t.Error("a missing socket must be an error")
	}
}

func TestSendCodexQueue(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	ok := filepath.Join(dir, "codex-ok")
	_ = os.WriteFile(ok, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsFile+"\necho \"Queued message abc for thread $3.\"\n"), 0o755)
	if err := SendCodexQueue(context.Background(), ok, "01a1", "do it"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(argsFile)
	if got := strings.Fields(string(b)); strings.Join(got, " ") != "queue --thread 01a1 --message do it" {
		t.Errorf("codex argv = %q", got)
	}

	silent := filepath.Join(dir, "codex-silent")
	_ = os.WriteFile(silent, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	if err := SendCodexQueue(context.Background(), silent, "01a1", "x"); err == nil {
		t.Error("exit 0 with no receipt must not count as delivered")
	}
	failing := filepath.Join(dir, "codex-fail")
	_ = os.WriteFile(failing, []byte("#!/bin/sh\necho 'no such thread' >&2\nexit 1\n"), 0o755)
	if err := SendCodexQueue(context.Background(), failing, "01a1", "x"); err == nil || !strings.Contains(err.Error(), "no such thread") {
		t.Errorf("err = %v, want codex's own message", err)
	}
}
