package tui

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/board"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// recorderNATS starts a real in-process NATS server. Real, because the point of
// these tests is that the optimistic answer is reachable over the actual path.
func recorderNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := &server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true}
	srv, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats not ready")
	}
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// An empty board is a CLAIM: it asserts "nothing was posted". When the recorder
// is dead that claim is false, and false in the most confident-looking way — a
// clean, empty view.
//
// Liveness is now ASKED, not inferred. Every failure on this track came from
// reading a proxy: a pane's idle flag, send.ok, os.path.isfile, a persisted
// roster entry, a KV revision. A heartbeat was one more, and it also wrote into
// the board's bucket, whose single-writer detector depends on nothing else
// writing there (internal/actors/abla_test.go).

func TestEmptyBoardWithALiveRecorderSaysNothingWasPosted(t *testing.T) {
	m := buildBoardModel(board.New(0))
	m.boardRecorder = RecorderLive

	rows := strings.Join(m.boardRows(100), "\n")
	if !strings.Contains(rows, "Nothing posted yet") {
		t.Errorf("a live recorder with no posts must say so plainly:\n%s", rows)
	}
	if strings.Contains(rows, "NOT RECORDING") {
		t.Errorf("a live recorder must not warn:\n%s", rows)
	}
}

// THE RULING: a board that cannot know must say so, never render a confident
// empty. This is the roster defect with the sign flipped.
func TestEmptyBoardWithADeadRecorderRefusesToClaimSilence(t *testing.T) {
	m := buildBoardModel(board.New(0))
	m.boardRecorder = RecorderStale

	rows := strings.Join(m.boardRows(100), "\n")
	if strings.Contains(rows, "Nothing posted yet") {
		t.Errorf("a dead recorder must NOT claim nothing was posted — that is the lie:\n%s", rows)
	}
	if !strings.Contains(rows, "NOT RECORDING") {
		t.Errorf("a dead recorder must say so:\n%s", rows)
	}
}

// "Cannot tell" stays distinct from "not recording". Collapsing them would cry
// wolf on every session without a bus, and a warning that fires when nothing is
// wrong gets ignored when something is. UNKNOWN is also the ZERO VALUE, so a
// view that has not yet asked cannot accidentally claim health.
func TestEmptyBoardWithNoBusSaysItCannotTell(t *testing.T) {
	m := buildBoardModel(board.New(0)) // boardRecorder unset == RecorderUnknown

	rows := strings.Join(m.boardRows(100), "\n")
	if strings.Contains(rows, "Nothing posted yet") {
		t.Errorf("with no way to ask, the board must not claim silence:\n%s", rows)
	}
	if !strings.Contains(rows, "unknown") {
		t.Errorf("expected an honest 'unknown' notice:\n%s", rows)
	}
}

// The question must never be answerable optimistically by default: an unasked
// question is UNKNOWN, not LIVE.
func TestUnaskedIsUnknownNotLive(t *testing.T) {
	var zero RecorderState
	if zero != RecorderUnknown {
		t.Fatalf("the zero value must be RecorderUnknown, got %v — a view that has "+
			"not asked would otherwise claim the board is recording", zero)
	}
}

// With no connection the command must answer UNKNOWN rather than block or
// pretend. A liveness check that can hang the TUI is worse than the bug.
func TestAskRecorderWithNoConnIsUnknown(t *testing.T) {
	m := buildBoardModel(board.New(0))
	m.boardConn = nil
	got, ok := m.askRecorderCmd()().(boardRecorderMsg)
	if !ok || got.state != RecorderUnknown {
		t.Fatalf("no bus must answer UNKNOWN, got %#v", got)
	}
}

// TestBoardViewReachesLiveThroughTheRealLoop closes the gap I named and the CEO
// insisted on: every test above proves the CAUTIOUS answers render correctly,
// and none proved the view ever reaches the optimistic one.
//
// That is the F-20 shape — every link present, the optimistic path never
// exercised end to end — and it is worse than F-20 in one way: a view stuck at
// UNKNOWN looks CAREFUL rather than broken. It wears our own defences as
// camouflage.
//
// So this drives the real path: a real NATS server, a real ABLA answering, the
// real command the loop arms, and the real Update that consumes its reply.
// Nothing is assigned by hand.
func TestBoardViewReachesLiveThroughTheRealLoop(t *testing.T) {
	nc := recorderNATS(t)

	// A real responder on the real subject. NOT the ABLA actor itself:
	// internal/tui cannot import internal/actors (import cycle), so the two
	// halves are proven in their own packages —
	// actors.TestABLAAnswersLivenessWhileRecording proves ABLA answers and goes
	// quiet when stopped; this proves the view transitions when something
	// answers. Both use real NATS on msg.BoardAliveSubject(); neither fakes the
	// transport, which is the part that could lie.
	sub, err := nc.Subscribe(msgpkg.BoardAliveSubject(), func(m *nats.Msg) {
		_ = m.Respond([]byte(msgpkg.BoardAliveReply))
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	m := buildBoardModel(board.New(0))
	m.boardConn = nc
	if m.boardRecorder != RecorderUnknown {
		t.Fatalf("precondition: a view must start UNKNOWN, got %v", m.boardRecorder)
	}

	// The command the loop arms, executed for real: a request over NATS that
	// only a live recorder answers.
	reply := m.askRecorderCmd()()
	got, ok := reply.(boardRecorderMsg)
	if !ok {
		t.Fatalf("askRecorderCmd produced %T, want boardRecorderMsg", reply)
	}
	if got.state != RecorderLive {
		t.Fatalf("a live ABLA did not make the view live: got %v — the optimistic "+
			"answer is unreachable, which renders as caution rather than as a bug",
			got.state)
	}

	// And the real Update must apply it AND re-arm, or the view goes live once
	// and never notices the recorder dying.
	updated, cmd := m.Update(got)
	after, ok := updated.(Model)
	if !ok {
		t.Fatalf("Update returned %T", updated)
	}
	if after.boardRecorder != RecorderLive {
		t.Errorf("Update did not apply the answer: boardRecorder = %v", after.boardRecorder)
	}
	if cmd == nil {
		t.Error("Update did not re-arm the question: the view would go live once and " +
			"never notice the recorder dying")
	}

	// The optimistic answer is not only reachable, it is RENDERED.
	rows := strings.Join(after.boardRows(100), "\n")
	if !strings.Contains(rows, "Nothing posted yet") {
		t.Errorf("a live recorder must let the board say nothing was posted:\n%s", rows)
	}
}

// TestInitArmsTheLivenessQuestion guards the wiring itself. A question that is
// built but never asked leaves the view at UNKNOWN forever — the failure that
// disguises itself as the safe default, and the one kind every other rule here
// would have let through.
func TestInitArmsTheLivenessQuestion(t *testing.T) {
	src, err := os.ReadFile("model.go")
	if err != nil {
		t.Fatalf("read model.go: %v", err)
	}
	body := string(src)
	i := strings.Index(body, "func (m Model) Init() tea.Cmd {")
	if i < 0 {
		t.Fatal("Init not found — this guard needs updating")
	}
	end := strings.Index(body[i:], "\n}")
	if !strings.Contains(body[i:i+end], "askRecorderCmd()") {
		t.Error("Init does not arm askRecorderCmd: the liveness question is never asked, " +
			"so the board sits at UNKNOWN forever and looks cautious rather than broken")
	}
}
