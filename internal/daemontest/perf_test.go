// SPDX-License-Identifier: Apache-2.0

package daemontest_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/daemontest"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// TestPerfPaneStartAndDaemonMemory measures the two numbers Wave 2 asked for
// (pane start time, daemon memory at ~40 panes) on a throwaway test daemon,
// never on a session a developer uses. ENV-GATED: RYSH_CLI_PERF=1 (a skip in
// the default suite: it starts 40 interactive shells and takes ~1 min).
// RYSH_CLI_PERF_PANES overrides the pane count.
//
// "Pane start" is the time from sending the create command to the new pane's
// own shell running a command: every pane is asked, by a workspace-wide
// ##cmd, to touch a file named after its RYSH_PANE, and the pane counts as
// started when its file appears. The broadcast is re-sent every pollEvery,
// because a shell that is still starting drops what it never read, so the
// figure is an upper bound with that resolution plus one CLI round-trip.
func TestPerfPaneStartAndDaemonMemory(t *testing.T) {
	if os.Getenv("RYSH_CLI_PERF") != "1" {
		t.Skip("perf: set RYSH_CLI_PERF=1 to run (starts ~40 shells on a test daemon)")
	}
	want := 40
	if v := os.Getenv("RYSH_CLI_PERF_PANES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 2 {
			t.Fatalf("RYSH_CLI_PERF_PANES=%q: want an integer >= 2", v)
		}
		want = n
	}
	const perTab = 10
	const pollEvery = 250 * time.Millisecond

	s := daemontest.Fresh(t)
	pid := daemonPID(t, s)
	ready := t.TempDir()
	touch := "##cmd ws -- touch " + filepath.Join(ready, "r-") + `"$RYSH_PANE"`
	count := func() int {
		ents, _ := os.ReadDir(ready)
		return len(ents)
	}
	waitFor := func(n int) time.Duration {
		start := time.Now()
		deadline := start.Add(30 * time.Second)
		var sent time.Time
		for time.Now().Before(deadline) {
			if time.Since(sent) >= pollEvery {
				s.Exec(t, touch)
				sent = time.Now()
			}
			if count() >= n {
				return time.Since(start)
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("perf: only %d of %d panes ran a command within 30s", count(), n)
		return 0
	}

	waitFor(1)
	rss0 := rssKB(t, pid)
	t.Logf("perf: daemon pid start, 1 pane: rss %d kB", rss0)

	var starts []time.Duration
	var slowest time.Duration
	var slowestN int
	var slowestCmd string
	for n := 2; n <= want; n++ {
		cmd := "##new pane"
		if (n-1)%perTab == 0 {
			cmd = "##new tab"
		}
		t0 := time.Now()
		s.MustSucceed(t, cmd)
		created := time.Since(t0)
		d := created + waitFor(n)
		starts = append(starts, d)
		if d > slowest {
			slowest, slowestN, slowestCmd = d, n, cmd
		}
		if n%10 == 0 || n == want {
			t.Logf("perf: %d panes: rss %d kB (last create %s, shell ran a command after %s)",
				n, rssKB(t, pid), created.Round(time.Millisecond), d.Round(time.Millisecond))
		}
	}
	rssN := rssKB(t, pid)
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	pct := func(p float64) time.Duration { return starts[int(p*float64(len(starts)-1))].Round(time.Millisecond) }
	t.Logf("perf: pane start (create -> shell ran a command), %d panes: p50 %s p90 %s max %s",
		len(starts), pct(0.5), pct(0.9), pct(1))
	t.Logf("perf: slowest start: pane %d (%s) %s", slowestN, slowestCmd, slowest.Round(time.Millisecond))
	t.Logf("perf: daemon rss 1 pane %d kB -> %d panes %d kB: %d kB per added pane",
		rss0, want, rssN, (rssN-rss0)/int64(want-1))
}

// daemonPID reads the test daemon's pid from its own session record.
func daemonPID(t *testing.T, s *daemontest.Session) int {
	t.Helper()
	st, err := session.NewStore(config.LoadFrom(s.Config))
	if err != nil {
		t.Fatalf("perf: session store: %v", err)
	}
	rec, err := st.Get(s.Name)
	if err != nil || rec.PID <= 0 {
		t.Fatalf("perf: no daemon pid for %s: %v", s.Name, err)
	}
	return rec.PID
}

// rssKB is the resident size of the test daemon (ps reads; never a signal).
func rssKB(t *testing.T, pid int) int64 {
	t.Helper()
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("perf: ps rss for the test daemon %d: %v", pid, err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		t.Fatalf("perf: rss %q: %v", out, err)
	}
	return n
}
