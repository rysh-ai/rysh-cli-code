// SPDX-License-Identifier: Apache-2.0

package main

// assistant_cmd.go — `rysh assistant`: one-command bring-up of the single-user
// always-on personal assistant profile (openclaw_roadmap design 007, WS7
// PM1–PM3), composing what already exists:
//
//  1. provider: reuse onboarding's provider step (design 004) when none is
//     configured — flag-driven (--provider/--key-env/…) or inline prompts on a
//     TTY;
//  2. profile: scaffold .rysh/humanoids/assistant/SKILL.md (PM1) — idempotent,
//     an edited file is never clobbered;
//  3. device-bridge preset (PM2): telegram | whatsapp | signal | imessage,
//     collecting creds as ${ENV} references (secrets land in .rysh/secrets)
//     and the OWNER's own contact id as the allowlist seed (PM3 — the WS3
//     admission gate is active by construction);
//  4. daemon: reuse a live session, or prompt-then-spawn (never silent;
//     headless requires --start), then activate the humanoid over the same
//     ##humanoid command path onboarding uses;
//  5. a doctor-style summary.
//
// `rysh assistant --install-daemon` writes the OS persistence unit
// (LaunchAgent / systemd user unit) that wraps `rysh daemon <session>` —
// generated content only; loading it is prompted and best-effort (PM3 §4.5).
//
// Headless/flag form (mirrors rysh onboard, testable in a temp dir):
//
//	rysh assistant --channel telegram --owner 123456 --bot-token-env TG_TOKEN \
//	    [--provider anthropic --key-env ANTHROPIC_API_KEY --no-validate] [--start]

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// yamlUnmarshal is aliased for the small provider-block probe.
var yamlUnmarshal = yaml.Unmarshal

// runAssistant brings up the personal assistant profile: onboarding when
// unconfigured, profile scaffold, device-bridge preset, daemon bring-up.
func runAssistant(cfg config.Config, configPath string, args []string) error {
	if hasFlag(args, "--help", "-h") {
		printAssistantUsage()
		return nil
	}
	if hasFlag(args, "--install-daemon") {
		return installAssistantDaemon(cfg)
	}

	return runAssistantWith(cfg, configPath, args, assistantEnv{
		in:   bufio.NewReader(os.Stdin),
		tty:  term.IsTerminal(int(os.Stdin.Fd())),
		deps: defaultDoctorDeps(cfg),
	})
}

// assistantEnv carries the bring-up's effectful edges: the prompt reader, the
// TTY decision and the doctor deps the honest summary checks through. Tests
// inject all three; the command wires stdin and the real deps.
type assistantEnv struct {
	in   *bufio.Reader
	tty  bool
	deps doctorDeps
	// jsonOut receives `rysh setup --json`'s one summary object; nil when
	// the run is not asked for JSON.
	jsonOut io.Writer
	// daemonLive reports whether the target session's daemon is already
	// running; nil = check the session store.
	daemonLive func() bool
}

// runAssistantWith is runAssistant with its edges injected.
func runAssistantWith(cfg config.Config, configPath string, args []string, env assistantEnv) error {
	in, tty := env.in, env.tty

	fmt.Println(progname.Rewrite("rysh assistant — personal assistant bring-up (one binary, one skill file; config is project-local)"))

	// Step 1 — provider (reuse onboarding, design 004 OB1–OB2).
	prov, err := ensureAssistantProvider(cfg, configPath, args, in, tty)
	if err != nil {
		return err
	}

	// The session is this directory's own (setup_session.go).
	if cfg, err = resolveAssistantSession(cfg, configPath, args, projectDir()); err != nil {
		return err
	}

	// Step 2+3 — profile scaffold (PM1) + device-bridge preset (PM2/PM3).
	channel, owner, profilePath, created, err := ensureAssistantProfile(cfg, args, in, tty, prov.Name, prov.Model, nil)
	if err != nil {
		return err
	}

	// The channel is checked BEFORE the daemon step, so a token that cannot
	// work is not started and the summary never says PASS for it (E33 G4).
	chk := checkAssistantChannel(profilePath, channel, env.deps)

	// Step 4 — daemon + humanoid activation (never a silent spawn).
	daemonNote, err := ensureAssistantDaemon(cfg, args, in, tty, channel, owner, chk)
	if err != nil {
		return err
	}

	// Step 5 — doctor-style summary.
	printAssistantSummary(assistantSummary{
		Provider: prov, Channel: channel, Owner: owner, ProfilePath: profilePath,
		Created: created, DaemonNote: daemonNote, Check: chk,
		Binding: assistantBindingLine(cfg, profilePath, channel, chk),
	}, env.deps)
	return nil
}

func printAssistantUsage() {
	fmt.Println(progname.Rewrite("usage: rysh assistant                      guided bring-up (TTY)"))
	fmt.Println(progname.Rewrite("       rysh assistant [flags]              scriptable, no TTY needed"))
	fmt.Println(progname.Rewrite("       rysh assistant --install-daemon     write the OS auto-start unit (opt-in)"))
	fmt.Println()
	fmt.Println(progname.Rewrite("provider flags (only needed when no provider is configured; same as rysh onboard):"))
	fmt.Println("  --provider <name>        anthropic (default) | openai | ollama")
	fmt.Println("  --key-env <VAR>          env var holding the API key (written as ${VAR})")
	fmt.Println("  --key <literal>          literal key; stored in .rysh/secrets (0600)")
	fmt.Println("  --model <model>          model to write (default: provider default)")
	fmt.Println("  --no-validate            skip the live validation ping")
	fmt.Println()
	fmt.Println("assistant flags:")
	fmt.Println("  --channel <preset>       slack (recommended) | telegram | discord | whatsapp | signal | imessage")
	fmt.Println("  --owner <id>             YOUR contact id on that channel — the ONLY allowlisted sender")
	fmt.Println("                           (Slack member ID U…/W…; Discord/Telegram numeric user id;")
	fmt.Println("                           WhatsApp/Signal +E.164 number; iMessage handle)")
	fmt.Println("  --<cred> / --<cred>-env  per-preset credentials, e.g. --bot-token-env TG_TOKEN,")
	fmt.Println("                           --phone <id> --api-key-env WHATSAPP_API_KEY, --sidecar-addr <addr>")
	fmt.Println("  --overwrite-profile      regenerate an existing SKILL.md (never done implicitly)")
	fmt.Println("  --start                  spawn the session daemon + assistant now (headless consent)")
	fmt.Println("  --verify / --no-verify   wait for your first real message to arrive (default: on in a")
	fmt.Println("                           TTY when a channel started; never blocks a scripted run)")
	fmt.Println()
	fmt.Println("defaults (design 007 PM3): the allowlist is YOU only and pairing_policy is")
	fmt.Println("request — that is what keeps strangers out. The rest is permissive on purpose,")
	fmt.Println("because you reach the assistant from a chat app with nobody at the keyboard:")
	fmt.Println("governance: ai (replies post straight back), reply_mode: mentions, and")
	fmt.Println("auto_approve: true (tool calls run without an approval dialog — set it to false")
	fmt.Println("in the profile to be asked first). Hard stops belong in policy: always_gate and")
	fmt.Println("bash.deny (design 013) gate regardless of auto_approve.")
}

// ---------------------------------------------------------------------------
// Step 1 — provider
// ---------------------------------------------------------------------------

// providerOutcome is what the provider step established: the name and model
// the profile declares, the ${NAME} the config's key points at, and whether
// THIS run actually pinged the provider. The summary reports PASS only for the
// last of those (E33 G4).
type providerOutcome struct {
	Name, Model string
	KeyRef      string // e.g. "${ANTHROPIC_API_KEY}"; "" when there is none
	KeyOptional bool
	Validated   bool
}

// ensureAssistantProvider returns the provider the profile should declare,
// running the onboarding provider step first when none is configured.
func ensureAssistantProvider(cfg config.Config, configPath string, args []string, in *bufio.Reader, tty bool) (providerOutcome, error) {
	p, known := providerByName(cfg.ProviderName)
	flagged := flagVal(args, "--provider") != "" || flagVal(args, "--key-env") != "" || flagVal(args, "--key") != ""
	// Configured = a resolvable key, a key-optional provider, or an explicit
	// provider block in the config. An unresolved ${ENV} key does NOT force a
	// re-onboard (idempotent re-run) — it is reported and doctor names the
	// exact missing ${VAR}.
	configured := known && (cfg.APIKey != "" || p.KeyOptional ||
		(cfg.ConfigFile != "" && configHasProviderBlock(cfg.ConfigFile)))

	if configured && !flagged {
		model := cfg.DefaultModel
		if model == "" {
			model = p.DefaultModel
		}
		src := cfg.ConfigFile
		if src == "" {
			src = "environment"
		}
		fmt.Printf("provider: %s (model %s) — already configured (%s)\n", p.Name, model, src)
		if cfg.APIKey == "" && !p.KeyOptional {
			fmt.Printf(progname.Rewrite("WARN provider key does not resolve yet — export %s (or add it to .rysh/secrets); `rysh doctor` names the exact missing ${VAR}\n"), p.KeyEnv)
		}
		return providerOutcome{Name: p.Name, Model: model, KeyOptional: p.KeyOptional,
			KeyRef: configuredKeyRef(cfg.ConfigFile, p)}, nil
	}

	if flagged {
		// Reuse onboarding's flag-driven provider step verbatim (one code path).
		if err := onboardProviderStepHeadless(cfg, configPath, args); err != nil {
			return providerOutcome{}, err
		}
		np, _ := providerByName(flagVal(args, "--provider"))
		model := flagVal(args, "--model")
		if model == "" {
			model = np.DefaultModel
		}
		// The headless step refuses to save on a failed ping, so reaching
		// here without --no-validate means the provider answered.
		return providerOutcome{Name: np.Name, Model: model, KeyOptional: np.KeyOptional,
			KeyRef: headlessKeyRef(np, args), Validated: !hasFlag(args, "--no-validate")}, nil
	}

	if !tty {
		return providerOutcome{}, errors.New("no provider configured and no TTY for the wizard; pass the flag form:\n" +
			progname.Rewrite("  rysh assistant --provider anthropic --key-env ANTHROPIC_API_KEY --channel telegram --owner <your-id> --bot-token-env TELEGRAM_BOT_TOKEN"))
	}

	// Inline TTY provider step (the onboarding wizard's step 1, prompt-driven).
	fmt.Println("\nStep 1 — model provider (no provider configured yet)")
	provs := onboardProviders()
	for i, cand := range provs {
		def := ""
		if i == 0 {
			def = "  (default)"
		}
		fmt.Printf("  %d) %s%s\n", i+1, cand.Name, def)
	}
	pick := promptLine(in, "pick a provider", "1")
	idx, err := strconv.Atoi(strings.TrimSpace(pick))
	if err != nil || idx < 1 || idx > len(provs) {
		idx = 1
	}
	p = provs[idx-1]
	model := promptLine(in, fmt.Sprintf("model for %s", p.Name), p.DefaultModel)
	fmt.Println("API key: enter a ${ENV} reference (recommended) or paste the key; a pasted")
	fmt.Println("literal is stored in .rysh/secrets (0600) — the config only gets the reference.")
	rawKey := readSecretInput(in, "API key [${"+p.KeyEnv+"}]")
	if rawKey == "" {
		rawKey = "${" + p.KeyEnv + "}"
	}
	key := normalizeKeyInput(p, rawKey)

	validated := false
	if !hasFlag(args, "--no-validate") {
		literal, found := resolveKeyForValidation(cfg.RyshDir, key)
		valErr := error(nil)
		if !found && !p.KeyOptional {
			valErr = fmt.Errorf("reference %s does not resolve (not in .rysh/secrets or the environment)", key.Ref)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), validateProviderTimeout)
			var detail string
			detail, valErr = validateProvider(ctx, p.Name, model, literal, "")
			cancel()
			if valErr == nil {
				validated = true
				fmt.Printf("validated: %s\n", detail)
			}
		}
		if valErr != nil {
			fmt.Printf("validation failed: %v\n", valErr)
			if !promptYes(in, "save anyway?", false) {
				return providerOutcome{}, fmt.Errorf("provider validation failed: %w", valErr)
			}
		}
	}

	path, notes, err := writeProviderConfig(configPath, cfg.RyshDir, p, model, key, "")
	if err != nil {
		return providerOutcome{}, err
	}
	fmt.Printf("wrote provider block (name: %s, model: %s, api_key: %s) to %s\n", p.Name, model, key.Ref, path)
	for _, n := range notes {
		fmt.Println(n)
	}
	return providerOutcome{Name: p.Name, Model: model, KeyRef: key.Ref,
		KeyOptional: p.KeyOptional, Validated: validated}, nil
}

// headlessKeyRef is the ${NAME} the headless provider step writes for these flags —
// the same key-spec resolution, so the summary checks the reference the
// config really holds.
func headlessKeyRef(p onboardProvider, args []string) string {
	switch {
	case flagVal(args, "--key-env") != "":
		return normalizeKeyInput(p, "${"+flagVal(args, "--key-env")+"}").Ref
	case flagVal(args, "--key") != "":
		return normalizeKeyInput(p, flagVal(args, "--key")).Ref
	}
	return normalizeKeyInput(p, "${"+p.KeyEnv+"}").Ref
}

// configuredKeyRef reads provider.api_key from an existing config, falling
// back to the provider's conventional ${ENV} when the file has none.
func configuredKeyRef(configFile string, p onboardProvider) string {
	if raw, err := os.ReadFile(configFile); err == nil {
		var parsed struct {
			Provider struct {
				APIKey string `yaml:"api_key"`
			} `yaml:"provider"`
		}
		if yamlUnmarshal(raw, &parsed) == nil && parsed.Provider.APIKey != "" {
			if len(config.EnvRefNames(parsed.Provider.APIKey)) > 0 {
				return parsed.Provider.APIKey
			}
			return "" // a literal in the file: nothing to resolve
		}
	}
	if p.KeyOptional {
		return ""
	}
	return "${" + p.KeyEnv + "}"
}

// configHasProviderBlock reports whether the YAML config declares a provider
// block (regardless of whether its ${ENV} key currently resolves).
func configHasProviderBlock(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var parsed struct {
		Provider map[string]any `yaml:"provider"`
	}
	if err := yamlUnmarshal(raw, &parsed); err != nil {
		return false
	}
	return len(parsed.Provider) > 0
}

// ---------------------------------------------------------------------------
// Steps 2+3 — profile scaffold + device-bridge preset
// ---------------------------------------------------------------------------

// profileHooks lets `rysh setup` drive the same profile step with its own
// picker order, the bot-creation steps, and a credential check before anything
// is written. nil hooks is `rysh assistant`'s behaviour.
type profileHooks struct {
	presets     []assistantPreset // picker order; nil = assistantPresets()
	heading     string            // picker heading; "" = the assistant's
	beforeCreds func(channel string)
	// checkCreds checks the collected credentials before the profile is
	// written; true asks for them again.
	checkCreds func(channel string, fields []credField, values []string) (again bool)
}

// ensureAssistantProfile scaffolds (or reuses) the assistant SKILL.md and
// returns the active channel, the owner contact (as far as it is knowable),
// the profile path, and whether the file was created on this run.
func ensureAssistantProfile(cfg config.Config, args []string, in *bufio.Reader, tty bool, provName, model string, hooks *profileHooks) (channel, owner, path string, created bool, err error) {
	if hooks == nil {
		hooks = &profileHooks{}
	}
	overwrite := hasFlag(args, "--overwrite-profile")
	path = assistantSkillPath()

	if skillFileExists(assistantName) && !overwrite {
		// Idempotent re-run: reuse, never clobber an edited profile.
		channel, owner = assistantProfileFacts(path)
		fmt.Printf("\nassistant profile: %s (existing — reused; pass --overwrite-profile to regenerate)\n", path)
		if fc := flagVal(args, "--channel"); fc != "" && fc != channel {
			fmt.Printf("note: --channel %s ignored — the existing profile bridges %q; edit %s to add channels\n", fc, channel, path)
		}
		return channel, owner, path, false, nil
	}

	preset, err := pickPresetFrom(hooks.presets, hooks.heading, args, in, tty)
	if err != nil {
		return "", "", path, false, err
	}
	channel = preset.Channel

	if hooks.beforeCreds != nil {
		hooks.beforeCreds(channel)
	}
	var fields []credField
	var values []string
	for attempt := 1; ; attempt++ {
		fields, values, err = collectAssistantCreds(channel, args, in, tty)
		if err != nil {
			return "", "", path, false, err
		}
		if hooks.checkCreds == nil || !hooks.checkCreds(channel, fields, values) || attempt >= maxCredAttempts {
			break
		}
	}

	owner = strings.TrimSpace(flagVal(args, "--owner"))
	if owner == "" && tty {
		hint := ownerContactHint(channel)
		fmt.Printf("\nOwner contact: %s\n", hint.What)
		if hint.Where != "" {
			fmt.Printf("  %s\n", hint.Where)
		}
		fmt.Println("It becomes the ONLY allowlisted sender — unknown senders are held for")
		fmt.Println("approval, never answered.")
		owner = strings.TrimSpace(promptLine(in, hint.Label, ""))
	}
	if owner == "" {
		return "", "", path, false, errors.New("an owner contact id is required (--owner <id>): the assistant's allowlist is seeded with the owner ONLY (design 007 PM3)")
	}

	// Secrets typed as literals are persisted to .rysh/secrets; the skill file
	// only ever gets ${ENV} references (onboarding's G4 contract).
	fileValues, secrets := extractFieldSecrets(fields, values)
	notes, err := persistFieldSecrets(cfg.RyshDir, secrets)
	for _, n := range notes {
		fmt.Println(n)
	}
	if err != nil {
		return "", "", path, false, err
	}

	content, err := buildAssistantSkill(provName, model, channel, fields, fileValues, owner)
	if err != nil {
		return "", "", path, false, err
	}
	path, created, err = scaffoldAssistantProfile(content, overwrite)
	if err != nil {
		return "", "", path, false, err
	}
	fmt.Printf("\nassistant profile written: %s\n", path)
	fmt.Printf("  %s\n", preset.Note)
	return channel, owner, path, created, nil
}

// maxCredAttempts bounds how often a rejected credential is asked for again.
const maxCredAttempts = 3

// pickAssistantPreset resolves the device-bridge preset from --channel or the
// interactive picker, gating iMessage on the actual host OS.
func pickAssistantPreset(args []string, in *bufio.Reader, tty bool) (assistantPreset, error) {
	return pickPresetFrom(nil, "", args, in, tty)
}

// pickPresetFrom is pickAssistantPreset over a given picker order and heading
// (nil/"" = the assistant's own).
func pickPresetFrom(presets []assistantPreset, heading string, args []string, in *bufio.Reader, tty bool) (assistantPreset, error) {
	if presets == nil {
		presets = assistantPresets()
	}
	if heading == "" {
		heading = "Step 2 — bridge one of YOUR chat apps:"
	}
	names := make([]string, len(presets))
	for i, p := range presets {
		names[i] = p.Channel
	}
	name := flagVal(args, "--channel")
	if name == "" {
		if !tty {
			return assistantPreset{}, errors.New("pick a device-bridge preset with --channel " + strings.Join(names, "|"))
		}
		fmt.Println("\n" + heading)
		for i, p := range presets {
			fmt.Printf("  %d) %-24s %s\n", i+1, p.Label, p.Note)
		}
		pick := promptLine(in, "pick a preset", "1")
		idx, err := strconv.Atoi(strings.TrimSpace(pick))
		if err != nil || idx < 1 || idx > len(presets) {
			idx = 1
		}
		name = presets[idx-1].Channel
	}
	preset, ok := assistantPresetByChannel(name)
	if !ok {
		return assistantPreset{}, fmt.Errorf("unknown preset %q (valid: %s)", name, strings.Join(names, ", "))
	}
	if preset.DarwinOnly && runtime.GOOS != "darwin" {
		return assistantPreset{}, fmt.Errorf("the %s preset is macOS-host-only (it reads the local Messages database); this host is %s — pick telegram, whatsapp, or signal", preset.Channel, runtime.GOOS)
	}
	return preset, nil
}

// collectAssistantCreds gathers the preset's credential fields, flags first
// (--<key> value for plain fields, --<key>-env VAR for secrets → "${VAR}"),
// then interactive prompts on a TTY, then field defaults. A secret given as a
// flag value is refused, as rysh setup refuses it: argv is visible to every
// process. The value is never echoed back.
func collectAssistantCreds(channel string, args []string, in *bufio.Reader, tty bool) ([]credField, []string, error) {
	fields := channelCredFields(channel)
	for _, f := range fields {
		flagName := "--" + strings.ReplaceAll(f.Key, "_", "-")
		if f.EnvName != "" && flagVal(args, flagName) != "" {
			return nil, nil, fmt.Errorf(progname.Rewrite("rysh assistant does not take %s as a flag value (argv is visible to every process): use %s-env <NAME>, or type it at the hidden TTY prompt"), flagName, flagName)
		}
	}
	values := make([]string, len(fields))
	var missing []string
	for i, f := range fields {
		flagName := "--" + strings.ReplaceAll(f.Key, "_", "-")
		switch {
		case f.EnvName != "" && flagVal(args, flagName+"-env") != "":
			values[i] = "${" + flagVal(args, flagName+"-env") + "}"
		case flagVal(args, flagName) != "":
			values[i] = flagVal(args, flagName)
		case tty && f.EnvName != "":
			// A secret is read with the terminal's echo off and never shown
			// back; empty means "reference the conventional ${NAME}".
			values[i] = readSecretInput(in, f.Label+" (stored as ${"+f.EnvName+"} in .rysh/secrets; or type ${NAME})")
			if values[i] == "" {
				values[i] = "${" + f.EnvName + "}"
			}
		case tty:
			values[i] = promptLine(in, f.Label, f.Default)
		default:
			values[i] = f.Default
			if f.EnvName != "" && f.Default == "" {
				// Secret with no source in headless mode: reference the
				// conventional env var rather than failing — doctor reports an
				// unresolved ${VAR} honestly if it is never exported.
				values[i] = "${" + f.EnvName + "}"
				missing = append(missing, fmt.Sprintf("%s (export %s or pass %s-env)", f.Key, f.EnvName, flagName))
			}
		}
	}
	if len(missing) > 0 {
		fmt.Printf("note: unset credentials default to their ${ENV} reference: %s\n", strings.Join(missing, ", "))
	}
	return fields, values, nil
}

// assistantProfileFacts extracts the first bridged channel and its allowlist
// seed from an existing profile, for idempotent re-runs and the summary.
func assistantProfileFacts(path string) (channel, owner string) {
	fm, _, err := parseSkillFile(path)
	if err != nil || fm == nil || len(fm.Contacts) == 0 {
		return "", ""
	}
	channels := make([]string, 0, len(fm.Contacts))
	for ct := range fm.Contacts {
		channels = append(channels, ct)
	}
	sort.Strings(channels)
	channel = channels[0]
	if cc := fm.Contacts[channel]; len(cc.Allowlist) > 0 {
		owner = cc.Allowlist[0]
	}
	return channel, owner
}

// assistantAutoApprove reads the profile's `auto_approve:` field, defaulting to
// true when absent — the same resolution autoApproveTools applies at runtime.
// Read rather than assumed: the summary is also printed for a REUSED profile
// the owner may have edited, and a summary that states the default instead of
// the file is how a "PASS" line ends up describing a system that is not running.
func assistantAutoApprove(path string) bool {
	fm, _, err := parseSkillFile(path)
	if err != nil || fm == nil || fm.AutoApprove == nil {
		return true
	}
	return *fm.AutoApprove
}

// assistantChannelPolicy reads the governance and reply mode a channel ACTUALLY
// carries in the profile, falling back to the runtime's own defaults when a
// field is absent. The summary printed these as string literals, which held
// only for a freshly scaffolded file — an idempotent re-run reuses an edited
// profile, so the summary would confidently report defaults the file no longer
// had.
func assistantChannelPolicy(path, channel string) (governance, replyMode string) {
	governance, replyMode = "ai", "messages" // humanoid.go / slack.go defaults
	fm, _, err := parseSkillFile(path)
	if err != nil || fm == nil {
		return governance, replyMode
	}
	cc, ok := fm.Contacts[channel]
	if !ok {
		return governance, replyMode
	}
	if cc.Governance != "" {
		governance = cc.Governance
	}
	if cc.ReplyMode != "" {
		replyMode = cc.ReplyMode
	}
	return governance, replyMode
}

// ---------------------------------------------------------------------------
// Step 4 — daemon + humanoid activation
// ---------------------------------------------------------------------------

// The daemon spawn and the ## command path ensureAssistantDaemon drives; tests
// replace both, so no test starts a real daemon or reaches a real bus.
var (
	assistantSpawnDaemon = spawnDaemon
	assistantRyshCommand = cli.RyshCommand
)

// ensureAssistantDaemon reuses a live session (spawning the assistant into
// it), spawns a daemon only with explicit consent (--start, or a TTY yes), and
// otherwise prints the exact commands — never a silent spawn. Returns a
// one-line daemon status for the summary.
//
// A channel whose check FAILed (chk) is not started: its token cannot work, so
// starting it only trades the summary's FAIL for a silent phone.
//
// owner is the allowlisted owner id: only a message from them verifies the
// round trip (a stranger's is held for pairing and never answered).
func ensureAssistantDaemon(cfg config.Config, args []string, in *bufio.Reader, tty bool, channel, owner string, chk *channelCheck) (string, error) {
	sessionName := cfg.SessionName
	if sessionName == "" {
		sessionName = "default"
	}
	spawnCmd := "##humanoid spawn " + assistantName
	startCmd := fmt.Sprintf("##humanoid channel start %s %s", assistantName, channel)

	store, storeErr := session.NewStore(cfg)
	live := false
	if storeErr == nil {
		// Surface the render caveats before prompting: the consent below names
		// the session, so the user should know a desktop-app session's web
		// panes will not paint here before agreeing to open it.
		if note := sessionOpenNote(store, sessionName, session.NormalizeSource(cfg.SessionSource)); note != "" {
			fmt.Println(note)
		}
		live = sessionRecordLive(store, sessionName)
	}

	start := hasFlag(args, "--start")
	if !start && tty {
		if live {
			start = promptYes(in, fmt.Sprintf("session %q is live — activate the assistant in it now?", sessionName), true)
		} else {
			start = promptYes(in, fmt.Sprintf("spawn the session daemon %q now and start the assistant?", sessionName), true)
		}
	}
	if !start {
		fmt.Println("\nto start it later:")
		fmt.Printf(progname.Rewrite("  rysh attach %s\n"), sessionName)
		fmt.Printf("  %s\n", spawnCmd)
		fmt.Printf("  %s\n", startCmd)
		fmt.Println(progname.Rewrite("then run \"rysh doctor\" to verify the channel connects."))
		if live {
			return fmt.Sprintf("session %q is live; assistant not yet activated (commands printed)", sessionName), nil
		}
		return fmt.Sprintf("no daemon spawned (session %q); commands printed", sessionName), nil
	}

	if storeErr != nil {
		return "", fmt.Errorf("open session store: %w", storeErr)
	}
	// The session must be this directory's own, on the bus as well as in the
	// store: a same-named daemon of another project answers its subjects too.
	ours, err := checkSessionOwnership(cfg, store, sessionName, mustGetwd())
	if err != nil {
		return "", err
	}
	live = ours
	if !live {
		fmt.Printf("spawning session daemon %q…\n", sessionName)
		h, err := assistantSpawnDaemon(sessionName, cfg.LogLevel, "", false)
		if err != nil {
			return "", err
		}
		defer h.cleanup()
		if _, err := waitForSession(store, sessionName, 10*time.Second, h); err != nil {
			return "", daemonStartError(sessionName, err)
		}
	}
	// Same mechanism onboarding's step 3 uses: the ##humanoid command path.
	// Re-running `spawn` on an existing assistant replaces (re-reads the skill
	// file), so an idempotent re-run refreshes rather than duplicates.
	// The skill file goes by absolute path, so it resolves whatever the
	// daemon's working directory (a path with whitespace would split into
	// arguments; the bare name resolves in this directory's own daemon).
	spawnArg := assistantName
	if abs := filepath.Join(mustGetwd(), assistantSkillPath()); !strings.ContainsAny(abs, " \t") {
		spawnArg = abs
	}
	fmt.Printf("running %s\n", spawnCmd)
	if err := assistantRyshCommand(store, sessionName, "", "", "humanoid spawn "+spawnArg); err != nil {
		return "", fmt.Errorf("humanoid spawn: %w", err)
	}
	channelStarted := false
	switch {
	case channel != "" && chk != nil && chk.Status == doctorFail:
		fmt.Printf("not starting the %s channel: %s (fix it, then: %s)\n", channel, chk.Detail, startCmd)
	case channel != "":
		channelStarted = true
		fmt.Printf("running %s\n", startCmd)
		if err := assistantRyshCommand(store, sessionName, "", "", fmt.Sprintf("humanoid channel start %s %s", assistantName, channel)); err != nil {
			return "", fmt.Errorf("humanoid channel start: %w", err)
		}
	}
	time.Sleep(2 * time.Second)
	_ = assistantRyshCommand(store, sessionName, "", "", "humanoid channels "+assistantName)

	note := fmt.Sprintf(progname.Rewrite("assistant active under session %q (attach with: rysh attach %s)"), sessionName, sessionName)

	// RA4: prove a real message reaches the assistant before we claim success.
	// Only worth doing when a channel was actually started, and only when a
	// human is present to send the message — a scripted run must not block on
	// someone opening Telegram. --verify forces it, --no-verify skips it.
	verify := channelStarted && tty && !hasFlag(args, "--no-verify")
	if hasFlag(args, "--verify") {
		verify = channelStarted
	}
	if verify {
		if chk != nil && chk.Identity.Handle() != "" {
			fmt.Printf("\nmessage %s on %s now.\n", chk.Identity.Handle(), channel)
		}
		note += "\n        " + verifyAssistantRoundTrip(store, sessionName, channel, owner, assistantVerifyTimeout)
	}
	return note, nil
}

// ---------------------------------------------------------------------------
// Step 5 — summary
// ---------------------------------------------------------------------------

// sessionRecordLive reports whether the session's daemon is running and
// reachable: a recorded, live PID with a NATS port.
func sessionRecordLive(store *session.Store, sessionName string) bool {
	rec, err := store.Get(sessionName)
	return err == nil && rec.PID > 0 && session.ProcessAlive(rec.PID) && rec.NATSPort > 0
}

// roundTripVerified reports whether a daemon note carries the round-trip
// check's VERIFIED result (assistant_verify.go's wording).
func roundTripVerified(note string) bool {
	return strings.Contains(note, "round trip VERIFIED on ")
}

// summaryLine is one line of the doctor-style summary.
type summaryLine struct {
	Group  string   `json:"group"`
	Status string   `json:"status"`
	Detail string   `json:"detail"`
	Fixes  []string `json:"fixes,omitempty"`
}

// assistantSummary is everything the closing summary reports.
type assistantSummary struct {
	Provider    providerOutcome
	Channel     string
	Owner       string
	ProfilePath string
	Created     bool
	DaemonNote  string
	Check       *channelCheck // nil when no channel is bridged
	// ProviderCheck overrides the provider line checkProviderOutcome would
	// compute (setup adds what a live daemon can see); nil = compute it.
	ProviderCheck *channelCheck
	Binding       string // the one "which chat drives what" line (G6)
}

// printAssistantSummary prints the doctor-style summary and returns its
// lines (summaryFails counts the FAILs). Every PASS here is a check this run actually made: the
// provider only when it was pinged, the channel only when its references
// resolved AND a validator accepted them (E33 G4).
func printAssistantSummary(s assistantSummary, deps doctorDeps) (lines []summaryLine) {
	line := func(status, group, detail string, fixes ...string) {
		fmt.Printf("%-4s %-8s %s\n", status, group, detail)
		for _, f := range fixes {
			fmt.Printf("     %-8s fix: %s\n", "", f)
		}
		lines = append(lines, summaryLine{Group: group, Status: status, Detail: detail, Fixes: fixes})
	}
	state := "existing (reused)"
	if s.Created {
		state = "created"
	}
	fmt.Println("\nsummary (doctor-style):")
	pc := checkProviderOutcome(s.Provider, deps)
	if s.ProviderCheck != nil {
		pc = *s.ProviderCheck
	}
	line(pc.Status, "provider", pc.Detail, pc.Fixes...)
	autoApprove := assistantAutoApprove(s.ProfilePath)
	toolNote := "tool calls run without an approval dialog (auto_approve: true)"
	if !autoApprove {
		toolNote = "every consequential tool call is gated on your approval (auto_approve: false)"
	}
	line(doctorPass, "profile", fmt.Sprintf("%s — %s; profile: assistant — %s", s.ProfilePath, state, toolNote))
	if s.Channel != "" {
		gov, mode := assistantChannelPolicy(s.ProfilePath, s.Channel)
		note := "replies post straight back to you"
		if gov == "human" {
			note = "draft-and-confirm — nothing posts until you release it"
		}
		chk := channelCheck{Status: doctorWarn, Detail: s.Channel + " — not checked"}
		if s.Check != nil {
			chk = *s.Check
		}
		line(chk.Status, "channel", fmt.Sprintf("%s; governance: %s (%s), reply_mode: %s", chk.Detail, gov, note, mode), chk.Fixes...)
	}
	if s.Owner != "" {
		line(doctorPass, "pairing", fmt.Sprintf("allowlist = {%s} (you only); unknown senders become pending requests", s.Owner))
	} else {
		line(doctorWarn, "pairing", fmt.Sprintf("no allowlist seed found in the profile — add your contact id under contacts.%s.allowlist", s.Channel))
	}
	line(doctorInfo, "daemon", s.DaemonNote)
	if s.Binding != "" {
		line(doctorInfo, "binding", s.Binding)
	}
	fmt.Println()
	fmt.Println("what you'll see: DM your bot (or @mention it in a channel) and it answers you.")
	if autoApprove {
		fmt.Println("It runs tools without asking. Set auto_approve: false in the profile to be")
		fmt.Println("asked first; policy always_gate / bash.deny gate regardless of that flag.")
	} else {
		fmt.Println("Before it runs a tool, you get an APPROVAL request — in the chat and in the")
		fmt.Println("pane — and reply \"yes\" to release it or \"no\" to reject.")
	}
	fmt.Println(progname.Rewrite("verify anytime: rysh doctor   |   OS auto-start: rysh assistant --install-daemon"))
	return lines
}

// summaryFails counts the FAIL lines of a summary.
func summaryFails(lines []summaryLine) (n int) {
	for _, l := range lines {
		if l.Status == doctorFail {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Honest checks behind the summary (E33 G4) and the binding line (E33 G6)
// ---------------------------------------------------------------------------

// channelCheck is one checked fact: the provider or a bridged channel.
type channelCheck struct {
	Status string // doctorPass | doctorWarn | doctorFail
	Detail string
	Fixes  []string
	Bot    string // the bot username the validator reported, no "@"; "" = unknown
	// Identity is everything the validator learned about the bot account
	// (channels.IdentityValidator); zero when it reported nothing.
	Identity channels.BotIdentity
}

// checkProviderOutcome turns the provider step's outcome into one line:
// FAIL when the key reference does not resolve, PASS only when this run
// pinged the provider, WARN otherwise.
func checkProviderOutcome(po providerOutcome, deps doctorDeps) channelCheck {
	what := fmt.Sprintf("%s (model %s)", po.Name, po.Model)
	var missing []string
	for _, name := range config.EnvRefNames(po.KeyRef) {
		if _, ok := deps.lookupRef(name); !ok {
			missing = append(missing, name)
		}
	}
	switch {
	case len(missing) > 0 && !po.KeyOptional:
		c := channelCheck{Status: doctorFail,
			Detail: fmt.Sprintf("%s — key ${%s} does not resolve", what, strings.Join(missing, "}, ${"))}
		for _, name := range missing {
			c.Fixes = append(c.Fixes, secretRefFix(name))
		}
		return c
	case po.Validated:
		return channelCheck{Status: doctorPass, Detail: what + " — validated by this run"}
	}
	return channelCheck{Status: doctorWarn,
		Detail: what + progname.Rewrite(" — not pinged by this run; `rysh doctor` checks it")}
}

// checkAssistantChannel checks the channel the profile bridges, or returns nil
// when there is no channel to check.
func checkAssistantChannel(profilePath, channel string, deps doctorDeps) *channelCheck {
	if channel == "" {
		return nil
	}
	fm, _, err := parseSkillFile(profilePath)
	if err != nil || fm == nil {
		return &channelCheck{Status: doctorFail, Detail: fmt.Sprintf("%s — cannot read %s: %v", channel, profilePath, err),
			Fixes: []string{"fix the YAML frontmatter in " + profilePath}}
	}
	cc, ok := fm.Contacts[channel]
	if !ok {
		return &channelCheck{Status: doctorFail, Detail: fmt.Sprintf("%s — %s has no contacts.%s block", channel, profilePath, channel),
			Fixes: []string{"add the block, or re-run with --overwrite-profile"}}
	}
	c := checkChannelConfig(channel, cc, deps)
	return &c
}

// checkChannelConfig is the doctor channel check for one config: every
// ${NAME} must resolve (through deps.lookupRef, doctor's precedence), then the
// adapter's non-binding validator — when it has one — must accept the
// credentials. No value is ever printed: an error text that echoes one is
// redacted to its ${NAME} first.
func checkChannelConfig(channel string, cc msg.ChannelConfig, deps doctorDeps) channelCheck {
	resolved, missing, err := resolveChannelConfigRefs(cc, deps.lookupRef)
	if err != nil {
		return channelCheck{Status: doctorFail, Detail: fmt.Sprintf("%s — cannot resolve the channel config: %v", channel, err)}
	}
	if len(missing) > 0 {
		c := channelCheck{Status: doctorFail,
			Detail: fmt.Sprintf("%s — ${%s} does not resolve, so the channel cannot connect", channel, strings.Join(missing, "}, ${"))}
		for _, name := range missing {
			c.Fixes = append(c.Fixes, secretRefFix(name))
		}
		return c
	}
	adapter, err := deps.newAdapter(channel, resolved)
	if err != nil {
		return channelCheck{Status: doctorFail,
			Detail: fmt.Sprintf("%s — %s", channel, redactResolvedRefs(err.Error(), cc, deps.lookupRef))}
	}
	ctx, cancel := context.WithTimeout(context.Background(), validateProviderTimeout)
	validated, id, verr := validateChannelIdentity(ctx, adapter)
	cancel()
	switch {
	case !validated:
		return channelCheck{Status: doctorWarn,
			Detail: fmt.Sprintf("%s — credentials resolve; %s has no non-binding check, so they are verified only when the channel starts", channel, channel)}
	case verr != nil && channelUnreachable(verr):
		// The token was never judged: do not send the user to replace it.
		return channelCheck{Status: doctorFail,
			Detail: fmt.Sprintf("%s — could not reach %s to check the credentials: %s", channel, channel, redactResolvedRefs(verr.Error(), cc, deps.lookupRef)),
			Fixes:  []string{progname.Rewrite("check the network (DNS, proxy, firewall), then run rysh doctor — the token itself was not judged")}}
	case verr != nil:
		return channelCheck{Status: doctorFail,
			Detail: fmt.Sprintf("%s — credentials rejected: %s", channel, redactResolvedRefs(verr.Error(), cc, deps.lookupRef)),
			Fixes:  []string{fmt.Sprintf(progname.Rewrite("replace the %s token (re-run rysh setup, or ##secret new <NAME> <value>), then rysh doctor"), channel)}}
	}
	detail := channel + " — credentials valid"
	if id.Handle() != "" {
		detail += " (" + id.Handle() + ")"
	}
	return channelCheck{Status: doctorPass, Detail: detail, Bot: id.Username, Identity: id}
}

// redactResolvedRefs replaces every resolved ${NAME} value of cc that appears
// in text with the reference itself, so an adapter error that quotes a token
// (a request URL, say) cannot carry it to the terminal.
func redactResolvedRefs(text string, cc msg.ChannelConfig, lookup func(string) (string, bool)) string {
	raw, err := yaml.Marshal(cc)
	if err != nil {
		return text
	}
	for _, name := range config.EnvRefNames(string(raw)) {
		if v, ok := lookup(name); ok && v != "" {
			text = strings.ReplaceAll(text, v, "${"+name+"}")
		}
	}
	return text
}

// assistantBindingLine is the G6 line for the assistant profile's channel, or
// "" when no channel is bridged.
func assistantBindingLine(cfg config.Config, profilePath, channel string, chk *channelCheck) string {
	if channel == "" {
		return ""
	}
	return bindingLine(channel, checkedBot(chk), assistantName, profileAllowlist(profilePath, channel), sessionNameOrDefault(cfg), projectDir())
}

// checkedBot is the bot username a channel check reported, or "".
func checkedBot(chk *channelCheck) string {
	if chk == nil {
		return ""
	}
	return chk.Bot
}

// profileAllowlist is the channel's allowlist in the profile at path.
func profileAllowlist(path, channel string) []string {
	if fm, _, err := parseSkillFile(path); err == nil && fm != nil {
		return fm.Contacts[channel].Allowlist
	}
	return nil
}

// bindingLine says which chat drives what, in one line (E33 G6):
//
//	telegram @<bot> → assistant (owner <id>) → session <name> in <dir>; drives panes via pane_send
//
// A chat reaches the humanoid, which drives the session's panes through its
// pane_send tool; no chat is bound to a pane directly.
func bindingLine(channel, bot, humanoid string, allowlist []string, sessionName, dir string) string {
	who := channel + " (bot not validated)"
	if bot != "" {
		who = channel + " @" + bot
	}
	gate := "(no allowlist)"
	switch len(allowlist) {
	case 0:
	case 1:
		gate = "(owner " + allowlist[0] + ")"
	default:
		gate = "(allowlist " + strings.Join(allowlist, ", ") + ")"
	}
	return fmt.Sprintf("%s → %s %s → session %s in %s; drives panes via pane_send", who, humanoid, gate, sessionName, dir)
}

// sessionNameOrDefault is the session setup and doctor talk about.
func sessionNameOrDefault(cfg config.Config) string {
	if cfg.SessionName == "" {
		return "default"
	}
	return cfg.SessionName
}

// projectDir is the directory the project-local config applies to.
func projectDir() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// ---------------------------------------------------------------------------
// --install-daemon (PM3, design 007 §4.5)
// ---------------------------------------------------------------------------

// installAssistantDaemon writes the OS persistence unit wrapping
// `rysh daemon <session>`. Writing is unconditional (the unit is inert until
// loaded); actually loading it (launchctl / systemctl) is prompted, opt-in,
// and best-effort.
func installAssistantDaemon(cfg config.Config) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	unitPath := assistantDaemonUnitPath(runtime.GOOS, home)
	if unitPath == "" {
		return fmt.Errorf("--install-daemon supports macOS (LaunchAgent) and Linux (systemd user unit); %s has no supported unit — run `rysh daemon <session>` under your own supervisor", runtime.GOOS)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	sessionName := cfg.SessionName
	if sessionName == "" {
		sessionName = "default"
	}

	var content string
	if runtime.GOOS == "darwin" {
		content = assistantLaunchdPlist(exe, sessionName, workDir)
	} else {
		content = assistantSystemdUnit(exe, sessionName, workDir)
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return fmt.Errorf("create unit directory: %w", err)
	}
	if err := os.WriteFile(unitPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	fmt.Printf("wrote %s\n", unitPath)
	fmt.Printf("it runs: %s daemon %s   (in %s — rysh state is project-local)\n", exe, sessionName, workDir)

	tty := term.IsTerminal(int(os.Stdin.Fd()))
	in := bufio.NewReader(os.Stdin)
	if runtime.GOOS == "darwin" {
		if tty && promptYes(in, "load it now with launchctl?", false) {
			runBestEffort("launchctl", "load", "-w", unitPath)
		} else {
			fmt.Printf("load it with: launchctl load -w %s\n", unitPath)
		}
		return nil
	}
	if tty && promptYes(in, "enable it now with systemctl --user?", false) {
		runBestEffort("systemctl", "--user", "daemon-reload")
		runBestEffort("systemctl", "--user", "enable", "--now", "rysh-daemon.service")
	} else {
		fmt.Println("enable it with: systemctl --user daemon-reload && systemctl --user enable --now rysh-daemon.service")
	}
	fmt.Println("to keep it running after logout: loginctl enable-linger $USER")
	return nil
}

// runBestEffort runs a command, reporting (not failing on) errors — loading
// the unit is a convenience on top of the written file.
func runBestEffort(name string, args ...string) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("%s %s failed (unit file is written; load it manually): %v\n%s", name, strings.Join(args, " "), err, out)
		return
	}
	fmt.Printf("%s %s: ok\n", name, strings.Join(args, " "))
}

// ---------------------------------------------------------------------------
// Small prompt helpers (plain stdin — no TUI dependency for this flow)
// ---------------------------------------------------------------------------

// readSecretInput reads one secret line. On a real terminal the echo is off
// (term.ReadPassword) so the value never appears on screen or in a scrollback;
// otherwise — piped input, tests — it reads the line from in. Tests replace it.
var readSecretInput = func(in *bufio.Reader, label string) string {
	fmt.Printf("%s (input hidden): ", label)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Println()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

// promptLine reads one trimmed line with a default.
func promptLine(in *bufio.Reader, label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, _ := in.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// promptYes asks a yes/no question with a default.
func promptYes(in *bufio.Reader, label string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	line, _ := func() (string, error) {
		fmt.Printf("%s [%s]: ", label, hint)
		return in.ReadString('\n')
	}()
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default:
		return def
	}
}
