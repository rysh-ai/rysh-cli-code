// SPDX-License-Identifier: Apache-2.0

package main

// Table tests for rysh doctor over synthetic on-disk states (design 004 §6):
// good config → PASS lines; missing key / unresolved ${VAR} / unparsable
// SKILL.md / no session → the right FAIL/WARN with actionable fix text. The
// Validator dispatch helper is tested directly with stub adapters.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// fakeDeps builds doctorDeps with a happy-path provider ping, env-only ref
// lookup, and a refusing NATS dialer — individual tests override fields.
func fakeDeps() doctorDeps {
	return doctorDeps{
		validateProvider: func(ctx context.Context, name, model, key, baseURL string) (string, error) {
			return "anthropic reachable (stub)", nil
		},
		newAdapter: channels.NewAdapter,
		dialNATS: func(port int, timeout time.Duration) error {
			return errors.New("connection refused")
		},
		lookupRef: func(name string) (string, bool) {
			// Mirror the real lookup's "empty counts as unresolved" behavior.
			v, ok := os.LookupEnv(name)
			v = strings.TrimSpace(v)
			return v, ok && v != ""
		},
		// Hermetic env for the clipboard check — a plain local terminal, so the
		// table tests don't depend on whether CI itself runs under SSH/tmux.
		getenv: stubEnv(map[string]string{"TERM": "xterm-256color"}),
	}
}

// stubEnv returns a doctorDeps.getenv backed by a map.
func stubEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// findCheck returns the first check in group whose detail contains substr.
func findCheck(checks []doctorCheck, group, substr string) (doctorCheck, bool) {
	for _, c := range checks {
		if c.Group == group && strings.Contains(c.Detail, substr) {
			return c, true
		}
	}
	return doctorCheck{}, false
}

func writeGoodState(t *testing.T, dir string) config.Config {
	t.Helper()
	cfgYAML := "provider:\n  name: anthropic\n  model: claude-sonnet-5\n  api_key: ${ANTHROPIC_API_KEY}\n"
	if err := os.WriteFile(filepath.Join(dir, "rysh.config.yaml"), []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	// R7: a "good" humanoid now declares its admission posture. Without an
	// allowlist (or an explicit pairing_policy) the channel is ungated and
	// doctor rightly WARNs, so the healthy fixture states one.
	skill := "---\nname: support\nmodel: claude-sonnet-5\ncontacts:\n  slack:\n    bot_token: \"${SLACK_BOT_TOKEN}\"\n    app_token: \"${SLACK_APP_TOKEN}\"\n    channels: [\"#support\"]\n    governance: human\n    allowlist: [\"U123OWNER\"]\n---\nYou are support.\n"
	if _, err := writeSkillFile("support", skill, false); err != nil {
		t.Fatal(err)
	}
	// Neutralize ambient overrides, then set the refs the synthetic state uses.
	for _, v := range []string{"RYSH_API_KEY", "RYSH_PROVIDER", "RYSH_MODEL", "RYSH_API_URL", "RYSH_DIR", "RYSH_SESSION_DIR"} {
		t.Setenv(v, "")
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	return config.Load()
}

// TestDoctorGoodConfig: a complete, resolvable setup yields PASS for provider
// and config, WARN only where expected (no live session; Slack cred check is
// exercised separately — here the stub adapter path is not reached because the
// real Slack adapter implements Validator, so we inject a passing validator
// via a stub adapter factory).
func TestDoctorGoodConfig(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cfg := writeGoodState(t, dir)

	deps := fakeDeps()
	// Substitute a stub adapter that validates successfully, so the test does
	// not hit the Slack API.
	deps.newAdapter = func(chType string, cc msg.ChannelConfig) (channels.ChannelAdapter, error) {
		return validatingStub{stubAdapter{typ: chType}}, nil
	}
	checks := doctorChecks(cfg, deps)

	if c, ok := findCheck(checks, "provider", "reachable"); !ok || c.Status != doctorPass {
		t.Errorf("provider check = %+v", c)
	}
	if c, ok := findCheck(checks, "channels", "support/slack"); !ok || c.Status != doctorPass {
		t.Errorf("channels check = %+v", c)
	}
	if c, ok := findCheck(checks, "daemon", "no live session"); !ok || c.Status != doctorWarn || !strings.Contains(c.Fix, "rysh attach") {
		t.Errorf("daemon check = %+v", c)
	}
	if c, ok := findCheck(checks, "config", "rysh.config.yaml parses"); !ok || c.Status != doctorPass {
		t.Errorf("config parse check = %+v", c)
	}
	if c, ok := findCheck(checks, "config", "SKILL.md parses"); !ok || c.Status != doctorPass {
		t.Errorf("skill parse check = %+v", c)
	}
	for _, c := range checks {
		if c.Status == doctorFail {
			t.Errorf("unexpected FAIL: %+v", c)
		}
	}
}

// TestDoctorMissingKey: no api_key anywhere → provider FAIL with the onboard
// fix line.
func TestDoctorMissingKey(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for _, v := range []string{"RYSH_API_KEY", "ANTHROPIC_API_KEY"} {
		t.Setenv(v, "")
	}
	cfg := config.Load()
	checks := doctorChecks(cfg, fakeDeps())
	c, ok := findCheck(checks, "provider", "no API key")
	if !ok || c.Status != doctorFail {
		t.Fatalf("provider check = %+v (ok=%v)", c, ok)
	}
	if !strings.Contains(c.Fix, "rysh onboard") {
		t.Errorf("fix = %q, want to mention rysh onboard", c.Fix)
	}
}

// TestDoctorUnresolvedRef: a ${VAR} that resolves nowhere is FAILed by name,
// with the file it appears in.
func TestDoctorUnresolvedRef(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cfg := writeGoodState(t, dir)
	skill := "---\nname: ghost\ncontacts:\n  slack:\n    bot_token: \"${TOTALLY_UNSET_VAR}\"\n    app_token: \"${SLACK_APP_TOKEN}\"\n---\nhi\n"
	if _, err := writeSkillFile("ghost", skill, false); err != nil {
		t.Fatal(err)
	}
	deps := fakeDeps()
	deps.newAdapter = func(chType string, cc msg.ChannelConfig) (channels.ChannelAdapter, error) {
		return stubAdapter{typ: chType}, nil
	}
	checks := doctorChecks(cfg, deps)
	c, ok := findCheck(checks, "config", "${TOTALLY_UNSET_VAR}")
	if !ok || c.Status != doctorFail {
		t.Fatalf("unresolved-ref check = %+v (ok=%v)", c, ok)
	}
	if !strings.Contains(c.Detail, filepath.Join(".rysh", "humanoids", "ghost", "SKILL.md")) {
		t.Errorf("detail does not name the file: %q", c.Detail)
	}
	if !strings.Contains(c.Fix, "TOTALLY_UNSET_VAR") {
		t.Errorf("fix does not name the var: %q", c.Fix)
	}
}

// TestDoctorUnparsableSkill: broken frontmatter YAML → config FAIL naming the
// file.
func TestDoctorUnparsableSkill(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cfg := writeGoodState(t, dir)
	bad := "---\nname: broken\ncontacts:\n  slack: [not a map\n---\nhi\n"
	if _, err := writeSkillFile("broken", bad, false); err != nil {
		t.Fatal(err)
	}
	deps := fakeDeps()
	deps.newAdapter = func(chType string, cc msg.ChannelConfig) (channels.ChannelAdapter, error) {
		return stubAdapter{typ: chType}, nil
	}
	checks := doctorChecks(cfg, deps)
	c, ok := findCheck(checks, "config", filepath.Join(".rysh", "humanoids", "broken", "SKILL.md"))
	if !ok || c.Status != doctorFail || !strings.Contains(c.Detail, "does not parse") {
		t.Fatalf("unparsable-skill check = %+v (ok=%v)", c, ok)
	}
	if !strings.Contains(c.Fix, "frontmatter") {
		t.Errorf("fix = %q", c.Fix)
	}
}

// TestDoctorProviderPingFailure: a failing ping is a provider FAIL carrying
// the decoded error.
func TestDoctorProviderPingFailure(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cfg := writeGoodState(t, dir)
	deps := fakeDeps()
	deps.validateProvider = func(ctx context.Context, name, model, key, baseURL string) (string, error) {
		return "", fmt.Errorf("401 — key rejected by anthropic")
	}
	checks := doctorChecks(cfg, deps)
	c, ok := findCheck(checks, "provider", "key rejected")
	if !ok || c.Status != doctorFail || !strings.Contains(c.Fix, "rysh onboard") {
		t.Fatalf("provider check = %+v (ok=%v)", c, ok)
	}
}

// stubAdapter is a minimal ChannelAdapter WITHOUT a Validate method (the
// "no non-binding validation" branch); validatingStub wraps it to add one.
type stubAdapter struct {
	typ         string
	validateErr error
}

func (s stubAdapter) Type() string                                               { return s.typ }
func (s stubAdapter) Start(ctx context.Context) error                            { return nil }
func (s stubAdapter) Stop() error                                                { return nil }
func (s stubAdapter) Send(ctx context.Context, o channels.OutboundMessage) error { return nil }
func (s stubAdapter) InboundCh() <-chan channels.InboundMessage                  { return nil }
func (s stubAdapter) Status() msg.ChannelStatus                                  { return msg.ChannelStatus{Type: s.typ} }
func (s stubAdapter) SetReplyMode(mode string)                                   {}

// validatingStub adds Validate, satisfying channels.Validator.
type validatingStub struct{ stubAdapter }

func (v validatingStub) Validate(ctx context.Context) error { return v.validateErr }

// TestValidateChannelAdapterDispatch: the optional-interface dispatch reports
// validated=false for plain adapters (→ WARN path) and surfaces Validate's
// result for implementers.
func TestValidateChannelAdapterDispatch(t *testing.T) {
	ctx := context.Background()
	if validated, _ := validateChannelAdapter(ctx, stubAdapter{typ: "phone"}); validated {
		t.Error("plain adapter reported as validated")
	}
	ok := validatingStub{stubAdapter{typ: "slack"}}
	if validated, err := validateChannelAdapter(ctx, ok); !validated || err != nil {
		t.Errorf("validating stub = %v, %v", validated, err)
	}
	bad := validatingStub{stubAdapter{typ: "slack", validateErr: errors.New("auth.test 401")}}
	if validated, err := validateChannelAdapter(ctx, bad); !validated || err == nil {
		t.Errorf("failing stub = %v, %v", validated, err)
	}
}

// TestDoctorWarnOnNoValidator: an adapter without Validate yields the
// "start the channel to fully verify" WARN, never a Start probe.
func TestDoctorWarnOnNoValidator(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cfg := writeGoodState(t, dir)
	deps := fakeDeps()
	deps.newAdapter = func(chType string, cc msg.ChannelConfig) (channels.ChannelAdapter, error) {
		return stubAdapter{typ: chType}, nil // no Validator
	}
	checks := doctorChecks(cfg, deps)
	c, ok := findCheck(checks, "channels", "no non-binding validation")
	if !ok || c.Status != doctorWarn {
		t.Fatalf("channels check = %+v (ok=%v)", c, ok)
	}
	if !strings.Contains(c.Fix, "channel start") {
		t.Errorf("fix = %q", c.Fix)
	}
}

// TestDoctorChannelValidateFailure: a failing Validate is a channels FAIL that
// names the unresolved ${ENV} when that's the likely cause.
func TestDoctorChannelValidateFailure(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	cfg := writeGoodState(t, dir)
	t.Setenv("SLACK_BOT_TOKEN", "") // now the ref is empty → validation fails
	deps := fakeDeps()
	deps.newAdapter = func(chType string, cc msg.ChannelConfig) (channels.ChannelAdapter, error) {
		return validatingStub{stubAdapter{typ: chType, validateErr: errors.New("slack: auth.test failed: invalid_auth")}}, nil
	}
	checks := doctorChecks(cfg, deps)
	c, ok := findCheck(checks, "channels", "auth.test failed")
	if !ok || c.Status != doctorFail {
		t.Fatalf("channels check = %+v (ok=%v)", c, ok)
	}
	if !strings.Contains(c.Fix, "${SLACK_BOT_TOKEN}") {
		t.Errorf("fix should name the empty ref: %q", c.Fix)
	}
}

// TestParseSkillFileMirrorsRuntime: files without frontmatter are whole-body
// prompts; the closing --- must be alone on its line.
func TestParseSkillFileMirrorsRuntime(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if _, err := writeSkillFile("plain", "just a prompt, no frontmatter", false); err != nil {
		t.Fatal(err)
	}
	fm, body, err := parseSkillFile(skillFilePath("plain"))
	if err != nil {
		t.Fatalf("parseSkillFile: %v", err)
	}
	if fm.Name != "plain" || !strings.Contains(body, "just a prompt") {
		t.Errorf("fm=%+v body=%q", fm, body)
	}
	// A "# --- section ---" divider inside frontmatter must not close it.
	content := "---\nname: divider\n# --- section ---\nmodel: m\n---\nbody\n"
	if _, err := writeSkillFile("divider", content, false); err != nil {
		t.Fatal(err)
	}
	fm, _, err = parseSkillFile(skillFilePath("divider"))
	if err != nil || fm.Model != "m" {
		t.Errorf("divider parse: fm=%+v err=%v", fm, err)
	}
}

// TestPairingUngatedMatchesRuntimeGate is the drift guard for R7. doctor's
// warning is only useful if it agrees with what HumanoidActor actually does;
// a doctor that says "ungated" while the runtime gates (or vice versa) is worse
// than no warning at all. The two implementations are separate on purpose
// (doctor must not import the actor package), so this pins them together.
func TestPairingUngatedMatchesRuntimeGate(t *testing.T) {
	cases := []struct {
		name           string
		cc             msg.ChannelConfig
		pairingDefault string
		wantUngated    bool
	}{
		{"bare + default open", msg.ChannelConfig{}, "", true},
		{"bare + default closed", msg.ChannelConfig{}, "closed", false},
		{"allowlist declared", msg.ChannelConfig{Allowlist: []string{"o"}}, "", false},
		{"policy request", msg.ChannelConfig{PairingPolicy: "request"}, "", false},
		{"policy drop", msg.ChannelConfig{PairingPolicy: "drop"}, "", false},
		{"policy open", msg.ChannelConfig{PairingPolicy: "open"}, "", false},
		{"policy open + closed default", msg.ChannelConfig{PairingPolicy: "open"}, "closed", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pairingUngated(c.cc, c.pairingDefault); got != c.wantUngated {
				t.Errorf("pairingUngated = %v, want %v", got, c.wantUngated)
			}
		})
	}
}

// TestDoctorClipboardCheck: the clipboard check classifies the terminal chain
// from env — plain terminals PASS, tmux/screen WARN with the config fix, and
// an SSH marker flips the detail to the local-clipboard-via-OSC-52 wording.
func TestDoctorClipboardCheck(t *testing.T) {
	cases := []struct {
		name       string
		env        map[string]string
		wantStatus string
		wantDetail string // substring
		wantFix    string // substring; "" = no fix expected
	}{
		{"plain local", map[string]string{"TERM": "xterm-256color"},
			doctorPass, "local session", ""},
		{"plain ssh", map[string]string{"TERM": "xterm-256color", "SSH_CONNECTION": "1.2.3.4 5 6.7.8.9 22"},
			doctorPass, "remote (SSH) session", ""},
		{"tmux wins over screen TERM", map[string]string{"TERM": "screen-256color", "TMUX": "/tmp/tmux-1000/default,1,0"},
			doctorWarn, "tmux in the chain", "set-clipboard"},
		{"screen", map[string]string{"TERM": "screen"},
			doctorWarn, "GNU screen in the chain", "--copy-test"},
		{"ssh + tmux", map[string]string{"TERM": "tmux-256color", "SSH_TTY": "/dev/pts/3"},
			doctorWarn, "remote (SSH) session", "allow-passthrough"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			deps := fakeDeps()
			deps.getenv = stubEnv(c.env)
			checks := checkClipboard(deps)
			if len(checks) != 1 {
				t.Fatalf("checkClipboard returned %d checks, want 1", len(checks))
			}
			got := checks[0]
			if got.Group != "clipboard" || got.Status != c.wantStatus {
				t.Errorf("check = %+v, want status %s", got, c.wantStatus)
			}
			if !strings.Contains(got.Detail, c.wantDetail) {
				t.Errorf("detail %q missing %q", got.Detail, c.wantDetail)
			}
			if c.wantFix == "" && got.Fix != "" {
				t.Errorf("unexpected fix on PASS: %q", got.Fix)
			}
			if c.wantFix != "" && !strings.Contains(got.Fix, c.wantFix) {
				t.Errorf("fix %q missing %q", got.Fix, c.wantFix)
			}
		})
	}
}
