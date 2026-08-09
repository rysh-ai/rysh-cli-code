package actors

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ANSA's tests are mostly tests of REFUSALS, which is unusual and is the point.
//
// The CEO's rule — *a board post is worth less than the control channel; a work
// order is worth more than either* — makes a silently dropped message the worst
// outcome this code can produce. Every branch below is a way a message does not
// get delivered, and every one of them asserts two things together: the caller
// was told, and nothing was sent. Either half alone is a bug.
//
// The fake transport exists because none of these can be provoked against a
// live daemon on demand: a pane that dies mid-route, a NATS publish that
// refuses, a session that cannot be enumerated. A seam is the difference
// between testing this property and asserting it in a comment.

// ---------------------------------------------------------------------------
// fake transport
// ---------------------------------------------------------------------------

type fakeAnsaTransport struct {
	mu sync.Mutex

	panes    []ansaPane
	panesErr error

	probeErr   error
	deliverErr error

	// delivered records every message that actually reached a pane inbox. Its
	// LENGTH is the assertion that matters: a refusal that still delivered is
	// worse than one that did not refuse.
	delivered []ansaDelivery
	probed    []string
}

type ansaDelivery struct {
	PaneID string
	Mode   string
	Text   string
}

func (f *fakeAnsaTransport) Panes() ([]ansaPane, error) {
	if f.panesErr != nil {
		return nil, f.panesErr
	}
	return f.panes, nil
}

func (f *fakeAnsaTransport) Probe(paneID string) error {
	f.mu.Lock()
	f.probed = append(f.probed, paneID)
	f.mu.Unlock()
	return f.probeErr
}

func (f *fakeAnsaTransport) Deliver(paneID, mode, text string) error {
	if f.deliverErr != nil {
		return f.deliverErr
	}
	f.mu.Lock()
	f.delivered = append(f.delivered, ansaDelivery{PaneID: paneID, Mode: mode, Text: text})
	f.mu.Unlock()
	return nil
}

func (f *fakeAnsaTransport) sent() []ansaDelivery {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ansaDelivery(nil), f.delivered...)
}

// twoPanesSharingAName is the session shape the whole safety argument rests on.
// It is legal today: IsGivenNameTakenInLane is LANE-scoped, and a fleet's
// managers and workers live in different lanes.
func twoPanesSharingAName() []ansaPane {
	return []ansaPane{
		{ID: "pane-uuid-aaaa", GivenName: "wkr-01", Title: "brave-otter"},
		{ID: "pane-uuid-bbbb", GivenName: "wkr-01", Title: "calm-heron"},
		{ID: "pane-uuid-cccc", GivenName: "mgr-01", Title: "eager-lynx"},
	}
}

// ---------------------------------------------------------------------------
// THE test — the one that carries the safety argument
// ---------------------------------------------------------------------------

// TestAnsaRefusesAnAmbiguousName is the reason ANSA can address by name at all.
//
// Two panes legally share the given-name "wkr-01". Resolving it must produce an
// ERROR, not a delivery. A first-match "helpful guess" would hand a work order
// to the wrong agent and report success — the single worst thing this code can
// do, and undetectable from the sender's side.
//
// Reverting the fix — collapsing ansaMatchByName to the first match — makes
// this report a resolved pane where it expects a refusal.
//
// This is also why given-name uniqueness is NOT a prerequisite for ANSA:
// ambiguity is handled here rather than assumed away, so session-unique names
// become an improvement to the design and not a safety gate on it.
func TestAnsaRefusesAnAmbiguousName(t *testing.T) {
	panes := twoPanesSharingAName()

	got, refusal := ansaResolveTarget(panes, "wkr-01")

	if refusal == nil {
		t.Fatalf("an ambiguous name resolved to pane %q instead of being refused.\n"+
			"Two panes legally share a given-name (IsGivenNameTakenInLane is LANE-scoped), so "+
			"picking one delivers a work order to the WRONG agent and reports success. "+
			"Refusing is always correct; guessing is never.", got.ID)
	}
	if refusal.OK {
		t.Fatal("refusal is marked OK")
	}
	if refusal.Code != msg.AnsaErrAmbiguousTarget {
		t.Errorf("code = %q, want %q — the caller must be able to branch on this "+
			"without parsing prose", refusal.Code, msg.AnsaErrAmbiguousTarget)
	}

	// A refusal that does not say how to succeed is a wall. The candidate ids
	// are what let the sender re-address without going to look.
	if len(refusal.Candidates) != 2 {
		t.Fatalf("candidates = %v, want both matching pane ids", refusal.Candidates)
	}
	if refusal.Candidates[0] != "pane-uuid-aaaa" || refusal.Candidates[1] != "pane-uuid-bbbb" {
		t.Errorf("candidates = %v, want [pane-uuid-aaaa pane-uuid-bbbb] sorted", refusal.Candidates)
	}
	for _, id := range refusal.Candidates {
		if !strings.Contains(refusal.Error, id) {
			t.Errorf("error text does not name candidate %s: %q", id, refusal.Error)
		}
	}
}

// TestAnsaAmbiguousNameDeliversNothing is the other half, and it is a separate
// test on purpose: refusing and not-delivering are two properties, and code
// that reports an error after already publishing would pass the test above.
func TestAnsaAmbiguousNameDeliversNothing(t *testing.T) {
	tr := &fakeAnsaTransport{panes: twoPanesSharingAName()}
	r := &ansaRouter{tr: tr}

	target, refusal := ansaResolveTarget(tr.panes, "@wkr-01")
	if refusal == nil {
		// Guard: if the edge ever resolves it, make sure the router is not then
		// handed the guess — route what the edge produced and show the damage.
		res := r.Route(msg.NewAnsaRoute("sender", target.ID, "", "deploy to prod"))
		t.Fatalf("ambiguous name resolved to %q and routing it returned OK=%v; "+
			"%d message(s) were delivered to a guessed target",
			target.ID, res.OK, len(tr.sent()))
	}
	if sent := tr.sent(); len(sent) != 0 {
		t.Errorf("a refused route still delivered %d message(s): %+v", len(sent), sent)
	}
}

// TestAnsaAtSigilIsStrippedAtTheEdge — @name is how people write it, and the
// sigil must not survive into an address.
func TestAnsaAtSigilIsStrippedAtTheEdge(t *testing.T) {
	panes := twoPanesSharingAName()
	got, refusal := ansaResolveTarget(panes, "@mgr-01")
	if refusal != nil {
		t.Fatalf("@mgr-01 was refused: %s", refusal.Error)
	}
	if got.ID != "pane-uuid-cccc" {
		t.Errorf("resolved to %q, want pane-uuid-cccc", got.ID)
	}
}

// TestAnsaUniqueNameResolves — the common case, which is all 61 panes today.
func TestAnsaUniqueNameResolves(t *testing.T) {
	panes := twoPanesSharingAName()
	got, refusal := ansaResolveTarget(panes, "mgr-01")
	if refusal != nil {
		t.Fatalf("a unique name was refused: %s", refusal.Error)
	}
	if got.ID != "pane-uuid-cccc" {
		t.Errorf("resolved to %q, want pane-uuid-cccc", got.ID)
	}
}

// TestAnsaIDBeatsAName. An id must resolve to itself even when some other pane
// carries that string as a name — otherwise "re-address by id", which is the
// fix an ambiguity refusal recommends, would not reliably work.
func TestAnsaIDBeatsAName(t *testing.T) {
	panes := []ansaPane{
		{ID: "pane-uuid-aaaa", GivenName: "decoy"},
		{ID: "decoy", GivenName: "something-else"},
	}
	got, refusal := ansaResolveTarget(panes, "decoy")
	if refusal != nil {
		t.Fatalf("refused: %s", refusal.Error)
	}
	if got.ID != "decoy" {
		t.Errorf("resolved to %q; an exact pane id must win over a name match", got.ID)
	}
}

// TestAnsaGivenNameBeatsAnAutoTitle — a generated title must never shadow a
// name a human assigned.
func TestAnsaGivenNameBeatsAnAutoTitle(t *testing.T) {
	panes := []ansaPane{
		{ID: "p1", Title: "wkr-01"}, // auto-title collision
		{ID: "p2", GivenName: "wkr-01", Title: "brave-otter"},
	}
	got, refusal := ansaResolveTarget(panes, "wkr-01")
	if refusal != nil {
		t.Fatalf("refused: %s", refusal.Error)
	}
	if got.ID != "p2" {
		t.Errorf("resolved to %q, want p2 — the explicit given-name outranks a generated title", got.ID)
	}
}

// ---------------------------------------------------------------------------
// the invariant: the router holds IDs, never names
// ---------------------------------------------------------------------------

// TestAnsaRouterRefusesANameAsAnAddress pins the invariant the CEO asked for
// explicitly: if ANSA internals ever hold a NAME where an ID belongs, that is
// the bug. A caller that skipped edge resolution is told precisely that, and
// the router does NOT quietly resolve on their behalf — doing so would re-open
// the ambiguity hole the edge closes.
func TestAnsaRouterRefusesANameAsAnAddress(t *testing.T) {
	tr := &fakeAnsaTransport{panes: twoPanesSharingAName()}
	r := &ansaRouter{tr: tr}

	res := r.Route(msg.NewAnsaRoute("sender", "mgr-01", "", "hello")) // a NAME, unique even

	if res.OK {
		t.Fatalf("the router resolved a name into an address; a name is a label for humans and " +
			"an id is an address — resolving late re-opens the ambiguity the edge closed")
	}
	if res.Code != msg.AnsaErrNotAnID {
		t.Errorf("code = %q, want %q", res.Code, msg.AnsaErrNotAnID)
	}
	if sent := tr.sent(); len(sent) != 0 {
		t.Errorf("delivered %d message(s) despite refusing: %+v", len(sent), sent)
	}
}

// ---------------------------------------------------------------------------
// the happy path
// ---------------------------------------------------------------------------

func TestAnsaRoutesByIDAndProbesFirst(t *testing.T) {
	tr := &fakeAnsaTransport{panes: twoPanesSharingAName()}
	r := &ansaRouter{tr: tr}

	res := r.Route(msg.NewAnsaRoute("sender", "pane-uuid-aaaa", "", "run the tests"))
	if !res.OK {
		t.Fatalf("route refused: %s (%s)", res.Code, res.Error)
	}
	if res.TargetPaneID != "pane-uuid-aaaa" {
		t.Errorf("target = %q", res.TargetPaneID)
	}
	if res.TargetPersona != "wkr-01" {
		t.Errorf("persona = %q, want wkr-01", res.TargetPersona)
	}

	sent := tr.sent()
	if len(sent) != 1 {
		t.Fatalf("delivered %d messages, want 1", len(sent))
	}
	if sent[0].Mode != msg.AnsaModeShell {
		t.Errorf("mode = %q, want the inherited shell default", sent[0].Mode)
	}
	if sent[0].Text != "run the tests" {
		t.Errorf("text = %q", sent[0].Text)
	}
	if len(tr.probed) != 1 || tr.probed[0] != "pane-uuid-aaaa" {
		t.Errorf("probed = %v; the target must be probed before publishing, because the inbox "+
			"is fire-and-forget and a dead pane is otherwise indistinguishable from a live one",
			tr.probed)
	}
}

func TestAnsaRoutesAPrompt(t *testing.T) {
	tr := &fakeAnsaTransport{panes: twoPanesSharingAName()}
	r := &ansaRouter{tr: tr}

	res := r.Route(msg.NewAnsaRoute("s", "pane-uuid-cccc", msg.AnsaModePrompt, "summarise the diff"))
	if !res.OK {
		t.Fatalf("refused: %s", res.Error)
	}
	sent := tr.sent()
	if len(sent) != 1 || sent[0].Mode != msg.AnsaModePrompt {
		t.Fatalf("delivered %+v, want one prompt-mode delivery", sent)
	}
}

// ---------------------------------------------------------------------------
// every remaining refusal: told, and nothing sent
// ---------------------------------------------------------------------------

func TestAnsaRefusalsNeverDeliver(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(*fakeAnsaTransport)
		req     *msg.MsgAnsaRoute
		wantErr string
	}{
		{
			name:    "no target",
			req:     msg.NewAnsaRoute("s", "", "", "hello"),
			wantErr: msg.AnsaErrNoTarget,
		},
		{
			name:    "no text",
			req:     msg.NewAnsaRoute("s", "pane-uuid-aaaa", "", "   "),
			wantErr: msg.AnsaErrNoText,
		},
		{
			name:    "unknown id",
			req:     msg.NewAnsaRoute("s", "pane-uuid-nope", "", "hello"),
			wantErr: msg.AnsaErrUnknownTarget,
		},
		{
			// A typo'd mode must NOT quietly become a shell command: that runs
			// somebody's sentence in bash rather than sending it as a prompt.
			name:    "bad mode",
			req:     msg.NewAnsaRoute("s", "pane-uuid-aaaa", "prmopt", "hello"),
			wantErr: msg.AnsaErrBadMode,
		},
		{
			name:    "directory unavailable",
			setup:   func(f *fakeAnsaTransport) { f.panesErr = errors.New("snapshot timeout") },
			req:     msg.NewAnsaRoute("s", "pane-uuid-aaaa", "", "hello"),
			wantErr: msg.AnsaErrDirectory,
		},
		{
			name:    "target does not answer",
			setup:   func(f *fakeAnsaTransport) { f.probeErr = errors.New("no responders") },
			req:     msg.NewAnsaRoute("s", "pane-uuid-aaaa", "", "hello"),
			wantErr: msg.AnsaErrUnreachable,
		},
		{
			name:    "publish refused",
			setup:   func(f *fakeAnsaTransport) { f.deliverErr = errors.New("nats: connection closed") },
			req:     msg.NewAnsaRoute("s", "pane-uuid-aaaa", "", "hello"),
			wantErr: msg.AnsaErrPublishFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &fakeAnsaTransport{panes: twoPanesSharingAName()}
			if tc.setup != nil {
				tc.setup(tr)
			}
			r := &ansaRouter{tr: tr}

			res := r.Route(tc.req)

			if res == nil {
				t.Fatal("Route returned nil — a route always answers")
			}
			if res.OK {
				t.Fatalf("expected refusal %q, got OK", tc.wantErr)
			}
			if res.Code != tc.wantErr {
				t.Errorf("code = %q, want %q", res.Code, tc.wantErr)
			}
			if res.Error == "" {
				t.Error("refusal has no human-readable text")
			}
			if sent := tr.sent(); len(sent) != 0 {
				t.Errorf("a refused route delivered %d message(s): %+v — this is the silent "+
					"drop's evil twin: the caller was told AND the message went anyway",
					len(sent), sent)
			}
		})
	}
}

// TestAnsaNilRequestIsAnswered — a nil never reaches a caller as a nil.
func TestAnsaNilRequestIsAnswered(t *testing.T) {
	r := &ansaRouter{tr: &fakeAnsaTransport{}}
	res := r.Route(nil)
	if res == nil || res.OK || res.Code == "" {
		t.Fatalf("nil request produced %+v; it must be a coded refusal", res)
	}
}

// TestAnsaPersonaGuardsTheApprovalPaneOverload — ANSA reuses the board's
// persona resolver, so an approval pane's overloaded GivenName
// ("requestID\x1FresponseSubject") never surfaces as somebody's name.
func TestAnsaPersonaGuardsTheApprovalPaneOverload(t *testing.T) {
	p := ansaPane{ID: "abcdef0123456789", GivenName: "req-42\x1frysh.approval.reply.42"}
	if got := ansaPersona(p); strings.ContainsRune(got, 0x1f) {
		t.Errorf("persona %q carries the unit separator", got)
	} else if got != "pane-abcdef01" {
		t.Errorf("persona = %q, want the pane-<8> fallback", got)
	}
}
