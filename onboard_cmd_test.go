package main

// Tests for the rysh onboard building blocks (design 004 §6): structured
// provider-block writes that preserve unrelated keys, SKILL.md generation that
// round-trips through the humanoid frontmatter format, secret handling that
// never emits a literal, idempotent re-runs, and the flag-driven end-to-end
// path. The Bubble Tea model is exercised through its pure step helpers (no
// teatest dependency).

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// chdirTemp moves the test into a fresh temp dir (project-local .rysh layout)
// and neutralizes ambient env overrides that would leak into config loading.
func chdirTemp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	for _, v := range []string{"RYSH_API_KEY", "ANTHROPIC_API_KEY", "RYSH_PROVIDER", "RYSH_MODEL", "RYSH_API_URL", "RYSH_DIR", "RYSH_SESSION_DIR"} {
		t.Setenv(v, "")
	}
	return dir
}

// TestWriteProviderConfigPreservesUnrelatedKeys: the provider write is a
// structured YAML round-trip — unrelated keys and comments survive, and the
// result loads back through config.LoadFrom.
func TestWriteProviderConfigPreservesUnrelatedKeys(t *testing.T) {
	dir := chdirTemp(t)
	path := filepath.Join(dir, "rysh.config.yaml")
	initial := "# hand-written config\n" +
		"rysh:\n" +
		"  session_name: mysess\n" +
		"ui:\n" +
		"  initial_tabs: 3\n" +
		"provider:\n" +
		"  name: claude\n" +
		"  max_tokens: 2048\n"
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	p, _ := providerByName("anthropic")
	key := normalizeKeyInput(p, "${ANTHROPIC_API_KEY}")
	wrote, _, err := writeProviderConfig(path, filepath.Join(dir, ".rysh"), p, "claude-sonnet-5", key, "")
	if err != nil {
		t.Fatalf("writeProviderConfig: %v", err)
	}
	if wrote != path {
		t.Errorf("wrote to %q, want %q", wrote, path)
	}

	raw, _ := os.ReadFile(path)
	for _, want := range []string{"# hand-written config", "session_name: mysess", "initial_tabs: 3", "max_tokens: 2048"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("unrelated content %q lost:\n%s", want, raw)
		}
	}

	t.Setenv("ANTHROPIC_API_KEY", "sk-test-roundtrip")
	cfg := config.LoadFrom(path)
	if cfg.ProviderName != "anthropic" {
		t.Errorf("ProviderName = %q, want anthropic", cfg.ProviderName)
	}
	if cfg.DefaultModel != "claude-sonnet-5" {
		t.Errorf("DefaultModel = %q, want claude-sonnet-5", cfg.DefaultModel)
	}
	// The ${ENV} reference resolves at load time — never the literal in YAML.
	if cfg.APIKey != "sk-test-roundtrip" {
		t.Errorf("APIKey = %q, want resolved env value", cfg.APIKey)
	}
	if cfg.MaxTokens != 2048 {
		t.Errorf("MaxTokens = %d, want preserved 2048", cfg.MaxTokens)
	}
}

// TestSkillMarkdownRoundTrip: the generated SKILL.md parses back through the
// same frontmatter contract parseHumanoidFile reads (mirrored by
// parseSkillFile), with the contacts block, governance default, and the WS3
// allowlist key present in the raw YAML.
func TestSkillMarkdownRoundTrip(t *testing.T) {
	chdirTemp(t)
	fields := channelCredFields("slack")
	values := []string{"${SLACK_BOT_TOKEN}", "${SLACK_APP_TOKEN}", "#support, #eng"}
	content, err := buildSkillMarkdown(skillSpec{
		Name:        "support",
		Description: "First humanoid created by rysh onboard",
		Model:       "claude-sonnet-5",
		Channel:     "slack",
		Fields:      fields,
		Values:      values,
		Allowlist:   []string{"U123456"},
	})
	if err != nil {
		t.Fatalf("buildSkillMarkdown: %v", err)
	}
	path, err := writeSkillFile("support", content, false)
	if err != nil {
		t.Fatalf("writeSkillFile: %v", err)
	}

	fm, body, err := parseSkillFile(path)
	if err != nil {
		t.Fatalf("parseSkillFile: %v", err)
	}
	if fm.Name != "support" || fm.Model != "claude-sonnet-5" {
		t.Errorf("frontmatter = %+v", fm)
	}
	cc, ok := fm.Contacts["slack"]
	if !ok {
		t.Fatalf("no slack contact block: %+v", fm.Contacts)
	}
	if cc.BotToken != "${SLACK_BOT_TOKEN}" || cc.AppToken != "${SLACK_APP_TOKEN}" {
		t.Errorf("tokens = %q / %q, want ${ENV} references", cc.BotToken, cc.AppToken)
	}
	if len(cc.Channels) != 2 || cc.Channels[0] != "#support" || cc.Channels[1] != "#eng" {
		t.Errorf("channels = %v", cc.Channels)
	}
	if cc.Governance != "human" {
		t.Errorf("governance = %q, want human (start-careful default)", cc.Governance)
	}
	if strings.TrimSpace(body) == "" {
		t.Error("system prompt body is empty")
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "allowlist:") || !strings.Contains(string(raw), "U123456") {
		t.Errorf("allowlist seed missing:\n%s", raw)
	}
}

// TestSkillMarkdownEmailNesting: email creds land under contacts.email.config
// with integer ports, matching msg.EmailChannelConfig's typed fields.
func TestSkillMarkdownEmailNesting(t *testing.T) {
	chdirTemp(t)
	fields := channelCredFields("email")
	values := []string{"gmail", "support@example.com", "imap.gmail.com", "993", "smtp.gmail.com", "587", "${EMAIL_USER}", "${EMAIL_PASS}"}
	content, err := buildSkillMarkdown(skillSpec{Name: "mailbot", Channel: "email", Fields: fields, Values: values})
	if err != nil {
		t.Fatalf("buildSkillMarkdown: %v", err)
	}
	path, err := writeSkillFile("mailbot", content, false)
	if err != nil {
		t.Fatalf("writeSkillFile: %v", err)
	}
	fm, _, err := parseSkillFile(path)
	if err != nil {
		t.Fatalf("parseSkillFile: %v", err)
	}
	cc := fm.Contacts["email"]
	if cc.EmailType != "gmail" {
		t.Errorf("EmailType = %q", cc.EmailType)
	}
	if cc.EmailConfig == nil {
		t.Fatal("EmailConfig not nested under config:")
	}
	if cc.EmailConfig.IMAPPort != 993 || cc.EmailConfig.SMTPPort != 587 {
		t.Errorf("ports = %d/%d, want 993/587", cc.EmailConfig.IMAPPort, cc.EmailConfig.SMTPPort)
	}
	if cc.EmailConfig.Password != "${EMAIL_PASS}" {
		t.Errorf("password = %q, want reference", cc.EmailConfig.Password)
	}
}

// TestSecretHandlingNeverEmitsLiteral: a typed literal key/cred goes to the
// .rysh/secrets tier (0600) and every generated file only carries the ${ENV}
// reference — asserted with a raw-secret scan over the generated files.
func TestSecretHandlingNeverEmitsLiteral(t *testing.T) {
	dir := chdirTemp(t)
	const rawSecret = "sk-ant-VERYSECRET123"
	secretPat := regexp.MustCompile(regexp.QuoteMeta(rawSecret))

	// Provider key literal.
	p, _ := providerByName("anthropic")
	key := normalizeKeyInput(p, rawSecret)
	if key.Ref != "${ANTHROPIC_API_KEY}" || key.Literal != rawSecret {
		t.Fatalf("normalizeKeyInput = %+v", key)
	}
	cfgPath := filepath.Join(dir, "rysh.config.yaml")
	ryshDir := filepath.Join(dir, ".rysh")
	if _, _, err := writeProviderConfig(cfgPath, ryshDir, p, "claude-sonnet-5", key, ""); err != nil {
		t.Fatalf("writeProviderConfig: %v", err)
	}

	// Channel cred literal.
	fields := channelCredFields("slack")
	fileValues, secrets := extractFieldSecrets(fields, []string{"xoxb-LITERAL-BOT", "${SLACK_APP_TOKEN}", "#support"})
	if fileValues[0] != "${SLACK_BOT_TOKEN}" {
		t.Errorf("literal bot token not converted to reference: %q", fileValues[0])
	}
	if fileValues[1] != "${SLACK_APP_TOKEN}" {
		t.Errorf("reference mangled: %q", fileValues[1])
	}
	if secrets["SLACK_BOT_TOKEN"] != "xoxb-LITERAL-BOT" {
		t.Errorf("secrets = %v", secrets)
	}
	if _, err := persistFieldSecrets(ryshDir, secrets); err != nil {
		t.Fatalf("persistFieldSecrets: %v", err)
	}
	content, err := buildSkillMarkdown(skillSpec{Name: "support", Channel: "slack", Fields: fields, Values: fileValues})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeSkillFile("support", content, false); err != nil {
		t.Fatal(err)
	}

	// No generated YAML/markdown contains a raw secret.
	for _, f := range []string{cfgPath, skillFilePath("support")} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if secretPat.Match(raw) || strings.Contains(string(raw), "xoxb-LITERAL-BOT") {
			t.Errorf("raw secret leaked into %s:\n%s", f, raw)
		}
	}

	// The literals live in the secrets tier with 0600 perms.
	keyFile := filepath.Join(ryshDir, "secrets", "default", "ANTHROPIC_API_KEY")
	if got, err := os.ReadFile(keyFile); err != nil || string(got) != rawSecret {
		t.Errorf("secret file %s = %q, %v", keyFile, got, err)
	}
	if info, err := os.Stat(keyFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("secret file mode = %v, %v; want 0600", info.Mode(), err)
	}
	if _, err := os.Stat(filepath.Join(ryshDir, "secrets", ".gitignore")); err != nil {
		t.Error("protective .gitignore missing from secrets root")
	}
	// And the stored literal resolves back through the config-level lookup.
	if v, ok := config.LookupSecretRef(ryshDir, "SLACK_BOT_TOKEN"); !ok || v != "xoxb-LITERAL-BOT" {
		t.Errorf("LookupSecretRef = %q, %v", v, ok)
	}
}

// TestOnboardHeadlessEndToEnd: the flag-driven path writes a loadable config
// in a fresh directory without a TTY, and a re-run is idempotent (no
// duplicated provider blocks, unrelated keys intact).
func TestOnboardHeadlessEndToEnd(t *testing.T) {
	dir := chdirTemp(t)
	cfg := config.Load() // fresh dir: defaults, RyshDir = <dir>/.rysh

	args := []string{"--provider", "anthropic", "--key-env", "ANTHROPIC_API_KEY", "--model", "claude-sonnet-5", "--no-validate"}
	if err := runOnboardHeadless(cfg, "", args); err != nil {
		t.Fatalf("runOnboardHeadless: %v", err)
	}
	path := filepath.Join(dir, "rysh.config.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	if !strings.Contains(string(raw), "api_key: ${ANTHROPIC_API_KEY}") {
		t.Errorf("api_key reference missing:\n%s", raw)
	}

	// Re-run (different model) — still exactly one provider block.
	if err := runOnboardHeadless(config.Load(), "", []string{"--provider", "anthropic", "--model", "claude-opus-4-8", "--no-validate"}); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	raw, _ = os.ReadFile(path)
	if got := strings.Count(string(raw), "provider:"); got != 1 {
		t.Errorf("provider block duplicated (%d occurrences):\n%s", got, raw)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-live")
	loaded := config.LoadFrom(path)
	if loaded.ProviderName != "anthropic" || loaded.DefaultModel != "claude-opus-4-8" || loaded.APIKey != "sk-live" {
		t.Errorf("loaded = provider %q model %q key %q", loaded.ProviderName, loaded.DefaultModel, loaded.APIKey)
	}
}

// TestOnboardHeadlessValidatesRefResolution: without --no-validate, an
// unresolvable key reference fails fast with guidance instead of pinging with
// an empty key.
func TestOnboardHeadlessValidatesRefResolution(t *testing.T) {
	chdirTemp(t)
	cfg := config.Load()
	err := runOnboardHeadless(cfg, "", []string{"--provider", "anthropic", "--key-env", "DOES_NOT_EXIST_XYZ"})
	if err == nil || !strings.Contains(err.Error(), "does not resolve") {
		t.Fatalf("err = %v, want unresolved-reference failure", err)
	}
}

// TestSkillFileIdempotency: existing SKILL.md is never silently clobbered —
// writeSkillFile refuses without overwrite, and nextFreeHumanoidName suggests
// a free rename target.
func TestSkillFileIdempotency(t *testing.T) {
	chdirTemp(t)
	if _, err := writeSkillFile("support", "---\nname: support\n---\nhi\n", false); err != nil {
		t.Fatal(err)
	}
	if _, err := writeSkillFile("support", "other", false); err == nil {
		t.Fatal("second write without overwrite succeeded; want refusal")
	}
	if got := nextFreeHumanoidName("support"); got != "support-2" {
		t.Errorf("nextFreeHumanoidName = %q, want support-2", got)
	}
	if _, err := writeSkillFile("support-2", "---\nname: support-2\n---\nhi\n", false); err != nil {
		t.Fatal(err)
	}
	if got := nextFreeHumanoidName("support"); got != "support-3" {
		t.Errorf("nextFreeHumanoidName = %q, want support-3", got)
	}
	// Overwrite is an explicit choice.
	if _, err := writeSkillFile("support", "---\nname: support\n---\nnew\n", true); err != nil {
		t.Errorf("explicit overwrite failed: %v", err)
	}
}

// TestNormalizeKeyInput covers the pure key-normalization branches.
func TestNormalizeKeyInput(t *testing.T) {
	p, _ := providerByName("openai")
	cases := []struct {
		raw  string
		want keySpec
	}{
		{"", keySpec{}},
		{"${MY_KEY}", keySpec{Ref: "${MY_KEY}", EnvName: "MY_KEY"}},
		{"  ${MY_KEY}  ", keySpec{Ref: "${MY_KEY}", EnvName: "MY_KEY"}},
		{"sk-literal", keySpec{Ref: "${OPENAI_API_KEY}", EnvName: "OPENAI_API_KEY", Literal: "sk-literal"}},
	}
	for _, c := range cases {
		if got := normalizeKeyInput(p, c.raw); got != c.want {
			t.Errorf("normalizeKeyInput(%q) = %+v, want %+v", c.raw, got, c.want)
		}
	}
}

// TestProviderRegistryAndPickerOrder: anthropic is the default; the channel
// picker leads with slack and email and covers every ValidChannelTypes entry.
func TestProviderRegistryAndPickerOrder(t *testing.T) {
	provs := onboardProviders()
	if len(provs) == 0 || provs[0].Name != "anthropic" {
		t.Errorf("providers = %+v, want anthropic first", provs)
	}
	if p, ok := providerByName("claude"); !ok || p.Name != "anthropic" {
		t.Errorf("claude alias = %+v, %v", p, ok)
	}
	chs := onboardChannelTypes()
	if chs[0] != "slack" || chs[1] != "email" {
		t.Errorf("channel order = %v, want slack, email first", chs)
	}
	// Every valid channel type is offered. "phone" was excluded while it was a
	// placeholder that sent and received nothing (X3); the Twilio transport
	// landed in B12, so withholding it now would hide a working channel.
	if len(chs) != len(channels.ValidChannelTypes) {
		t.Errorf("channel picker has %d entries, want all %d valid types: %v",
			len(chs), len(channels.ValidChannelTypes), chs)
	}
	for _, want := range channels.ValidChannelTypes {
		if !slices.Contains(chs, want) {
			t.Errorf("channel picker omits %q", want)
		}
	}
	for _, ch := range chs {
		if ch != "imessage" && ch != "chatbot" && ch != "signal" && len(channelCredFields(ch)) == 0 {
			t.Errorf("channel %q has no cred fields", ch)
		}
	}
}

// TestMoveCursor covers the pure picker-navigation helper.
func TestMoveCursor(t *testing.T) {
	cases := []struct{ cur, delta, n, want int }{
		{0, -1, 3, 0}, {0, 1, 3, 1}, {2, 1, 3, 2}, {1, -1, 3, 0}, {0, 1, 0, 0},
	}
	for _, c := range cases {
		if got := moveCursor(c.cur, c.delta, c.n); got != c.want {
			t.Errorf("moveCursor(%d,%d,%d) = %d, want %d", c.cur, c.delta, c.n, got, c.want)
		}
	}
}

// TestParseLayoutSpec covers the TO2 layout input: several separators are
// accepted, and anything unparseable KEEPS the current value rather than
// silently resetting a layout the user did not ask to change.
func TestParseLayoutSpec(t *testing.T) {
	cases := []struct {
		in                  string
		curTabs, curPanes   int
		wantTabs, wantPanes int
	}{
		{"2x3", 1, 1, 2, 3},
		{"2X3", 1, 1, 2, 3},
		{"2 3", 1, 1, 2, 3},
		{"2,3", 1, 1, 2, 3},
		{" 2 x 3 ", 1, 1, 2, 3},
		{"4", 1, 1, 4, 1},       // tabs only — panes keep current
		{"", 2, 5, 2, 5},        // empty keeps both
		{"garbage", 2, 5, 2, 5}, // unparseable keeps both
		{"0x0", 2, 5, 2, 5},     // non-positive is ignored, not written
		{"-1x-1", 2, 5, 2, 5},
	}
	for _, tc := range cases {
		gotTabs, gotPanes := parseLayoutSpec(tc.in, tc.curTabs, tc.curPanes)
		if gotTabs != tc.wantTabs || gotPanes != tc.wantPanes {
			t.Errorf("parseLayoutSpec(%q, %d, %d) = %d,%d want %d,%d",
				tc.in, tc.curTabs, tc.curPanes, gotTabs, gotPanes, tc.wantTabs, tc.wantPanes)
		}
	}
}

// TestOnboardWizardHasNoChannelSteps is the design-008 TO1 guard: terminal
// onboarding must not ask a channel question. If someone reintroduces a
// humanoid/channel step here, remote setup has leaked back into the wrong
// command and this fails.
func TestOnboardWizardHasNoChannelSteps(t *testing.T) {
	if stepDone <= stepLaunch {
		t.Fatal("stepDone must be the final step")
	}
	// The wizard's step space is provider → prefs → launch. Any channel or
	// humanoid step would have to live in here.
	if int(stepDone) != 8 {
		t.Errorf("unexpected step count %d — did a channel/humanoid step come back?", int(stepDone))
	}
}

// TestOnboardWritesTerminalPrefs drives the wizard's own write path (TO2) —
// not just config.SetUIBlock underneath it — so a regression in how the model
// passes its collected values is caught. The TUI itself cannot be exercised
// without a TTY, which makes this seam the thing worth asserting.
func TestOnboardWritesTerminalPrefs(t *testing.T) {
	dir := chdirTemp(t)
	path := filepath.Join(dir, "rysh.config.yaml")
	if err := os.WriteFile(path, []byte("provider:\n  name: anthropic\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &onboardModel{
		cfg:          config.Load(),
		configPath:   path,
		shell:        "/bin/fish",
		initialTabs:  2,
		initialPanes: 3,
	}
	m.outcome.ConfigPath = path
	if err := m.writeTerminalPrefs(); err != nil {
		t.Fatalf("writeTerminalPrefs: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{"shell: /bin/fish", "initial_tabs: 2", "initial_panes: 3", "name: anthropic"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in written config:\n%s", want, got)
		}
	}
	if len(m.notes) == 0 {
		t.Error("expected a summary note for the user")
	}

	// The written config must load through the real loader with the values in
	// effect — the point of the step is that the user never edits YAML.
	cfg := config.LoadFrom(path)
	if cfg.DefaultShell != "/bin/fish" {
		t.Errorf("DefaultShell = %q, want /bin/fish", cfg.DefaultShell)
	}
	if cfg.InitialTabs != 2 || cfg.InitialPanes != 3 {
		t.Errorf("layout = %dx%d, want 2x3", cfg.InitialTabs, cfg.InitialPanes)
	}
}
