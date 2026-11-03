// SPDX-License-Identifier: Apache-2.0

package boardstamp

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Design 030 §9.3 / founder ruling D-24, wave W3, with security-lead's early
// notes (out/e20-030-w3-plan-notes.md @ root 88d9a5726): a door stamps an
// accepted fleet-board post; the recorder verifies the stamp, so a raw post
// that bypassed a door reads unverified whatever pane it claims.

var now = time.Date(2026, 11, 1, 17, 30, 0, 0, time.UTC)

func doorPost() *msg.MsgBoardPost {
	return &msg.MsgBoardPost{PaneID: "pane-head", Persona: "ceo", Kind: "milestone",
		Text: "M1 shipped", ThreadID: "t/1", TS: now.UnixMilli()}
}

func TestADoorStampVerifies(t *testing.T) {
	p := doorPost()
	p.Stamp = Stamp("fleet", p)
	if len(p.Stamp) != 64 {
		t.Fatalf("stamp %q: want 64 hex chars", p.Stamp)
	}
	if !Verify("fleet", p) || !Verify("FLEET", p) {
		t.Fatal("a door stamp does not verify")
	}
}

// Every bound field changes the stamp: a stamp lifted from one post fits no
// other post.
func TestAStampFitsOnlyItsOwnPost(t *testing.T) {
	base := doorPost()
	stamp := Stamp("fleet", base)
	for name, mutate := range map[string]func(p *msg.MsgBoardPost){
		"pane":   func(p *msg.MsgBoardPost) { p.PaneID = "pane-other" },
		"kind":   func(p *msg.MsgBoardPost) { p.Kind = "done" },
		"text":   func(p *msg.MsgBoardPost) { p.Text = "M1 shipped!" },
		"thread": func(p *msg.MsgBoardPost) { p.ThreadID = "t/2" },
		"ts":     func(p *msg.MsgBoardPost) { p.TS++ },
	} {
		p := doorPost()
		mutate(p)
		p.Stamp = stamp
		if Verify("fleet", p) {
			t.Errorf("a stamp survived a changed %s", name)
		}
	}
	p := doorPost()
	p.Stamp = stamp
	if Verify("epic-a", p) {
		t.Error("a fleet-board stamp verified on another board")
	}
}

func TestNoOrForgedStampFails(t *testing.T) {
	for _, s := range []string{"", "00", strings.Repeat("0", 64), strings.Repeat("z", 64)} {
		p := doorPost()
		p.Stamp = s
		if Verify("fleet", p) {
			t.Errorf("stamp %q verified", s)
		}
	}
}

// The key never leaves the process: its hex is not the stamp and does not
// appear in a stamped post's JSON.
func TestTheKeyIsNotInTheStampOrThePost(t *testing.T) {
	p := doorPost()
	p.Stamp = Stamp("fleet", p)
	k := hex.EncodeToString(key())
	if len(k) != 64 {
		t.Fatalf("key is %d hex chars, want 64", len(k))
	}
	if p.Stamp == k || strings.Contains(p.Stamp, k) {
		t.Fatal("the stamp is the key")
	}
	data, _ := json.Marshal(p)
	if n := strings.Count(string(data), k); n != 0 {
		t.Fatalf("the key's hex appears %d times in the post", n)
	}
}

func TestReplayGuard(t *testing.T) {
	g := NewReplayGuard(5 * time.Minute)
	p := doorPost()
	p.Stamp = Stamp("fleet", p)

	if !g.Accept(p, now) {
		t.Fatal("a fresh stamp was refused")
	}
	if g.Accept(p, now.Add(time.Second)) {
		t.Fatal("a replayed stamp was accepted")
	}
	old := doorPost()
	old.TS = now.Add(-6 * time.Minute).UnixMilli()
	old.Stamp = Stamp("fleet", old)
	if g.Accept(old, now) {
		t.Fatal("a stamp older than the window was accepted")
	}
	future := doorPost()
	future.TS = now.Add(6 * time.Minute).UnixMilli()
	future.Stamp = Stamp("fleet", future)
	if g.Accept(future, now) {
		t.Fatal("a stamp from beyond the window was accepted")
	}
	if g.Accept(&msg.MsgBoardPost{TS: now.UnixMilli()}, now) {
		t.Fatal("an empty stamp was accepted")
	}
}

// Memory stays bounded: seen stamps are kept only for the window.
func TestReplayGuardStaysBounded(t *testing.T) {
	g := NewReplayGuard(5 * time.Minute)
	clock := now
	for i := 0; i < 20000; i++ {
		p := doorPost()
		p.TS = clock.UnixMilli()
		p.Text = strings.Repeat("x", i%7) + string(rune('a'+i%26))
		p.ThreadID = "t/" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
		p.Stamp = Stamp("fleet", p)
		g.Accept(p, clock)
		clock = clock.Add(100 * time.Millisecond) // 20000 posts over ~33 min
	}
	// At 10 posts a second, a 5-minute window holds at most ~3000 stamps (plus
	// the window on the other side for clock skew).
	if n := g.Len(); n > 6100 {
		t.Fatalf("the guard holds %d stamps after 33 minutes; it must keep only the window", n)
	}
}
