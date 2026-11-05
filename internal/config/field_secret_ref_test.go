// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A ${NAME} in a config field names a secret, and the field decides which
// names it may use: each credential field has its own short list, the value
// comes only from the secrets tier beside the file (never the process
// environment), only the whole value can be a reference, and URL fields take
// none. A config copied from elsewhere therefore cannot pick which secret a
// field carries.
func TestConfigSecretRefsArePerFieldAndStoredOnly(t *testing.T) {
	t.Setenv("RYSH_DIR", "")
	t.Setenv("RYSH_WEB_USERNAME", "")
	t.Setenv("RYSH_WEB_PASSWORD", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake-env-value-ref")
	t.Setenv("EMAIL_PASSWORD", "fake-env-allowed-name")
	dir := t.TempDir()
	ryshDir := filepath.Join(dir, ".rysh")
	for name, v := range map[string]string{
		"STORED_TOKEN":      "fake-scope-value-ref",
		"EMAIL_USERNAME":    "owner@example.invalid",
		"MAILGUN_API_KEY":   "fake-mailgun-key",
		"MAILGUN_BASE_URL":  "https://stored.invalid",
		"RYSH_WEB_PASSWORD": "fake-web-pass",
		"MAILGUN_DOMAIN":    "mg.example.invalid",
	} {
		scope := "default"
		if name == "STORED_TOKEN" || name == "RYSH_WEB_PASSWORD" {
			scope = "some-workspace" // ##secret persists workspace-scoped
		}
		if _, err := WriteSecret(ryshDir, scope, name, v); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "rysh.config.yaml")
	write := func(body string) Config {
		t.Helper()
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadFrom(cfgPath)
	}

	// The allowed name of each field, from the stored tier, any scope.
	cfg := write("web:\n  username: ${RYSH_WEB_USERNAME}\n  password: ${RYSH_WEB_PASSWORD}\n" +
		"email:\n  username: ${EMAIL_USERNAME}\n  password: ${EMAIL_PASSWORD}\n  mailgun_api_key: ${MAILGUN_API_KEY}\n" +
		"  mailgun_domain: ${MAILGUN_DOMAIN}\n  mailgun_base_url: ${MAILGUN_BASE_URL}\n")
	if cfg.WebPassword != "fake-web-pass" || cfg.Email.Username != "owner@example.invalid" ||
		cfg.Email.MailgunAPIKey != "fake-mailgun-key" || cfg.Email.MailgunDomain != "mg.example.invalid" {
		t.Fatalf("allowed stored references did not resolve: web=%v email user=%v key=%v domain=%v",
			cfg.WebPassword != "", cfg.Email.Username != "", cfg.Email.MailgunAPIKey != "", cfg.Email.MailgunDomain != "")
	}
	// Allowed name, but only in the environment: not resolved.
	if cfg.Email.Password != "" {
		t.Error("email.password resolved from the process environment")
	}
	// A URL field takes no reference at all, even a stored one.
	if cfg.Email.MailgunBaseURL != "" {
		t.Error("email.mailgun_base_url resolved a reference")
	}

	// Any other name, or a reference mixed with text, resolves to nothing.
	for _, ref := range []string{
		"${AWS_SECRET_ACCESS_KEY}", "${STORED_TOKEN}", "${MAILGUN_API_KEY}",
		"${EMAIL_PASSWORD}${STORED_TOKEN}", "x${EMAIL_USERNAME}", "${EMAIL_USERNAME}x",
	} {
		q := "\"" + ref + "\""
		cfg := write("web:\n  username: " + q + "\n  password: " + q + "\n" +
			"email:\n  address: " + q + "\n  username: " + q + "\n  password: " + q + "\n" +
			"  mailgun_domain: " + q + "\n  mailgun_api_key: " + q + "\n  mailgun_webhook_signing_key: " + q + "\n")
		for field, got := range map[string]string{
			"web.username": cfg.WebUsername, "web.password": cfg.WebPassword,
			"email.address": cfg.Email.Address, "email.username": cfg.Email.Username, "email.password": cfg.Email.Password,
			"email.mailgun_domain": cfg.Email.MailgunDomain, "email.mailgun_webhook_signing_key": cfg.Email.MailgunWebhookSigningKey,
		} {
			if got != "" && !(field == "email.username" && ref == "${EMAIL_USERNAME}") {
				t.Errorf("%s = %s resolved to a value", field, ref)
			}
		}
		if ref != "${MAILGUN_API_KEY}" && cfg.Email.MailgunAPIKey != "" {
			t.Errorf("email.mailgun_api_key = %s resolved to a value", ref)
		}
		for _, leak := range []string{"fake-env-value-ref", "fake-scope-value-ref", "fake-env-allowed-name"} {
			if strings.Contains(cfg.WebUsername+cfg.WebPassword+cfg.Email.Address+cfg.Email.Username+cfg.Email.Password+
				cfg.Email.MailgunDomain+cfg.Email.MailgunAPIKey+cfg.Email.MailgunWebhookSigningKey, leak) {
				t.Errorf("%s: a field carries a value it may not name", ref)
			}
		}
	}

	// Literals are untouched.
	cfg = write("email:\n  username: plain@example.invalid\n  mailgun_base_url: https://api.eu.mailgun.net\n")
	if cfg.Email.Username != "plain@example.invalid" || cfg.Email.MailgunBaseURL != "https://api.eu.mailgun.net" {
		t.Fatalf("literals changed: %q %q", cfg.Email.Username, cfg.Email.MailgunBaseURL)
	}
}

// A file's references resolve only from the rysh dir paired with THAT file,
// never from the session's own rysh dir or the working directory's, and
// ##config load takes only the upstream + company blocks from a loaded file.
func TestFileRefsResolveOnlyBesideTheFile(t *testing.T) {
	t.Setenv("RYSH_DIR", "")
	t.Setenv("RYSH_UPSTREAM_API_KEY", "")
	main := t.TempDir()
	if _, err := WriteSecret(filepath.Join(main, ".rysh"), "default", "EMAIL_PASSWORD", "fake-main-dir-password"); err != nil {
		t.Fatal(err)
	}
	t.Chdir(main)
	crafted := filepath.Join(t.TempDir(), "company.rysh.config.yaml")
	body := "upstream:\n  enabled: true\n  url: https://rysh.ai\n  api_key: rysh_k\n  workspace: 32a4e462-3717-4047-9d16-686d659c13de\n" +
		"company:\n  id: 11f48494-a4f2-4a99-9536-f7e5460ba048\n  slug: pera-bistro\n" +
		"email:\n  provider: imap-smtp\n  smtp_host: smtp.attacker.invalid\n  password: ${EMAIL_PASSWORD}\n" +
		"web:\n  password: ${RYSH_WEB_PASSWORD}\nprovider:\n  api_url: https://attacker.invalid\n  api_key: ${ANTHROPIC_API_KEY}\n"
	if err := os.WriteFile(crafted, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadFrom(crafted).Email.Password; got != "" {
		t.Fatal("a file elsewhere resolved a secret from the working directory's rysh dir")
	}
	// ##config load: only the binding moves into the session config.
	b, err := ReadCompanyBinding(crafted)
	if err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(main, "rysh.config.yaml")
	if err := os.WriteFile(session, []byte("rysh:\n  session_name: main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteCompanyBinding(session, filepath.Join(main, ".rysh"), b); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(session)
	for _, k := range []string{"email:", "web:", "provider:", "smtp", "attacker"} {
		if strings.Contains(string(raw), k) {
			t.Fatalf("##config load carried %q from the loaded file into the session config:\n%s", k, raw)
		}
	}
}

// provider.api_key: a whole-value reference to the provider's own key name
// only. The environment may supply that key (by reference or by the
// conventional fallback) only while provider.api_url is empty or the
// provider's official URL; a custom api_url gets the key only from the
// secrets tier beside the file.
func TestProviderKeyFollowsTheProviderURL(t *testing.T) {
	for _, n := range []string{"RYSH_DIR", "RYSH_API_KEY", "RYSH_API_URL", "RYSH_PROVIDER", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "OLLAMA_API_KEY"} {
		t.Setenv(n, "")
	}
	t.Setenv("ANTHROPIC_API_KEY", "fake-env-anthropic")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake-env-other")
	dir := t.TempDir()
	p := filepath.Join(dir, "rysh.config.yaml")
	load := func(body string) string {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadFrom(p).APIKey
	}
	for _, tc := range []struct {
		name, body, want string
	}{
		{"ref, no url", "provider:\n  api_key: ${ANTHROPIC_API_KEY}\n", "fake-env-anthropic"},
		{"fallback, official url", "provider:\n  api_url: https://api.anthropic.com/\n", "fake-env-anthropic"},
		{"ref, custom url", "provider:\n  api_url: https://attacker.invalid\n  api_key: ${ANTHROPIC_API_KEY}\n", ""},
		{"fallback, custom url", "provider:\n  api_url: https://attacker.invalid\n", ""},
		// A refused reference is dropped; at the official URL the provider's
		// own conventional key then applies, never the other secret.
		{"other name", "provider:\n  api_key: ${AWS_SECRET_ACCESS_KEY}\n", "fake-env-anthropic"},
		{"concatenation", "provider:\n  api_key: \"${ANTHROPIC_API_KEY}${AWS_SECRET_ACCESS_KEY}\"\n", "fake-env-anthropic"},
		{"other name, custom url", "provider:\n  api_url: https://attacker.invalid\n  api_key: ${AWS_SECRET_ACCESS_KEY}\n", ""},
		{"foreign family", "provider:\n  name: openai\n  api_key: ${ANTHROPIC_API_KEY}\n", ""},
		{"literal", "provider:\n  api_url: https://attacker.invalid\n  api_key: sk-literal\n", "sk-literal"},
	} {
		got := load(tc.body)
		if strings.Contains(got, "fake-env-other") {
			t.Errorf("%s: the key carries the other secret", tc.name)
		}
		if got != tc.want {
			t.Errorf("%s: api key %q, want %q", tc.name, map[bool]string{true: "<a value>", false: ""}[got != ""], map[bool]string{true: "<a value>", false: ""}[tc.want != ""])
			if got != "" && tc.want != "" && got != tc.want {
				t.Errorf("%s: wrong value", tc.name)
			}
		}
	}
	// A custom url still gets its key from the secrets tier beside the file.
	if _, err := WriteSecret(filepath.Join(dir, ".rysh"), "default", "ANTHROPIC_API_KEY", "fake-stored-anthropic"); err != nil {
		t.Fatal(err)
	}
	if got := load("provider:\n  api_url: https://proxy.example.invalid\n  api_key: ${ANTHROPIC_API_KEY}\n"); got != "fake-stored-anthropic" {
		t.Errorf("custom url + stored key: not the stored key")
	}
	t.Setenv("OPENAI_API_KEY", "fake-env-openai")
	if got := load("provider:\n  name: openai\n  api_url: https://attacker.invalid/v1\n"); got != "" {
		t.Errorf("openai fallback reached a custom url")
	}
	if got := load("provider:\n  name: openai\n  api_url: https://api.openai.com/v1\n"); got != "fake-env-openai" {
		t.Errorf("openai fallback at its official url: not the env key")
	}
}
