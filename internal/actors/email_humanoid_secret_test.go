// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// closedLoopbackPort reserves and frees a loopback port so a later dial to it is
// refused instantly (no network, no timeout).
func closedLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// newSecretStoreOverBus stands up a real embedded NATS + JetStream session
// secret bucket (the exact production path) and returns a secretStore over it.
// It skips the test if embedded NATS cannot start in this environment.
func newSecretStoreOverBus(t *testing.T) *namedStore {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	b, err := bus.New(bus.Config{
		Mode:        "embedded",
		SessionName: "verify-email-secrets",
		DataDir:     t.TempDir(),
		Port:        port,
	})
	if err != nil {
		t.Skipf("embedded NATS unavailable: %v", err)
	}
	t.Cleanup(b.Close)
	kv := b.SecretKV()
	if kv == nil {
		t.Skip("secret KV unavailable")
	}
	return newSecretStore(kv, nil)
}

// TestEmailHumanoidTabSecretsEndToEnd verifies the shipped email humanoid
// (examples/humanoids/email-manager.md) picks up a TAB's secrets through every
// resolution tier, and that a different tab cannot see them. It drives the real
// code path: secretStore -> parseHumanoidFile -> resolved EmailChannelConfig.
//
// It does not connect to a live IMAP/SMTP server (that needs real credentials);
// it proves the secret -> humanoid wiring, which is what tab scoping governs.
func TestEmailHumanoidTabSecretsEndToEnd(t *testing.T) {
	exampleAbs, err := filepath.Abs(filepath.Join("..", "..", "examples", "humanoids", "email-manager.md"))
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	if _, err := os.Stat(exampleAbs); err != nil {
		t.Skipf("shipped email humanoid skill not found: %v", err)
	}

	// The environment tier is a global fallback; clear the email vars so the
	// cross-tab isolation assertions are deterministic regardless of the host env.
	for _, k := range []string{"EMAIL_ADDRESS", "EMAIL_USERNAME", "EMAIL_PASSWORD"} {
		os.Unsetenv(k)
	}

	const (
		tabA = "tab-aaaaaaaa-1111-2222-3333-444444444444"
		tabB = "tab-bbbbbbbb-5555-6666-7777-888888888888"
		addr = "alice@example.com"
		user = "alice@example.com"
		pass = "app-pass-abcd-1234"
	)
	secrets := map[string]string{
		"EMAIL_ADDRESS":  addr,
		"EMAIL_USERNAME": user,
		"EMAIL_PASSWORD": pass,
	}

	parseFor := func(t *testing.T, store *namedStore, scope string) *msg.EmailChannelConfig {
		t.Helper()
		def, err := parseHumanoidFile(exampleAbs, store.expandFunc(scope))
		if err != nil {
			t.Fatalf("parse humanoid: %v", err)
		}
		cc, ok := def.Contacts["email"]
		if !ok || cc.EmailConfig == nil {
			t.Fatalf("email contact missing in parsed humanoid: %+v", def.Contacts)
		}
		return cc.EmailConfig
	}
	assertResolved := func(t *testing.T, ec *msg.EmailChannelConfig) {
		t.Helper()
		if ec.Address != addr || ec.Username != user || ec.Password != pass {
			t.Fatalf("tab secrets not resolved into email config: addr=%q user=%q pass=%q", ec.Address, ec.Username, ec.Password)
		}
		// Static (non-placeholder) fields are untouched.
		if ec.IMAPHost != "imap.gmail.com" || ec.SMTPHost != "smtp.gmail.com" {
			t.Fatalf("static email config altered: imap=%q smtp=%q", ec.IMAPHost, ec.SMTPHost)
		}
	}
	assertIsolated := func(t *testing.T, ec *msg.EmailChannelConfig) {
		t.Helper()
		// A different tab must not resolve tab A's secrets — placeholders stay literal.
		if ec.Address != "${EMAIL_ADDRESS}" || ec.Username != "${EMAIL_USERNAME}" || ec.Password != "${EMAIL_PASSWORD}" {
			t.Fatalf("secret leaked across tabs: addr=%q user=%q pass=%q", ec.Address, ec.Username, ec.Password)
		}
	}

	// Tier 1 — runtime `##secret new` (session JetStream KV) over a real bus.
	t.Run("session_kv", func(t *testing.T) {
		store := newSecretStoreOverBus(t)
		for k, v := range secrets {
			if err := store.Set(tabA, k, v, false); err != nil {
				t.Fatalf("set %s: %v", k, err)
			}
		}
		assertResolved(t, parseFor(t, store, tabA))
		assertIsolated(t, parseFor(t, store, tabB))
	})

	// Tier 2 — persisted `--persist` files under .rysh/secrets/<tab>/.
	t.Run("persist", func(t *testing.T) {
		t.Chdir(t.TempDir())
		store := newSecretStore(nil, nil)
		for k, v := range secrets {
			if err := store.Set(tabA, k, v, true); err != nil {
				t.Fatalf("persist %s: %v", k, err)
			}
		}
		assertResolved(t, parseFor(t, store, tabA))
		assertIsolated(t, parseFor(t, store, tabB))
	})

	// Tier 3 — per-tab config secrets (rysh.config.yaml [secrets].<tab>).
	t.Run("config", func(t *testing.T) {
		t.Chdir(t.TempDir()) // isolate from any ambient .rysh/secrets
		store := newSecretStore(nil, map[string]map[string]string{
			tabA: {"EMAIL_ADDRESS": addr, "EMAIL_USERNAME": user, "EMAIL_PASSWORD": pass},
		})
		assertResolved(t, parseFor(t, store, tabA))
		assertIsolated(t, parseFor(t, store, tabB))
	})

	// Closes the loop to the actual EmailAdapter: the resolved tab secrets must
	// reach the IMAP connection attempt. Pointing imap_host at a refused loopback
	// port keeps this network-free and instant.
	t.Run("adapter_consumes_resolved_config", func(t *testing.T) {
		t.Chdir(t.TempDir())
		store := newSecretStore(nil, map[string]map[string]string{
			tabA: {"EMAIL_ADDRESS": addr, "EMAIL_USERNAME": user, "EMAIL_PASSWORD": pass},
		})
		ec := parseFor(t, store, tabA)
		assertResolved(t, ec)

		// Redirect IMAP to a closed loopback port (resolved creds, no real server).
		ec.IMAPHost = "127.0.0.1"
		ec.IMAPPort = closedLoopbackPort(t)
		adapter := channels.NewEmailAdapter(msg.ChannelConfig{EmailType: "email", EmailConfig: ec})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := adapter.Start(ctx)
		if err == nil {
			_ = adapter.Stop()
			t.Fatalf("expected the IMAP dial to a closed port to fail")
		}
		// Got past the required-field checks (username/password present) and
		// attempted the connection — proving the resolved secrets reached the
		// adapter. A missing secret would instead fail with "is required".
		if !strings.Contains(err.Error(), "IMAP connection failed") {
			t.Fatalf("expected a connection failure with resolved creds, got: %v", err)
		}

		// Sanity: an empty password (the failure mode if a secret did NOT resolve)
		// is rejected up front, confirming the adapter reads the password field.
		ec.Password = ""
		empty := channels.NewEmailAdapter(msg.ChannelConfig{EmailType: "email", EmailConfig: ec})
		if err := empty.Start(ctx); err == nil || !strings.Contains(err.Error(), "password is required") {
			t.Fatalf("expected 'password is required' for an empty password, got: %v", err)
		}
	})
}
