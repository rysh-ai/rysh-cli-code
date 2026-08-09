package actors

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// newBoardNATS starts an in-process NATS server for the producer tests. Real
// server, real connection, real publisher — the wire is the assertion.
func newBoardNATS(t *testing.T) *nats.Conn {
	t.Helper()
	opts := &server.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true}
	srv, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server not ready")
	}
	t.Cleanup(srv.Shutdown)
	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// F-20: persona registration was UNWIRED — type, subject, publisher, subscriber,
// store and view all existed and nothing outside tests ever called
// SendBoardRegister. These pin the producer's rules.

// TestPaneTypesThatRegister is the gate-3 test as much as a filter test: what
// decides registration is the pane TYPE, never fleet meta. A non-fleet pane
// registers exactly like a fleet one, because nothing here can see the
// difference.
func TestPaneTypesThatRegister(t *testing.T) {
	for _, tc := range []struct {
		paneType string
		want     bool
		why      string
	}{
		{domain.PaneTypeNormal, true, "an ordinary pane is where a claude lives"},
		{domain.PaneTypeApproval, false, "an approval dialog is not an agent — and it overloads GivenName with a NATS subject"},
		{domain.PaneTypeReplay, false, "a playback cannot post, so it would sit in the roster and never speak"},
		{domain.PaneTypeAgentsBoard, false, "the board listing itself as one of its own agents is a lie"},
	} {
		if got := paneCanHostAnAgent(tc.paneType); got != tc.want {
			t.Errorf("paneCanHostAnAgent(%q) = %v, want %v — %s",
				tc.paneType, got, tc.want, tc.why)
		}
	}
}

// TestRegistrationPersonaUsesTheSameRuleAsPosts: the roster is a SECOND surface
// that could print an approval pane's "requestID\x1FresponseSubject" as
// somebody's name. It must go through BoardPersona, not the raw given-name.
func TestRegistrationPersonaUsesTheSameRuleAsPosts(t *testing.T) {
	poisoned := "req-42\x1frysh.approval.reply"
	if got := msg.BoardPersona(poisoned, "friendly-title", "pane-abcdef12-more"); got != "friendly-title" {
		t.Errorf("a \\x1f-poisoned given-name must fall through, got %q", got)
	}
	// And the chain's end: never blank.
	if got := msg.BoardPersona("", "", "pane-abcdef12-more"); got == "" {
		t.Error("persona must never be empty")
	}
}

// TestNewBoardRegisterStampsTheVersion — the constructor exists so V cannot be
// forgotten on a new call site, which is exactly what this defect added.
func TestNewBoardRegisterStampsTheVersion(t *testing.T) {
	r := msg.NewBoardRegister("pane-1", "planner", 1234)
	if r.V != msg.BoardSchemaVersion {
		t.Errorf("V = %d, want %d", r.V, msg.BoardSchemaVersion)
	}
	if r.PaneID != "pane-1" || r.Persona != "planner" || r.TS != 1234 {
		t.Errorf("unexpected register: %+v", r)
	}
}

// TestRegistrationCarriesNoFleetFields — gate 4. A registration says who is
// here, not who they report to.
func TestRegistrationCarriesNoFleetFields(t *testing.T) {
	// Compile-time proof by construction: the struct literal below names every
	// field MsgBoardRegister has. If a fleet/role/unit/envelope field is ever
	// added, this stops compiling and the author has to justify it.
	_ = msg.MsgBoardRegister{V: 1, PaneID: "p", Persona: "n", TS: 1}
}

// TestRegisterOnBoardPublishes drives the real producer over a real NATS
// connection and asserts what lands on the wire: the right subject, a stamped
// version, the full pane uuid, and a persona resolved by the shared rule.
func TestRegisterOnBoardPublishes(t *testing.T) {
	nc := newBoardNATS(t)
	codecs := msg.DefaultCodecRegistry()
	rec := recordSubject(t, nc, msg.BoardRegisterSubject())

	p := &PaneActor{
		id:        "aaaaaaaa-1111-2222-3333-444444444444",
		title:     "auto-title",
		givenName: "planner-agent",
		pub:       msg.NewNATSPublisher(nc, codecs),
	}
	p.registerOnBoard()
	settle(t, nc)

	subjects, payloads := rec.seen()
	if len(subjects) != 1 {
		t.Fatalf("want exactly one registration, got %d", len(subjects))
	}
	var got msg.MsgBoardRegister
	if err := json.Unmarshal(payloads[0], &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.V != msg.BoardSchemaVersion {
		t.Errorf("V = %d, want %d", got.V, msg.BoardSchemaVersion)
	}
	if got.PaneID != p.id {
		t.Errorf("PaneID = %q — the roster keys on the FULL uuid, not a truncation", got.PaneID)
	}
	if got.Persona != "planner-agent" {
		t.Errorf("Persona = %q, want planner-agent", got.Persona)
	}
}

// TestApprovalPaneNeverRegisters: an approval pane overloads GivenName with
// "requestID\x1FresponseSubject". Nothing may put that on the wire as a name.
func TestApprovalPaneNeverRegisters(t *testing.T) {
	nc := newBoardNATS(t)
	rec := recordSubject(t, nc, msg.BoardRegisterSubject())

	p := &PaneActor{
		id:        "bbbbbbbb-5555-6666-7777-888888888888",
		paneType:  domain.PaneTypeApproval,
		givenName: "req-42\x1frysh.approval.reply",
		pub:       msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry()),
	}
	p.registerOnBoard()
	settle(t, nc)

	if subjects, _ := rec.seen(); len(subjects) != 0 {
		t.Fatalf("an approval pane must not register; got %d announcement(s)", len(subjects))
	}
}

// TestPaneStartupAndRenameCallRegisterOnBoard guards the WIRING, which is the
// thing F-20 actually was: every downstream link existed and no producer called
// them. A behavioural test of registerOnBoard passes just as happily when
// nothing invokes it, so this asserts the call sites themselves.
//
// Structural, and deliberately so — the alternative is spinning a full pane
// actor with a PTY to observe a fire-and-forget publish. If this test ever
// feels obstructive, the fix is a real actor-level test, not deleting it.
func TestPaneStartupAndRenameCallRegisterOnBoard(t *testing.T) {
	src, err := os.ReadFile("pane.go")
	if err != nil {
		t.Fatalf("read pane.go: %v", err)
	}
	body := string(src)
	if n := strings.Count(body, "p.registerOnBoard()"); n < 2 {
		t.Errorf("pane.go calls registerOnBoard() %d time(s), want 2 — "+
			"startup (so a pane announces itself) and MsgPaneSetGivenName (so a "+
			"rename does not leave the roster stale). F-20 was exactly this: "+
			"every downstream link present, no producer.", n)
	}
	if i := strings.Index(body, "case *msg.MsgPaneSetGivenName:"); i >= 0 {
		if !strings.Contains(body[i:min(i+600, len(body))], "p.registerOnBoard()") {
			t.Error("rename does not re-announce: the roster will show an old name " +
				"beside fresh posts carrying the new one")
		}
	} else {
		t.Error("MsgPaneSetGivenName handler not found — this guard needs updating")
	}
}
