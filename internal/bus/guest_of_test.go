// SPDX-License-Identifier: Apache-2.0

package bus

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// Design 028 §9 limit 7: a daemon that joins an existing NATS server as a
// GUEST keeps its session's state in that server's store. From 2026-10-02 to
// 10-09 the live daemon was a guest of a stray `test` daemon and nothing said
// so. Guest mode stays (the desktop sidecar depends on it); these tests pin
// that it is no longer SILENT.
//
// Every server here is in-process, on a throwaway port, over a t.TempDir()
// store, and is shut down in cleanup. None of them touches 24242 or
// ~/.rysh/nats.

// syncBuf is a goroutine-safe log sink: the host query logs from its own
// goroutine.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func testLogger() (*slog.Logger, *syncBuf) {
	buf := &syncBuf{}
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// startHostBus starts a daemon-like Bus that OWNS its embedded server on port.
func startHostBus(t *testing.T, port int, session, workspace string) *Bus {
	t.Helper()
	host, err := New(Config{Mode: "embedded", Port: port, SessionName: session,
		Workspace: workspace, DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("host bus: %v", err)
	}
	t.Cleanup(host.Close)
	if host.guest || host.ns == nil {
		t.Fatalf("host on :%d did not start its own server (port taken?)", port)
	}
	return host
}

// waitGuestOf waits for the bounded host query to settle.
func waitGuestOf(t *testing.T, b *Bus) GuestOf {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		g, ok := b.GuestOf()
		if !ok {
			t.Fatal("GuestOf() reports not-a-guest on a guest bus")
		}
		if !g.Pending {
			return g
		}
		if time.Now().After(deadline) {
			t.Fatalf("host query never settled: %+v", g)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestGuestOfAHostFromAnotherWorkspaceWarns(t *testing.T) {
	port := freePort(t)
	startHostBus(t, port, "test", "/stray/test-daemon")
	log, logs := testLogger()

	g, err := New(Config{Mode: "embedded", Port: port, SessionName: "rysh-macminim5",
		Workspace: "/Users/me/rysh-ai", Logger: log})
	if err != nil {
		t.Fatalf("guest bus: %v", err)
	}
	defer g.Close()

	got := waitGuestOf(t, g)
	if got.Port != port || got.ServerID == "" {
		t.Errorf("guest_of server = id %q port %d; want the host's id and port %d", got.ServerID, got.Port, port)
	}
	if got.HostSession != "test" || got.HostWorkspace != "/stray/test-daemon" {
		t.Errorf("guest_of host = session %q workspace %q; want test, /stray/test-daemon", got.HostSession, got.HostWorkspace)
	}
	if !got.Mismatch || got.Warning == "" {
		t.Errorf("mismatch=%v warning=%q; a host from another session/workspace must warn", got.Mismatch, got.Warning)
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "test") {
		t.Errorf("no loud WARN naming the host in the log:\n%s", out)
	}
}

func TestGuestOfAMatchingHostIsSilent(t *testing.T) {
	port := freePort(t)
	// The desktop sidecar case: the host IS this session's daemon.
	startHostBus(t, port, "s1", "/Users/me/proj")
	log, logs := testLogger()

	g, err := New(Config{Mode: "embedded", Port: port, SessionName: "s1",
		Workspace: "/Users/me/proj/", Logger: log})
	if err != nil {
		t.Fatalf("guest bus: %v", err)
	}
	defer g.Close()

	got := waitGuestOf(t, g)
	if got.HostSession != "s1" || got.ServerID == "" {
		t.Errorf("guest_of = %+v; want the host recorded", got)
	}
	if got.Mismatch || got.Warning != "" {
		t.Errorf("mismatch=%v warning=%q; a matching host must stay silent", got.Mismatch, got.Warning)
	}
	if out := logs.String(); strings.Contains(out, "level=WARN") {
		t.Errorf("a matching host logged a warning:\n%s", out)
	}
	if out := logs.String(); !strings.Contains(strings.ToLower(out), "joined an existing nats server as a guest") {
		t.Errorf("joining as a guest was not logged at all:\n%s", out)
	}
}

func TestANonGuestHasNoGuestOf(t *testing.T) {
	b := startHostBus(t, freePort(t), "own", "/Users/me/proj")
	if g, ok := b.GuestOf(); ok || g != (GuestOf{}) {
		t.Errorf("GuestOf() = %+v, %v on a bus that owns its server; want empty, false", g, ok)
	}
	private, err := New(Config{Mode: "embedded", Port: 0, SessionName: "p", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("private bus: %v", err)
	}
	defer private.Close()
	if g, ok := private.GuestOf(); ok || g != (GuestOf{}) {
		t.Errorf("GuestOf() = %+v, %v on a private bus; want empty, false", g, ok)
	}
}

// A host that never answers must not hold the guest's startup, and must be
// recorded as "unknown" with the reason.
func TestAHostThatDoesNotAnswerIsUnknownAndNeverBlocksStartup(t *testing.T) {
	port := freePort(t)
	startNATS(t, port)
	// Something answers on the identity subject, too slowly.
	nc, err := nats.Connect(natsURL(port))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer nc.Close()
	release := make(chan struct{})
	defer close(release)
	if _, err := nc.Subscribe(HostIdentitySubject, func(m *nats.Msg) {
		<-release
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	log, _ := testLogger()
	start := time.Now()
	g, err := New(Config{Mode: "embedded", Port: port, SessionName: "s", Workspace: "/w",
		Logger: log, HostQueryTimeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatalf("guest bus: %v", err)
	}
	defer g.Close()
	if d := time.Since(start); d > 250*time.Millisecond+2*time.Second {
		t.Errorf("New took %v; the host query must not block startup", d)
	}
	got := waitGuestOf(t, g)
	if got.HostSession != "unknown" || got.HostWorkspace != "unknown" {
		t.Errorf("guest_of host = %q/%q; want unknown/unknown", got.HostSession, got.HostWorkspace)
	}
	if !strings.Contains(got.UnknownReason, "did not answer") {
		t.Errorf("unknown reason = %q; want it to say the host did not answer in time", got.UnknownReason)
	}
	if got.Mismatch {
		t.Error("an unknown host cannot be called a mismatch")
	}
}

// A bare NATS server (not a rysh daemon, or one older than this field) has no
// responder at all: unknown, and the reason says so.
func TestAHostWithNoResponderIsUnknown(t *testing.T) {
	port := freePort(t)
	startNATS(t, port)
	g, err := New(Config{Mode: "embedded", Port: port, SessionName: "s", Workspace: "/w"})
	if err != nil {
		t.Fatalf("guest bus: %v", err)
	}
	defer g.Close()
	got := waitGuestOf(t, g)
	if got.HostSession != "unknown" || !strings.Contains(got.UnknownReason, "no rysh daemon answered") {
		t.Errorf("guest_of = %+v; want unknown with a no-responder reason", got)
	}
}

func natsURL(port int) string {
	return "nats://127.0.0.1:" + strconv.Itoa(port)
}

// /health is public: its view of guest_of must not carry a filesystem path,
// and the warning text is where paths would otherwise leak.
func TestThePublicViewCarriesNoPaths(t *testing.T) {
	port := freePort(t)
	startHostBus(t, port, "test", "/stray/test-daemon")
	g, err := New(Config{Mode: "embedded", Port: port, SessionName: "me", Workspace: "/Users/me/rysh-ai"})
	if err != nil {
		t.Fatalf("guest bus: %v", err)
	}
	defer g.Close()
	full := waitGuestOf(t, g)
	pub := full.Public()
	if !pub.Mismatch || pub.Warning == "" || pub.HostSession != "test" {
		t.Errorf("public view lost the mismatch: %+v", pub)
	}
	b, _ := json.Marshal(pub)
	for _, path := range []string{"/stray/test-daemon", "/Users/me/rysh-ai"} {
		if strings.Contains(string(b), path) {
			t.Errorf("public guest_of leaks %q: %s", path, b)
		}
	}
	if strings.Contains(string(b), `"host_workspace"`) {
		t.Errorf("public guest_of still has a host_workspace key: %s", b)
	}
}

// ORDER-13 / root-0273 ruling: WARN on a WORKSPACE mismatch only. A host that
// is another SESSION of the same project (the desktop sidecar's daemon under
// its own session name) is INFO: recorded, not a warning.

// (a) same workspace, different session: INFO, no WARN; the status field shows
// the difference but carries no warning.
func TestASessionOnlyMismatchIsInfoNotWarn(t *testing.T) {
	port := freePort(t)
	startHostBus(t, port, "app-sidecar", "/Users/me/proj")
	log, logs := testLogger()
	g, err := New(Config{Mode: "embedded", Port: port, SessionName: "cli",
		Workspace: "/Users/me/proj", Logger: log})
	if err != nil {
		t.Fatalf("guest bus: %v", err)
	}
	defer g.Close()
	got := waitGuestOf(t, g)
	if !got.SessionMismatch || got.WorkspaceMismatch {
		t.Errorf("session_mismatch=%v workspace_mismatch=%v; want true, false", got.SessionMismatch, got.WorkspaceMismatch)
	}
	if got.Mismatch || got.Warning != "" {
		t.Errorf("mismatch=%v warning=%q; a session-only difference is not a warning", got.Mismatch, got.Warning)
	}
	if !strings.Contains(got.Note, "app-sidecar") {
		t.Errorf("note = %q; want it to name the host session", got.Note)
	}
	out := logs.String()
	if strings.Contains(out, "level=WARN") {
		t.Errorf("a session-only mismatch logged a WARN:\n%s", out)
	}
	if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "app-sidecar") {
		t.Errorf("a session-only mismatch must still be logged at INFO, naming the host:\n%s", out)
	}
}

// (b) same session name, different workspace: WARN.
func TestAWorkspaceMismatchWarns(t *testing.T) {
	port := freePort(t)
	startHostBus(t, port, "s1", "/Users/me/other-project")
	log, logs := testLogger()
	g, err := New(Config{Mode: "embedded", Port: port, SessionName: "s1",
		Workspace: "/Users/me/proj", Logger: log})
	if err != nil {
		t.Fatalf("guest bus: %v", err)
	}
	defer g.Close()
	got := waitGuestOf(t, g)
	if got.SessionMismatch || !got.WorkspaceMismatch || !got.Mismatch || got.Warning == "" {
		t.Errorf("guest_of = %+v; want workspace_mismatch, mismatch and a warning, no session_mismatch", got)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") {
		t.Errorf("a workspace mismatch did not WARN:\n%s", out)
	}
}

// (c) the 10-02 shape, both differ: WARN, and both flags set.
func TestTheTenOhTwoShapeWarnsWithBothFlags(t *testing.T) {
	port := freePort(t)
	startHostBus(t, port, "test", "/Users/example")
	log, logs := testLogger()
	g, err := New(Config{Mode: "embedded", Port: port, SessionName: "rysh-macminim5",
		Workspace: "/Users/example/root/github/rysh-ai", Logger: log})
	if err != nil {
		t.Fatalf("guest bus: %v", err)
	}
	defer g.Close()
	got := waitGuestOf(t, g)
	if !got.SessionMismatch || !got.WorkspaceMismatch || !got.Mismatch || got.Warning == "" {
		t.Errorf("guest_of = %+v; want both flags, mismatch and a warning", got)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") {
		t.Errorf("the 10-02 shape did not WARN:\n%s", out)
	}
}
