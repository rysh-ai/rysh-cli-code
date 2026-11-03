// SPDX-License-Identifier: Apache-2.0

package channels

// IDLE must never have two readers on the connection. waitIDLE starts a
// goroutine that blocks in readLine while the server is idle. When IDLE ends
// by timeout or by context cancel, that goroutine is still reading; if
// waitIDLE then reads the reply to DONE itself, two goroutines share one
// bufio.Reader. That raced (go test -race) and could panic with
// "slice bounds out of range" in bufio.ReadSlice, or leave the tagged OK with
// the goroutine so the caller waited minutes for a line that never came.

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// idleScriptServer plays the server side of one IDLE cycle plus one NOOP over
// a net.Pipe. afterDone is what the server sends once it reads DONE.
func idleScriptServer(t *testing.T, afterDone string) *imapClient {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close(); _ = serverConn.Close() })

	go func() {
		buf := make([]byte, 4096)
		read := func() (string, bool) {
			_ = serverConn.SetReadDeadline(time.Now().Add(5 * time.Second))
			n, err := serverConn.Read(buf)
			if err != nil {
				return "", false
			}
			return string(buf[:n]), true
		}
		write := func(s string) bool {
			_ = serverConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_, err := serverConn.Write([]byte(s))
			return err == nil
		}
		if l, ok := read(); !ok || !strings.HasSuffix(l, "IDLE\r\n") {
			return
		}
		if !write("+ idling\r\n") {
			return
		}
		if l, ok := read(); !ok || l != "DONE\r\n" {
			return
		}
		if !write(afterDone) {
			return
		}
		if l, ok := read(); !ok || !strings.HasSuffix(l, "NOOP\r\n") {
			return
		}
		_ = write("A0002 OK NOOP completed\r\n")
	}()
	return newIMAPClient(clientConn)
}

// runIDLEThenNOOP runs one waitIDLE and one NOOP, failing the test if either
// errors or does not finish within a few seconds.
func runIDLEThenNOOP(t *testing.T, ctx context.Context, c *imapClient, timeout time.Duration) {
	t.Helper()
	done := make(chan error, 2)
	go func() {
		e := &EmailAdapter{}
		if err := e.waitIDLE(ctx, c, timeout); err != nil {
			done <- err
			return
		}
		done <- c.command("NOOP", "")
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("IDLE then NOOP: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("IDLE then NOOP did not finish: the reply to DONE went to the wrong reader")
	}
}

func TestEmailIDLETimeoutHasOneReader(t *testing.T) {
	c := idleScriptServer(t, "A0001 OK IDLE terminated\r\n")
	runIDLEThenNOOP(t, context.Background(), c, 20*time.Millisecond)
}

func TestEmailIDLECancelHasOneReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	c := idleScriptServer(t, "A0001 OK IDLE terminated\r\n")
	runIDLEThenNOOP(t, ctx, c, time.Minute)
}

// A server may send an untagged line (new mail) after DONE and before the
// tagged OK. The goroutine takes the untagged line; the tagged OK must still
// be found.
func TestEmailIDLEUntaggedBeforeTaggedOK(t *testing.T) {
	c := idleScriptServer(t, "* 3 EXISTS\r\nA0001 OK IDLE terminated\r\n")
	runIDLEThenNOOP(t, context.Background(), c, 20*time.Millisecond)
}

// The reply to DONE is still checked: a tagged NO is an error.
func TestEmailIDLETaggedNOAfterDoneIsError(t *testing.T) {
	c := idleScriptServer(t, "A0001 NO IDLE failed\r\n")
	done := make(chan error, 1)
	go func() { done <- (&EmailAdapter{}).waitIDLE(context.Background(), c, 20*time.Millisecond) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "A0001 NO") {
			t.Fatalf("want the tagged NO as an error, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waitIDLE did not finish")
	}
}
