// SPDX-License-Identifier: Apache-2.0

package daemontest_test

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/daemontest"
)

// F-55, end to end against a real daemon.
//
// `##lane info <ref>` and `##panegroup info <ref>` ignored their positional
// argument and described the CALLER's lane/stack — a complete, plausible,
// exit-0 answer about the wrong thing. The unit tests in internal/actors pin
// the resolution rules; these pin that the wiring actually reaches them, which
// is the half a mocked snapshot cannot prove.
//
// The load-bearing assertion in both is that DIFFERENT refs produce DIFFERENT
// answers. Before the fix every ref produced the same block, so any test that
// checked only one ref would have passed against the defect.

// infoField pulls "  position    : 2 of 3" out of an info block.
func infoField(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func TestLaneInfoAnswersAboutTheLaneItWasGiven(t *testing.T) {
	s := daemontest.Fresh(t)
	s.MustSucceed(t, "##new lane")
	s.MustSucceed(t, "##new lane")

	if lanes, _ := layout(t, s); lanes != 3 {
		t.Fatalf("setup produced %d lanes, want 3", lanes)
	}

	// Each index must describe ITS OWN lane. Before the fix all three of these
	// returned the caller's lane, so all three positions read the same.
	ids := make(map[string]string, 3)
	for _, idx := range []string{"1", "2", "3"} {
		out := s.MustSucceed(t, "##lane info "+idx)
		want := idx + " of 3"
		if got := infoField(out, "position"); got != want {
			t.Errorf("##lane info %s reported position %q, want %q — the ref was ignored\n%s",
				idx, got, want, out)
		}
		id := infoField(out, "id")
		if id == "" {
			t.Fatalf("##lane info %s printed no id:\n%s", idx, out)
		}
		if prev, dup := ids[id]; dup {
			t.Fatalf("##lane info %s and ##lane info %s both reported lane %s — "+
				"different refs must not resolve to the same lane", prev, idx, id)
		}
		ids[id] = idx
	}

	// The bare form still reports the caller's own lane, successfully. That is
	// long-standing behaviour and the fix must not turn it into an error.
	bare := s.MustSucceed(t, "##lane info")
	if infoField(bare, "id") == "" {
		t.Errorf("bare ##lane info printed no lane:\n%s", bare)
	}

	// The eight characters `##lane list` prints must be a ref you can paste
	// back. They were not: the resolver matched only the full uuid.
	var full string
	for id := range ids {
		if ids[id] == "2" {
			full = id
		}
	}
	if len(full) < 8 {
		t.Fatalf("lane id %q is too short to abbreviate", full)
	}
	byPrefix := s.MustSucceed(t, "##lane info "+full[:8])
	if got := infoField(byPrefix, "id"); got != full {
		t.Errorf("the 8-char id from the listing resolved to %q, want %q\n%s", got, full, byPrefix)
	}

	// REFUSE, DO NOT GUESS.
	out := s.MustFail(t, "##lane info nonexistent-ref-xyz")
	if !strings.Contains(out, "nonexistent-ref-xyz") {
		t.Errorf("the refusal does not name the ref that failed:\n%s", out)
	}
	if strings.Contains(out, "position") {
		t.Errorf("an info block was printed for an unresolvable ref:\n%s", out)
	}
}

func TestStackInfoAnswersAboutTheStackItWasGiven(t *testing.T) {
	s := daemontest.Fresh(t)
	s.MustSucceed(t, "##new pane") // a second stack in the active lane

	if _, stacks := layout(t, s); stacks != 2 {
		t.Fatalf("setup produced %d stacks, want 2", stacks)
	}

	ids := make(map[string]string, 2)
	for _, idx := range []string{"1", "2"} {
		out := s.MustSucceed(t, "##pg info "+idx)
		want := idx + " of 2"
		if got := infoField(out, "group"); got != want {
			t.Errorf("##pg info %s reported group %q, want %q — the ref was ignored\n%s",
				idx, got, want, out)
		}
		id := infoField(out, "id")
		if prev, dup := ids[id]; dup {
			t.Fatalf("##pg info %s and ##pg info %s both reported stack %s", prev, idx, id)
		}
		ids[id] = idx
	}

	if bare := s.MustSucceed(t, "##pg info"); infoField(bare, "id") == "" {
		t.Errorf("bare ##pg info printed no stack:\n%s", bare)
	}

	var full string
	for id := range ids {
		if ids[id] == "2" {
			full = id
		}
	}
	if len(full) >= 8 {
		byPrefix := s.MustSucceed(t, "##pg info "+full[:8])
		if got := infoField(byPrefix, "id"); got != full {
			t.Errorf("the 8-char id from the listing resolved to %q, want %q\n%s", got, full, byPrefix)
		}
	}

	out := s.MustFail(t, "##pg info nonexistent-ref-xyz")
	if !strings.Contains(out, "nonexistent-ref-xyz") {
		t.Errorf("the refusal does not name the ref that failed:\n%s", out)
	}
}
