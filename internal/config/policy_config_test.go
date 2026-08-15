// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPolicyOrgFileFromConfig: `policy.org_file` maps into Config with "~"
// expansion and an absolute result, and stays empty when not configured (the
// no-org-policy path must be indistinguishable from before the feature).
func TestPolicyOrgFileFromConfig(t *testing.T) {
	writeConfig(t, "policy:\n  org_file: /etc/rysh/org-policy.yaml\n")
	cfg, err := loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.Policy.OrgFile != "/etc/rysh/org-policy.yaml" {
		t.Errorf("Policy.OrgFile = %q, want /etc/rysh/org-policy.yaml", cfg.Policy.OrgFile)
	}

	// "~" expands to the (test-scoped) home directory.
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeConfig(t, "policy:\n  org_file: ~/org-policy.yaml\n")
	cfg, err = loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if want := filepath.Join(home, "org-policy.yaml"); cfg.Policy.OrgFile != want {
		t.Errorf("Policy.OrgFile = %q, want %q", cfg.Policy.OrgFile, want)
	}

	// Not configured → empty (project policy only).
	writeConfig(t, "rysh:\n  session_name: no-org\n")
	cfg, err = loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.Policy.OrgFile != "" {
		t.Errorf("Policy.OrgFile = %q, want empty when unconfigured", cfg.Policy.OrgFile)
	}
}

// TestPolicyOrgFileEnvOverride: RYSH_ORG_POLICY wins over the config file.
func TestPolicyOrgFileEnvOverride(t *testing.T) {
	writeConfig(t, "policy:\n  org_file: /etc/rysh/org-policy.yaml\n")
	other := filepath.Join(t.TempDir(), "override.yaml")
	if err := os.WriteFile(other, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RYSH_ORG_POLICY", other)
	cfg, err := loadFrom("", new(strings.Builder))
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if cfg.Policy.OrgFile != other {
		t.Errorf("Policy.OrgFile = %q, want env override %q", cfg.Policy.OrgFile, other)
	}
}
