// SPDX-License-Identifier: Apache-2.0

package forgecmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestForgeAddSkipLineCitesTheRuling is T3's error-text half at the SECOND
// user-facing site — the one a sweep for the hard error alone would miss.
//
// ingest/grpc.go's error only fires when NOTHING is callable. The common case
// is a service that mixes unary and client/bidi methods: ingest succeeds, and
// the refusal is reported instead as an add-time summary line from
// skippedSummary(). That line is what most users actually see, so it carries
// the same obligation — name the exclusion as a ruling and cite its reasons.
func TestForgeAddSkipLineCitesTheRuling(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "feed.pb"), streamingDescriptorSet(t), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := Run(workDir, []string{
		"add", "feed", "feed.pb",
		"--base-url", "http://127.0.0.1:8080",
		"--targets", "docs",
	}, &out); err != nil {
		t.Fatalf("forge add: %v\noutput:\n%s", err, out.String())
	}
	got := out.String()

	if !strings.Contains(got, "skipped") {
		t.Fatalf("fixture is malformed: add reported no skipped methods, so there is no refusal line to check:\n%s", got)
	}
	if !strings.Contains(got, "design 015 §5.1") {
		t.Errorf("add-time skip line does not cite where the reasons live (want %q):\n%s", "design 015 §5.1", got)
	}
	if !strings.Contains(got, "ruling") {
		t.Errorf("add-time skip line does not name the exclusion a ruling, so it reads as a gap awaiting work:\n%s", got)
	}
}
