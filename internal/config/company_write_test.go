// SPDX-License-Identifier: Apache-2.0

package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The company workspace file `rysh connect` writes (design 037): the upstream
// connection, the company block and the session name land in one file, every
// unrelated key survives, and the loader reads them back.
func TestCompanyWorkspaceConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rysh.config.yaml")
	seed := "# keep me\nprovider:\n  name: anthropic # keep this comment too\nui:\n  shell: zsh\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetUpstreamConnection(path, "https://letchat.ai", "rysh_key123", "11111111-1111-1111-1111-111111111111"); err != nil {
		t.Fatalf("SetUpstreamConnection: %v", err)
	}
	company := CompanyConfig{ID: "22222222-2222-2222-2222-222222222222", Name: "Modern Bistro", Slug: "firat", FQDN: "firat.dns.rysh.ai", Server: "https://letchat.ai/"}
	if err := SetCompanyBlock(path, company); err != nil {
		t.Fatalf("SetCompanyBlock: %v", err)
	}
	if err := SetSessionName(path, "firat"); err != nil {
		t.Fatalf("SetSessionName: %v", err)
	}

	raw, _ := os.ReadFile(path)
	for _, want := range []string{"# keep me", "keep this comment too", "shell: zsh", "session_name: firat", "slug: firat", "server: https://letchat.ai\n"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("written config lacks %q:\n%s", want, raw)
		}
	}

	cfg, err := loadFrom(path, io.Discard)
	if err != nil {
		t.Fatalf("loadFrom: %v", err)
	}
	if !cfg.Company.Enabled() || cfg.Company.ID != company.ID || cfg.Company.Slug != "firat" || cfg.Company.FQDN != company.FQDN || cfg.Company.Server != "https://letchat.ai" {
		t.Fatalf("company block did not round-trip: %+v", cfg.Company)
	}
	if !cfg.Upstream.Enabled || cfg.Upstream.URL != "https://letchat.ai" || cfg.Upstream.APIKey != "rysh_key123" || cfg.Upstream.Workspace != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("upstream block did not round-trip: %+v", cfg.Upstream)
	}
	if cfg.SessionName != "firat" {
		t.Fatalf("session name: %q", cfg.SessionName)
	}

	// Idempotent: writing the same block again changes nothing.
	before, _ := os.ReadFile(path)
	if err := SetCompanyBlock(path, company); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("second write changed the file:\n%s\n---\n%s", before, after)
	}
}

func TestSetCompanyBlockRejectsIncompleteAndNonMapping(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rysh.config.yaml")
	if err := os.WriteFile(path, []byte("company: nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetCompanyBlock(path, CompanyConfig{ID: "x"}); err == nil {
		t.Fatal("a company block without a slug must be refused")
	}
	if err := SetCompanyBlock(path, CompanyConfig{ID: "x", Slug: "y"}); err == nil || !strings.Contains(err.Error(), "not a mapping") {
		t.Fatalf("a scalar `company` key must be refused, got %v", err)
	}
	if (CompanyConfig{}).Enabled() {
		t.Fatal("an empty company block must not count as bound")
	}
}
