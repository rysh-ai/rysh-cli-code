// SPDX-License-Identifier: Apache-2.0

package msg

import (
	"strings"
	"testing"
)

// Board ids (design 028, gate D-12). These tests pin the two properties the
// ruling rests on: a board is addressed by SUBJECT, and the default board's
// subject did not change.

// TestDefaultBoardKeepsTheLegacySubject is the compatibility guarantee.
//
// Every brief, script and pane in the field publishes on the three-token
// subject. If this ever grows a fourth token, every pre-028 reader in the
// session goes quiet while looking perfectly healthy — the F-23 shape.
func TestDefaultBoardKeepsTheLegacySubject(t *testing.T) {
	original := SessionPrefix()
	t.Cleanup(func() { SetSessionPrefix(original) })
	SetSessionPrefix("macmini-rysh")

	for _, spelling := range []string{"", DefaultBoardID, "SESSION", "  session  "} {
		if got, want := BoardPostSubject(spelling), "macmini-rysh.board.post"; got != want {
			t.Errorf("BoardPostSubject(%q) = %q, want the legacy subject %q", spelling, got, want)
		}
	}
	if got, want := BoardRegisterSubject(""), "macmini-rysh.board.register"; got != want {
		t.Errorf("BoardRegisterSubject(\"\") = %q, want %q", got, want)
	}
	if got, want := BoardAliveSubject(""), "macmini-rysh.board.alive"; got != want {
		t.Errorf("BoardAliveSubject(\"\") = %q, want %q", got, want)
	}
	if got, want := BoardQuerySubject(""), "macmini-rysh.board.query"; got != want {
		t.Errorf("BoardQuerySubject(\"\") = %q, want %q", got, want)
	}
}

// TestNamedBoardGetsItsOwnSubject — the whole feature, in one assertion.
func TestNamedBoardGetsItsOwnSubject(t *testing.T) {
	original := SessionPrefix()
	t.Cleanup(func() { SetSessionPrefix(original) })
	SetSessionPrefix("macmini-rysh")

	if got, want := BoardPostSubject("epic-07"), "macmini-rysh.board.epic-07.post"; got != want {
		t.Fatalf("BoardPostSubject(epic-07) = %q, want %q", got, want)
	}
	if BoardPostSubject("epic-07") == BoardPostSubject("epic-08") {
		t.Fatal("two boards share a subject: they would share a stream")
	}
	// The wildcard must cover named boards and NOT the default one, because the
	// two are subscribed to separately (board.Subscribe).
	if got, want := BoardPostPattern(), "macmini-rysh.board.*.post"; got != want {
		t.Fatalf("BoardPostPattern() = %q, want %q", got, want)
	}
}

// TestBoardIDRejectsSubjectInjection. A board id becomes a subject TOKEN, so an
// id containing "." would publish into a subject nobody subscribes to, and "*"
// or ">" would publish into (or read) every board at once. Both fail silently
// at the publisher, which is why the edges refuse rather than sanitise.
func TestBoardIDRejectsSubjectInjection(t *testing.T) {
	bad := []string{
		"epic.07",                            // extra token
		"*",                                  // every board
		">",                                  // every board, recursively
		"epic 07",                            // whitespace
		"-leading",                           // not a letter or digit
		"_leading",                           //
		"Épic",                               // non-ascii
		"epic/07",                            //
		"0123456789012345678901234567890123", // 34 chars
	}
	for _, id := range bad {
		if err := ValidateBoardID(id); err == nil {
			t.Errorf("ValidateBoardID(%q) = nil, want a refusal", id)
		}
	}
	for _, id := range []string{"epic-07", "epic_07", "e", "session", "b2b", "EPIC-07"} {
		if err := ValidateBoardID(id); err != nil {
			t.Errorf("ValidateBoardID(%q) = %v, want nil", id, err)
		}
	}
}

// TestInvalidBoardIDCannotReachTheWire is the defence-in-depth half: even if an
// unvalidated id somehow arrives at a subject builder, the subject it produces
// is a legal one for the SESSION board rather than an injected pattern.
func TestInvalidBoardIDCannotReachTheWire(t *testing.T) {
	original := SessionPrefix()
	t.Cleanup(func() { SetSessionPrefix(original) })
	SetSessionPrefix("s")

	for _, id := range []string{"a.b", "*", ">", "with space"} {
		if got, want := BoardPostSubject(id), "s.board.post"; got != want {
			t.Errorf("BoardPostSubject(%q) = %q, want the session board's subject %q", id, got, want)
		}
	}
}

// TestBoardIDFromSubjectParsesFromTheEnd.
//
// THE SESSION NAME MAY CONTAIN A DOT. Counting tokens from the left would then
// read part of the prefix as a board id, and the symptom would be a board that
// is silently empty rather than an error.
func TestBoardIDFromSubjectParsesFromTheEnd(t *testing.T) {
	cases := []struct {
		subject string
		leaf    string
		want    string
		ok      bool
	}{
		{"rysh.board.post", "post", DefaultBoardID, true},
		{"rysh.board.epic-07.post", "post", "epic-07", true},
		{"my.dotted.session.board.post", "post", DefaultBoardID, true},
		{"my.dotted.session.board.epic-07.post", "post", "epic-07", true},
		{"rysh.board.register", "register", DefaultBoardID, true},
		{"rysh.board.epic-07.register", "register", "epic-07", true},
		{"rysh.pane.abc.output", "post", "", false},
		{"rysh.board.post", "register", "", false},
	}
	for _, c := range cases {
		got, ok := BoardIDFromSubject(c.subject, c.leaf)
		if ok != c.ok || got != c.want {
			t.Errorf("BoardIDFromSubject(%q, %q) = (%q, %v), want (%q, %v)",
				c.subject, c.leaf, got, ok, c.want, c.ok)
		}
	}
}

// TestEverySubjectRoundTrips: what a builder produces, the parser must read
// back. The two are the only halves of the addressing scheme, and a
// disagreement between them is a board that receives nothing.
func TestEverySubjectRoundTrips(t *testing.T) {
	original := SessionPrefix()
	t.Cleanup(func() { SetSessionPrefix(original) })
	SetSessionPrefix("some.session.name")

	for _, id := range []string{"", DefaultBoardID, "epic-07", "b2b_x"} {
		want := NormalizeBoardID(id)
		for _, c := range []struct {
			subject string
			leaf    string
		}{
			{BoardPostSubject(id), "post"},
			{BoardRegisterSubject(id), "register"},
			{BoardQuerySubject(id), "query"},
			{BoardAliveSubject(id), "alive"},
		} {
			got, ok := BoardIDFromSubject(c.subject, c.leaf)
			if !ok || got != want {
				t.Errorf("round trip %q via %q: got (%q, %v), want %q", id, c.subject, got, ok, want)
			}
		}
	}
}

// Gate 4 for 028 needs no new test: TestBoardPostCarriesNoFleetFields already
// asserts an ALLOWLIST of wire fields on MsgBoardPost, so a `board` field added
// to the post would fail it. Restating the rule here would be the second copy
// this codebase keeps paying for.

// TestBoardIDFromMetaResolvesEveryPaneToABoard pins the predicate BOTH board
// surfaces resolve a pane through — the TUI (tui.Model.boardIDForPane) and the
// web server's board_get (internal/web/board.go).
//
// It is one function precisely because the two surfaces would otherwise each
// carry the rule, and a drift between them would be INVISIBLE: both copies
// return a valid board id, so nothing errors and nothing is empty. The app
// would simply render a different board than the terminal UI shows for the same
// pane, and the two would each look right on their own.
func TestBoardIDFromMetaResolvesEveryPaneToABoard(t *testing.T) {
	for _, tc := range []struct {
		name, meta, want string
	}{
		{"no meta at all — every pre-028 pane", "", DefaultBoardID},
		{"whitespace only", "   ", DefaultBoardID},
		{"a fleet board", "desktop", "desktop"},
		{"case is not a different board", "DeskTop", "desktop"},
		{"padded by a hand edit", "  desktop  ", "desktop"},
		{"a dot would be extra subject tokens", "epic.07", DefaultBoardID},
		{"a wildcard would subscribe to every board", "*", DefaultBoardID},
		{"'>' is the other wildcard", ">", DefaultBoardID},
		{"leading dash is not a token start", "-desktop", DefaultBoardID},
		{"over the length bound", strings.Repeat("b", MaxBoardIDLen+1), DefaultBoardID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := BoardIDFromMeta(tc.meta); got != tc.want {
				t.Fatalf("BoardIDFromMeta(%q) = %q, want %q", tc.meta, got, tc.want)
			}
		})
	}
}

// TestBoardIDFromMetaNeverEscapesIntoASubject is the security half of the
// predicate: pane meta is writable by anything that can set it, so a value that
// could never be a subject token must not become one. Falling back to the
// session board is the bounded wrong answer boardSubject already takes.
func TestBoardIDFromMetaNeverEscapesIntoASubject(t *testing.T) {
	for _, hostile := range []string{"a.b", "*", ">", "a b", "a/b", "a\tb", "a>b"} {
		got := BoardIDFromMeta(hostile)
		if err := ValidateBoardID(got); err != nil {
			t.Fatalf("BoardIDFromMeta(%q) = %q, which is itself invalid: %v", hostile, got, err)
		}
		if got != DefaultBoardID {
			t.Fatalf("BoardIDFromMeta(%q) = %q, want the session board — an id that "+
				"cannot be a subject token must not be carried forward", hostile, got)
		}
	}
}
