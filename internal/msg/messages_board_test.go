package msg

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	testPaneA = "aaaa1111-f8bb-45fb-9d02-2657b16706ae"
	testPaneB = "bbbb2222-ff1c-4435-84bc-ab2eed4aa435"
)

// TestBoardPersonaFallbackChain pins the order the board renders a speaker in:
// given-name, then the auto-title, then "pane-<8>". Never empty, and never the
// literal "no-name" that ##pane info prints.
func TestBoardPersonaFallbackChain(t *testing.T) {
	cases := []struct {
		name                     string
		given, title, pane, want string
	}{
		{"given-name wins", "mgr-01", "dynamic-jackal", testPaneA, "mgr-01"},
		{"falls back to title", "", "dynamic-jackal", testPaneA, "dynamic-jackal"},
		{"falls back to pane-8", "", "", testPaneA, "pane-aaaa1111"},
		{"short pane id", "", "", "abc", "pane-abc"},
		{"nothing at all", "", "", "", "pane-unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BoardPersona(c.given, c.title, c.pane); got != c.want {
				t.Fatalf("BoardPersona(%q,%q,%q) = %q, want %q",
					c.given, c.title, c.pane, got, c.want)
			}
		})
	}
}

// TestBoardPersonaRejectsApprovalPaneOverload is the trap wave 1 found.
// Approval panes OVERLOAD GivenName to carry "requestID\x1FresponseSubject"
// (internal/actors/approval_pane.go:76-78). A board that renders a given-name
// blindly would one day print a NATS subject as somebody's name.
func TestBoardPersonaRejectsApprovalPaneOverload(t *testing.T) {
	overloaded := "req-42\x1frysh.pane.abc.approval.reply"

	got := BoardPersona(overloaded, "", testPaneA)
	if got != "pane-aaaa1111" {
		t.Fatalf("approval-pane given-name leaked into the persona: %q", got)
	}
	if strings.ContainsRune(got, 0x1f) {
		t.Fatalf("persona still carries the unit separator: %q", got)
	}

	// With a title available it falls through to the title, not to the uuid.
	if got := BoardPersona(overloaded, "approval", testPaneA); got != "approval" {
		t.Fatalf("want the title as the next candidate, got %q", got)
	}
}

// TestMintThreadIDIsOwnedByItsPane: the id carries its owner, which is what
// lets the store tell a root from a reply without an is-root flag, and what
// stops two agents that share a given-name from minting colliding ids.
func TestMintThreadIDIsOwnedByItsPane(t *testing.T) {
	a1 := MintThreadID(testPaneA, 1)
	a2 := MintThreadID(testPaneA, 2)
	b1 := MintThreadID(testPaneB, 1)

	if a1 != testPaneA+"/1" {
		t.Fatalf("unexpected shape: %q", a1)
	}
	if a1 == a2 || a1 == b1 {
		t.Fatalf("thread ids collide: %q %q %q", a1, a2, b1)
	}
	if !strings.HasPrefix(a1, testPaneA+"/") {
		t.Fatalf("%q does not carry its owner", a1)
	}
	if strings.HasPrefix(b1, testPaneA+"/") {
		t.Fatalf("%q claims to be owned by pane A", b1)
	}
}

// TestNewBoardPostStampsVersionAndDefaultKind: V must never be forgotten on a
// new call site, and an unspecified kind is a milestone rather than empty.
func TestNewBoardPostStampsVersionAndDefaultKind(t *testing.T) {
	p := NewBoardPost(testPaneA, "mgr-01", "", "shipped", 1234)
	if p.V != BoardSchemaVersion {
		t.Fatalf("V = %d, want %d", p.V, BoardSchemaVersion)
	}
	if p.Kind != BoardKindMilestone {
		t.Fatalf("Kind = %q, want %q", p.Kind, BoardKindMilestone)
	}
	if p.TS != 1234 || p.PaneID != testPaneA || p.Persona != "mgr-01" {
		t.Fatalf("fields not carried: %+v", p)
	}
	if p.ThreadID != "" {
		t.Fatalf("a fresh post must not invent a thread id: %q", p.ThreadID)
	}

	explicit := NewBoardPost(testPaneA, "mgr-01", "some-future-kind", "x", 1)
	if explicit.Kind != "some-future-kind" {
		t.Fatalf("free-form kind was overwritten: %q", explicit.Kind)
	}
}

// TestBoardPostCarriesNoFleetFields is founder gate 4, enforced rather than
// remembered: a board post is CHAT — who spoke, under which thread. The fleet's
// FROM/TO routing envelope is a fleet concern and must not re-enter the schema.
//
// This is what makes gate 3 ("every claude may post, not only fleet members")
// structural: with no fleet fields at all, there is no field that can be missing
// for a non-fleet poster, so nothing downstream can treat one as second-class.
func TestBoardPostCarriesNoFleetFields(t *testing.T) {
	p := NewBoardPost(testPaneA, "mgr-01", BoardKindMilestone, "hello", 1)
	p.ThreadID = MintThreadID(testPaneA, 1)

	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	banned := []string{"envelope", "fleet", "role", "unit", "to_persona", "to_pane_id"}
	for _, f := range banned {
		if _, present := generic[f]; present {
			t.Errorf("MsgBoardPost carries %q — gate 4 removed the fleet envelope "+
				"from the board schema; wire JSON: %s", f, data)
		}
	}

	want := map[string]bool{"v": true, "pane_id": true, "persona": true,
		"kind": true, "text": true, "thread_id": true, "ts": true}
	for k := range generic {
		if !want[k] {
			t.Errorf("unexpected field %q on the wire: %s", k, data)
		}
	}
}

func TestBoardRegisterCarriesNoFleetFields(t *testing.T) {
	data, err := json.Marshal(&MsgBoardRegister{
		V: BoardSchemaVersion, PaneID: testPaneA, Persona: "mgr-01", TS: 1,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, f := range []string{"fleet", "role", "unit"} {
		if _, present := generic[f]; present {
			t.Errorf("MsgBoardRegister carries %q — gate 4 removed it: %s", f, data)
		}
	}
}

// TestBoardSubjectsAreBuiltFromT: the subject prefix is the SESSION NAME, not
// the constant "rysh" (rysh-shared/msg/topics.go). A literal would break the
// board the moment a session is named anything else — which is every real
// session.
func TestBoardSubjectsAreBuiltFromT(t *testing.T) {
	original := SessionPrefix()
	t.Cleanup(func() { SetSessionPrefix(original) })

	SetSessionPrefix("macmini-rysh-elect")
	if got, want := BoardPostSubject(), "macmini-rysh-elect.board.post"; got != want {
		t.Fatalf("BoardPostSubject() = %q, want %q", got, want)
	}
	if got, want := BoardRegisterSubject(), "macmini-rysh-elect.board.register"; got != want {
		t.Fatalf("BoardRegisterSubject() = %q, want %q", got, want)
	}

	SetSessionPrefix("other")
	if got := BoardPostSubject(); got != "other.board.post" {
		t.Fatalf("subject did not follow the session prefix: %q", got)
	}
}

// TestBoardCodecRoundTrip: the tags are registered, so a post survives the
// envelope the bus wraps it in.
func TestBoardCodecRoundTrip(t *testing.T) {
	r := DefaultCodecRegistry()

	p := NewBoardPost(testPaneA, "mgr-01", BoardKindBlocked, "blocked on the gate", 7)
	p.ThreadID = MintThreadID(testPaneA, 3)
	payload, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded, err := r.Decode(TagBoardPost, payload)
	if err != nil {
		t.Fatalf("Decode(%s): %v", TagBoardPost, err)
	}
	got, ok := decoded.(*MsgBoardPost)
	if !ok {
		t.Fatalf("decoded to %T, want *MsgBoardPost", decoded)
	}
	if got.Text != p.Text || got.ThreadID != p.ThreadID || got.V != p.V {
		t.Fatalf("round trip lost fields: %+v", got)
	}

	regPayload, _ := json.Marshal(&MsgBoardRegister{
		V: BoardSchemaVersion, PaneID: testPaneB, Persona: "wkr-01", TS: 8,
	})
	regDecoded, err := r.Decode(TagBoardRegister, regPayload)
	if err != nil {
		t.Fatalf("Decode(%s): %v", TagBoardRegister, err)
	}
	if _, ok := regDecoded.(*MsgBoardRegister); !ok {
		t.Fatalf("decoded to %T, want *MsgBoardRegister", regDecoded)
	}
}

// TestDirectedPostCarriesARecipientButNotAChain pins the reopened gate 4: a
// directed message says who was spoken TO, and still says nothing about the
// fleet chain. The recipient is chat (an @mention), not routing — ANSA
// delivers, this field only renders.
func TestDirectedPostCarriesARecipientButNotAChain(t *testing.T) {
	p := NewBoardPost("pane-a", "planner-agent", BoardKindMilestone, "over to you", 1)
	p.ToPersona = "builder-agent"
	p.ToPaneID = "pane-b"

	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{`"to_persona":"builder-agent"`, `"to_pane_id":"pane-b"`} {
		if !strings.Contains(got, want) {
			t.Errorf("directed post is missing %s: %s", want, got)
		}
	}
	// The chain must still be absent — gate 4 reopened for a recipient, not
	// for the FROM/TO envelope.
	for _, forbidden := range []string{"envelope", "fleet", "\"role\"", "\"unit\"", "msg-"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("a board post must not carry %q: %s", forbidden, got)
		}
	}
}

// A broadcast stays the default: omitempty means an undirected post is byte-for
// -byte what it was before gate 4 reopened, so nothing downstream sees a new
// empty field it has to interpret.
func TestBroadcastPostOmitsTheRecipient(t *testing.T) {
	p := NewBoardPost("pane-a", "planner-agent", BoardKindMilestone, "hello", 1)
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), "to_persona") || strings.Contains(string(b), "to_pane_id") {
		t.Errorf("an undirected post must omit the recipient entirely: %s", b)
	}
}
