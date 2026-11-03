// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// fakeBackend is an in-memory remote session: a fixed layout, per-pane screens,
// and a record of every command applied to it.
type fakeBackend struct {
	mu       sync.Mutex
	tabs     []domain.TabSnapshot
	screens  map[string]Screen
	applied  []Command
	applyErr error
	watched  []string
	events   chan Event
	closed   bool
}

func newFakeBackend(tabs ...domain.TabSnapshot) *fakeBackend {
	return &fakeBackend{tabs: tabs, screens: map[string]Screen{}, events: make(chan Event, 64)}
}

func (b *fakeBackend) Layout() ([]domain.TabSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]domain.TabSnapshot(nil), b.tabs...), nil
}

func (b *fakeBackend) Screen(paneID string) (Screen, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.screens[paneID], nil
}

func (b *fakeBackend) Apply(c Command) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.applied = append(b.applied, c)
	return b.applyErr
}

func (b *fakeBackend) Watch(ids []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.watched = append([]string(nil), ids...)
}

func (b *fakeBackend) Events() <-chan Event { return b.events }

func (b *fakeBackend) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
}

func (b *fakeBackend) setScreen(id string, s Screen) {
	b.mu.Lock()
	b.screens[id] = s
	b.mu.Unlock()
	b.events <- Event{Pane: id}
}

func (b *fakeBackend) setTabs(tabs ...domain.TabSnapshot) {
	b.mu.Lock()
	b.tabs = tabs
	b.mu.Unlock()
	b.events <- Event{Layout: true}
}

func (b *fakeBackend) appliedCmds() []Command {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Command(nil), b.applied...)
}

func oneTab(tabID string, paneIDs ...string) domain.TabSnapshot {
	g := domain.PaneGroupSnapshot{ID: "g-" + tabID, RowFlex: 10}
	for _, id := range paneIDs {
		g.Panes = append(g.Panes, domain.PaneSnapshot{ID: id, Title: id, Mode: "shell"})
	}
	if len(paneIDs) > 0 {
		g.ActivePaneID = paneIDs[0]
	}
	return domain.TabSnapshot{
		ID: tabID, Title: "tab " + tabID, ActivePaneID: g.ActivePaneID,
		Lanes: []domain.LaneSnapshot{{ID: "l-" + tabID, Flex: 10, ActivePaneID: g.ActivePaneID, PaneGroups: []domain.PaneGroupSnapshot{g}}},
	}
}

// duplex is one end of an in-process bidirectional pipe.
type duplex struct {
	io.Reader
	io.Writer
	closers []io.Closer
}

func (d *duplex) Close() error {
	for _, c := range d.closers {
		_ = c.Close()
	}
	return nil
}

// pipePair returns the client end and the server's reader/writer.
func pipePair() (client *duplex, srvR *io.PipeReader, srvW *io.PipeWriter) {
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	client = &duplex{Reader: s2cR, Writer: c2sW, closers: []io.Closer{c2sW, s2cR}}
	return client, c2sR, s2cW
}

// startPair runs Serve on a fake backend and returns a handshaken client.
func startPair(t *testing.T, b *fakeBackend, so ServeOptions, co ClientOptions) (*Client, chan error) {
	t.Helper()
	cEnd, sr, sw := pipePair()
	served := make(chan error, 1)
	go func() {
		err := Serve(context.Background(), sr, sw, b, so)
		_ = sw.Close()
		_ = sr.Close()
		served <- err
	}()
	c := NewClient(cEnd, co)
	t.Cleanup(func() { _ = c.Close() })
	return c, served
}

func nextFrame(t *testing.T, c *Client, typ string) Frame {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case f, ok := <-c.Frames():
			if !ok {
				t.Fatalf("frames closed waiting for %q: %v", typ, c.Err())
			}
			if f.T == typ {
				return f
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %q frame", typ)
		}
	}
}

func TestHandshakeOK(t *testing.T) {
	b := newFakeBackend(oneTab("t1", "p1"))
	c, _ := startPair(t, b, ServeOptions{Version: "v9.9.9", Session: "s1"}, ClientOptions{Version: "v1.0.0"})
	h, err := c.Handshake(context.Background())
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if h.Proto != ProtoVersion || h.Version != "v9.9.9" || h.Session != "s1" {
		t.Fatalf("hello = %+v", h)
	}
}

func TestHandshakeProtoMismatchRefused(t *testing.T) {
	b := newFakeBackend(oneTab("t1", "p1"))
	c, served := startPair(t, b, ServeOptions{Proto: ProtoVersion + 1, Version: "v2"}, ClientOptions{Version: "v1"})
	_, err := c.Handshake(context.Background())
	if !errors.Is(err, ErrProtoMismatch) {
		t.Fatalf("want ErrProtoMismatch, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "2") || !strings.Contains(msg, "1") {
		t.Fatalf("mismatch error should name both versions: %q", msg)
	}
	select {
	case serr := <-served:
		if !errors.Is(serr, ErrProtoMismatch) {
			t.Fatalf("server should end with ErrProtoMismatch, got %v", serr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not end after a mismatch")
	}
	if len(b.appliedCmds()) != 0 {
		t.Fatal("no command may reach the backend after a refused handshake")
	}
}

func TestClientRefusesServerWithOtherProto(t *testing.T) {
	// The client checks too: an old server that answers hello without refusing
	// (it does not know the client's proto) is still refused locally.
	cEnd, sr, sw := pipePair()
	go func() {
		br := bufio.NewReader(sr)
		_, _ = br.ReadBytes('\n')
		_ = json.NewEncoder(sw).Encode(Frame{T: FrameHello, Proto: 99, Version: "old"})
	}()
	c := NewClient(cEnd, ClientOptions{})
	defer c.Close()
	if _, err := c.Handshake(context.Background()); !errors.Is(err, ErrProtoMismatch) {
		t.Fatalf("want ErrProtoMismatch, got %v", err)
	}
}

func TestHandshakeSkipsBannerNoise(t *testing.T) {
	// A login shell rc that prints to stdout must not break the stream.
	cEnd, sr, sw := pipePair()
	go func() {
		br := bufio.NewReader(sr)
		_, _ = br.ReadBytes('\n')
		_, _ = sw.Write([]byte("Welcome to host\n\n"))
		_ = json.NewEncoder(sw).Encode(Frame{T: FrameHello, Proto: ProtoVersion, Version: "x"})
	}()
	c := NewClient(cEnd, ClientOptions{})
	defer c.Close()
	if _, err := c.Handshake(context.Background()); err != nil {
		t.Fatalf("handshake with banner: %v", err)
	}
}

func TestLayoutStreamedAndUpdated(t *testing.T) {
	b := newFakeBackend(oneTab("t1", "p1"))
	c, _ := startPair(t, b, ServeOptions{PollInterval: time.Hour}, ClientOptions{})
	if _, err := c.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := nextFrame(t, c, FrameLayout)
	if len(f.Tabs) != 1 || f.Tabs[0].ID != "t1" {
		t.Fatalf("first layout = %+v", f.Tabs)
	}
	b.setTabs(oneTab("t1", "p1", "p2"), oneTab("t2", "p3"))
	f = nextFrame(t, c, FrameLayout)
	if len(f.Tabs) != 2 {
		t.Fatalf("updated layout has %d tabs", len(f.Tabs))
	}
	// The server watches every pane in the layout for screen changes.
	deadline := time.Now().Add(2 * time.Second)
	for {
		b.mu.Lock()
		w := strings.Join(b.watched, ",")
		b.mu.Unlock()
		if w == "p1,p2,p3" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("watched = %q", w)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestScreenPushedOnDirty(t *testing.T) {
	b := newFakeBackend(oneTab("t1", "p1"))
	c, _ := startPair(t, b, ServeOptions{PollInterval: time.Hour, ScreenThrottle: time.Millisecond}, ClientOptions{})
	if _, err := c.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	nextFrame(t, c, FrameLayout)
	b.setScreen("p1", Screen{Interactive: true, Rows: []string{"$ echo hi", "hi"}, CursorRow: 1, CursorCol: 2})
	f := nextFrame(t, c, FrameScreen)
	if f.Pane != "p1" || !f.Interactive || len(f.Screen) != 2 || f.Screen[1] != "hi" || f.CursorRow != 1 || f.CursorCol != 2 {
		t.Fatalf("screen frame = %+v", f)
	}
}

func TestTextPaneRepolledAndDeduplicated(t *testing.T) {
	b := newFakeBackend(oneTab("t1", "p1"))
	b.screens["p1"] = Screen{Text: "$ ls\na b\n"}
	c, _ := startPair(t, b, ServeOptions{PollInterval: 20 * time.Millisecond, ScreenThrottle: time.Millisecond}, ClientOptions{})
	if _, err := c.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := nextFrame(t, c, FrameScreen)
	if f.Interactive || f.Text != "$ ls\na b\n" {
		t.Fatalf("text frame = %+v", f)
	}
	// No event at all: the poll alone must pick up new text.
	b.mu.Lock()
	b.screens["p1"] = Screen{Text: "$ ls\na b\n$ pwd\n/tmp\n"}
	b.mu.Unlock()
	f = nextFrame(t, c, FrameScreen)
	if f.Text != "$ ls\na b\n$ pwd\n/tmp\n" {
		t.Fatalf("repolled text = %q", f.Text)
	}
	// Unchanged text is not re-sent on later polls.
	select {
	case g := <-c.Frames():
		if g.T == FrameScreen {
			t.Fatalf("unchanged screen re-sent: %+v", g)
		}
	case <-time.After(150 * time.Millisecond):
	}
}

func TestCommandAppliedAndAcked(t *testing.T) {
	b := newFakeBackend(oneTab("t1", "p1"))
	c, _ := startPair(t, b, ServeOptions{PollInterval: time.Hour}, ClientOptions{})
	if _, err := c.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	nextFrame(t, c, FrameLayout)
	cmd := Command{Type: CmdRawKeystroke, Pane: "p1", Payload: "aGk="}
	if err := c.Do(context.Background(), cmd); err != nil {
		t.Fatalf("do: %v", err)
	}
	got := b.appliedCmds()
	if len(got) != 1 || got[0] != cmd {
		t.Fatalf("applied = %+v", got)
	}
}

func TestDoAsyncKeepsOrderAndAcksEach(t *testing.T) {
	b := newFakeBackend(oneTab("t1", "p1"))
	c, _ := startPair(t, b, ServeOptions{PollInterval: time.Hour}, ClientOptions{})
	if _, err := c.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	nextFrame(t, c, FrameLayout)
	var results []<-chan error
	for _, k := range []string{"YQ==", "Yg==", "Yw==", "ZA=="} {
		results = append(results, c.DoAsync(Command{Type: CmdRawKeystroke, Pane: "p1", Payload: k}))
	}
	for i, r := range results {
		select {
		case err := <-r:
			if err != nil {
				t.Fatalf("key %d: %v", i, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("key %d: no outcome", i)
		}
	}
	var got []string
	for _, a := range b.appliedCmds() {
		got = append(got, a.Payload)
	}
	if strings.Join(got, ",") != "YQ==,Yg==,Yw==,ZA==" {
		t.Fatalf("order = %v", got)
	}
	bad := c.DoAsync(Command{Type: CmdRawKeystroke, Pane: "gone", Payload: "YQ=="})
	if err := <-bad; err == nil {
		t.Fatal("refused async command must report an error")
	}
}

func TestCommandBackendErrorReturned(t *testing.T) {
	b := newFakeBackend(oneTab("t1", "p1"))
	b.applyErr = errors.New("pane busy")
	c, _ := startPair(t, b, ServeOptions{PollInterval: time.Hour}, ClientOptions{})
	if _, err := c.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	nextFrame(t, c, FrameLayout)
	err := c.Do(context.Background(), Command{Type: CmdExecShell, Pane: "p1", Payload: "ls"})
	if err == nil || !strings.Contains(err.Error(), "pane busy") {
		t.Fatalf("want backend error, got %v", err)
	}
}

func TestCommandRefusedForUnknownPaneOrType(t *testing.T) {
	b := newFakeBackend(oneTab("t1", "p1"))
	c, _ := startPair(t, b, ServeOptions{PollInterval: time.Hour}, ClientOptions{})
	if _, err := c.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	nextFrame(t, c, FrameLayout)
	if err := c.Do(context.Background(), Command{Type: CmdExecShell, Pane: "nope", Payload: "ls"}); err == nil {
		t.Fatal("a pane outside the remote layout must be refused")
	}
	if err := c.Do(context.Background(), Command{Type: "format_disk", Pane: "p1"}); err == nil {
		t.Fatal("an unknown command type must be refused")
	}
	if n := len(b.appliedCmds()); n != 0 {
		t.Fatalf("refused commands reached the backend: %d", n)
	}
}

func TestCommandFailsClosedOnDeadTransport(t *testing.T) {
	cEnd, sr, sw := pipePair()
	go func() {
		br := bufio.NewReader(sr)
		_, _ = br.ReadBytes('\n')
		_ = json.NewEncoder(sw).Encode(Frame{T: FrameHello, Proto: ProtoVersion})
		_ = sw.Close() // transport drops right after the handshake
	}()
	c := NewClient(cEnd, ClientOptions{AckTimeout: 5 * time.Second})
	defer c.Close()
	if _, err := c.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("client did not notice the dead transport")
	}
	start := time.Now()
	if err := c.Do(context.Background(), Command{Type: CmdRawKeystroke, Pane: "p1", Payload: "eA=="}); err == nil {
		t.Fatal("Do on a dead transport must fail")
	}
	if time.Since(start) > time.Second {
		t.Fatal("Do on a dead transport must fail fast, not wait for the ack timeout")
	}
}

func TestCommandAckTimeout(t *testing.T) {
	cEnd, sr, sw := pipePair()
	go func() {
		br := bufio.NewReader(sr)
		_, _ = br.ReadBytes('\n')
		_ = json.NewEncoder(sw).Encode(Frame{T: FrameHello, Proto: ProtoVersion})
		for { // swallow everything, never ack
			if _, err := br.ReadBytes('\n'); err != nil {
				return
			}
		}
	}()
	c := NewClient(cEnd, ClientOptions{AckTimeout: 50 * time.Millisecond})
	defer c.Close()
	if _, err := c.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := c.Do(context.Background(), Command{Type: CmdRawKeystroke, Pane: "p1", Payload: "eA=="})
	if !errors.Is(err, ErrNotDelivered) {
		t.Fatalf("want ErrNotDelivered, got %v", err)
	}
}

func TestPingPong(t *testing.T) {
	b := newFakeBackend(oneTab("t1", "p1"))
	c, _ := startPair(t, b, ServeOptions{PollInterval: time.Hour}, ClientOptions{})
	if _, err := c.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestServerClosesBackendWhenClientLeaves(t *testing.T) {
	b := newFakeBackend(oneTab("t1", "p1"))
	c, served := startPair(t, b, ServeOptions{PollInterval: time.Hour}, ClientOptions{})
	if _, err := c.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not end when the client left")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		t.Fatal("backend not closed")
	}
}
