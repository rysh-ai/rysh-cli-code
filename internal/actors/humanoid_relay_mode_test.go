// SPDX-License-Identifier: Apache-2.0

package actors

// Regression tests for the `mode: relay` config trap (design 017, gap 1):
// the WhatsApp adapter and the upstream credential fetch key exclusively off
// ChannelConfig.Relay, but the documented alias `mode: relay` was never read —
// a humanoid configured with it silently ran DIRECT mode, pulling the Graph
// token down to the very laptop relay mode exists to keep it away from. The
// parser now folds `mode:` into Relay at load and fails loudly on typos and
// conflicts, mirroring how agent_skillfile.go rejects an `isolation:` typo.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeHumanoidSkillFile writes content to <dir>/SKILL.md and returns its path,
// so tests exercise the real parseHumanoidFile entry point (probe included).
func writeHumanoidSkillFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWhatsAppModeRelayAliasSetsRelay(t *testing.T) {
	def, err := parseHumanoidFile(writeHumanoidSkillFile(t,
		"---\nname: concierge\ncontacts:\n  whatsapp:\n"+
			"    enabled: true\n"+
			"    mode: relay\n"+
			"    governance: human\n"+
			"---\nbody\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	wa := def.Contacts["whatsapp"]
	if !wa.Relay {
		t.Fatal("mode: relay did not set Relay — the humanoid would silently run direct mode")
	}
	if !wa.Enabled {
		t.Error("whatsapp block lost its Enabled default")
	}
}

func TestWhatsAppRelayTrueCanonicalUnchanged(t *testing.T) {
	def, err := parseHumanoidFile(writeHumanoidSkillFile(t,
		"---\nname: concierge\ncontacts:\n  whatsapp:\n"+
			"    relay: true\n"+
			"---\nbody\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !def.Contacts["whatsapp"].Relay {
		t.Fatal("canonical relay: true no longer sets Relay")
	}
}

func TestWhatsAppRelayTrueWithMatchingModeOK(t *testing.T) {
	def, err := parseHumanoidFile(writeHumanoidSkillFile(t,
		"---\nname: concierge\ncontacts:\n  whatsapp:\n"+
			"    relay: true\n"+
			"    mode: relay\n"+
			"---\nbody\n"), nil)
	if err != nil {
		t.Fatalf("consistent relay: true + mode: relay must parse, got: %v", err)
	}
	if !def.Contacts["whatsapp"].Relay {
		t.Fatal("Relay not set")
	}
}

func TestWhatsAppUnknownModeFailsLoudly(t *testing.T) {
	// A typo like `mode: realy` silently running direct mode is exactly the
	// trap this guards against — same convention as `isolation:` typos.
	_, err := parseHumanoidFile(writeHumanoidSkillFile(t,
		"---\nname: concierge\ncontacts:\n  whatsapp:\n"+
			"    mode: realy\n"+
			"---\nbody\n"), nil)
	if err == nil {
		t.Fatal("unknown mode value parsed without error")
	}
	if !strings.Contains(err.Error(), `unknown mode "realy"`) {
		t.Errorf("error should name the bad value, got: %v", err)
	}
}

func TestWhatsAppModeDirectExplicitOK(t *testing.T) {
	def, err := parseHumanoidFile(writeHumanoidSkillFile(t,
		"---\nname: concierge\ncontacts:\n  whatsapp:\n"+
			"    mode: direct\n"+
			"    api_key: \"tok\"\n"+
			"    phone: \"123\"\n"+
			"---\nbody\n"), nil)
	if err != nil {
		t.Fatalf("explicit mode: direct must parse, got: %v", err)
	}
	if def.Contacts["whatsapp"].Relay {
		t.Fatal("mode: direct set Relay")
	}
}

func TestWhatsAppConflictingRelayFalseModeRelayErrors(t *testing.T) {
	_, err := parseHumanoidFile(writeHumanoidSkillFile(t,
		"---\nname: concierge\ncontacts:\n  whatsapp:\n"+
			"    relay: false\n"+
			"    mode: relay\n"+
			"---\nbody\n"), nil)
	if err == nil {
		t.Fatal("relay: false + mode: relay parsed without error — one side was silently picked")
	}
	if !strings.Contains(err.Error(), "conflicting relay settings") {
		t.Errorf("error should call out the conflict, got: %v", err)
	}
}

func TestWhatsAppConflictingRelayTrueModeDirectErrors(t *testing.T) {
	_, err := parseHumanoidFile(writeHumanoidSkillFile(t,
		"---\nname: concierge\ncontacts:\n  whatsapp:\n"+
			"    relay: true\n"+
			"    mode: direct\n"+
			"---\nbody\n"), nil)
	if err == nil {
		t.Fatal("relay: true + mode: direct parsed without error")
	}
	if !strings.Contains(err.Error(), "conflicting relay settings") {
		t.Errorf("error should call out the conflict, got: %v", err)
	}
}

func TestTelegramModeUntouchedByWhatsAppNormalization(t *testing.T) {
	// Mode is channel-polysemous: telegram's "poll"/"webhook" values must not
	// hit the whatsapp relay validation.
	def, err := parseHumanoidFile(writeHumanoidSkillFile(t,
		"---\nname: tgbot\ncontacts:\n  telegram:\n"+
			"    bot_token: \"tok\"\n"+
			"    mode: poll\n"+
			"---\nbody\n"), nil)
	if err != nil {
		t.Fatalf("telegram mode: poll must parse, got: %v", err)
	}
	tg := def.Contacts["telegram"]
	if tg.Mode != "poll" || tg.Relay {
		t.Fatalf("telegram config mangled: mode=%q relay=%v", tg.Mode, tg.Relay)
	}
}
