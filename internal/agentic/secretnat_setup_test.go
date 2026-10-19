// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// cfgWithCustomDetectors builds a minimal config carrying only the [snat]
// custom detector list under test.
func cfgWithCustomDetectors(ds ...config.SNATCustomDetector) config.Config {
	var cfg config.Config
	cfg.SNAT.Enabled = true
	cfg.SNAT.Mode = "semantic"
	cfg.SNAT.CustomDetectors = ds
	return cfg
}

// TestCustomDetectorSurvivesInvalidSibling is T1(a).
//
// One unparseable pattern used to discard EVERY custom detector: the manager
// was rebuilt with opts.CustomDetectors = nil, so a user with five patterns
// and one typo silently lost the coverage of all five. A valid pattern must
// survive an invalid sibling — only the offender is dropped.
func TestCustomDetectorSurvivesInvalidSibling(t *testing.T) {
	cfg := cfgWithCustomDetectors(
		config.SNATCustomDetector{Name: "goodone", Pattern: `acme_[a-z0-9]{10}`, Prefix: "acme_"},
		config.SNATCustomDetector{Name: "badone", Pattern: `acme_[a-z0-9`, Prefix: "acme_"}, // unclosed class
		config.SNATCustomDetector{Name: "goodtwo", Pattern: `zeta_[0-9]{6}`, Prefix: "zeta_"},
	)

	mgr, _ := newSecretNATManager(cfg)
	if mgr == nil {
		t.Fatal("newSecretNATManager returned nil manager")
	}

	names := strings.Join(mgr.DetectorNames(), ",")
	for _, want := range []string{"goodone", "goodtwo"} {
		if !strings.Contains(names, want) {
			t.Errorf("valid custom detector %q did not survive an invalid sibling; detectors = %s", want, names)
		}
	}
	if strings.Contains(names, "badone") {
		t.Errorf("invalid custom detector %q was registered; detectors = %s", "badone", names)
	}
}

// TestRejectedCustomDetectorsAreReported is T1(b) at the seam: the daemon must
// carry WHICH pattern failed and why, so the ##snat status surface can name it.
// A warning in the daemon log is not a surface the user reads.
func TestRejectedCustomDetectorsAreReported(t *testing.T) {
	cfg := cfgWithCustomDetectors(
		config.SNATCustomDetector{Name: "goodone", Pattern: `acme_[a-z0-9]{10}`},
		config.SNATCustomDetector{Name: "badone", Pattern: `acme_[a-z0-9`},
	)

	_, rejected := newSecretNATManager(cfg)
	if len(rejected) != 1 {
		t.Fatalf("want exactly 1 rejected detector, got %d: %+v", len(rejected), rejected)
	}
	r := rejected[0]
	if r.Name != "badone" {
		t.Errorf("rejected detector name = %q, want %q", r.Name, "badone")
	}
	if r.Pattern != `acme_[a-z0-9` {
		t.Errorf("rejected detector pattern = %q, want the offending pattern", r.Pattern)
	}
	if r.Reason == "" {
		t.Error("rejected detector carries no reason; a typo must be actionable")
	}
}

// TestAllValidCustomDetectorsReportNothing guards the other direction: a clean
// config must not manufacture a rejection (and so must not put a scary line on
// ##snat status).
func TestAllValidCustomDetectorsReportNothing(t *testing.T) {
	cfg := cfgWithCustomDetectors(
		config.SNATCustomDetector{Name: "goodone", Pattern: `acme_[a-z0-9]{10}`},
	)
	mgr, rejected := newSecretNATManager(cfg)
	if len(rejected) != 0 {
		t.Errorf("clean config reported rejections: %+v", rejected)
	}
	if !strings.Contains(strings.Join(mgr.DetectorNames(), ","), "goodone") {
		t.Errorf("valid detector missing: %v", mgr.DetectorNames())
	}
}

// TestUnnamedCustomDetectorIsReported: a detector with no name is dropped by
// NewDefaultRegistry's `name == ""` guard without any error at all — the same
// "user believes they are covered and is not" defect as an unparseable
// pattern, so it is surfaced the same way.
func TestUnnamedCustomDetectorIsReported(t *testing.T) {
	cfg := cfgWithCustomDetectors(
		config.SNATCustomDetector{Name: "   ", Pattern: `acme_[a-z0-9]{10}`},
	)
	_, rejected := newSecretNATManager(cfg)
	if len(rejected) != 1 {
		t.Fatalf("want 1 rejected detector for an unnamed entry, got %d: %+v", len(rejected), rejected)
	}
	if rejected[0].Reason == "" {
		t.Error("unnamed detector carries no reason")
	}
}
