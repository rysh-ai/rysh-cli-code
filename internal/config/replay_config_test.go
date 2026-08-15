// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReplayRetentionFromConfigFile: the [replay] retention / max_bytes seam
// (design 006 §3.1) flows from YAML into the typed ReplayConfig used to bound
// the durable stream.
func TestReplayRetentionFromConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rysh.config.yaml")
	content := "replay:\n  enabled: true\n  retention: \"36h\"\n  max_bytes: \"512MB\"\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFromFile(path, applyDefaults())
	if err != nil {
		t.Fatalf("loadFromFile: %v", err)
	}
	if !cfg.Replay.Enabled {
		t.Fatal("Replay.Enabled = false, want true")
	}
	if cfg.Replay.Retention != 36*time.Hour {
		t.Fatalf("Replay.Retention = %v, want 36h", cfg.Replay.Retention)
	}
	if cfg.Replay.MaxBytes != 512<<20 {
		t.Fatalf("Replay.MaxBytes = %d, want 512MiB", cfg.Replay.MaxBytes)
	}
}

// TestReplayRetentionUnsetStaysZero: with no [replay] limits configured the
// typed fields stay zero, which the replay package maps to its 7d / 1GiB
// defaults — config does not duplicate those constants.
func TestReplayRetentionUnsetStaysZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rysh.config.yaml")
	if err := os.WriteFile(path, []byte("replay:\n  enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFromFile(path, applyDefaults())
	if err != nil {
		t.Fatalf("loadFromFile: %v", err)
	}
	if cfg.Replay.Retention != 0 || cfg.Replay.MaxBytes != 0 {
		t.Fatalf("limits = (%v, %d), want zero (replay-package defaults apply)",
			cfg.Replay.Retention, cfg.Replay.MaxBytes)
	}
}

func TestParseDurationDays(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"7d", 7 * 24 * time.Hour, true},
		{"1.5d", 36 * time.Hour, true},
		{"36h", 36 * time.Hour, true},
		{"", 0, false},
		{"-3h", 0, false},
		{"soon", 0, false},
	}
	for _, c := range cases {
		got, ok := parseDurationDays(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("parseDurationDays(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseByteSize(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1GB", 1 << 30, true},
		{"512MB", 512 << 20, true},
		{"2MiB", 2 << 20, true},
		{"64kb", 64 << 10, true},
		{"1048576", 1 << 20, true},
		{"", 0, false},
		{"-1GB", 0, false},
		{"lots", 0, false},
	}
	for _, c := range cases {
		got, ok := parseByteSize(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("parseByteSize(%q) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
