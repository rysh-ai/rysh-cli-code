// SPDX-License-Identifier: Apache-2.0

package channels

// F-STOP (email): Stop must not return while the IMAP watcher is still
// running. Before this, Stop only cancelled the watcher's context and
// returned: the session stayed connected for a moment (and a stuck server
// could hold it for minutes), and the watcher could still fetch one more mail
// after Stop and publish it into a channel nobody reads (the QA-27 shape).

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels/fakeplatform"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

func TestEmailStopWaitsForTheWatcher(t *testing.T) {
	fake := fakeplatform.NewEmail("assistant@rysh-e2e.invalid")
	defer fake.Close()
	t.Chdir(t.TempDir())
	cc := msg.ChannelConfig{Enabled: true, EmailConfig: &msg.EmailChannelConfig{
		Address: fake.Account, IMAPHost: "127.0.0.1", IMAPPort: 993,
		SMTPHost: "127.0.0.1", SMTPPort: fake.SMTPPort(),
		Username: fake.Account, Password: "synthetic-password",
	}}
	// Keep the adapter's side of its newest IMAP connection.
	var mu sync.Mutex
	var last net.Conn
	e := NewEmailAdapterWithDialerForTest(cc, func(addr string) (net.Conn, error) {
		c, err := fake.Dial(addr)
		mu.Lock()
		last = c
		mu.Unlock()
		return c, err
	})
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !fake.WaitIdle(5 * time.Second) {
		t.Fatal("adapter never watched the fake INBOX")
	}
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// The watcher must already have hung up: a write on a closed pipe fails
	// with io.ErrClosedPipe at once; on an open one it times out instead.
	mu.Lock()
	c := last
	mu.Unlock()
	_ = c.SetWriteDeadline(time.Now())
	if _, err := c.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Stop returned while the IMAP watcher's connection was still open (write: %v)", err)
	}
}

// hungIDLEServer answers the login steps and enters IDLE, then never answers
// DONE: a stuck server. It reports when the client side closes the connection.
func hungIDLEServer(t *testing.T) (dial func(string) (net.Conn, error), closed <-chan struct{}) {
	t.Helper()
	gone := make(chan struct{})
	var once sync.Once
	dial = func(string) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer once.Do(func() { close(gone) })
			defer server.Close()
			_, _ = io.WriteString(server, "* OK fake IMAP ready\r\n")
			r := bufio.NewReader(server)
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					return // the client closed the connection
				}
				line = strings.TrimSpace(line)
				fields := strings.Fields(line)
				switch {
				case line == "DONE":
					// Stuck: never answer, keep reading until the client hangs up.
				case len(fields) >= 2 && strings.EqualFold(fields[1], "IDLE"):
					_, _ = io.WriteString(server, "+ idling\r\n")
				case len(fields) >= 2:
					_, _ = io.WriteString(server, "* CAPABILITY IMAP4rev1 IDLE\r\n* STATUS INBOX (UIDNEXT 1)\r\n"+fields[0]+" OK done\r\n")
				}
			}
		}()
		return client, nil
	}
	return dial, gone
}

func TestEmailStopReturnsAndHangsUpOnAStuckServer(t *testing.T) {
	dial, closed := hungIDLEServer(t)
	cc := msg.ChannelConfig{Enabled: true, EmailConfig: &msg.EmailChannelConfig{
		Address: "assistant@rysh-e2e.invalid", IMAPHost: "127.0.0.1", IMAPPort: 993,
		SMTPHost: "127.0.0.1", SMTPPort: 25,
		Username: "assistant@rysh-e2e.invalid", Password: "synthetic-password",
	}}
	t.Chdir(t.TempDir())
	// The first dial is Start's connectivity check, closed at once; the second
	// is the watcher's session. Use a fresh pipe per dial.
	var dials int32
	var dialledOnce sync.Once
	dialled := make(chan struct{})
	e := NewEmailAdapterWithDialerForTest(cc, func(addr string) (net.Conn, error) {
		if atomic.AddInt32(&dials, 1) == 1 {
			c, s := net.Pipe()
			go func() { _, _ = io.Copy(io.Discard, s) }()
			return c, nil
		}
		dialledOnce.Do(func() { close(dialled) })
		return dial(addr)
	})
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Let the watcher reach IDLE on the stuck server.
	select {
	case <-dialled:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher never dialled")
	}
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	done := make(chan struct{})
	go func() { _ = e.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop blocked on a server that never answers DONE")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Stop took %v on a stuck server; want it bounded (a few seconds)", d)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("after Stop the watcher's connection to the stuck server is still open")
	}
}

// security-lead N1: a stop that lands after the watcher's cancel check, while
// a fetch is in flight, must still keep that mail out of the stopped adapter.
// The hook cancels the watcher's context between fetch and dispatch.
func TestEmailNoDispatchAfterAStopDuringTheFetch(t *testing.T) {
	fake := fakeplatform.NewEmail("assistant@rysh-e2e.invalid")
	defer fake.Close()
	e := emailApprovalAdapter(t, fake)
	var once sync.Once
	emailBeforeDispatch = func() { once.Do(e.cancel) } // a Stop arriving mid-fetch
	t.Cleanup(func() { emailBeforeDispatch = nil })

	fake.DeliverMsg(fakeplatform.EmailMsg{From: "owner@rysh-e2e.invalid", Subject: "x", Body: "yes"})
	select {
	case in := <-e.InboundCh():
		t.Fatalf("a mail fetched as the adapter stopped was dispatched anyway: %q", in.Metadata["subject"])
	case <-time.After(500 * time.Millisecond):
	}
}
