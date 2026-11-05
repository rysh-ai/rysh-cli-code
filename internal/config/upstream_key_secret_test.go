// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The company connect key lives in the .rysh/secrets tier, never in plain
// YAML (design 037, sprint 0.2.15 CD2): rysh.config.yaml carries the
// ${RYSH_UPSTREAM_API_KEY} reference and the loader resolves it.

func TestStoreUpstreamKeyWritesTheSecretTierOwnerOnly(t *testing.T) {
	ryshDir := filepath.Join(t.TempDir(), ".rysh")
	// A secrets tree left too open by an older writer is tightened.
	if err := os.MkdirAll(filepath.Join(ryshDir, "secrets", "default"), 0o755); err != nil {
		t.Fatal(err)
	}
	ref, path, err := StoreUpstreamKey(ryshDir, "rysh_first")
	if err != nil {
		t.Fatalf("StoreUpstreamKey: %v", err)
	}
	if ref != "${RYSH_UPSTREAM_API_KEY}" {
		t.Fatalf("ref = %q", ref)
	}
	if want := filepath.Join(ryshDir, "secrets", "default", "RYSH_UPSTREAM_API_KEY"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("path is not absolute: %q", path)
	}
	assertMode(t, path, 0o600)
	assertMode(t, filepath.Join(ryshDir, "secrets"), 0o700)
	assertMode(t, filepath.Join(ryshDir, "secrets", "default"), 0o700)

	// A second connect replaces the value in place, with no temp file left.
	if _, _, err := StoreUpstreamKey(ryshDir, "rysh_second"); err != nil {
		t.Fatalf("second StoreUpstreamKey: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "rysh_second" {
		t.Fatal("the second key did not replace the first")
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("secrets/default holds %d entries, want only the key file", len(entries))
	}
	if _, _, err := StoreUpstreamKey(ryshDir, "  "); err == nil {
		t.Fatal("an empty key must be refused")
	}
}

func TestLoadResolvesTheUpstreamKeyReference(t *testing.T) {
	t.Setenv("RYSH_UPSTREAM_API_KEY", "")
	t.Setenv("RYSH_DIR", "")
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "rysh.config.yaml")
	body := "upstream:\n  enabled: true\n  url: https://rysh.ai\n  api_key: ${RYSH_UPSTREAM_API_KEY}\n  workspace: 32a4e462-3717-4047-9d16-686d659c13de\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Unresolved: the key is empty, never the literal reference.
	if got := LoadFrom(cfgPath).Upstream.APIKey; got != "" {
		t.Fatalf("an unresolved reference must load as empty, got %q", got)
	}
	if _, _, err := StoreUpstreamKey(filepath.Join(dir, ".rysh"), "rysh_stored"); err != nil {
		t.Fatal(err)
	}
	if got := LoadFrom(cfgPath).Upstream.APIKey; got != "rysh_stored" {
		t.Fatalf("Upstream.APIKey = %q, want the stored key", got)
	}
}

func TestReadCompanyBindingResolvesAKeyReference(t *testing.T) {
	t.Setenv("RYSH_UPSTREAM_API_KEY", "")
	p := writeBinding(t, strings.Replace(goodBinding, `api_key: "rysh_abcdef"`, `api_key: "${RYSH_UPSTREAM_API_KEY}"`, 1))
	if _, err := ReadCompanyBinding(p); err == nil || !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("an unresolved reference must be refused like a missing key, got %v", err)
	}
	if _, _, err := StoreUpstreamKey(filepath.Join(filepath.Dir(p), ".rysh"), "rysh_fromsecret"); err != nil {
		t.Fatal(err)
	}
	b, err := ReadCompanyBinding(p)
	if err != nil {
		t.Fatalf("ReadCompanyBinding: %v", err)
	}
	if b.APIKey != "rysh_fromsecret" || !b.KeyFromSecret {
		t.Fatalf("binding key = %q fromSecret=%v, want the stored key", b.APIKey, b.KeyFromSecret)
	}
	// A plain key is still read (a dashboard download), and marked as plain.
	b, err = ReadCompanyBinding(writeBinding(t, goodBinding))
	if err != nil || b.APIKey != "rysh_abcdef" || b.KeyFromSecret {
		t.Fatalf("plain binding: %+v %v", b, err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode %o, want %o", filepath.Base(path), got, want)
	}
}

// WriteCompanyBinding is what both `rysh connect` and `##config load` write:
// the key into the secrets tier, the reference + company block into the YAML.
// Loading a file that still holds a plain key migrates it in place, even when
// the file loaded IS the session's own config.
func TestWriteCompanyBindingMigratesAPlainKeyInPlace(t *testing.T) {
	t.Setenv("RYSH_UPSTREAM_API_KEY", "")
	t.Setenv("RYSH_DIR", "")
	p := writeBinding(t, goodBinding) // plain api_key, as downloaded
	b, err := ReadCompanyBinding(p)
	if err != nil {
		t.Fatal(err)
	}
	ryshDir := filepath.Join(filepath.Dir(p), ".rysh")
	secretsPath, err := WriteCompanyBinding(p, ryshDir, b)
	if err != nil {
		t.Fatalf("WriteCompanyBinding: %v", err)
	}
	raw, _ := os.ReadFile(p)
	if strings.Contains(string(raw), "rysh_abcdef") || !strings.Contains(string(raw), "${RYSH_UPSTREAM_API_KEY}") {
		t.Fatalf("the plain key was not replaced by the reference:\n%s", raw)
	}
	if !strings.Contains(string(raw), "Pera Bistro") || !strings.Contains(string(raw), "session_name") {
		t.Fatalf("unrelated keys were lost:\n%s", raw)
	}
	assertMode(t, p, 0o600)
	assertMode(t, secretsPath, 0o600)
	again, err := ReadCompanyBinding(p)
	if err != nil || again.APIKey != "rysh_abcdef" || !again.KeyFromSecret {
		t.Fatalf("the migrated file does not read back: %+v %v", again, err)
	}
	// Idempotent: writing the resolved binding again keeps the reference.
	if _, err := WriteCompanyBinding(p, ryshDir, again); err != nil {
		t.Fatal(err)
	}
	if raw2, _ := os.ReadFile(p); string(raw2) != string(raw) {
		t.Fatalf("a second write changed the file:\n%s", raw2)
	}
}

// security-lead ledger 167 X1: upstream.api_key accepts a literal or EXACTLY
// ${RYSH_UPSTREAM_API_KEY}. Any other reference would let a crafted config
// (an attacker's url + api_key: ${SOME_SECRET}) send that secret upstream, so
// it is refused, and nothing it names is ever resolved.
func TestUpstreamKeyRefusesEveryOtherReference(t *testing.T) {
	t.Setenv("RYSH_UPSTREAM_API_KEY", "")
	t.Setenv("RYSH_DIR", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake-env-value-for-x1")
	dir := t.TempDir()
	ryshDir := filepath.Join(dir, ".rysh")
	if _, err := WriteSecret(ryshDir, "other", "STORED_TOKEN", "fake-scope-value-for-x1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := StoreUpstreamKey(ryshDir, "rysh_real"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{
		"${AWS_SECRET_ACCESS_KEY}",
		"${STORED_TOKEN}",
		"${AWS_SECRET_ACCESS_KEY}${STORED_TOKEN}",
		"${RYSH_UPSTREAM_API_KEY}x",
		"x${RYSH_UPSTREAM_API_KEY}",
		"${RYSH_UPSTREAM_API_KEY}${STORED_TOKEN}",
		"rysh_${STORED_TOKEN}",
	} {
		body := "upstream:\n  enabled: true\n  url: https://attacker.invalid\n  api_key: \"" + v + "\"\n  workspace: 32a4e462-3717-4047-9d16-686d659c13de\n" +
			"company:\n  id: 11f48494-a4f2-4a99-9536-f7e5460ba048\n  slug: pera-bistro\n"
		cfgPath := filepath.Join(dir, "rysh.config.yaml")
		if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := LoadFrom(cfgPath).Upstream.APIKey; got != "" {
			t.Errorf("api_key %s loaded as a non-empty key", v)
		}
		_, err := ReadCompanyBinding(cfgPath)
		if err == nil || !strings.Contains(err.Error(), "upstream.api_key") {
			t.Errorf("api_key %s: ReadCompanyBinding must refuse naming the field, got %v", v, err)
		}
		if err != nil && (strings.Contains(err.Error(), "fake-env-value") || strings.Contains(err.Error(), "fake-scope-value") || strings.Contains(err.Error(), "rysh_real")) {
			t.Errorf("api_key %s: the refusal carries a secret value", v)
		}
	}
}

// The exact reference resolves only from secrets/default beside the file,
// then from the environment variable of that one name.
func TestUpstreamKeyReferenceResolvesFromDefaultScopeOnly(t *testing.T) {
	t.Setenv("RYSH_UPSTREAM_API_KEY", "")
	t.Setenv("RYSH_DIR", "")
	dir := t.TempDir()
	ryshDir := filepath.Join(dir, ".rysh")
	cfgPath := filepath.Join(dir, "rysh.config.yaml")
	body := "upstream:\n  enabled: true\n  url: https://rysh.ai\n  api_key: ${RYSH_UPSTREAM_API_KEY}\n  workspace: 32a4e462-3717-4047-9d16-686d659c13de\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteSecret(ryshDir, "other", UpstreamKeySecretName, "rysh_wrong_scope"); err != nil {
		t.Fatal(err)
	}
	if got := LoadFrom(cfgPath).Upstream.APIKey; got != "" {
		t.Fatalf("resolved from a non-default scope: %q", got)
	}
	t.Setenv("RYSH_UPSTREAM_API_KEY", "rysh_from_env")
	if got := LoadFrom(cfgPath).Upstream.APIKey; got != "rysh_from_env" {
		t.Fatalf("the env fallback of the one name: %q", got)
	}
	t.Setenv("RYSH_UPSTREAM_API_KEY", "")
	if _, _, err := StoreUpstreamKey(ryshDir, "rysh_default"); err != nil {
		t.Fatal(err)
	}
	if got := LoadFrom(cfgPath).Upstream.APIKey; got != "rysh_default" {
		t.Fatalf("Upstream.APIKey = %q, want the default-scope key", got)
	}
}

// ledger 167 X2: the secrets tree is never written through a symlink.
func TestWriteSecretRefusesASymlinkedSecretsTree(t *testing.T) {
	elsewhere := t.TempDir()
	for _, link := range []string{"secrets", filepath.Join("secrets", "default")} {
		ryshDir := filepath.Join(t.TempDir(), ".rysh")
		if err := os.MkdirAll(filepath.Dir(filepath.Join(ryshDir, link)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, filepath.Join(ryshDir, link)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := StoreUpstreamKey(ryshDir, "rysh_key"); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("%s as a symlink: want a refusal naming the symlink, got %v", link, err)
		}
		if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
			t.Fatalf("%s as a symlink: %d file(s) written through it", link, len(entries))
		}
	}
}
