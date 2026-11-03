// SPDX-License-Identifier: Apache-2.0

package main

// e25_wa_setup_test.go — `rysh setup` and `rysh assistant` collect the
// WhatsApp app_secret (E25, root security task 2026-10-28). The adapter now
// refuses to start without it, so a scaffolded profile that never asked for
// it would be a profile that cannot start. It is a secret: a hidden prompt or
// a ${NAME} reference, never a flag value.

import (
	"bufio"
	"strings"
	"testing"
)

func TestE25WhatsAppCredFieldsIncludeAppSecret(t *testing.T) {
	var found *credField
	for _, f := range channelCredFields("whatsapp") {
		if f.Key == "app_secret" {
			f := f
			found = &f
		}
	}
	if found == nil {
		t.Fatal("channelCredFields(\"whatsapp\") has no app_secret field")
	}
	if found.EnvName != "WHATSAPP_APP_SECRET" {
		t.Errorf("app_secret EnvName = %q, want WHATSAPP_APP_SECRET (a secret, hidden prompt)", found.EnvName)
	}
	if found.Default != "" {
		t.Error("app_secret has a default value")
	}
}

// Headless assistant with no source references ${WHATSAPP_APP_SECRET}.
func TestE25AssistantWhatsAppHeadlessReferencesAppSecret(t *testing.T) {
	fields, values, err := collectAssistantCreds("whatsapp", []string{"--phone", "100000000000001"}, bufio.NewReader(strings.NewReader("")), false)
	if err != nil {
		t.Fatalf("collectAssistantCreds: %v", err)
	}
	for i, f := range fields {
		if f.Key == "app_secret" {
			if values[i] != "${WHATSAPP_APP_SECRET}" {
				t.Errorf("app_secret value = %q, want ${WHATSAPP_APP_SECRET}", values[i])
			}
			return
		}
	}
	t.Fatal("no app_secret field collected")
}

// --app-secret-env NAME is the flag form; it becomes a ${NAME} reference.
func TestE25AssistantWhatsAppAppSecretEnvFlag(t *testing.T) {
	fields, values, err := collectAssistantCreds("whatsapp", []string{"--app-secret-env", "E25_WA_SECRET_NAME"}, bufio.NewReader(strings.NewReader("")), false)
	if err != nil {
		t.Fatalf("collectAssistantCreds: %v", err)
	}
	for i, f := range fields {
		if f.Key == "app_secret" {
			if values[i] != "${E25_WA_SECRET_NAME}" {
				t.Errorf("app_secret value = %q, want ${E25_WA_SECRET_NAME}", values[i])
			}
			return
		}
	}
	t.Fatal("no app_secret field collected")
}

// A secret as a flag value is refused by rysh assistant too (argv is visible
// to every process), and the refusal never echoes the value.
func TestE25AssistantRefusesSecretFlagValues(t *testing.T) {
	for _, tc := range []struct{ channel, flag string }{
		{"whatsapp", "--app-secret"},
		{"whatsapp", "--api-key"},
		{"whatsapp", "--verify-token"},
		{"telegram", "--bot-token"},
	} {
		_, _, err := collectAssistantCreds(tc.channel, []string{tc.flag, e33FakeValue}, bufio.NewReader(strings.NewReader("")), false)
		if err == nil {
			t.Errorf("%s %s <value>: accepted; want a refusal", tc.channel, tc.flag)
			continue
		}
		if !strings.Contains(err.Error(), tc.flag+"-env") {
			t.Errorf("%s %s: err = %v, want it to name %s-env", tc.channel, tc.flag, err, tc.flag)
		}
		if strings.Contains(err.Error(), e33FakeValue) {
			t.Errorf("%s %s: the refusal echoed the value", tc.channel, tc.flag)
		}
	}
}

// rysh setup already refuses --app-secret as a value (LINE added it); pin it
// for WhatsApp.
func TestE25SetupRefusesWhatsAppAppSecretFlag(t *testing.T) {
	setupWorkspace(t)
	_, err := runSetupCaptured(t, []string{"--channel", "whatsapp", "--owner", "15550100", "--app-secret", e33FakeValue},
		assistantEnv{in: bufio.NewReader(strings.NewReader("")), deps: e33Deps(nil, stubAdapter{typ: "whatsapp"})})
	if err == nil || !strings.Contains(err.Error(), "--app-secret-env") {
		t.Fatalf("err = %v, want a refusal naming --app-secret-env", err)
	}
	if strings.Contains(err.Error(), e33FakeValue) {
		t.Error("the refusal echoed the value")
	}
}
