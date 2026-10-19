// SPDX-License-Identifier: Apache-2.0

package board

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata golden files")

// goldenPage is a page exercising every rule the renderer owes: the field
// grid, continuation lines, a paragraph before the first field, a STATUS
// badge per tone, links, and a <script> that must stay text.
func goldenPage() HTMLPage {
	s := New(0)
	post := func(pane, persona, text string, n int, ts int64) {
		s.Apply(itemRoot(pane, persona, text, n, ts))
	}
	post(pane314, "issue-314", "Picked up the codec work.\n"+
		"STATUS: in progress - wiring codec\n"+
		"BLOCKED: none\n"+
		"PRS: https://github.com/o/r/pull/9\n"+
		"  and https://github.com/o/r/pull/10.\n"+
		"DEPENDS ON: #312", 1, 7_000)
	post(pane31, "issue-31", "STATUS: BLOCKED on CI\nBRIEF: <script>alert(1)</script> & more", 1, 2_000)
	post(pane31, "issue-31", "STATUS: waiting for review", 2, 3_000)
	post(pane314, "issue-314", "STATUS: done\nISSUE RELATED TO: #314", 2, 4_000)
	post(paneMgr, "mgr", "STATUS: idle", 1, 5_000)
	post(paneMgr, "mgr", "STATUS: milestone reached", 2, 6_000)
	post(paneMgr, "mgr", "no fields at all, just prose", 3, 1_000) // the oldest: outside "last 6"
	items := Flatten(reply(s))
	picked, matched, total := Select(items, "", 6)
	return HTMLPage{Board: "etl-fix", N: 6, Picked: picked, Matched: matched, Total: total,
		Now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), Loc: time.UTC}
}

func TestRenderHTMLGolden(t *testing.T) {
	got, err := RenderHTML(goldenPage())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "board_output.golden.html")
	if *updateGolden {
		_ = os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("RenderHTML differs from %s (run `go test ./internal/board -run Golden -update` after checking the diff)\n--- got ---\n%s", path, got)
	}
}

// The golden pins bytes; these pin the MEANING, so a regenerated golden
// cannot quietly lose a rule.
func TestRenderHTMLRules(t *testing.T) {
	out, err := RenderHTML(goldenPage())
	if err != nil {
		t.Fatal(err)
	}
	page := string(out)
	for _, want := range []string{
		"<title>Agent board · etl-fix</title>",
		"last 6 of 7 · board has 7 messages · generated 2026-09-30 12:00",
		`<dt>STATUS</dt><dd>in progress - wiring codec</dd>`,
		`<dt>DEPENDS ON</dt><dd>#312</dd>`,
		// Continuation line joined into PRS, both URLs linked, full URL shown,
		// the trailing full stop outside the link.
		`<dt>PRS</dt><dd><a href="https://github.com/o/r/pull/9">https://github.com/o/r/pull/9</a>` + "\n" +
			`  and <a href="https://github.com/o/r/pull/10">https://github.com/o/r/pull/10</a>.</dd>`,
		`<p class="loose">Picked up the codec work.</p>`,
		`<span class="badge info">in progress</span>`,
		`<span class="badge bad">BLOCKED on CI</span>`,
		`<span class="badge warn">waiting for review</span>`,
		`<span class="badge good">done</span>`,
		`<span class="badge muted">idle</span>`,
		`<span class="badge good">milestone reached</span>`,
		"&lt;script&gt;alert(1)&lt;/script&gt; &amp; more",
		`<time datetime="1970-01-01T00:00:02">1970-01-01 00:00:02</time>`,
		"break-inside:avoid",
		"prefers-color-scheme: dark",
		"@media print",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	if strings.Contains(page, "<script>") {
		t.Fatal("a <script> in a post reached the page unescaped")
	}
	if strings.Contains(page, "no fields at all") {
		t.Error("the oldest message is outside \"last 6\" but was rendered")
	}

	// Worker scope in the title and meta; "k messages" when nothing was cut.
	p := goldenPage()
	p.Worker, p.N = "issue-314", 0
	p.Picked, p.Matched, _ = Select(Flatten(reply(fleet())), "issue-314", 0)
	out, _ = RenderHTML(p)
	if !strings.Contains(string(out), "<title>Agent board · etl-fix · issue-314</title>") ||
		!strings.Contains(string(out), "issue-314 · 2 messages · board has 7 messages") {
		t.Fatalf("worker page header wrong:\n%s", out)
	}
}

func TestStatusTone(t *testing.T) {
	for in, want := range map[string]string{
		"BLOCKED on CI": "bad", "waiting": "warn", "in progress": "info", "Done": "good",
		"idle": "muted", "milestone": "good", "shipping": "muted",
	} {
		if got := StatusTone(in); got != want {
			t.Errorf("StatusTone(%q) = %q, want %q", in, got, want)
		}
	}
}
