package main

// Tests for `rysh assistant` (design 007, WS7 PM1–PM3): the profile scaffold's
// exact format (round-tripped through the same skill-file mirror the runtime
// parses), the owner-only allowlist seed, the headless flag-driven bring-up in
// a temp dir (print-commands path — no daemon spawn), idempotent re-runs that
// never clobber an edited profile, and the --install-daemon unit files.

import (
	"bytes"
	"encoding/xml"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// assistantHeadlessArgs is the canonical scriptable bring-up used across the
// tests: provider step + telegram preset + owner seed, validation skipped,
// print-commands path (no --start → no daemon spawn).
func assistantHeadlessArgs() []string {
	return []string{
		"--provider", "anthropic", "--key-env", "ANTHROPIC_API_KEY", "--no-validate",
		"--channel", "telegram", "--owner", "424242", "--bot-token-env", "TG_TOKEN",
	}
}

// assistantFrontmatter mirrors the full frontmatter the runtime parser
// (internal/actors parseHumanoidFile) reads, including the MP2/PM3 fields the
// doctor mirror struct predates.
type assistantFrontmatter struct {
	Name        string                       `yaml:"name"`
	Description string                       `yaml:"description"`
	Provider    string                       `yaml:"provider"`
	Model       string                       `yaml:"model"`
	Profile     string                       `yaml:"profile"`
	Contacts    map[string]msg.ChannelConfig `yaml:"contacts"`
}

func parseAssistantSkill(t *testing.T, path string) (assistantFrontmatter, string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fmRaw, body, ok := splitSkillFrontmatter(string(raw))
	if !ok {
		t.Fatalf("no frontmatter in %s:\n%s", path, raw)
	}
	var fm assistantFrontmatter
	if err := yaml.Unmarshal([]byte(fmRaw), &fm); err != nil {
		t.Fatalf("frontmatter does not parse: %v\n%s", err, fmRaw)
	}
	return fm, body
}

// TestAssistantScaffoldFormat: the PM1 scaffold matches design 007 §4.1 —
// name/description/provider/model, the PM3 markers (profile: assistant,
// governance: ai, reply_mode: mentions, pairing_policy: request), the
// owner-only allowlist, and the draft-and-confirm system prompt — and it
// round-trips through the runtime's frontmatter contract (parseSkillFile).
func TestAssistantScaffoldFormat(t *testing.T) {
	chdirTemp(t)
	fields := channelCredFields("telegram")
	content, err := buildAssistantSkill("anthropic", "claude-sonnet-5", "telegram",
		fields, []string{"${TG_TOKEN}", "poll"}, "424242")
	if err != nil {
		t.Fatalf("buildAssistantSkill: %v", err)
	}
	path, created, err := scaffoldAssistantProfile(content, false)
	if err != nil || !created {
		t.Fatalf("scaffold: created=%v err=%v", created, err)
	}
	if path != filepath.Join(".rysh", "humanoids", "assistant", "SKILL.md") {
		t.Errorf("scaffold path = %q", path)
	}

	// Runtime-mirror parse (the same contract parseHumanoidFile reads).
	if _, _, err := parseSkillFile(path); err != nil {
		t.Fatalf("scaffold does not round-trip through the skill parser: %v", err)
	}

	fm, body := parseAssistantSkill(t, path)
	if fm.Name != "assistant" || fm.Description != "My personal assistant" {
		t.Errorf("name/description = %q/%q", fm.Name, fm.Description)
	}
	if fm.Provider != "anthropic" || fm.Model != "claude-sonnet-5" {
		t.Errorf("provider/model = %q/%q", fm.Provider, fm.Model)
	}
	if fm.Profile != "assistant" {
		t.Errorf("profile = %q, want assistant (the PM3 fail-closed marker)", fm.Profile)
	}
	cc, ok := fm.Contacts["telegram"]
	if !ok {
		t.Fatalf("no telegram contact block: %+v", fm.Contacts)
	}
	if cc.BotToken != "${TG_TOKEN}" {
		t.Errorf("bot_token = %q, want ${ENV} reference", cc.BotToken)
	}
	if cc.Governance != "ai" || cc.ReplyMode != "mentions" || cc.PairingPolicy != "request" {
		t.Errorf("safety defaults = governance %q reply_mode %q pairing_policy %q",
			cc.Governance, cc.ReplyMode, cc.PairingPolicy)
	}
	// Owner-only allowlist seed → the WS3 admission gate is active by construction.
	if len(cc.Allowlist) != 1 || cc.Allowlist[0] != "424242" {
		t.Errorf("allowlist = %v, want owner only", cc.Allowlist)
	}
	// design 008 RA2: the assistant is a session OPERATOR, not a correspondent.
	// The prompt must name the tools it actually has — a model that does not
	// know rysh_command exists falls back to pane_send and silently runs "##"
	// commands as shell commands — and must keep the fail-closed confirmation
	// posture that PM3 enforces at runtime.
	for _, want := range []string{
		"rysh_command", "pane_inspect", "pane_send", // tool awareness
		"operate my rysh terminal session", // operator framing
		"waiting for my confirmation",      // fail-closed posture preserved
	} {
		if !strings.Contains(body, want) {
			t.Errorf("system prompt missing %q:\n%s", want, body)
		}
	}
	// It must NOT revert to the pre-008 correspondent framing.
	if strings.Contains(body, "You are my personal assistant") {
		t.Error("system prompt reverted to the correspondent framing (pre-design-008)")
	}
}

// TestAssistantHeadlessBringUpEndToEnd: the flag form runs the full bring-up
// in a fresh directory with no TTY — provider block written, profile
// scaffolded with the owner seed, and NO daemon spawned (print-commands path).
func TestAssistantHeadlessBringUpEndToEnd(t *testing.T) {
	dir := chdirTemp(t)
	cfg := config.Load()

	if err := runAssistant(cfg, "", assistantHeadlessArgs()); err != nil {
		t.Fatalf("runAssistant: %v", err)
	}

	// Provider block written (onboarding step reused).
	raw, err := os.ReadFile(filepath.Join(dir, "rysh.config.yaml"))
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if !strings.Contains(string(raw), "name: anthropic") ||
		!strings.Contains(string(raw), "api_key: ${ANTHROPIC_API_KEY}") {
		t.Errorf("provider block missing:\n%s", raw)
	}

	// Profile scaffolded with the safety defaults + owner seed.
	fm, _ := parseAssistantSkill(t, assistantSkillPath())
	cc := fm.Contacts["telegram"]
	if fm.Profile != "assistant" || cc.Governance != "ai" ||
		len(cc.Allowlist) != 1 || cc.Allowlist[0] != "424242" {
		t.Errorf("scaffold facts = profile %q governance %q allowlist %v",
			fm.Profile, cc.Governance, cc.Allowlist)
	}
	if cc.BotToken != "${TG_TOKEN}" {
		t.Errorf("bot_token = %q", cc.BotToken)
	}

	// No daemon spawned: the session registry holds no live record.
	entries, _ := os.ReadDir(filepath.Join(dir, ".rysh", "sessions"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			t.Errorf("session record %q exists — headless run must not spawn a daemon without --start", e.Name())
		}
	}
}

// TestAssistantIdempotentRerun: a re-run reuses the existing profile — an
// edited SKILL.md is never clobbered, and --channel changes are reported, not
// applied.
func TestAssistantIdempotentRerun(t *testing.T) {
	chdirTemp(t)
	cfg := config.Load()
	if err := runAssistant(cfg, "", assistantHeadlessArgs()); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// The owner hand-edits the profile.
	edited := "# my edit — must survive re-runs\n"
	raw, _ := os.ReadFile(assistantSkillPath())
	if err := os.WriteFile(assistantSkillPath(), append([]byte(edited), raw...), 0o644); err != nil {
		t.Fatal(err)
	}

	// Re-run (even asking for a different channel) reuses the profile.
	args := []string{"--channel", "whatsapp", "--owner", "999"}
	if err := runAssistant(config.Load(), "", args); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	after, _ := os.ReadFile(assistantSkillPath())
	if !bytes.HasPrefix(after, []byte(edited)) {
		t.Fatalf("edited profile was clobbered:\n%s", after)
	}
	if !bytes.Contains(after, []byte("424242")) || bytes.Contains(after, []byte("999")) {
		t.Errorf("re-run rewrote the profile contents:\n%s", after)
	}

	// Explicit overwrite regenerates.
	args = append(assistantHeadlessArgs(), "--overwrite-profile")
	if err := runAssistant(config.Load(), "", args); err != nil {
		t.Fatalf("overwrite run: %v", err)
	}
	after, _ = os.ReadFile(assistantSkillPath())
	if bytes.HasPrefix(after, []byte(edited)) {
		t.Error("--overwrite-profile did not regenerate the file")
	}
}

// TestAssistantOwnerRequiredHeadless: the owner contact is the allowlist seed;
// a headless run without it fails closed with guidance.
func TestAssistantOwnerRequiredHeadless(t *testing.T) {
	chdirTemp(t)
	err := runAssistant(config.Load(), "", []string{
		"--provider", "anthropic", "--key-env", "K", "--no-validate",
		"--channel", "telegram", "--bot-token-env", "TG_TOKEN",
	})
	if err == nil || !strings.Contains(err.Error(), "--owner") {
		t.Fatalf("err = %v, want owner-required failure", err)
	}
	if skillFileExists(assistantName) {
		t.Error("profile written without an owner seed")
	}
}

// TestAssistantIMessagePlatformGate: the iMessage preset is honestly gated to
// macOS hosts.
func TestAssistantIMessagePlatformGate(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("gate only applies off-macOS")
	}
	chdirTemp(t)
	err := runAssistant(config.Load(), "", []string{
		"--provider", "anthropic", "--key-env", "K", "--no-validate",
		"--channel", "imessage", "--owner", "me@example.com",
	})
	if err == nil || !strings.Contains(err.Error(), "macOS") {
		t.Fatalf("err = %v, want macOS-only gate", err)
	}
}

// TestAssistantDaemonUnitFilesWellFormed: the generated LaunchAgent plist is
// valid XML with the KeepAlive contract, and the systemd unit has the
// [Service]/ExecStart shape — both run `rysh daemon <session>` in the project
// directory (rysh state is project-local).
func TestAssistantDaemonUnitFilesWellFormed(t *testing.T) {
	exe := "/usr/local/bin/rysh"
	plist := assistantLaunchdPlist(exe, "default", "/home/me/proj")

	// Parses as XML end to end.
	dec := xml.NewDecoder(strings.NewReader(plist))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("plist is not well-formed XML: %v\n%s", err, plist)
		}
	}
	for _, want := range []string{
		"<key>Label</key>", "ai.rysh.daemon", "<key>KeepAlive</key>", "<true/>",
		"<string>" + exe + "</string>", "<string>daemon</string>", "<string>default</string>",
		"<key>WorkingDirectory</key>", "/home/me/proj",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}

	unit := assistantSystemdUnit(exe, "default", "/home/me/proj")
	for _, want := range []string{
		"[Unit]", "[Service]", "ExecStart=" + exe + " daemon default",
		"WorkingDirectory=/home/me/proj", "Restart=always", "[Install]",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("systemd unit missing %q:\n%s", want, unit)
		}
	}

	// Unit locations per platform; unsupported platforms are honestly empty.
	if p := assistantDaemonUnitPath("darwin", "/Users/me"); p != "/Users/me/Library/LaunchAgents/ai.rysh.daemon.plist" {
		t.Errorf("darwin unit path = %q", p)
	}
	if p := assistantDaemonUnitPath("linux", "/home/me"); p != "/home/me/.config/systemd/user/rysh-daemon.service" {
		t.Errorf("linux unit path = %q", p)
	}
	if p := assistantDaemonUnitPath("windows", `C:\Users\me`); p != "" {
		t.Errorf("windows unit path = %q, want unsupported", p)
	}
}

// TestAssistantInstallDaemonWritesUnit: --install-daemon writes the unit under
// $HOME without loading it (no TTY → no launchctl/systemctl attempt).
func TestAssistantInstallDaemonWritesUnit(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no supported unit on this platform")
	}
	chdirTemp(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.Load()
	if err := runAssistant(cfg, "", []string{"--install-daemon"}); err != nil {
		t.Fatalf("--install-daemon: %v", err)
	}
	unitPath := assistantDaemonUnitPath(runtime.GOOS, home)
	raw, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("unit not written at %s: %v", unitPath, err)
	}
	if runtime.GOOS == "linux" {
		if !strings.Contains(string(raw), "[Service]") || !strings.Contains(string(raw), "ExecStart=") {
			t.Errorf("unit malformed:\n%s", raw)
		}
	} else if !strings.Contains(string(raw), "<key>KeepAlive</key>") {
		t.Errorf("plist malformed:\n%s", raw)
	}
}

// TestOwnerContactHintCoversEveryPreset is a drift guard on the owner-contact
// prompt. It asked for a "Telegram numeric user id / WhatsApp number / Signal
// number / iMessage handle" long after Slack and Discord had been added as
// presets — and Slack had become preset #1 — so the recommended path prompted
// for an identifier format that does not exist on it. A wrong owner id fails
// SILENTLY (the sender is held as a pending pairing request rather than
// answered), so the prompt is the only place the format is ever stated.
func TestOwnerContactHintCoversEveryPreset(t *testing.T) {
	generic := ownerContactHint("no-such-channel")
	seen := map[string]string{}
	for _, p := range assistantPresets() {
		hint := ownerContactHint(p.Channel)
		if hint.What == generic.What || hint.Label == generic.Label {
			t.Errorf("channel %q falls back to the generic hint — it was added as a preset without teaching the prompt its id format", p.Channel)
		}
		if prev, dup := seen[hint.Label]; dup {
			t.Errorf("channel %q reuses %q's prompt label %q", p.Channel, prev, hint.Label)
		}
		seen[hint.Label] = p.Channel
	}
}

// TestOwnerContactHintSlackNamesMemberID pins the specific wording for the
// recommended preset: a Slack member ID is neither a display name nor an
// email, and the admission gate compares it byte-for-byte.
func TestOwnerContactHintSlackNamesMemberID(t *testing.T) {
	hint := ownerContactHint("slack")
	if !strings.Contains(hint.What, "member ID") {
		t.Errorf("Slack hint must name the member ID, got %q", hint.What)
	}
	if !strings.Contains(hint.Where, "Copy member ID") {
		t.Errorf("Slack hint must say where to find it, got %q", hint.Where)
	}
}

// TestAssistantChannelPolicyReadsTheFile guards the summary against reporting
// policy it does not have. The channel line used to print "governance: human,
// reply_mode: messages" as string literals, which was true only for a
// freshly-scaffolded profile — an idempotent re-run reuses an EDITED file, so
// the summary would vouch for defaults the profile no longer carried.
func TestAssistantChannelPolicyReadsTheFile(t *testing.T) {
	chdirTemp(t)
	fields := channelCredFields("telegram")
	content, err := buildAssistantSkill("anthropic", "m", "telegram", fields,
		make([]string, len(fields)), "424242")
	if err != nil {
		t.Fatal(err)
	}
	path, _, err := scaffoldAssistantProfile(content, true)
	if err != nil {
		t.Fatal(err)
	}

	// Scaffold defaults come back as written.
	if gov, mode := assistantChannelPolicy(path, "telegram"); gov != "ai" || mode != "mentions" {
		t.Errorf("scaffold policy = %q/%q, want ai/mentions", gov, mode)
	}

	// An edited profile must be reported as edited, not as the default.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "governance: ai", "governance: human", 1)
	if edited == string(raw) {
		t.Fatal("fixture did not contain the governance line")
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if gov, _ := assistantChannelPolicy(path, "telegram"); gov != "human" {
		t.Errorf("edited profile reported governance %q, want human", gov)
	}

	// An unreadable path falls back to the RUNTIME defaults, so the summary
	// never claims a stricter posture than the daemon would actually apply.
	if gov, mode := assistantChannelPolicy(filepath.Join(t.TempDir(), "nope.md"), "telegram"); gov != "ai" || mode != "messages" {
		t.Errorf("fallback = %q/%q, want the runtime defaults ai/messages", gov, mode)
	}
}
