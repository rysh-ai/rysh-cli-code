// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodBinding = `# rysh company config — Pera Bistro
upstream:
  enabled: true
  url: "https://rysh.ai/"
  api_key: "rysh_abcdef"
  workspace: "32a4e462-3717-4047-9d16-686d659c13de"
company:
  id: "11f48494-a4f2-4a99-9536-f7e5460ba048"
  name: "Pera Bistro"
  slug: "pera-bistro"
  fqdn: "pera-bistro.dns.rysh.ai"
  server: "https://rysh.ai"
rysh:
  session_name: "pera-bistro"
`

func writeBinding(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pera-bistro.rysh.config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadCompanyBinding(t *testing.T) {
	b, err := ReadCompanyBinding(writeBinding(t, goodBinding))
	if err != nil {
		t.Fatalf("ReadCompanyBinding: %v", err)
	}
	if b.URL != "https://rysh.ai" || b.APIKey != "rysh_abcdef" || b.WorkspaceID != "32a4e462-3717-4047-9d16-686d659c13de" {
		t.Fatalf("upstream half wrong: %+v", b)
	}
	if b.Company.ID != "11f48494-a4f2-4a99-9536-f7e5460ba048" || b.Company.Slug != "pera-bistro" || b.Company.Server != "https://rysh.ai" || b.Company.Name != "Pera Bistro" {
		t.Fatalf("company half wrong: %+v", b.Company)
	}
}

func TestReadCompanyBindingRefusesTheQuietFailures(t *testing.T) {
	cases := map[string]string{
		"workspace name where the id belongs": strings.Replace(goodBinding, "32a4e462-3717-4047-9d16-686d659c13de", "company-pera-bistro", 1),
		"no key":                              strings.Replace(goodBinding, `api_key: "rysh_abcdef"`, `api_key: ""`, 1),
		"key of the wrong shape":              strings.Replace(goodBinding, "rysh_abcdef", "sk-live-abc", 1),
		"no company id":                       strings.Replace(goodBinding, `id: "11f48494-a4f2-4a99-9536-f7e5460ba048"`, `id: ""`, 1),
		"no slug":                             strings.Replace(goodBinding, `slug: "pera-bistro"`, `slug: ""`, 1),
		"not a url":                           strings.Replace(goodBinding, `url: "https://rysh.ai/"`, `url: "rysh.ai"`, 1),
		"not yaml":                            "{{{{",
	}
	for name, body := range cases {
		if _, err := ReadCompanyBinding(writeBinding(t, body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := ReadCompanyBinding(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Errorf("missing file: expected an error")
	}
}

func TestReadCompanyBindingDefaultsServerAndName(t *testing.T) {
	body := strings.Replace(strings.Replace(goodBinding, `  server: "https://rysh.ai"`+"\n", "", 1), `  name: "Pera Bistro"`+"\n", "", 1)
	b, err := ReadCompanyBinding(writeBinding(t, body))
	if err != nil {
		t.Fatal(err)
	}
	if b.Company.Server != "https://rysh.ai" || b.Company.Name != "pera-bistro" {
		t.Fatalf("defaults: %+v", b.Company)
	}
}
