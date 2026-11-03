// SPDX-License-Identifier: Apache-2.0

package main

// Tests for `rysh setup` (E33 slice A) and the assistant-path fixes it builds
// on: preset error texts (G10), the honest summary (G4), the binding line
// (G6), the wizard itself (G9), the auto-start offer (G11) and the rule that
// setup never writes a looser gate than the scaffold (approval rule).
//
// Every credential here is a synthetic name with a fake value, every
// validator is a fake, and every run happens in a throwaway directory.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// e33FakeValue is the synthetic credential every fake lookup returns. It is
// not a token shape; it exists so tests can assert it is never printed.
const e33FakeValue = "e33-synthetic-not-a-token"

// botStub is a fake adapter whose non-binding validator succeeds or fails on
// demand and reports the bot's identity, as slice B's channels.IdentityValidator
// does (Telegram getMe, Discord users/@me).
type botStub struct {
	stubAdapter
	bot string
	app string // Discord application id, for the invite URL
}

func (b botStub) Validate(ctx context.Context) error { return b.validateErr }
func (b botStub) ValidateIdentity(ctx context.Context) (channels.BotIdentity, error) {
	if b.validateErr != nil {
		return channels.BotIdentity{}, b.validateErr
	}
	return channels.BotIdentity{ID: "4242", Username: b.bot, ApplicationID: b.app}, nil
}

// e33Deps returns doctorDeps whose lookup resolves exactly the given names
// (to the synthetic value) and whose adapter factory returns adapter.
func e33Deps(resolvable []string, adapter channels.ChannelAdapter) doctorDeps {
	deps := fakeDeps()
	deps.lookupRef = func(name string) (string, bool) {
		for _, n := range resolvable {
			if n == name {
				return e33FakeValue, true
			}
		}
		// Like the real lookup, a value this run stored in the throwaway
		// workspace's .rysh/secrets resolves; the process env is never read.
		if b, err := os.ReadFile(filepath.Join(".rysh", "secrets", "default", name)); err == nil {
			return strings.TrimSpace(string(b)), true
		}
		return "", false
	}
	deps.newAdapter = func(chType string, cc msg.ChannelConfig) (channels.ChannelAdapter, error) {
		return adapter, nil
	}
	// The resolvable names stand for the setup process's own environment;
	// the on-disk tier a live daemon also sees is the workspace's .rysh/secrets.
	deps.getenv = func(name string) string {
		for _, n := range resolvable {
			if n == name {
				return e33FakeValue
			}
		}
		return ""
	}
	deps.lookupStored = func(name string) (string, bool) {
		if b, err := os.ReadFile(filepath.Join(".rysh", "secrets", "default", name)); err == nil {
			return strings.TrimSpace(string(b)), true
		}
		return "", false
	}
	return deps
}

// liveDaemon makes setup see the target session's daemon as already running.
func liveDaemon() bool { return true }

// masked is out with the synthetic value masked: failure messages land in
// committed RED logs, so they name a planted value and never print it.
func masked(out string) string {
	return strings.ReplaceAll(out, e33FakeValue, "<synthetic value>")
}

// runAssistantCaptured runs the headless assistant bring-up with injected deps
// and returns its stdout.
func runAssistantCaptured(t *testing.T, args []string, deps doctorDeps) (string, error) {
	t.Helper()
	t.Setenv("RYSH_SESSION", "") // the pane's own session must not leak in
	var err error
	out := captureStdout(t, func() {
		err = runAssistantWith(config.Load(), "", args, assistantEnv{
			in: bufio.NewReader(strings.NewReader("")), deps: deps})
	})
	return out, err
}

// TestPresetErrorsListEveryPreset (G10): both preset error texts name all six
// presets, so the headless user is never told slack/discord do not exist.
func TestPresetErrorsListEveryPreset(t *testing.T) {
	chdirTemp(t)
	_, errNone := pickAssistantPreset(nil, nil, false)
	_, errBad := pickAssistantPreset([]string{"--channel", "nosuch"}, nil, false)
	for name, err := range map[string]error{"no --channel": errNone, "unknown --channel": errBad} {
		if err == nil {
			t.Fatalf("%s: no error", name)
		}
		for _, p := range assistantPresets() {
			if !strings.Contains(err.Error(), p.Channel) {
				t.Errorf("%s: error %q does not list preset %q", name, err, p.Channel)
			}
		}
	}
}

// TestAssistantSummaryUnresolvedRefsFail (G4): with neither the bot token nor
// the provider key resolvable, the summary prints FAIL — never PASS — for the
// channel and the provider, each with doctor's exact ##secret new fix.
func TestAssistantSummaryUnresolvedRefsFail(t *testing.T) {
	chdirTemp(t)
	out, err := runAssistantCaptured(t, assistantHeadlessArgs(),
		e33Deps(nil, botStub{stubAdapter: stubAdapter{typ: "telegram"}, bot: "e33_fake_bot"}))
	if err != nil {
		t.Fatalf("runAssistant: %v", err)
	}
	for _, want := range []string{
		"FAIL channel", secretRefFix("TG_TOKEN"),
		"FAIL provider", secretRefFix("ANTHROPIC_API_KEY"),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, masked(out))
		}
	}
	for _, bad := range []string{"PASS channel", "PASS provider"} {
		if strings.Contains(out, bad) {
			t.Errorf("summary prints %q for an unresolved reference:\n%s", bad, masked(out))
		}
	}
}

// TestAssistantSummaryValidatedChannelPasses (G4+G6): a resolvable token and
// a passing validator give PASS channel and the one binding line, with the
// bot username from the validator; the value itself is never printed.
func TestAssistantSummaryValidatedChannelPasses(t *testing.T) {
	dir := chdirTemp(t)
	out, err := runAssistantCaptured(t, assistantHeadlessArgs(),
		e33Deps([]string{"TG_TOKEN", "ANTHROPIC_API_KEY"},
			botStub{stubAdapter: stubAdapter{typ: "telegram"}, bot: "e33_fake_bot"}))
	if err != nil {
		t.Fatalf("runAssistant: %v", err)
	}
	if !strings.Contains(out, "PASS channel") {
		t.Errorf("no PASS channel for a validated token:\n%s", masked(out))
	}
	wd, _ := os.Getwd()
	_ = dir
	// A fresh directory runs the assistant in a session of its own.
	binding := "telegram @e33_fake_bot → assistant (owner 424242) → session " + derivedSessionName(wd) + " in " + wd + "; drives panes via pane_send"
	if !strings.Contains(out, binding) {
		t.Errorf("binding line missing; want %q in:\n%s", binding, masked(out))
	}
	if strings.Contains(out, e33FakeValue) {
		t.Error("summary printed the credential value")
	}
	// The provider key resolves but this run did not ping it (--no-validate):
	// that is a WARN, not a PASS.
	if strings.Contains(out, "PASS provider") || !strings.Contains(out, "WARN provider") {
		t.Errorf("unpinged provider must WARN, not PASS:\n%s", masked(out))
	}
}

// TestAssistantSummaryRejectedTokenFailsRedacted (G4): a validator rejection is
// FAIL, and an error text that echoes the credential is redacted to its
// ${NAME} before it is printed.
func TestAssistantSummaryRejectedTokenFailsRedacted(t *testing.T) {
	chdirTemp(t)
	rejected := botStub{stubAdapter: stubAdapter{typ: "telegram",
		validateErr: errors.New("telegram: GET /bot" + e33FakeValue + "/getMe: 401 Unauthorized")}}
	out, err := runAssistantCaptured(t, assistantHeadlessArgs(),
		e33Deps([]string{"TG_TOKEN", "ANTHROPIC_API_KEY"}, rejected))
	if err != nil {
		t.Fatalf("runAssistant: %v", err)
	}
	if !strings.Contains(out, "FAIL channel") || strings.Contains(out, "PASS channel") {
		t.Errorf("rejected token must FAIL:\n%s", masked(out))
	}
	if strings.Contains(out, e33FakeValue) {
		t.Error("validator error leaked the credential value into the summary")
	}
	if !strings.Contains(out, "${TG_TOKEN}") {
		t.Errorf("redacted error should name ${TG_TOKEN}:\n%s", masked(out))
	}
}

// TestAssistantSummaryUnvalidatedChannelWarns (G4): an adapter with no
// non-binding validator is WARN (resolves, not verified), never PASS.
func TestAssistantSummaryUnvalidatedChannelWarns(t *testing.T) {
	chdirTemp(t)
	out, err := runAssistantCaptured(t, assistantHeadlessArgs(),
		e33Deps([]string{"TG_TOKEN", "ANTHROPIC_API_KEY"}, stubAdapter{typ: "telegram"}))
	if err != nil {
		t.Fatalf("runAssistant: %v", err)
	}
	if !strings.Contains(out, "WARN channel") || strings.Contains(out, "PASS channel") {
		t.Errorf("unvalidated channel must WARN:\n%s", masked(out))
	}
	if !strings.Contains(out, "telegram (bot not validated) → assistant (owner 424242)") {
		t.Errorf("binding line must say the bot is unvalidated:\n%s", masked(out))
	}
}

// TestDoctorBindingLine (G6): doctor prints the same binding line per
// humanoid × channel, with the validator's bot username.
func TestDoctorBindingLine(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("RYSH_SESSION", "")
	cfg := writeGoodState(t, dir)
	deps := fakeDeps()
	deps.newAdapter = func(chType string, cc msg.ChannelConfig) (channels.ChannelAdapter, error) {
		return botStub{stubAdapter: stubAdapter{typ: chType}, bot: "e33_support_bot"}, nil
	}
	checks := doctorChecks(cfg, deps)
	if _, ok := findCheck(checks, "channels", "credentials valid (@e33_support_bot)"); !ok {
		t.Errorf("doctor's PASS line does not name the bot: %+v", checks)
	}
	wd, _ := os.Getwd()
	want := "slack @e33_support_bot → support (owner U123OWNER) → session default in " + wd + "; drives panes via pane_send"
	c, ok := findCheck(checks, "binding", want)
	if !ok {
		t.Fatalf("no binding line %q in %+v", want, checks)
	}
	if c.Status == doctorPass || c.Status == doctorFail {
		t.Errorf("binding line status = %s, want INFO (it is a fact, not a check)", c.Status)
	}
}

// ---------------------------------------------------------------------------
// G9 `rysh setup`, G11 auto-start offer, approval rule
// ---------------------------------------------------------------------------

// setupWorkspace is a throwaway project dir with a throwaway HOME, the pane's
// session name neutralised, and the auto-start installer replaced by a
// recorder (it would write under HOME and offer launchctl).
func setupWorkspace(t *testing.T) (dir string, installs *int) {
	t.Helper()
	dir = chdirTemp(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RYSH_SESSION", "")
	n := 0
	prev := setupInstallDaemon
	setupInstallDaemon = func(cfg config.Config) error { n++; return nil }
	t.Cleanup(func() { setupInstallDaemon = prev })
	return dir, &n
}

// setupHeadlessArgs is the scriptable form with synthetic names only.
func setupHeadlessArgs() []string {
	return []string{
		"--provider", "anthropic", "--key-env", "E33_FAKE_KEY", "--no-validate",
		"--channel", "telegram", "--owner", "123456789", "--bot-token-env", "TG_E33_FAKE",
	}
}

func runSetupCaptured(t *testing.T, args []string, env assistantEnv) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() { err = runSetupWith(config.Load(), "", args, env) })
	return out, err
}

// TestSetupHeadlessEndToEnd (G9+G11): the flag form writes the provider and
// the profile, never spawns a daemon without --start, prints the binding line
// and the directory the setup applies to, and exits 0 when nothing FAILed.
func TestSetupHeadlessEndToEnd(t *testing.T) {
	dir, installs := setupWorkspace(t)
	out, err := runSetupCaptured(t, setupHeadlessArgs(), assistantEnv{
		in:   bufio.NewReader(strings.NewReader("")),
		deps: e33Deps([]string{"E33_FAKE_KEY", "TG_E33_FAKE"}, botStub{stubAdapter: stubAdapter{typ: "telegram"}, bot: "e33_fake_bot"}),
	})
	if err != nil {
		t.Fatalf("runSetup: %v\n%s", err, masked(out))
	}
	fm, _ := parseAssistantSkill(t, assistantSkillPath())
	if cc := fm.Contacts["telegram"]; cc.BotToken != "${TG_E33_FAKE}" || len(cc.Allowlist) != 1 || cc.Allowlist[0] != "123456789" {
		t.Errorf("profile telegram block = %+v", cc)
	}
	if raw, err := os.ReadFile("rysh.config.yaml"); err != nil || !strings.Contains(string(raw), "${E33_FAKE_KEY}") {
		t.Errorf("provider block not written (%v)", err)
	}
	wd, _ := os.Getwd()
	for _, want := range []string{
		"telegram @e33_fake_bot → assistant (owner 123456789) → session " + derivedSessionName(wd) + " in " + wd + "; drives panes via pane_send",
		"applies to " + wd + " only",
		"rysh setup --install-daemon",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, masked(out))
		}
	}
	if *installs != 0 {
		t.Error("auto-start installed without --install-daemon or consent")
	}
	entries, _ := os.ReadDir(dir + "/.rysh/sessions")
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			t.Errorf("session record %q — headless setup must not spawn a daemon without --start", e.Name())
		}
	}
	if strings.Contains(out, e33FakeValue) {
		t.Error("setup printed the credential value")
	}
}

// TestSetupHeadlessFailIsNonZero (G4/G9): unresolved references are saved but
// reported, and the command exits non-zero so a script cannot read success.
func TestSetupHeadlessFailIsNonZero(t *testing.T) {
	setupWorkspace(t)
	out, err := runSetupCaptured(t, setupHeadlessArgs(), assistantEnv{
		in:   bufio.NewReader(strings.NewReader("")),
		deps: e33Deps(nil, botStub{stubAdapter: stubAdapter{typ: "telegram"}, bot: "e33_fake_bot"}),
	})
	if err == nil || !strings.Contains(err.Error(), "FAIL") {
		t.Fatalf("err = %v, want a FAIL exit", err)
	}
	if !strings.Contains(out, secretRefFix("TG_E33_FAKE")) {
		t.Errorf("missing the ##secret new fix:\n%s", masked(out))
	}
	if !skillFileExists(assistantName) {
		t.Error("profile not saved; setup should save and report")
	}
}

// TestSetupRefusesSecretsInArgv (G9 hard rule): setup never takes a secret as
// a flag value — argv is visible to every process on the host. This is the
// only test that passes such a flag, to prove the refusal; its value is the
// synthetic marker.
func TestSetupRefusesSecretsInArgv(t *testing.T) {
	for _, flag := range []string{"--bot-token", "--key", "--app-token"} {
		t.Run(flag, func(t *testing.T) {
			setupWorkspace(t)
			args := []string{"--provider", "anthropic", "--key-env", "E33_FAKE_KEY", "--no-validate",
				"--channel", "telegram", "--owner", "123456789", flag, e33FakeValue}
			if flag == "--key" {
				args = []string{"--provider", "anthropic", flag, e33FakeValue, "--no-validate",
					"--channel", "telegram", "--owner", "123456789", "--bot-token-env", "TG_E33_FAKE"}
			}
			out, err := runSetupCaptured(t, args, assistantEnv{
				in: bufio.NewReader(strings.NewReader("")), deps: e33Deps(nil, stubAdapter{typ: "telegram"})})
			if err == nil || !strings.Contains(err.Error(), "-env") {
				t.Fatalf("err = %v, want a refusal naming the -env form", err)
			}
			if strings.Contains(out, e33FakeValue) || strings.Contains(err.Error(), e33FakeValue) {
				t.Error("refusal echoed the value")
			}
			if skillFileExists(assistantName) {
				t.Error("profile written despite the refusal")
			}
			if _, statErr := os.Stat("rysh.config.yaml"); statErr == nil {
				t.Error("config written despite the refusal")
			}
		})
	}
}

// TestSetupTTYFlow (G9): the interactive path through injected input — the
// provider step, Telegram listed first, the bot-creation steps BEFORE the
// token prompt, the token read through the no-echo reader and stored in
// .rysh/secrets (0600) with only ${NAME} in the profile, validation naming
// the bot, the owner hint, consent for the daemon, the auto-start offer.
func TestSetupTTYFlow(t *testing.T) {
	_, installs := setupWorkspace(t)
	secretReads := 0
	prev := readSecretInput
	readSecretInput = func(in *bufio.Reader, label string) string {
		if strings.HasPrefix(label, "API key") {
			return "${E33_FAKE_KEY}" // the provider key, by reference
		}
		secretReads++
		return e33FakeValue
	}
	t.Cleanup(func() { readSecretInput = prev })

	input := strings.Join([]string{
		"1",         // provider: anthropic
		"",          // model: default (the API key is read hidden)
		"1",         // channel: first in the list
		"",          // telegram inbound mode: default
		"123456789", // owner id
		"n",         // do not spawn the daemon
		"n",         // do not install auto-start
	}, "\n") + "\n"
	out, err := runSetupCaptured(t, []string{"--no-validate"}, assistantEnv{
		in: bufio.NewReader(strings.NewReader(input)), tty: true,
		deps: e33Deps([]string{"E33_FAKE_KEY"}, botStub{stubAdapter: stubAdapter{typ: "telegram"}, bot: "e33_fake_bot"}),
	})
	if err != nil {
		t.Fatalf("runSetup: %v\n%s", err, masked(out))
	}
	if secretReads != 1 {
		t.Errorf("token read %d times through the no-echo reader, want 1", secretReads)
	}
	if strings.Contains(out, e33FakeValue) {
		t.Fatal("the token was printed")
	}
	// Telegram is the first choice offered.
	if i := strings.Index(out, "1) Telegram"); i < 0 {
		t.Errorf("Telegram is not choice 1:\n%s", masked(out))
	}
	if strings.Contains(out, "Slack (recommended)") {
		t.Error("setup offers Telegram first but still labels Slack recommended")
	}
	// slice B's bot-creation steps come before the token is asked for.
	steps, ok := channels.BotSetupSteps("telegram", channels.BotIdentity{})
	if !ok || len(steps) == 0 || !strings.Contains(out, steps[0]) {
		t.Errorf("bot-creation steps not printed:\n%s", masked(out))
	}
	if !strings.Contains(out, "@e33_fake_bot") {
		t.Errorf("validated bot name not shown:\n%s", masked(out))
	}
	if !strings.Contains(out, ownerContactHint("telegram").Where) {
		t.Errorf("owner hint not shown:\n%s", masked(out))
	}
	fm, _ := parseAssistantSkill(t, assistantSkillPath())
	if got := fm.Contacts["telegram"].BotToken; got != "${TELEGRAM_BOT_TOKEN}" {
		t.Errorf("profile bot_token = %q, want the ${NAME} reference", got)
	}
	info, err := os.Stat(".rysh/secrets/default/TELEGRAM_BOT_TOKEN")
	if err != nil {
		t.Fatalf("token not stored in .rysh/secrets: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("secret file mode = %v, want 0600", info.Mode().Perm())
	}
	if *installs != 0 {
		t.Error("auto-start installed after the user said no")
	}
}

// TestSetupTTYRetriesRejectedToken (G9): a token the validator rejects is
// asked for again, and nothing is written until one is accepted or the user
// stops.
func TestSetupTTYRetriesRejectedToken(t *testing.T) {
	setupWorkspace(t)
	reads := 0
	prev := readSecretInput
	readSecretInput = func(in *bufio.Reader, label string) string {
		if strings.HasPrefix(label, "API key") {
			return "${E33_FAKE_KEY}"
		}
		reads++
		return e33FakeValue
	}
	t.Cleanup(func() { readSecretInput = prev })

	calls := 0
	deps := e33Deps([]string{"E33_FAKE_KEY"}, nil)
	deps.newAdapter = func(chType string, cc msg.ChannelConfig) (channels.ChannelAdapter, error) {
		calls++
		if calls == 1 {
			return botStub{stubAdapter: stubAdapter{typ: chType, validateErr: errors.New("401 Unauthorized")}}, nil
		}
		return botStub{stubAdapter: stubAdapter{typ: chType}, bot: "e33_fake_bot"}, nil
	}
	input := strings.Join([]string{"1", "", "1", "", "y", "", "123456789", "n", "n"}, "\n") + "\n"
	out, err := runSetupCaptured(t, []string{"--no-validate"}, assistantEnv{
		in: bufio.NewReader(strings.NewReader(input)), tty: true, deps: deps})
	if err != nil {
		t.Fatalf("runSetup: %v\n%s", err, masked(out))
	}
	if reads != 2 {
		t.Errorf("token asked %d times, want 2 (rejected, then accepted)", reads)
	}
	if !strings.Contains(out, "credentials rejected") {
		t.Errorf("rejection not reported:\n%s", masked(out))
	}
}

// TestSetupInstallDaemonOffer (G11): --install-daemon installs at the end of a
// headless run; on a TTY a yes installs it.
func TestSetupInstallDaemonOffer(t *testing.T) {
	_, installs := setupWorkspace(t)
	deps := e33Deps([]string{"E33_FAKE_KEY", "TG_E33_FAKE"}, botStub{stubAdapter: stubAdapter{typ: "telegram"}, bot: "e33_fake_bot"})
	if _, err := runSetupCaptured(t, append(setupHeadlessArgs(), "--install-daemon"),
		assistantEnv{in: bufio.NewReader(strings.NewReader("")), deps: deps}); err != nil {
		t.Fatalf("headless: %v", err)
	}
	if *installs != 1 {
		t.Fatalf("installs = %d after --install-daemon, want 1", *installs)
	}
	// Re-run on a TTY (profile reused): decline the daemon, accept auto-start.
	if _, err := runSetupCaptured(t, setupHeadlessArgs(),
		assistantEnv{in: bufio.NewReader(strings.NewReader("n\ny\n")), tty: true, deps: deps}); err != nil {
		t.Fatalf("tty: %v", err)
	}
	if *installs != 2 {
		t.Errorf("installs = %d after a TTY yes, want 2", *installs)
	}
}

// TestSetupNeverLoosensTheGate (approval rule): every gate field the setup
// writes equals the assistant scaffold's default, and setup writes no
// session-wide gate setting at all.
func TestSetupNeverLoosensTheGate(t *testing.T) {
	setupWorkspace(t)
	if _, err := runSetupCaptured(t, setupHeadlessArgs(), assistantEnv{
		in:   bufio.NewReader(strings.NewReader("")),
		deps: e33Deps([]string{"E33_FAKE_KEY", "TG_E33_FAKE"}, stubAdapter{typ: "telegram"}),
	}); err != nil {
		t.Fatalf("runSetup: %v", err)
	}
	got, _, err := parseSkillFile(assistantSkillPath())
	if err != nil {
		t.Fatal(err)
	}
	scaffold, err := buildAssistantSkill("anthropic", "claude-sonnet-5", "telegram",
		channelCredFields("telegram"), []string{"${TG_E33_FAKE}", "poll"}, "123456789")
	if err != nil {
		t.Fatal(err)
	}
	ref := filepathJoinTemp(t, scaffold)
	want, _, err := parseSkillFile(ref)
	if err != nil {
		t.Fatal(err)
	}
	// The scaffold's named default (assistantScaffoldAutoApprove, false since
	// founder Q4) — never a literal here.
	if got.AutoApprove == nil || *got.AutoApprove != assistantScaffoldAutoApprove {
		t.Errorf("auto_approve = %v, want the scaffold const %v", got.AutoApprove, assistantScaffoldAutoApprove)
	}
	if (got.AutoApprove == nil) != (want.AutoApprove == nil) ||
		(got.AutoApprove != nil && *got.AutoApprove != *want.AutoApprove) {
		t.Errorf("auto_approve = %v, scaffold default %v", got.AutoApprove, want.AutoApprove)
	}
	if got.Profile != want.Profile {
		t.Errorf("profile = %q, scaffold %q", got.Profile, want.Profile)
	}
	g, w := got.Contacts["telegram"], want.Contacts["telegram"]
	if g.Governance != w.Governance || g.PairingPolicy != w.PairingPolicy || g.ReplyMode != w.ReplyMode ||
		strings.Join(g.Allowlist, ",") != strings.Join(w.Allowlist, ",") {
		t.Errorf("gate fields differ from the scaffold:\n got  governance=%q pairing=%q reply=%q allow=%v\n want governance=%q pairing=%q reply=%q allow=%v",
			g.Governance, g.PairingPolicy, g.ReplyMode, g.Allowlist, w.Governance, w.PairingPolicy, w.ReplyMode, w.Allowlist)
	}
	raw, _ := os.ReadFile("rysh.config.yaml")
	for _, key := range []string{"humanoid_defaults", "pairing_default", "auto_approve", "governance", "policy"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("setup wrote session-wide gate key %q into rysh.config.yaml", key)
		}
	}
}

// filepathJoinTemp writes content to a temp SKILL.md and returns its path.
func filepathJoinTemp(t *testing.T, content string) string {
	t.Helper()
	p := t.TempDir() + "/SKILL.md"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSetupIsDiscoverable (G9): `rysh --help` and onboard's closing lines
// point to the setup command in one line, and the dispatcher routes it.
func TestSetupIsDiscoverable(t *testing.T) {
	if usage := captureStdout(t, printUsage); !strings.Contains(usage, "rysh "+setupCommand) {
		t.Error("rysh --help does not mention rysh setup")
	}
	chdirTemp(t)
	out := captureStdout(t, func() {
		_ = runOnboardHeadless(config.Load(), "", []string{"--provider", "anthropic", "--key-env", "E33_FAKE_KEY", "--no-validate"})
	})
	if !strings.Contains(out, "rysh "+setupCommand) {
		t.Errorf("onboard does not point to rysh setup:\n%s", out)
	}
	help := captureStdout(t, func() {
		if err := run(config.Load(), slog.New(slog.NewTextHandler(io.Discard, nil)), []string{setupCommand, "--help"}, "", false, false, false); err != nil {
			t.Errorf("dispatch: %v", err)
		}
	})
	if !strings.Contains(help, "usage: rysh "+setupCommand) {
		t.Errorf("setup --help not dispatched:\n%s", help)
	}
}

// TestSetupDoesNotPrintOnboardHints: the provider step is onboarding's, but
// onboarding's closing "next: rysh onboard …" hints belong to `rysh onboard`
// alone — inside setup they point the user away mid-flow.
func TestSetupDoesNotPrintOnboardHints(t *testing.T) {
	setupWorkspace(t)
	out, _ := runSetupCaptured(t, setupHeadlessArgs(), assistantEnv{
		in:   bufio.NewReader(strings.NewReader("")),
		deps: e33Deps([]string{"E33_FAKE_KEY", "TG_E33_FAKE"}, stubAdapter{typ: "telegram"}),
	})
	if strings.Contains(out, "next: \"rysh doctor\" to verify, \"rysh onboard\"") {
		t.Errorf("setup printed onboard's closing hints:\n%s", masked(out))
	}
}

// TestSetupRerunKeepsExistingProfile (founder Q4, root-order-052): re-running
// setup on a profile that already exists never rewrites it — not its
// auto_approve, not any other gate field, not one byte.
func TestSetupRerunKeepsExistingProfile(t *testing.T) {
	setupWorkspace(t)
	env := assistantEnv{
		in:   bufio.NewReader(strings.NewReader("")),
		deps: e33Deps([]string{"E33_FAKE_KEY", "TG_E33_FAKE"}, stubAdapter{typ: "telegram"}),
	}
	if _, err := runSetupCaptured(t, setupHeadlessArgs(), env); err != nil {
		t.Fatalf("first setup: %v", err)
	}
	path := assistantSkillPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The owner's own edits: the opposite auto_approve of the scaffold, a
	// different governance and reply mode.
	edited := string(raw)
	if strings.Contains(edited, "auto_approve: true") {
		edited = strings.Replace(edited, "auto_approve: true", "auto_approve: false", 1)
	} else {
		edited = strings.Replace(edited, "auto_approve: false", "auto_approve: true", 1)
	}
	edited = strings.Replace(edited, "governance: ai", "governance: human", 1)
	edited = strings.Replace(edited, "reply_mode: mentions", "reply_mode: all", 1)
	if edited == string(raw) {
		t.Fatal("fixture: no gate field was edited")
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		setupHeadlessArgs(),
		append(setupHeadlessArgs(), "--channel", "discord"),
		append(setupHeadlessArgs(), "--overwrite-profile"),
	} {
		_, _ = runSetupCaptured(t, args, env)
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != edited {
			t.Errorf("setup %v rewrote the existing profile", args[len(args)-1:])
		}
	}
}

// TestSetupJSONSummary (founder Q5): --json gives the desktop app one
// machine-readable object: every summary check, the binding and the
// auto-start outcome — names only, never a value.
func TestSetupJSONSummary(t *testing.T) {
	setupWorkspace(t)
	var js strings.Builder
	out, err := runSetupCaptured(t, append(setupHeadlessArgs(), "--json"), assistantEnv{
		in:      bufio.NewReader(strings.NewReader("")),
		deps:    e33Deps([]string{"E33_FAKE_KEY", "TG_E33_FAKE"}, botStub{stubAdapter: stubAdapter{typ: "telegram"}, bot: "e33_fake_bot"}),
		jsonOut: &js,
	})
	if err != nil {
		t.Fatalf("runSetup: %v\n%s", err, masked(out))
	}
	var got setupJSON
	if err := json.Unmarshal([]byte(js.String()), &got); err != nil {
		t.Fatalf("--json output is not one JSON object: %v\n%s", err, masked(js.String()))
	}
	wd, _ := os.Getwd()
	if got.Version != 1 || !got.OK || got.Fails != 0 || got.Dir != wd || got.Session != derivedSessionName(wd) || got.Channel != "telegram" {
		t.Errorf("header = version %d ok %v fails %d dir-match %v session %q channel %q",
			got.Version, got.OK, got.Fails, got.Dir == wd, got.Session, got.Channel)
	}
	groups := map[string]string{}
	for _, c := range got.Checks {
		groups[c.Group] = c.Status
	}
	for g, want := range map[string]string{"provider": doctorWarn, "profile": doctorPass, "channel": doctorPass, "pairing": doctorPass, "daemon": doctorInfo, "binding": doctorInfo} {
		if groups[g] != want {
			t.Errorf("check %s = %q, want %q", g, groups[g], want)
		}
	}
	if b := got.Binding; b == nil || b.Bot != "e33_fake_bot" || b.Humanoid != assistantName ||
		strings.Join(b.Allowlist, ",") != "123456789" || !strings.HasPrefix(b.Line, "telegram @e33_fake_bot → ") {
		t.Errorf("binding = %+v", got.Binding)
	}
	if got.Profile == nil || !got.Profile.Created || got.Profile.Path == "" {
		t.Errorf("profile = %+v", got.Profile)
	}
	if got.AutoStart.Installed {
		t.Error("auto_start.installed without --install-daemon")
	}
	if strings.Contains(js.String(), e33FakeValue) {
		t.Error("--json output carries the credential value")
	}
}

// TestSetupJSONOnError: a run that stops early still answers with one JSON
// object (ok false, the reason), so the app never parses prose.
func TestSetupJSONOnError(t *testing.T) {
	setupWorkspace(t)
	var js strings.Builder
	_, err := runSetupCaptured(t, []string{"--json", "--channel", "telegram", "--owner", "1", "--bot-token", e33FakeValue}, assistantEnv{
		in: bufio.NewReader(strings.NewReader("")), deps: e33Deps(nil, stubAdapter{typ: "telegram"}), jsonOut: &js,
	})
	if err == nil {
		t.Fatal("a secret flag value was accepted")
	}
	var got setupJSON
	if jerr := json.Unmarshal([]byte(js.String()), &got); jerr != nil {
		t.Fatalf("no JSON object on error: %v\n%s", jerr, masked(js.String()))
	}
	if got.OK || got.Error == "" || got.Version != 1 {
		t.Errorf("error object = ok %v error-set %v version %d", got.OK, got.Error != "", got.Version)
	}
	if strings.Contains(js.String(), e33FakeValue) {
		t.Error("--json error carries the refused value")
	}
}

// TestSetupJSONStdoutIsOnlyJSON: through the real entry point, --json leaves
// exactly one JSON object on stdout; the human text goes to stderr.
func TestSetupJSONStdoutIsOnlyJSON(t *testing.T) {
	setupWorkspace(t)
	var err error
	out := captureStdout(t, func() {
		err = runSetup(config.Load(), "", append(setupHeadlessArgs(), "--json"))
	})
	if err == nil {
		t.Error("unresolved references did not make the run fail")
	}
	var got setupJSON
	dec := json.NewDecoder(strings.NewReader(out))
	if jerr := dec.Decode(&got); jerr != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", jerr, masked(out))
	}
	if dec.More() {
		t.Errorf("stdout carries more than one JSON object:\n%s", masked(out))
	}
	if got.OK || got.Fails == 0 {
		t.Errorf("unresolved run reported ok %v fails %d", got.OK, got.Fails)
	}
}

// TestSetupWiresSliceBGuides: setup prints slice B's guides — the Discord
// steps, then after validation the invite URL built from the application id
// and the bot to DM; for Slack, the app manifest and its create link.
func TestSetupWiresSliceBGuides(t *testing.T) {
	t.Run("discord", func(t *testing.T) {
		setupWorkspace(t)
		var js strings.Builder
		out, err := runSetupCaptured(t, []string{
			"--provider", "anthropic", "--key-env", "E33_FAKE_KEY", "--no-validate",
			"--channel", "discord", "--owner", "111222333", "--bot-token-env", "DC_E33_FAKE", "--json",
		}, assistantEnv{
			in:      bufio.NewReader(strings.NewReader("")),
			deps:    e33Deps([]string{"E33_FAKE_KEY", "DC_E33_FAKE"}, botStub{stubAdapter: stubAdapter{typ: "discord"}, bot: "e33_dc_bot", app: "424242"}),
			jsonOut: &js,
		})
		if err != nil {
			t.Fatalf("runSetup: %v\n%s", err, masked(out))
		}
		before, _ := channels.BotSetupSteps("discord", channels.BotIdentity{})
		invite := channels.DiscordInviteURL("424242")
		for _, want := range []string{before[0], invite, "@e33_dc_bot", "DM @e33_dc_bot"} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, masked(out))
			}
		}
		if i, j := strings.Index(out, before[0]), strings.Index(out, invite); i < 0 || j < i {
			t.Error("the invite URL must follow the steps, once validation learned the application id")
		}
		var got setupJSON
		if err := json.Unmarshal([]byte(js.String()), &got); err != nil {
			t.Fatal(err)
		}
		if got.Binding == nil || got.Binding.InviteURL != invite || got.Binding.Handle != "@e33_dc_bot" {
			t.Errorf("json binding = %+v", got.Binding)
		}
	})
	t.Run("slack", func(t *testing.T) {
		setupWorkspace(t)
		out, _ := runSetupCaptured(t, []string{
			"--provider", "anthropic", "--key-env", "E33_FAKE_KEY", "--no-validate",
			"--channel", "slack", "--owner", "U0E33OWNER",
			"--bot-token-env", "SL_E33_BOT", "--app-token-env", "SL_E33_APP",
		}, assistantEnv{
			in:   bufio.NewReader(strings.NewReader("")),
			deps: e33Deps([]string{"E33_FAKE_KEY", "SL_E33_BOT", "SL_E33_APP"}, botStub{stubAdapter: stubAdapter{typ: "slack"}, bot: "e33_sl_bot"}),
		})
		steps, _ := channels.BotSetupSteps("slack", channels.BotIdentity{})
		for _, want := range []string{steps[0], "socket_mode_enabled: true", channels.SlackCreateFromManifestURL("")} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, masked(out))
			}
		}
	})
}

// TestVerifyCountsOnlyTheOwner: the round-trip check verifies only on a
// message from the owner it was given. A stranger's message reaches the
// inbox too (held for pairing), and must not read as VERIFIED.
func TestVerifyCountsOnlyTheOwner(t *testing.T) {
	stranger := inboundEnvelope(t, msg.MsgHumanoidInboundMessage{
		ChannelType: "telegram", SenderID: "999", Content: "hi", Metadata: map[string]string{"direct_message": "true"},
	})
	owner := inboundEnvelope(t, msg.MsgHumanoidInboundMessage{
		ChannelType: "telegram", SenderID: "123456789", Content: "hello", Metadata: map[string]string{"direct_message": "true"},
	})
	next, seen := ownerInboundFilter(feed(stranger, owner), "123456789")
	got, _ := awaitAddressedInbound(next, time.Now().Add(time.Second))
	if got == nil || got.SenderID != "123456789" {
		t.Fatalf("got %+v, want the owner's message", got)
	}
	if *seen != "999" {
		t.Errorf("stranger seen = %q", *seen)
	}

	next, seen = ownerInboundFilter(feed(stranger), "123456789")
	if got, _ := awaitAddressedInbound(next, time.Now().Add(50*time.Millisecond)); got != nil {
		t.Fatalf("a stranger's message VERIFIED the round trip: %+v", got)
	}
	note := verifyStrangerNote(time.Second, *seen, "123456789")
	for _, want := range []string{"NOT verified", "999", "123456789"} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q: %s", want, note)
		}
	}
}

// TestChannelUnreachableIsNotARejectedToken: when the platform cannot be
// reached, the token was never judged — the check FAILs, but it must not say
// "rejected" or tell the user to replace a token that may be fine.
func TestChannelUnreachableIsNotARejectedToken(t *testing.T) {
	setupWorkspace(t)
	adapter := botStub{stubAdapter: stubAdapter{typ: "telegram",
		validateErr: errors.New("telegram: Bot API unreachable: getMe request: dial tcp: lookup api.telegram.org: no such host")}}
	cc := msg.ChannelConfig{BotToken: "${TG_E33_FAKE}"}
	c := checkChannelConfig("telegram", cc, e33Deps([]string{"TG_E33_FAKE"}, adapter))
	if c.Status != doctorFail {
		t.Errorf("status = %s, want FAIL (nothing was verified)", c.Status)
	}
	if strings.Contains(c.Detail, "rejected") || strings.Contains(strings.Join(c.Fixes, " "), "replace the") {
		t.Errorf("an unreachable platform reads as a bad token: %s / %v", c.Detail, c.Fixes)
	}
	if !strings.Contains(c.Detail, "could not reach") || !strings.Contains(strings.Join(c.Fixes, " "), "network") {
		t.Errorf("detail/fix do not say the network is the problem: %s / %v", c.Detail, c.Fixes)
	}
}

// ---------------------------------------------------------------------------
// Slice A2: a live daemon cannot see setup's environment
// ---------------------------------------------------------------------------

// storedSecret reports whether name is in the workspace's .rysh/secrets with
// mode 0600 and holds the synthetic value (compared in-process, never shown).
func storedSecret(t *testing.T, name string) bool {
	t.Helper()
	p := filepath.Join(".rysh", "secrets", "default", name)
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("%s mode = %v, want 0600", name, info.Mode().Perm())
	}
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b)) == e33FakeValue
}

func checkByGroup(t *testing.T, js string, group string) summaryLine {
	t.Helper()
	var got setupJSON
	if err := json.Unmarshal([]byte(js), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, masked(js))
	}
	for _, c := range got.Checks {
		if c.Group == group {
			return c
		}
	}
	t.Fatalf("no %s check in %s", group, masked(js))
	return summaryLine{}
}

// TestSetupStoreSecretsHeadless (the app path): --store-secrets writes each
// ${NAME} the profile and provider use, from setup's own environment, to
// .rysh/secrets once the checks pass — so a daemon that is already running
// resolves them. The value is never printed and never in argv.
func TestSetupStoreSecretsHeadless(t *testing.T) {
	setupWorkspace(t)
	var js strings.Builder
	out, err := runSetupCaptured(t, append(setupHeadlessArgs(), "--store-secrets", "--json"), assistantEnv{
		in:         bufio.NewReader(strings.NewReader("")),
		deps:       e33Deps([]string{"E33_FAKE_KEY", "TG_E33_FAKE"}, botStub{stubAdapter: stubAdapter{typ: "telegram"}, bot: "e33_fake_bot"}),
		jsonOut:    &js,
		daemonLive: liveDaemon,
	})
	if err != nil {
		t.Fatalf("runSetup: %v\n%s", err, masked(out))
	}
	for _, n := range []string{"TG_E33_FAKE", "E33_FAKE_KEY"} {
		if !storedSecret(t, n) {
			t.Errorf("%s not stored in .rysh/secrets", n)
		}
	}
	if c := checkByGroup(t, js.String(), "channel"); c.Status != doctorPass {
		t.Errorf("channel = %s after storing, want PASS: %s", c.Status, c.Detail)
	}
	var got setupJSON
	_ = json.Unmarshal([]byte(js.String()), &got)
	if strings.Join(got.StoredSecrets, ",") != "E33_FAKE_KEY,TG_E33_FAKE" {
		t.Errorf("stored_secrets = %v", got.StoredSecrets)
	}
	if strings.Contains(out, e33FakeValue) || strings.Contains(js.String(), e33FakeValue) {
		t.Error("a stored value was printed")
	}
	if !strings.Contains(captureStdout(t, printSetupUsage), "--store-secrets") {
		t.Error("rysh setup --help does not list --store-secrets (the app probes for it)")
	}
}

// TestSetupStoreSecretsNeedsAPassingCheck: a token the validator rejects is
// never stored.
func TestSetupStoreSecretsNeedsAPassingCheck(t *testing.T) {
	setupWorkspace(t)
	_, _ = runSetupCaptured(t, append(setupHeadlessArgs(), "--store-secrets"), assistantEnv{
		in: bufio.NewReader(strings.NewReader("")),
		deps: e33Deps([]string{"E33_FAKE_KEY", "TG_E33_FAKE"}, botStub{stubAdapter: stubAdapter{typ: "telegram",
			validateErr: errors.New("telegram: 401 Unauthorized — get a new token from @BotFather")}}),
		daemonLive: liveDaemon,
	})
	if storedSecret(t, "TG_E33_FAKE") {
		t.Error("a rejected token was stored")
	}
}

// TestSetupLiveDaemonRefIsNotPass: with the daemon already live, a ${NAME}
// that only setup's environment holds is not PASS — the daemon resolves it
// only from .rysh/secrets or its own environment, which setup cannot see.
// The line names both fixes. Headless (the app) and TTY alike.
func TestSetupLiveDaemonRefIsNotPass(t *testing.T) {
	assertWarned := func(t *testing.T, c summaryLine, name string) {
		t.Helper()
		if c.Status == doctorPass {
			t.Errorf("%s line PASS although the live daemon cannot be shown to resolve ${%s}: %s", c.Group, name, c.Detail)
		}
		fixes := strings.Join(c.Fixes, " | ")
		if !strings.Contains(fixes, "--store-secrets") || !strings.Contains(fixes, "##secret new "+name) {
			t.Errorf("%s fixes = %q, want --store-secrets and ##secret new %s", c.Group, fixes, name)
		}
	}
	t.Run("headless", func(t *testing.T) {
		setupWorkspace(t)
		var js strings.Builder
		out, err := runSetupCaptured(t, append(setupHeadlessArgs(), "--json"), assistantEnv{
			in:         bufio.NewReader(strings.NewReader("")),
			deps:       e33Deps([]string{"E33_FAKE_KEY", "TG_E33_FAKE"}, botStub{stubAdapter: stubAdapter{typ: "telegram"}, bot: "e33_fake_bot"}),
			jsonOut:    &js,
			daemonLive: liveDaemon,
		})
		if err != nil {
			t.Fatalf("runSetup: %v\n%s", err, masked(out))
		}
		assertWarned(t, checkByGroup(t, js.String(), "channel"), "TG_E33_FAKE")
		assertWarned(t, checkByGroup(t, js.String(), "provider"), "E33_FAKE_KEY")
		if storedSecret(t, "TG_E33_FAKE") {
			t.Error("stored without --store-secrets")
		}
	})
	t.Run("tty", func(t *testing.T) {
		setupWorkspace(t)
		prev := readSecretInput
		readSecretInput = func(in *bufio.Reader, label string) string {
			if strings.HasPrefix(label, "API key") {
				return "${E33_FAKE_KEY}"
			}
			return "" // the token: keep the default ${TELEGRAM_BOT_TOKEN} reference
		}
		t.Cleanup(func() { readSecretInput = prev })
		input := strings.Join([]string{"1", "", "1", "", "123456789", "n", "n"}, "\n") + "\n"
		out, err := runSetupCaptured(t, []string{"--no-validate"}, assistantEnv{
			in: bufio.NewReader(strings.NewReader(input)), tty: true,
			deps:       e33Deps([]string{"E33_FAKE_KEY", "TELEGRAM_BOT_TOKEN"}, botStub{stubAdapter: stubAdapter{typ: "telegram"}, bot: "e33_fake_bot"}),
			daemonLive: liveDaemon,
		})
		if err != nil {
			t.Fatalf("runSetup: %v\n%s", err, masked(out))
		}
		// Step 4's pre-write check may say the token is valid — it is; the
		// claim that must not be made is the summary's PASS for this daemon.
		i := strings.Index(out, "summary (doctor-style):")
		if i < 0 {
			t.Fatalf("no summary:\n%s", masked(out))
		}
		summary := out[i:]
		if strings.Contains(summary, "PASS channel") {
			t.Errorf("TTY summary says PASS channel against a live daemon:\n%s", masked(summary))
		}
		if !strings.Contains(summary, "--store-secrets") || !strings.Contains(summary, "##secret new TELEGRAM_BOT_TOKEN") {
			t.Errorf("TTY summary lacks the fixes:\n%s", masked(summary))
		}
	})
}

// TestSetupJSONVerified: --json carries the round trip as a boolean, so the
// app stops parsing "round trip VERIFIED" out of the daemon line.
func TestSetupJSONVerified(t *testing.T) {
	setupWorkspace(t)
	var js strings.Builder
	_, _ = runSetupCaptured(t, append(setupHeadlessArgs(), "--json"), assistantEnv{
		in:      bufio.NewReader(strings.NewReader("")),
		deps:    e33Deps([]string{"E33_FAKE_KEY", "TG_E33_FAKE"}, botStub{stubAdapter: stubAdapter{typ: "telegram"}}),
		jsonOut: &js,
	})
	if !strings.Contains(js.String(), `"verified":false`) {
		t.Errorf("no verified:false in %s", masked(js.String()))
	}
	if roundTripVerified(verifyTimeoutNote(time.Second, true)) || roundTripVerified(verifyStrangerNote(time.Second, "9", "1")) {
		t.Error("a NOT-verified note reads as verified")
	}
	if !roundTripVerified(fmt.Sprintf("round trip VERIFIED on %s — received %q from %s", "telegram", "hi", "1")) {
		t.Error("the VERIFIED note does not read as verified")
	}
}

// TestSetupStepsOnly: --steps-only prints the guides (steps, the Slack
// manifest and its link) and writes nothing; with --json, one object.
func TestSetupStepsOnly(t *testing.T) {
	setupWorkspace(t)
	var js strings.Builder
	if _, err := runSetupCaptured(t, []string{"--steps-only", "--json"}, assistantEnv{
		in: bufio.NewReader(strings.NewReader("")), deps: e33Deps(nil, stubAdapter{typ: "telegram"}), jsonOut: &js,
	}); err != nil {
		t.Fatalf("steps-only: %v", err)
	}
	var got setupStepsJSON
	if err := json.Unmarshal([]byte(js.String()), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, js.String())
	}
	var chs []string
	for _, g := range got.Guides {
		chs = append(chs, g.Channel)
		want, _ := channels.BotSetupSteps(g.Channel, channels.BotIdentity{})
		if strings.Join(g.Steps, "\n") != strings.Join(want, "\n") || len(want) == 0 {
			t.Errorf("%s steps differ from channels.BotSetupSteps", g.Channel)
		}
		if g.Channel == "slack" && (g.Manifest != channels.SlackAppManifest("") || g.CreateURL != channels.SlackCreateFromManifestURL("")) {
			t.Error("slack guide lacks the manifest or its create link")
		}
	}
	if strings.Join(chs, ",") != "telegram,discord,slack,mattermost,matrix,line" || got.Version != 1 || !got.OK {
		t.Errorf("guides = %v version %d ok %v", chs, got.Version, got.OK)
	}
	for _, p := range []string{"rysh.config.yaml", assistantSkillPath(), ".rysh/secrets"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("--steps-only wrote %s", p)
		}
	}
	js.Reset()
	_, _ = runSetupCaptured(t, []string{"--steps-only", "--json", "--channel", "discord"}, assistantEnv{
		in: bufio.NewReader(strings.NewReader("")), deps: e33Deps(nil, stubAdapter{typ: "discord"}), jsonOut: &js,
	})
	_ = json.Unmarshal([]byte(js.String()), &got)
	if len(got.Guides) != 1 || got.Guides[0].Channel != "discord" {
		t.Errorf("--channel discord guides = %+v", got.Guides)
	}
}

// E33 Q-E33-2 (root ruling 2026-10-28): a setup that creates the assistant
// also turns on its chat alert (notify.needs_you.assistant), and a re-run on
// an existing profile changes nothing.
func TestSetupTurnsOnTheChatAlertForANewAssistantOnly(t *testing.T) {
	setupWorkspace(t)
	env := assistantEnv{
		in:   bufio.NewReader(strings.NewReader("")),
		deps: e33Deps([]string{"E33_FAKE_KEY", "TG_E33_FAKE"}, botStub{stubAdapter: stubAdapter{typ: "telegram"}, bot: "e33_fake_bot"}),
	}
	out, err := runSetupCaptured(t, setupHeadlessArgs(), env)
	if err != nil {
		t.Fatalf("runSetup: %v\n%s", err, masked(out))
	}
	if n := config.LoadFrom("rysh.config.yaml").NotifyNeedsYou; n.Assistant != assistantName {
		t.Fatalf("notify.needs_you.assistant = %q, want %q", n.Assistant, assistantName)
	}
	if !strings.Contains(out, "notify test") {
		t.Errorf("setup did not say how to check the alert:\n%s", masked(out))
	}

	// The owner turns it off; a re-run leaves their choice alone.
	if err := os.WriteFile("rysh.config.yaml", []byte("notify:\n  needs_you:\n    command: [\"true\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runSetupCaptured(t, setupHeadlessArgs(), env); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if n := config.LoadFrom("rysh.config.yaml").NotifyNeedsYou; n.Assistant != "" {
		t.Fatalf("a re-run on an existing profile set notify.needs_you.assistant = %q", n.Assistant)
	}
}
