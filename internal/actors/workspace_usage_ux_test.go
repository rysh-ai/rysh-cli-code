package actors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// TestCeilingWarn covers the status-bar yellow trigger (design 003 §3.5): any
// active ceiling at ≥80% of its limit.
func TestCeilingWarn(t *testing.T) {
	cases := []struct {
		name     string
		ceilings []msg.UsageCeiling
		want     bool
	}{
		{"none", nil, false},
		{"under 80%", []msg.UsageCeiling{{CeilingTokens: 1000, SpentTokens: 799}}, false},
		{"exactly 80%", []msg.UsageCeiling{{CeilingTokens: 1000, SpentTokens: 800}}, true},
		{"over", []msg.UsageCeiling{{CeilingTokens: 1000, SpentTokens: 950}}, true},
		{"no ceiling set", []msg.UsageCeiling{{CeilingTokens: 0, SpentTokens: 5000}}, false},
		{"one of many", []msg.UsageCeiling{{CeilingTokens: 1000, SpentTokens: 10}, {CeilingTokens: 100, SpentTokens: 90}}, true},
	}
	for _, c := range cases {
		if got := ceilingWarn(c.ceilings); got != c.want {
			t.Errorf("%s: ceilingWarn = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsoWeekKey(t *testing.T) {
	// 2026-07-22 is in ISO week 30 of 2026.
	got := isoWeekKey(time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC))
	if got != "2026-W30" {
		t.Fatalf("isoWeekKey = %q, want 2026-W30", got)
	}
}

// TestWriteWeeklyDigest proves the digest is written once per ISO week: the
// first call writes the file + marker; a second call for the same week is a
// no-op (wrote=false); a new week writes again.
func TestWriteWeeklyDigest(t *testing.T) {
	dir := t.TempDir()
	reply := &msg.MsgUsageSnapshotReply{
		Window: "week", SessionCostMicroUSD: 1_420_000, SessionTokens: 512_000,
		ByAgent: []msg.UsageAgg{{Key: "code-reviewer", CostMicroUSD: 900_000, InTokens: 300_000, OutTokens: 12_000}},
		ByPane:  []msg.UsageAgg{{Key: "b1f9-uuid", CostMicroUSD: 520_000, InTokens: 180_000, OutTokens: 20_000}},
	}

	path, wrote, err := writeWeeklyDigest(dir, "2026-W30", reply)
	if err != nil || !wrote {
		t.Fatalf("first write: wrote=%v err=%v", wrote, err)
	}
	body, _ := os.ReadFile(path)
	s := string(body)
	for _, want := range []string{"# rysh usage — 2026-W30", "Total:", "By agent", "code-reviewer", "By pane"} {
		if !strings.Contains(s, want) {
			t.Fatalf("digest missing %q:\n%s", want, s)
		}
	}
	// Marker records the week.
	if digestedWeek(dir) != "2026-W30" {
		t.Fatalf("marker = %q, want 2026-W30", digestedWeek(dir))
	}

	// Same week again → no-op.
	if _, wrote, _ := writeWeeklyDigest(dir, "2026-W30", reply); wrote {
		t.Fatal("second write for the same week should be a no-op")
	}

	// A new week writes again.
	if _, wrote, err := writeWeeklyDigest(dir, "2026-W31", reply); err != nil || !wrote {
		t.Fatalf("new-week write: wrote=%v err=%v", wrote, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "usage-2026-W31.md")); err != nil {
		t.Fatalf("new-week digest not written: %v", err)
	}
}
