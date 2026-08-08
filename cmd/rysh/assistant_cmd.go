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

	"github.com/rysh-ai/rysh-cli-code/internal/cli"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
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

	in := bufio.NewReader(os.Stdin)
	tty := term.IsTerminal(int(os.Stdin.Fd()))

	fmt.Println(progname.Rewrite("rysh assistant — personal assistant bring-up (one binary, one skill file; config is project-local)"))

	// Step 1 — provider (reuse onboarding, design 004 OB1–OB2).
	provName, model, err := ensureAssistantProvider(cfg, configPath, args, in, tty)
	if err != nil {
		return err
	}

	// Step 2+3 — profile scaffold (PM1) + device-bridge preset (PM2/PM3).
	channel, owner, profilePath, created, err := ensureAssistantProfile(cfg, args, in, tty, provName, model)
	if err != nil {
		return err
	}

	// Step 4 — daemon + humanoid activation (never a silent spawn).
	daemonNote, err := ensureAssistantDaemon(cfg, args, in, tty, channel)
	if err != nil {
		return err
	}

	// Step 5 — doctor-style summary.
	printAssistantSummary(provName, model, channel, owner, profilePath, created, daemonNote)
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

// ensureAssistantProvider returns the provider name + model the profile should
// declare, running the onboarding provider step first when none is configured.
func ensureAssistantProvider(cfg config.Config, configPath string, args []string, in *bufio.Reader, tty bool) (string, string, error) {
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
		return p.Name, model, nil
	}

	if flagged {
		// Reuse onboarding's flag-driven provider step verbatim (one code path).
		if err := runOnboardHeadless(cfg, configPath, args); err != nil {
			return "", "", err
		}
		np, _ := providerByName(flagVal(args, "--provider"))
		model := flagVal(args, "--model")
		if model == "" {
			model = np.DefaultModel
		}
		return np.Name, model, nil
	}

	if !tty {
		return "", "", errors.New("no provider configured and no TTY for the wizard; pass the flag form:\n" +
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
	key := normalizeKeyInput(p, promptLine(in, "API key", "${"+p.KeyEnv+"}"))

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
				fmt.Printf("validated: %s\n", detail)
			}
		}
		if valErr != nil {
			fmt.Printf("validation failed: %v\n", valErr)
			if !promptYes(in, "save anyway?", false) {
				return "", "", fmt.Errorf("provider validation failed: %w", valErr)
			}
		}
	}

	path, notes, err := writeProviderConfig(configPath, cfg.RyshDir, p, model, key, "")
	if err != nil {
		return "", "", err
	}
	fmt.Printf("wrote provider block (name: %s, model: %s, api_key: %s) to %s\n", p.Name, model, key.Ref, path)
	for _, n := range notes {
		fmt.Println(n)
	}
	return p.Name, model, nil
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

// ensureAssistantProfile scaffolds (or reuses) the assistant SKILL.md and
// returns the active channel, the owner contact (as far as it is knowable),
// the profile path, and whether the file was created on this run.
func ensureAssistantProfile(cfg config.Config, args []string, in *bufio.Reader, tty bool, provName, model string) (channel, owner, path string, created bool, err error) {
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

	preset, err := pickAssistantPreset(args, in, tty)
	if err != nil {
		return "", "", path, false, err
	}
	channel = preset.Channel

	fields, values, err := collectAssistantCreds(channel, args, in, tty)
	if err != nil {
		return "", "", path, false, err
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

// pickAssistantPreset resolves the device-bridge preset from --channel or the
// interactive picker, gating iMessage on the actual host OS.
func pickAssistantPreset(args []string, in *bufio.Reader, tty bool) (assistantPreset, error) {
	name := flagVal(args, "--channel")
	if name == "" {
		if !tty {
			return assistantPreset{}, errors.New("pick a device-bridge preset with --channel telegram|whatsapp|signal|imessage")
		}
		fmt.Println("\nStep 2 — bridge one of YOUR chat apps:")
		presets := assistantPresets()
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
		return assistantPreset{}, fmt.Errorf("unknown preset %q (valid: telegram, whatsapp, signal, imessage)", name)
	}
	if preset.DarwinOnly && runtime.GOOS != "darwin" {
		return assistantPreset{}, fmt.Errorf("the %s preset is macOS-host-only (it reads the local Messages database); this host is %s — pick telegram, whatsapp, or signal", preset.Channel, runtime.GOOS)
	}
	return preset, nil
}

// collectAssistantCreds gathers the preset's credential fields, flags first
// (--<key> value, or --<key>-env VAR for secrets → "${VAR}"), then interactive
// prompts on a TTY, then field defaults.
func collectAssistantCreds(channel string, args []string, in *bufio.Reader, tty bool) ([]credField, []string, error) {
	fields := channelCredFields(channel)
	values := make([]string, len(fields))
	var missing []string
	for i, f := range fields {
		flagName := "--" + strings.ReplaceAll(f.Key, "_", "-")
		switch {
		case f.EnvName != "" && flagVal(args, flagName+"-env") != "":
			values[i] = "${" + flagVal(args, flagName+"-env") + "}"
		case flagVal(args, flagName) != "":
			values[i] = flagVal(args, flagName)
		case tty:
			label := f.Label
			if f.EnvName != "" {
				label += " (literal is stored as ${" + f.EnvName + "} in .rysh/secrets)"
			}
			values[i] = promptLine(in, label, f.Default)
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

// ensureAssistantDaemon reuses a live session (spawning the assistant into
// it), spawns a daemon only with explicit consent (--start, or a TTY yes), and
// otherwise prints the exact commands — never a silent spawn. Returns a
// one-line daemon status for the summary.
func ensureAssistantDaemon(cfg config.Config, args []string, in *bufio.Reader, tty bool, channel string) (string, error) {
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
		if rec, err := store.Get(sessionName); err == nil && rec.PID > 0 && session.ProcessAlive(rec.PID) && rec.NATSPort > 0 {
			live = true
		}
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
	if !live {
		fmt.Printf("spawning session daemon %q…\n", sessionName)
		h, err := spawnDaemon(sessionName, cfg.LogLevel, "", false)
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
	fmt.Printf("running %s\n", spawnCmd)
	if err := cli.RyshCommand(store, sessionName, "", "", "humanoid spawn "+assistantName); err != nil {
		return "", fmt.Errorf("humanoid spawn: %w", err)
	}
	if channel != "" {
		fmt.Printf("running %s\n", startCmd)
		if err := cli.RyshCommand(store, sessionName, "", "", fmt.Sprintf("humanoid channel start %s %s", assistantName, channel)); err != nil {
			return "", fmt.Errorf("humanoid channel start: %w", err)
		}
	}
	time.Sleep(2 * time.Second)
	_ = cli.RyshCommand(store, sessionName, "", "", "humanoid channels "+assistantName)

	note := fmt.Sprintf(progname.Rewrite("assistant active under session %q (attach with: rysh attach %s)"), sessionName, sessionName)

	// RA4: prove a real message reaches the assistant before we claim success.
	// Only worth doing when a channel was actually started, and only when a
	// human is present to send the message — a scripted run must not block on
	// someone opening Telegram. --verify forces it, --no-verify skips it.
	verify := channel != "" && tty && !hasFlag(args, "--no-verify")
	if hasFlag(args, "--verify") {
		verify = channel != ""
	}
	if verify {
		note += "\n        " + verifyAssistantRoundTrip(store, sessionName, channel, assistantVerifyTimeout)
	}
	return note, nil
}

// ---------------------------------------------------------------------------
// Step 5 — summary
// ---------------------------------------------------------------------------

func printAssistantSummary(provName, model, channel, owner, profilePath string, created bool, daemonNote string) {
	state := "existing (reused)"
	if created {
		state = "created"
	}
	fmt.Println("\nsummary (doctor-style):")
	fmt.Printf("PASS provider  %s (model %s)\n", provName, model)
	autoApprove := assistantAutoApprove(profilePath)
	toolNote := "tool calls run without an approval dialog (auto_approve: true)"
	if !autoApprove {
		toolNote = "every consequential tool call is gated on your approval (auto_approve: false)"
	}
	fmt.Printf("PASS profile   %s — %s; profile: assistant — %s\n", profilePath, state, toolNote)
	if channel != "" {
		gov, mode := assistantChannelPolicy(profilePath, channel)
		note := "replies post straight back to you"
		if gov == "human" {
			note = "draft-and-confirm — nothing posts until you release it"
		}
		fmt.Printf("PASS channel   %s — governance: %s (%s), reply_mode: %s\n", channel, gov, note, mode)
	}
	if owner != "" {
		fmt.Printf("PASS pairing   allowlist = {%s} (you only); unknown senders become pending requests\n", owner)
	} else {
		fmt.Printf("WARN pairing   no allowlist seed found in the profile — add your contact id under contacts.%s.allowlist\n", channel)
	}
	fmt.Printf("INFO daemon    %s\n", daemonNote)
	fmt.Println()
	fmt.Println("what you'll see: @mention your assistant from your own app and it answers you.")
	if autoApprove {
		fmt.Println("It runs tools without asking. Set auto_approve: false in the profile to be")
		fmt.Println("asked first; policy always_gate / bash.deny gate regardless of that flag.")
	} else {
		fmt.Println("Before it runs a tool, you get an APPROVAL request — in the chat and in the")
		fmt.Println("pane — and reply \"yes\" to release it or \"no\" to reject.")
	}
	fmt.Println(progname.Rewrite("verify anytime: rysh doctor   |   OS auto-start: rysh assistant --install-daemon"))
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
