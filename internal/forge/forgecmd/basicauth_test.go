// SPDX-License-Identifier: Apache-2.0

package forgecmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/forge"
)

// basicAuthSpec declares HTTP Basic as its only security scheme — the one auth
// mode the runtime implements end to end (InjectAuth → "Authorization: Basic")
// and the store persists (Integration.BasicUserEnv/BasicPassEnv), but which no
// `forge add` flag could reach before T4.
const basicAuthSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "vault", "version": "1.0"},
  "servers": [{"url": "https://vault.example.com"}],
  "components": {"securitySchemes": {"basicAuth": {"type": "http", "scheme": "basic"}}},
  "security": [{"basicAuth": []}],
  "paths": {"/secrets": {"get": {"operationId": "listSecrets", "responses": {"200": {"description": "ok"}}}}}
}`

// TestForgeAdd_BindsBasicAuthEnvFlags is T4 for HTTP Basic: the flags must
// reach the persisted Integration, because that struct is what Manager.enable
// reads to build the runtime Credential.
//
// Before T4 this failed at the flag parser itself — `--basic-user-env` was not
// defined, so `forge add` rejected the command outright. Basic auth was
// therefore configured by hand-editing .rysh/forge/integrations.json, or not
// at all.
//
// The flags bind env-var NAMES, never secret values: the subsystem's standing
// rule (store.go) is that credentials are not persisted, and a literal secret
// on a command line would land in shell history and the process table.
func TestForgeAdd_BindsBasicAuthEnvFlags(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "spec.json"), []byte(basicAuthSpec), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	err := Run(work, []string{
		"add", "vault", "spec.json",
		"--basic-user-env", "VAULT_USER",
		"--basic-pass-env", "VAULT_PASS",
		"--targets", "rysh-toolpack",
	}, &buf)
	if err != nil {
		t.Fatalf("add with basic-auth flags: %v\noutput:\n%s", err, buf.String())
	}

	defs, err := forge.LoadStore(work)
	if err != nil || len(defs) != 1 {
		t.Fatalf("load store: err=%v defs=%v", err, defs)
	}
	if got := defs[0].BasicUserEnv; got != "VAULT_USER" {
		t.Errorf("BasicUserEnv = %q, want %q — the flag did not reach the stored integration", got, "VAULT_USER")
	}
	if got := defs[0].BasicPassEnv; got != "VAULT_PASS" {
		t.Errorf("BasicPassEnv = %q, want %q — the flag did not reach the stored integration", got, "VAULT_PASS")
	}
}

// TestForgeAdd_BasicAuthFlagsStoreNamesNotValues pins the secrets rule. The
// stored file must contain the env-var NAME and must not contain the secret,
// even though the secret is present in the environment at add time.
func TestForgeAdd_BasicAuthFlagsStoreNamesNotValues(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "spec.json"), []byte(basicAuthSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VAULT_USER", "admin")
	t.Setenv("VAULT_PASS", "hunter2-do-not-persist")

	var buf bytes.Buffer
	if err := Run(work, []string{
		"add", "vault", "spec.json",
		"--basic-user-env", "VAULT_USER",
		"--basic-pass-env", "VAULT_PASS",
		"--targets", "rysh-toolpack",
	}, &buf); err != nil {
		t.Fatalf("add: %v\noutput:\n%s", err, buf.String())
	}

	raw, err := os.ReadFile(filepath.Join(work, ".rysh", "forge", "integrations.json"))
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	store := string(raw)
	for _, secret := range []string{"hunter2-do-not-persist", "admin"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("stored integration leaks the secret value %q — only env-var names may be persisted:\n%s", secret, store)
		}
	}
	if !bytes.Contains(raw, []byte("VAULT_PASS")) {
		t.Errorf("stored integration does not record the env-var name VAULT_PASS:\n%s", store)
	}
}
