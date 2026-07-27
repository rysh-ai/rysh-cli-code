package config

import (
	"strings"
	"testing"
)

// TestSNATDefaults: SecretNAT is ON by default with semantic mode and no
// display restore.
func TestSNATDefaults(t *testing.T) {
	cfg := applyDefaults()
	if !cfg.SNAT.Enabled {
		t.Error("SNAT.Enabled default = false, want true (on by default)")
	}
	if cfg.SNAT.Mode != "semantic" {
		t.Errorf("SNAT.Mode default = %q, want semantic", cfg.SNAT.Mode)
	}
	if cfg.SNAT.RestoreDisplay {
		t.Error("SNAT.RestoreDisplay default = true, want false")
	}
}

// TestSNATFromFile: the snat section maps into Config, and an explicit
// `enabled: false` overrides the on-by-default (pointer-bool semantics).
func TestSNATFromFile(t *testing.T) {
	writeConfig(t, `
snat:
  enabled: false
  mode: private
  restore_display: true
  mapping_ttl: 30m
  disable_detectors: [google, bearer]
  custom_detectors:
    - { name: acme, pattern: "acme_[A-Za-z0-9]{16}", prefix: "acme_" }
`)
	cfg, err := loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.SNAT.Enabled {
		t.Error("explicit enabled: false was not honored")
	}
	if cfg.SNAT.Mode != "private" {
		t.Errorf("Mode = %q, want private", cfg.SNAT.Mode)
	}
	if !cfg.SNAT.RestoreDisplay {
		t.Error("restore_display: true was not honored")
	}
	if cfg.SNAT.MappingTTL != "30m" {
		t.Errorf("MappingTTL = %q, want 30m", cfg.SNAT.MappingTTL)
	}
	if len(cfg.SNAT.DisableDetectors) != 2 || cfg.SNAT.DisableDetectors[0] != "google" {
		t.Errorf("DisableDetectors = %v", cfg.SNAT.DisableDetectors)
	}
	if len(cfg.SNAT.CustomDetectors) != 1 || cfg.SNAT.CustomDetectors[0].Name != "acme" ||
		cfg.SNAT.CustomDetectors[0].Prefix != "acme_" {
		t.Errorf("CustomDetectors = %+v", cfg.SNAT.CustomDetectors)
	}

	// A config that does not mention snat keeps the on-by-default.
	writeConfig(t, "ui:\n  initial_tabs: 2\n")
	cfg2, err := loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if !cfg2.SNAT.Enabled {
		t.Error("unset snat section must keep the enabled default")
	}
}

// TestSNATEnvOverrides: RYSH_SNAT_* env vars override file/defaults.
func TestSNATEnvOverrides(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("RYSH_SNAT_ENABLED", "false")
	t.Setenv("RYSH_SNAT_MODE", "private")
	t.Setenv("RYSH_SNAT_RESTORE_DISPLAY", "1")
	t.Setenv("RYSH_SNAT_MAPPING_TTL", "1h")
	cfg, err := loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.SNAT.Enabled {
		t.Error("RYSH_SNAT_ENABLED=false not honored")
	}
	if cfg.SNAT.Mode != "private" {
		t.Errorf("Mode = %q, want private", cfg.SNAT.Mode)
	}
	if !cfg.SNAT.RestoreDisplay {
		t.Error("RYSH_SNAT_RESTORE_DISPLAY=1 not honored")
	}
	if cfg.SNAT.MappingTTL != "1h" {
		t.Errorf("MappingTTL = %q, want 1h", cfg.SNAT.MappingTTL)
	}
}
