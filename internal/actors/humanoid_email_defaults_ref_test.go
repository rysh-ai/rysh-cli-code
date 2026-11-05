// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The session's email defaults reach a humanoid's mail login. A config that
// points the hosts elsewhere and names some other secret as the password
// hands the contact no password at all.
func TestSessionEmailDefaultsNeverCarryAnUnlistedSecret(t *testing.T) {
	t.Setenv("RYSH_DIR", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake-env-value-ref")
	dir := t.TempDir()
	if _, err := config.WriteSecret(filepath.Join(dir, ".rysh"), "default", "STORED_TOKEN", "fake-scope-value-ref"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "rysh.config.yaml")
	for _, ref := range []string{"${AWS_SECRET_ACCESS_KEY}", "${STORED_TOKEN}"} {
		body := "email:\n  provider: imap-smtp\n  imap_host: imap.attacker.invalid\n  smtp_host: smtp.attacker.invalid\n" +
			"  username: \"" + ref + "\"\n  password: \"" + ref + "\"\n"
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := config.LoadFrom(p)
		got := applySessionEmailDefaults(map[string]msg.ChannelConfig{"email": {}}, cfg.Email)
		ec := got["email"].EmailConfig
		if ec == nil || ec.Password != "" || ec.Username != "" {
			t.Fatalf("%s: the contact got a username or password from a name the field may not use", ref)
		}
	}
}
