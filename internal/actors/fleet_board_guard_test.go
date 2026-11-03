// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/boardstamp"
	"github.com/rysh-ai/rysh-cli-code/internal/fleet"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Who may post to the reserved fleet board (design 030 §9.3, founder ruling
// D-24, wave W3): the doors refuse non-heads (fail closed), stamp what they
// accept; the recorder tags every post that has no valid, fresh stamp from a
// head `unverified` — and never drops it.

// guardFleets: pB is a worker in epic-a, pA its ceo (head).
func guardFleets() []fleet.Fleet {
	return []fleet.Fleet{{Name: "epic-a", BoardID: "epic-a", State: fleet.StateUp, Members: []fleet.Member{
		{PaneID: "pA", Role: "ceo"},
		{PaneID: "pB", Role: "worker", Parent: "pA"},
	}}}
}

func newGuardWorkspace(t *testing.T, fleets []fleet.Fleet) (*WorkspaceActor, *subjectRecorder, *fakeFleetRegistry) {
	t.Helper()
	w, nc := newBoardTestWorkspace(t)
	w.nc = nc
	var reg *fakeFleetRegistry
	if fleets != nil {
		reg = startFakeFleetRegistry(t, nc, fleets)
	}
	return w, recordSubject(t, nc, msg.BoardPostSubject(msg.FleetBoardID)), reg
}

func decodePosts(t *testing.T, rec *subjectRecorder) []msg.MsgBoardPost {
	t.Helper()
	_, payloads := rec.seen()
	var out []msg.MsgBoardPost
	for _, p := range payloads {
		var got msg.MsgBoardPost
		if err := json.Unmarshal(p, &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		out = append(out, got)
	}
	return out
}

// The W3 headline (design §9.5): a worker pane's `--board fleet` post is
// accepted today. It must be refused, and NOTHING may reach the bus.
func TestW3AWorkerCannotPostToTheFleetBoard(t *testing.T) {
	w, rec, _ := newGuardWorkspace(t, guardFleets())
	for _, board := range []string{"fleet", "FLEET", " Fleet "} {
		resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{AsPaneID: "pB", Board: board, Text: "I am the ceo"})
		if resp.OK {
			t.Fatalf("board %q: a worker's fleet-board post was accepted", board)
		}
		for _, want := range []string{"fleet head", "roadmap-manager", "ceo", "portfolio-lead", "nothing was posted"} {
			if !strings.Contains(resp.Error, want) {
				t.Errorf("board %q: refusal %q does not name %q", board, resp.Error, want)
			}
		}
	}
	var out strings.Builder
	if err := w.handleBoardCommand(&out, "pB", []string{"--board", "fleet", "post", "hi"}); err == nil {
		t.Fatalf("##board post from a worker to the fleet board was accepted: %s", out.String())
	}
	settle(t, w.nc)
	if n := len(decodePosts(t, rec)); n != 0 {
		t.Fatalf("%d refused posts reached the fleet board", n)
	}
}

func TestW3AHeadPostsToTheFleetBoardWithAStamp(t *testing.T) {
	for _, role := range fleet.TopRoles {
		fleets := []fleet.Fleet{{Name: "f", BoardID: "f", Members: []fleet.Member{{PaneID: "pB", Role: role}}}}
		w, rec, _ := newGuardWorkspace(t, fleets)
		resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{AsPaneID: "pB", Board: "fleet", Text: "[f] MILESTONE: M1"})
		if !resp.OK {
			t.Fatalf("role %s: a head's fleet-board post was refused: %s", role, resp.Error)
		}
		settle(t, w.nc)
		posts := decodePosts(t, rec)
		if len(posts) != 1 || !boardstamp.Verify("fleet", &posts[0]) {
			t.Fatalf("role %s: %d posts, want 1 with a valid stamp: %+v", role, len(posts), posts)
		}
	}
}

func TestW3TheDoorsFailClosedWithoutTheRegistry(t *testing.T) {
	w, rec, _ := newGuardWorkspace(t, nil)
	resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{AsPaneID: "pA", Board: "fleet", Text: "x"})
	if resp.OK || !strings.Contains(resp.Error, "registry did not answer") {
		t.Fatalf("no registry: %+v; want a refusal saying the registry did not answer", resp)
	}
	settle(t, w.nc)
	if n := len(decodePosts(t, rec)); n != 0 {
		t.Fatalf("%d posts reached the fleet board without a registry", n)
	}
}

// Every other board is unchanged: no registry needed, no stamp.
func TestW3OtherBoardsAreUnchanged(t *testing.T) {
	w, _, _ := newGuardWorkspace(t, nil)
	rec := recordSubject(t, w.nc, msg.BoardPostSubject("epic-a"))
	if resp := w.handleCLIBoardPost(&msg.MsgCLIBoardPost{AsPaneID: "pB", Board: "epic-a", Text: "x"}); !resp.OK {
		t.Fatalf("a post to a fleet's own board was refused: %s", resp.Error)
	}
	settle(t, w.nc)
	posts := decodePosts(t, rec)
	if len(posts) != 1 || posts[0].Stamp != "" {
		t.Fatalf("posts %+v: want one, unstamped", posts)
	}
}

// One warn line per refusal: board, claimed pane, rule; never the body.
func TestW3ARefusalLogsOneLineWithoutTheBody(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	w, _, _ := newGuardWorkspace(t, guardFleets())
	w.handleCLIBoardPost(&msg.MsgCLIBoardPost{AsPaneID: "pB", Board: "fleet", Text: "SECRET-BODY-TEXT"})
	logs := buf.String()
	if n := strings.Count(logs, "fleet board post refused"); n != 1 {
		t.Fatalf("%d refusal lines, want 1:\n%s", n, logs)
	}
	if strings.Contains(logs, "SECRET-BODY-TEXT") {
		t.Fatalf("the refusal log carries the post body:\n%s", logs)
	}
	if !strings.Contains(logs, "pB") || !strings.Contains(logs, "board=fleet") {
		t.Fatalf("the refusal log does not name the pane and board:\n%s", logs)
	}
}

// --- the recorder's tag -------------------------------------------------------

func newGuardTagger(t *testing.T, fleets []fleet.Fleet) *fleetBoardTagger {
	t.Helper()
	nc := newABLATestNATS(t)
	startFakeFleetRegistry(t, nc, fleets)
	return newFleetBoardTagger(nc)
}

func stampedPost(pane, text string) *msg.MsgBoardPost {
	p := &msg.MsgBoardPost{PaneID: pane, Kind: "milestone", Text: text, TS: time.Now().UnixMilli()}
	p.Stamp = boardstamp.Stamp("fleet", p)
	return p
}

func TestW3TheRecorderTagsWhatNoDoorStamped(t *testing.T) {
	tg := newGuardTagger(t, guardFleets())

	door := stampedPost("pA", "door post")
	tg.tag("fleet", door)
	if door.Unverified || door.Stamp != "" {
		t.Fatalf("a head's door post: unverified=%v stamp=%q; want verified, stamp cleared", door.Unverified, door.Stamp)
	}

	raw := &msg.MsgBoardPost{PaneID: "pA", Kind: "milestone", Text: "raw, claiming the head", TS: time.Now().UnixMilli()}
	tg.tag("fleet", raw)
	if !raw.Unverified {
		t.Fatal("a raw post claiming a head's pane id was not tagged")
	}

	forged := &msg.MsgBoardPost{PaneID: "pA", Text: "forged", TS: time.Now().UnixMilli(), Stamp: strings.Repeat("ab", 32)}
	tg.tag("fleet", forged)
	if !forged.Unverified {
		t.Fatal("a forged stamp was not tagged")
	}

	replay := stampedPost("pA", "replayed")
	again := *replay
	tg.tag("fleet", replay)
	tg.tag("fleet", &again)
	if replay.Unverified || !again.Unverified {
		t.Fatalf("replay: first unverified=%v, second unverified=%v; want false, true", replay.Unverified, again.Unverified)
	}

	old := &msg.MsgBoardPost{PaneID: "pA", Text: "old", TS: time.Now().Add(-10 * time.Minute).UnixMilli()}
	old.Stamp = boardstamp.Stamp("fleet", old)
	tg.tag("fleet", old)
	if !old.Unverified {
		t.Fatal("a stamp older than the window was not tagged")
	}

	// A valid stamp on a non-head's post (e.g. a role change after the door)
	// is still tagged: the role check stays.
	worker := stampedPost("pB", "worker with a stamp")
	tg.tag("fleet", worker)
	if !worker.Unverified {
		t.Fatal("a non-head's post was verified")
	}

	// A poster cannot pre-clear the tag, and on other boards the recorder
	// clears both fields.
	liar := &msg.MsgBoardPost{PaneID: "pB", Text: "x", TS: time.Now().UnixMilli(), Unverified: false}
	tg.tag("FLEET", liar)
	if !liar.Unverified {
		t.Fatal("a non-head's raw post kept the poster's unverified=false")
	}
	elsewhere := &msg.MsgBoardPost{PaneID: "pB", Text: "x", Unverified: true, Stamp: "zz"}
	tg.tag("epic-a", elsewhere)
	if elsewhere.Unverified || elsewhere.Stamp != "" {
		t.Fatalf("on another board: %+v; want both fields cleared", elsewhere)
	}
}

// ABLA records a tagged post (never drops it), with the stamp cleared; the
// key's hex is in no stored post and on no captured bus message.
func TestW3ABLARecordsTaggedPostsAndNeverTheKey(t *testing.T) {
	nc := newABLATestNATS(t)
	kv := newABLATestKV(t, nc)
	startFakeFleetRegistry(t, nc, guardFleets())
	codecs := msg.DefaultCodecRegistry()
	pub := msg.NewNATSPublisher(nc, codecs)
	bus := recordSubject(t, nc, msg.T("board", ">"))
	var logs bytes.Buffer
	oldLog := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(oldLog)

	abla := NewAgentBoardListenerActor(nc, codecs, kv)
	abla.Start()
	defer abla.Stop()

	raw := &msg.MsgBoardPost{V: msg.BoardSchemaVersion, PaneID: "pA", Persona: "ceo", Kind: "milestone",
		Text: "raw", TS: time.Now().UnixMilli()}
	door := &msg.MsgBoardPost{V: msg.BoardSchemaVersion, PaneID: "pA", Persona: "ceo", Kind: "milestone",
		Text: "door", TS: time.Now().UnixMilli() + 1}
	door.Stamp = boardstamp.Stamp("fleet", door)
	for _, p := range []*msg.MsgBoardPost{raw, door} {
		if err := msg.SendBoardPost(pub, "fleet", p); err != nil {
			t.Fatal(err)
		}
	}

	var stored map[string]msg.MsgBoardPost
	ablaWaitFor(t, "both posts to be recorded", func() bool {
		stored = map[string]msg.MsgBoardPost{}
		keys, _ := kv.Keys()
		for _, k := range keys {
			if e, err := kv.Get(k); err == nil {
				var p msg.MsgBoardPost
				if json.Unmarshal(e.Value(), &p) == nil && p.Text != "" {
					stored[p.Text] = p
				}
			}
		}
		return len(stored) == 2
	})
	if !stored["raw"].Unverified || stored["door"].Unverified {
		t.Fatalf("stored: raw unverified=%v, door unverified=%v; want true, false", stored["raw"].Unverified, stored["door"].Unverified)
	}
	if stored["door"].Stamp != "" || stored["raw"].Stamp != "" {
		t.Fatal("a stored post still carries a stamp")
	}

	// The key is unexported on purpose (no accessor, not even for tests), so
	// the leak check works without it: every 64-hex run on the bus must be the
	// one stamp the test itself published, and stored posts and the log carry
	// none. boardstamp's own tests check the key directly.
	hex64 := regexp.MustCompile(`[0-9a-f]{64}`)
	_, payloads := bus.seen()
	for _, p := range payloads {
		for _, m := range hex64.FindAllString(string(p), -1) {
			if m != door.Stamp {
				t.Fatalf("an unknown 64-hex value is on the bus: %s…", m[:8])
			}
		}
	}
	for _, p := range stored {
		data, _ := json.Marshal(p)
		if n := len(hex64.FindAllString(string(data), -1)); n != 0 {
			t.Fatalf("a stored post carries %d 64-hex values", n)
		}
	}
	if n := len(hex64.FindAllString(logs.String(), -1)); n != 0 {
		t.Fatalf("the log carries %d 64-hex values", n)
	}
}
