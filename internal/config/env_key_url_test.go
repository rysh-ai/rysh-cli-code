// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The environment's own key overrides (RYSH_UPSTREAM_API_KEY, RYSH_API_KEY)
// apply only where the URL is not chosen by a file: the URL came from the
// environment too, or it is a first-party / official URL.
func TestEnvKeyOverridesFollowTheURL(t *testing.T) {
	for _, n := range []string{"RYSH_DIR", "RYSH_UPSTREAM_URL", "RYSH_API_URL", "RYSH_PROVIDER", "ANTHROPIC_API_KEY"} {
		t.Setenv(n, "")
	}
	t.Setenv("RYSH_UPSTREAM_API_KEY", "fake-env-upstream")
	t.Setenv("RYSH_API_KEY", "fake-env-provider")
	p := filepath.Join(t.TempDir(), "rysh.config.yaml")
	load := func(body string) Config {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadFrom(p)
	}
	up := func(url, extra string) string {
		return "upstream:\n  enabled: true\n  url: " + url + "\n  api_key: rysh_file_key\n  workspace: 11111111-1111-1111-1111-111111111111\n" + extra
	}
	for _, tc := range []struct{ name, body, want string }{
		{"first-party rysh.ai", up("https://rysh.ai", ""), "fake-env-upstream"},
		{"first-party letchat.ai, trailing slash", up("https://letchat.ai/", ""), "fake-env-upstream"},
		{"file-chosen url", up("https://attacker.invalid", ""), "rysh_file_key"},
		{"lookalike host", up("https://rysh.ai.attacker.invalid", ""), "rysh_file_key"},
		{"first-party url, file-chosen company server", up("https://rysh.ai", "company:\n  id: 22222222-2222-2222-2222-222222222222\n  slug: s\n  server: https://attacker.invalid\n"), "rysh_file_key"},
	} {
		if got := load(tc.body).Upstream.APIKey; got != tc.want {
			t.Errorf("%s: upstream key is %s", tc.name, map[string]string{"fake-env-upstream": "the env key", "rysh_file_key": "the file key", "": "empty"}[got])
		}
	}
	t.Setenv("RYSH_UPSTREAM_URL", "https://dev.example.invalid")
	if got := load(up("https://attacker.invalid", "")).Upstream.APIKey; got != "fake-env-upstream" {
		t.Errorf("url and key both from env: the env key must apply")
	}
	t.Setenv("RYSH_UPSTREAM_URL", "")

	for _, tc := range []struct{ name, body, want string }{
		{"provider, no url", "provider:\n  api_key: sk-file\n", "fake-env-provider"},
		{"provider, official url", "provider:\n  api_url: https://api.anthropic.com\n  api_key: sk-file\n", "fake-env-provider"},
		{"provider, custom url", "provider:\n  api_url: https://attacker.invalid\n  api_key: sk-file\n", "sk-file"},
	} {
		if got := load(tc.body).APIKey; got != tc.want {
			t.Errorf("%s: provider key is %s", tc.name, map[string]string{"fake-env-provider": "the env key", "sk-file": "the file key", "": "empty"}[got])
		}
	}
	t.Setenv("RYSH_API_URL", "https://gateway.example.invalid")
	if got := load("provider:\n  api_url: https://attacker.invalid\n").APIKey; got != "fake-env-provider" {
		t.Errorf("api url and key both from env: the env key must apply")
	}
}

// An exported RYSH_UPSTREAM_API_KEY that is used over a key the file already
// has (the stored one ##upstream connect / rysh connect wrote) says so, by
// name only (channels' review note 1). No warning when only one of them is set.
func TestEnvUpstreamKeyOverStoredKeyWarns(t *testing.T) {
	for _, n := range []string{"RYSH_DIR", "RYSH_UPSTREAM_URL", "RYSH_UPSTREAM_API_KEY"} {
		t.Setenv(n, "")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "rysh.config.yaml")
	body := "upstream:\n  enabled: true\n  url: https://rysh.ai\n  api_key: ${RYSH_UPSTREAM_API_KEY}\n  workspace: 11111111-1111-1111-1111-111111111111\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := StoreUpstreamKey(RyshDirForConfigFile(p), "rysh_stored_fake"); err != nil {
		t.Fatal(err)
	}
	warn := func() string {
		t.Helper()
		var w bytes.Buffer
		if _, err := loadFrom(p, &w); err != nil {
			t.Fatal(err)
		}
		return w.String()
	}
	const line = "RYSH_UPSTREAM_API_KEY is set in the environment and is used instead of the upstream key"
	if got := warn(); strings.Contains(got, "RYSH_UPSTREAM_API_KEY is set") {
		t.Fatalf("warned with no env key: %q", got)
	}
	t.Setenv("RYSH_UPSTREAM_API_KEY", "fake-env-upstream")
	got := warn()
	if !strings.Contains(got, line) {
		t.Fatalf("no override warning; got %q", got)
	}
	for _, v := range []string{"fake-env-upstream", "rysh_stored_fake"} {
		if strings.Contains(got, v) {
			t.Fatalf("the warning prints a key value")
		}
	}
}
