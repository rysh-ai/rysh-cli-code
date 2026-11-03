// SPDX-License-Identifier: Apache-2.0

package needsyou

import (
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

var t0 = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func needs(id, reason string, since int64) Pane {
	return Pane{ID: id, Name: id, Reason: reason, Since: since}
}

func ids(ps []Pane) string {
	var s []string
	for _, p := range ps {
		s = append(s, p.ID)
	}
	return strings.Join(s, ",")
}

func TestNoAlertBeforeAfter(t *testing.T) {
	w := NewWatcher(time.Minute, 5*time.Minute)
	if got := w.Observe(t0, []Pane{needs("p1", "approval", 1)}); len(got) != 0 {
		t.Fatalf("alerted at once: %v", ids(got))
	}
	if got := w.Observe(t0.Add(59*time.Second), []Pane{needs("p1", "approval", 1)}); len(got) != 0 {
		t.Fatalf("alerted before after: %v", ids(got))
	}
	if got := w.Observe(t0.Add(60*time.Second), []Pane{needs("p1", "approval", 1)}); ids(got) != "p1" {
		t.Fatalf("no alert at after: %q", ids(got))
	}
}

func TestAnsweredBeforeAfterNeverAlerts(t *testing.T) {
	w := NewWatcher(time.Minute, 5*time.Minute)
	w.Observe(t0, []Pane{needs("p1", "prompt", 1)})
	// The person at the keyboard answered: the pane no longer needs anyone.
	if got := w.Observe(t0.Add(30*time.Second), nil); len(got) != 0 {
		t.Fatalf("alert for a cleared pane: %v", ids(got))
	}
	if got := w.Observe(t0.Add(2*time.Minute), nil); len(got) != 0 {
		t.Fatalf("alert after clear: %v", ids(got))
	}
}

func TestOneAlertPerRaise(t *testing.T) {
	w := NewWatcher(0, 0)
	if got := w.Observe(t0, []Pane{needs("p1", "question", 1)}); ids(got) != "p1" {
		t.Fatalf("after=0 should alert at once: %q", ids(got))
	}
	for i := 1; i < 5; i++ {
		if got := w.Observe(t0.Add(time.Duration(i)*time.Minute), []Pane{needs("p1", "question", 1)}); len(got) != 0 {
			t.Fatalf("second alert for the same raise: %v", ids(got))
		}
	}
}

func TestNewRaiseAlertsAgainOnlyAfterCooldown(t *testing.T) {
	w := NewWatcher(0, 5*time.Minute)
	w.Observe(t0, []Pane{needs("p1", "approval", 1)})
	w.Observe(t0.Add(time.Minute), nil) // answered
	// A new raise (new since) inside the cooldown is held, not dropped.
	if got := w.Observe(t0.Add(2*time.Minute), []Pane{needs("p1", "approval", 2)}); len(got) != 0 {
		t.Fatalf("alert inside cooldown: %v", ids(got))
	}
	if got := w.Observe(t0.Add(5*time.Minute), []Pane{needs("p1", "approval", 2)}); ids(got) != "p1" {
		t.Fatalf("held raise not sent once cooldown passed: %q", ids(got))
	}
}

func TestSinceChangeWithoutClearIsANewRaise(t *testing.T) {
	w := NewWatcher(0, 0)
	w.Observe(t0, []Pane{needs("p1", "approval", 1)})
	// The approval was answered and a new one raised between two looks.
	if got := w.Observe(t0.Add(time.Second), []Pane{needs("p1", "approval", 2)}); ids(got) != "p1" {
		t.Fatalf("new raise missed: %q", ids(got))
	}
}

func TestPanesAreIndependent(t *testing.T) {
	w := NewWatcher(time.Minute, 5*time.Minute)
	w.Observe(t0, []Pane{needs("p1", "approval", 1)})
	got := w.Observe(t0.Add(time.Minute), []Pane{needs("p1", "approval", 1), needs("p2", "prompt", 7)})
	if ids(got) != "p1" {
		t.Fatalf("got %q, want p1 only (p2 just raised)", ids(got))
	}
	got = w.Observe(t0.Add(2*time.Minute), []Pane{needs("p1", "approval", 1), needs("p2", "prompt", 7)})
	if ids(got) != "p2" {
		t.Fatalf("got %q, want p2", ids(got))
	}
}

func TestPendingReportsUnsentRaises(t *testing.T) {
	w := NewWatcher(time.Minute, 0)
	if w.Pending() {
		t.Fatal("pending with nothing seen")
	}
	w.Observe(t0, []Pane{needs("p1", "approval", 1)})
	if !w.Pending() {
		t.Fatal("not pending while a raise waits for after")
	}
	w.Observe(t0.Add(time.Minute), []Pane{needs("p1", "approval", 1)})
	if w.Pending() {
		t.Fatal("still pending after the alert went out")
	}
}

func tabWith(id string, panes ...domain.PaneSnapshot) domain.TabSnapshot {
	return domain.TabSnapshot{ID: id, Lanes: []domain.LaneSnapshot{{PaneGroups: []domain.PaneGroupSnapshot{{Panes: panes}}}}}
}

func TestPanesFromTabsReadsAttentionAndSkipsMirrors(t *testing.T) {
	attn := func(reason string) map[string]string {
		return map[string]string{
			domain.PaneMetaAttention:       domain.AttentionNeedsYou,
			domain.PaneMetaAttentionReason: reason,
			domain.PaneMetaAttentionSince:  "1700000000000",
		}
	}
	tabs := []domain.TabSnapshot{
		tabWith("t1",
			domain.PaneSnapshot{ID: "0123456789abcdef", Title: "claude --dangerous rm -rf secret", Meta: attn("prompt")},
			domain.PaneSnapshot{ID: "quiet-pane", Meta: map[string]string{}},
			domain.PaneSnapshot{ID: "named-pane", GivenName: "builder\x1b]9;x\a", Meta: attn("approval")},
		),
		// A remote host's pane is notified by the daemon it runs on.
		tabWith("mirror:remote:devbox:t9", domain.PaneSnapshot{ID: "remote-pane", Meta: attn("question")}),
	}
	got := PanesFromTabs(tabs)
	if len(got) != 2 {
		t.Fatalf("got %d panes, want 2: %+v", len(got), got)
	}
	if got[0].ID != "0123456789abcdef" || got[0].Name != "01234567" || got[0].Reason != "prompt" || got[0].Since != 1700000000000 {
		t.Fatalf("pane 0 = %+v (want short id as name, never the title)", got[0])
	}
	if got[1].Name != "builder]9;x" || got[1].Reason != "approval" {
		t.Fatalf("pane 1 = %+v (want given name with control bytes dropped)", got[1])
	}
}
