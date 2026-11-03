// SPDX-License-Identifier: Apache-2.0

package main

// setup_cmd.go — `rysh setup` (E33 slice A, gap G9): one guided entry point
// from "nothing configured" to "your agent answers you on Telegram, Discord,
// Slack, Mattermost, Matrix or LINE". It is `rysh assistant` with the
// remote-first steps made explicit, not a fork of it — every step reuses the
// assistant's own functions:
//
//  1. provider — ensureAssistantProvider (onboarding's step 1, one path);
//  2. chat app — the assistant presets, Telegram first;
//  3. bot-creation steps for that app (channels.BotSetupSteps; for Slack the
//     app manifest, for Discord the invite URL once validation knows the
//     application id);
//  4. token — read with the terminal echo off; a literal goes to .rysh/secrets
//     (0600) and the profile only gets ${NAME}. Headless: --<cred>-env NAME;
//     a secret is never accepted as a flag value, because argv is visible to
//     every process on the host;
//  5. validate — doctor's channel check, before anything is written; a
//     rejected token is asked for again on a TTY;
//  6. owner id — with the per-channel hints;
//  7. daemon — only with consent (--start or a TTY yes), then the round-trip
//     verify;
//  8. summary — PASS only for what was checked, plus the binding line saying
//     which chat drives which agent and session;
//  9. auto-start offer and the directory the setup applies to (G11).
//
// Headless form (mirrors rysh assistant):
//
//	rysh setup --channel telegram --owner <id> --bot-token-env TELEGRAM_BOT_TOKEN \
//	    [--provider anthropic --key-env ANTHROPIC_API_KEY] [--start] [--install-daemon] [--json]
//
// --json prints one setupJSON object on stdout and the human text on stderr,
// for a caller that drives setup headless (the desktop app). An existing
// profile is reused and never rewritten (founder Q4).

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"golang.org/x/term"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// setupCommand is the command's name, kept in this one constant: the entry
// point's name is an open founder question (E33 Q1).
const setupCommand = "setup"

// setupPresetOrder is the order the chat apps are offered in: Telegram first
// (a bot in ~2 minutes, no workspace admin; E33 Q2), then Discord and Slack;
// the self-hosted and regional apps gap05 added come last.
var setupPresetOrder = []string{"telegram", "discord", "slack", "whatsapp", "signal", "imessage", "mattermost", "matrix", "line"}

// setupInstallDaemon writes the OS auto-start unit. A variable so tests can
// record the call instead of writing under HOME.
var setupInstallDaemon = installAssistantDaemon

// setupPresets returns the assistant presets in setupPresetOrder.
func setupPresets() []assistantPreset {
	var out []assistantPreset
	for _, name := range setupPresetOrder {
		if p, ok := assistantPresetByChannel(name); ok {
			// The assistant's table recommends Slack; setup's order makes
			// its own recommendation, so the label must not contradict it.
			p.Label = strings.TrimSuffix(p.Label, " (recommended)")
			if p.Channel == setupPresetOrder[0] {
				p.Label += " (quickest)"
			}
			out = append(out, p)
		}
	}
	return out
}

// setupJSON is `rysh setup --json`: one object on stdout for a caller that
// drives setup headless (the desktop app, founder Q5). It carries names and
// statuses only — never a credential value. Version 1; fields are only ever
// added.
type setupJSON struct {
	Version       int               `json:"version"`
	OK            bool              `json:"ok"`
	Error         string            `json:"error,omitempty"`
	Dir           string            `json:"dir"`
	Session       string            `json:"session"`
	Channel       string            `json:"channel,omitempty"`
	Profile       *setupJSONProfile `json:"profile,omitempty"`
	Checks        []summaryLine     `json:"checks"`
	Binding       *setupJSONBinding `json:"binding,omitempty"`
	AutoStart     setupJSONAuto     `json:"auto_start"`
	Verified      bool              `json:"verified"`
	StoredSecrets []string          `json:"stored_secrets,omitempty"`
	Fails         int               `json:"fails"`
}

type setupJSONProfile struct {
	Path    string `json:"path"`
	Created bool   `json:"created"`
}

// setupJSONBinding is the binding line's parts (E33 G6).
type setupJSONBinding struct {
	Channel   string   `json:"channel"`
	Bot       string   `json:"bot,omitempty"`        // "" = not validated
	Handle    string   `json:"handle,omitempty"`     // "@bot", the account to message
	InviteURL string   `json:"invite_url,omitempty"` // Discord: adds the bot to a server
	Humanoid  string   `json:"humanoid"`
	Allowlist []string `json:"allowlist"`
	Session   string   `json:"session"`
	Dir       string   `json:"dir"`
	Line      string   `json:"line"`
}

type setupJSONAuto struct {
	Installed bool   `json:"installed"`
	Error     string `json:"error,omitempty"`
}

// runSetup dispatches `rysh setup`.
func runSetup(cfg config.Config, configPath string, args []string) error {
	if hasFlag(args, "--help", "-h") {
		printSetupUsage()
		return nil
	}
	env := assistantEnv{
		in:   bufio.NewReader(os.Stdin),
		tty:  term.IsTerminal(int(os.Stdin.Fd())),
		deps: defaultDoctorDeps(cfg),
	}
	if hasFlag(args, "--json") {
		// stdout carries the one JSON object and nothing else; the human
		// text a run prints goes to stderr.
		stdout := os.Stdout
		os.Stdout = os.Stderr
		defer func() { os.Stdout = stdout }()
		env.jsonOut = stdout
	}
	return runSetupWith(cfg, configPath, args, env)
}

func printSetupUsage() {
	fmt.Println(progname.Rewrite("usage: rysh " + setupCommand + "                  guided (TTY): talk to your agent from your phone"))
	fmt.Println(progname.Rewrite("       rysh " + setupCommand + " [flags]          scriptable, no TTY needed"))
	fmt.Println()
	fmt.Println("Walks you from nothing configured to your agent answering you on Telegram,")
	fmt.Println("Discord, Slack, Mattermost, Matrix or LINE: provider, chat app, bot-creation")
	fmt.Println("steps, token (typed hidden, stored in .rysh/secrets), validation, your owner")
	fmt.Println("id, the session daemon, and a summary that says which chat drives which agent")
	fmt.Println("and session.")
	fmt.Println()
	fmt.Println("flags:")
	fmt.Println("  --provider <name>        anthropic (default) | openai | ollama (when none is configured)")
	fmt.Println("  --key-env <VAR>          env var or .rysh/secrets name holding the API key")
	fmt.Println("  --model <model>          model to write (default: provider default)")
	fmt.Println("  --no-validate            skip the provider ping")
	fmt.Println("  --channel <app>          " + strings.Join(setupPresetOrder, " | "))
	fmt.Println("  --owner <id>             YOUR id on that app — the ONLY allowlisted sender")
	fmt.Println("  --<cred>-env <VAR>       a credential by name, e.g. --bot-token-env TELEGRAM_BOT_TOKEN")
	fmt.Println("                           (secrets are never taken as flag values: argv is visible")
	fmt.Println("                           to every process; type them at the hidden TTY prompt)")
	fmt.Println("  --start                  spawn the session daemon + assistant now")
	fmt.Println("  --session <name>         the session to use (default: this directory's own, saved")
	fmt.Println("                           as rysh.session_name; a live session of another")
	fmt.Println("                           directory is refused)")
	fmt.Println("  --verify / --no-verify   wait for your first real message (default: on in a TTY)")
	fmt.Println("  --install-daemon         also write the OS auto-start unit at the end")
	fmt.Println("  --store-secrets          after the checks pass, write each --<cred>-env / --key-env NAME")
	fmt.Println("                           this environment holds to .rysh/secrets/default/NAME (0600),")
	fmt.Println("                           so a daemon that is already running can resolve it")
	fmt.Println("  --steps-only             print the bot-creation steps (Slack: the app manifest) and")
	fmt.Println("                           write nothing; with --channel <app> for one app")
	fmt.Println("  --json                   one JSON summary object on stdout (human text to stderr);")
	fmt.Println("                           implies no prompts")
	fmt.Println()
	fmt.Println("An existing assistant profile is reused and never rewritten: setup leaves its")
	fmt.Println(progname.Rewrite("auto_approve and every other gate field as you set them (rysh assistant"))
	fmt.Println("--overwrite-profile regenerates it on purpose).")
	fmt.Println()
	fmt.Println("Config is PROJECT-LOCAL: the setup applies to the directory you run it in.")
}

// runSetupWith is runSetup with its edges injected.
func runSetupWith(cfg config.Config, configPath string, args []string, env assistantEnv) (err error) {
	if hasFlag(args, "--steps-only") {
		if err := refuseSecretFlags(args); err != nil {
			return err
		}
		return runSetupStepsOnly(args, env)
	}
	dir := projectDir()
	res := setupJSON{Version: 1, Dir: dir, Session: sessionNameOrDefault(cfg), Checks: []summaryLine{}}
	if env.jsonOut != nil {
		// A machine caller never answers a prompt.
		env.tty = false
		defer func() {
			res.OK = err == nil
			if err != nil {
				res.Error = err.Error()
			}
			enc := json.NewEncoder(env.jsonOut)
			enc.SetEscapeHTML(false)
			_ = enc.Encode(res)
		}()
	}
	if err := refuseSecretFlags(args); err != nil {
		return err
	}
	// Founder Q4: setup never rewrites an existing profile's gate fields, so
	// it never regenerates one.
	if hasFlag(args, "--overwrite-profile") && skillFileExists(assistantName) {
		return fmt.Errorf(progname.Rewrite("rysh %s never rewrites an existing assistant profile (its auto_approve and gate fields stay yours): edit %s, or regenerate it on purpose with rysh assistant --overwrite-profile"), setupCommand, assistantSkillPath())
	}
	in, tty := env.in, env.tty

	fmt.Println(progname.Rewrite("rysh " + setupCommand + " — from nothing to talking to your agent from your phone"))
	fmt.Printf("this sets up %s (rysh config is project-local)\n", dir)

	// 1 — provider (onboarding's step, the same code path as rysh assistant).
	prov, err := ensureAssistantProvider(cfg, configPath, args, in, tty)
	if err != nil {
		return err
	}
	// The session is this directory's own (setup_session.go): never "default"
	// or a rysh pane's session by accident, since either may be another
	// project's daemon on the shared bus.
	if cfg, err = resolveAssistantSession(cfg, configPath, args, dir); err != nil {
		return err
	}
	res.Session = sessionNameOrDefault(cfg)

	// 2–6 — chat app, bot steps, token, validation, owner; written last.
	channel, owner, profilePath, created, err := ensureAssistantProfile(cfg, args, in, tty, prov.Name, prov.Model, &profileHooks{
		presets:     setupPresets(),
		heading:     "Step 2 — the chat app you will talk to your agent from:",
		beforeCreds: printSetupBotSteps,
		checkCreds:  setupCredCheck(env, args),
	})
	if err != nil {
		return err
	}
	// Q-E33-2: a new assistant also tells its owner, in their DM, when a pane
	// waits on them (design 086). Only for a profile this run created, and
	// never over a needs-you block the owner already has.
	if created {
		if on, nerr := config.SetNeedsYouAssistantIfUnset(config.ConfigPath(configPath), assistantName); nerr != nil {
			fmt.Printf("WARN could not turn on the chat alert (%v); add notify: needs_you: assistant: %s to the config yourself\n", nerr, assistantName)
		} else if on {
			fmt.Println(progname.Rewrite("chat alert: on — when a pane waits on you for a minute, the bot tells you in your DM (check it with: rysh notify test)"))
		}
	}
	chk := checkAssistantChannel(profilePath, channel, env.deps)
	pc := checkProviderOutcome(prov, env.deps)

	// --store-secrets (slice A2): once the checks pass, the references this
	// environment holds go to .rysh/secrets, where any daemon finds them.
	chRefs, provRefs := channelRefNames(profilePath, channel), config.EnvRefNames(prov.KeyRef)
	if hasFlag(args, "--store-secrets") {
		var names []string
		if chk != nil && chk.Status != doctorFail {
			names = append(names, chRefs...)
		}
		if pc.Status != doctorFail {
			names = append(names, provRefs...)
		}
		if res.StoredSecrets, err = storeSetupSecrets(cfg, names, env.deps); err != nil {
			return err
		}
	}
	// A daemon that is already running did not inherit this environment.
	live := env.daemonLive
	if live == nil {
		live = func() bool {
			store, err := session.NewStore(cfg)
			return err == nil && sessionRecordLive(store, sessionNameOrDefault(cfg))
		}
	}
	if live() {
		liveDaemonCaveat(chk, chRefs, sessionNameOrDefault(cfg), env.deps)
		liveDaemonCaveat(&pc, provRefs, sessionNameOrDefault(cfg), env.deps)
	}

	// 7 — daemon (consent), activation and round-trip verify.
	daemonNote, err := ensureAssistantDaemon(cfg, args, in, tty, channel, owner, chk)
	if err != nil {
		return err
	}
	res.Verified = roundTripVerified(daemonNote)

	// 8 — summary with the binding line.
	binding := assistantBindingLine(cfg, profilePath, channel, chk)
	res.Checks = printAssistantSummary(assistantSummary{
		Provider: prov, Channel: channel, Owner: owner, ProfilePath: profilePath,
		Created: created, DaemonNote: daemonNote, Check: chk, ProviderCheck: &pc, Binding: binding,
	}, env.deps)
	fails := summaryFails(res.Checks)
	res.Fails, res.Channel = fails, channel
	res.Profile = &setupJSONProfile{Path: profilePath, Created: created}
	if channel != "" {
		var id channels.BotIdentity
		if chk != nil {
			id = chk.Identity
		}
		res.Binding = &setupJSONBinding{Channel: channel, Bot: checkedBot(chk), Handle: id.Handle(),
			InviteURL: setupInviteURL(channel, id), Humanoid: assistantName,
			Allowlist: profileAllowlist(profilePath, channel), Session: res.Session, Dir: dir, Line: binding}
	}

	// 9 — auto-start offer and the directory (G11).
	fmt.Println()
	install := hasFlag(args, "--install-daemon")
	if !install && tty {
		install = promptYes(in, fmt.Sprintf("keep session %q running after a reboot (install OS auto-start)?", sessionNameOrDefault(cfg)), false)
	}
	if install {
		if err := setupInstallDaemon(cfg); err != nil {
			fmt.Printf("WARN auto-start not installed: %v\n", err)
			res.AutoStart.Error = err.Error()
		} else {
			res.AutoStart.Installed = true
		}
	} else {
		fmt.Printf(progname.Rewrite("auto-start: not installed — after a reboot your phone gets no answer until you run \"rysh attach %s\" here.\n"), sessionNameOrDefault(cfg))
		fmt.Println(progname.Rewrite("  install it with: rysh " + setupCommand + " --install-daemon"))
	}
	fmt.Printf("this setup applies to %s only — rysh config is project-local; run it again in another directory for that one.\n", dir)

	if fails > 0 {
		return fmt.Errorf(progname.Rewrite("setup saved, but %d check(s) FAILed — apply the fix lines above, then run: rysh doctor"), fails)
	}
	return nil
}

// printSetupBotSteps prints slice B's bot-creation steps before the token
// prompt; for Slack also the app manifest and the link that pre-fills it.
func printSetupBotSteps(channel string) {
	fmt.Printf("\nStep 3 — create your %s bot:\n", channel)
	steps, ok := channels.BotSetupSteps(channel, channels.BotIdentity{})
	if !ok {
		steps = []string{fmt.Sprintf("Create the %s credentials, then enter them at the next prompts.", channel)}
	}
	for _, step := range steps {
		fmt.Println("  " + step)
	}
	if channel == "slack" {
		fmt.Println("\n  Slack app manifest (paste it under \"From an app manifest\"):")
		for _, l := range strings.Split(strings.TrimRight(channels.SlackAppManifest(""), "\n"), "\n") {
			fmt.Println("    " + l)
		}
		fmt.Println("  or open this link, which pre-fills it:")
		fmt.Println("    " + channels.SlackCreateFromManifestURL(""))
	}
}

// printSetupBotNext prints what the validator made possible: the Discord
// invite URL (it needs the application id) and the bot to message — the last
// of slice B's steps once the bot's username is known.
func printSetupBotNext(channel string, id channels.BotIdentity) {
	if invite := setupInviteURL(channel, id); invite != "" {
		fmt.Println("add the bot to your server: " + invite)
	}
	if id.Username == "" {
		return
	}
	if steps, ok := channels.BotSetupSteps(channel, id); ok && len(steps) > 0 {
		last := steps[len(steps)-1]
		if i := strings.Index(last, ". "); i > 0 {
			last = last[i+2:]
		}
		fmt.Println("then: " + last)
	}
}

// setupInviteURL is the Discord invite for id, or "" on other channels.
func setupInviteURL(channel string, id channels.BotIdentity) string {
	if channel != "discord" {
		return ""
	}
	return channels.DiscordInviteURL(id.ApplicationID)
}

// setupCredCheck validates the collected credentials before anything is
// written, through doctor's channel check, with the literals typed this run
// overlaid on the lookup (they reach .rysh/secrets only after the check). On a
// TTY a FAIL asks whether to enter them again — unless they came from flags,
// where asking again would only read the same flags.
func setupCredCheck(env assistantEnv, args []string) func(string, []credField, []string) bool {
	return func(channel string, fields []credField, values []string) bool {
		fileValues, typed := extractFieldSecrets(fields, values)
		cc, err := channelConfigFromFields(channel, fields, fileValues)
		if err != nil {
			fmt.Printf("FAIL channel   %s — %v\n", channel, err)
			return false
		}
		deps := env.deps
		inner := deps.lookupRef
		deps.lookupRef = func(name string) (string, bool) {
			if v, ok := typed[name]; ok {
				return v, true
			}
			return inner(name)
		}
		c := checkChannelConfig(channel, cc, deps)
		fmt.Printf("\nStep 4 — check the credentials:\n%-4s %-8s %s\n", c.Status, "channel", c.Detail)
		for _, f := range c.Fixes {
			fmt.Printf("     %-8s fix: %s\n", "", f)
		}
		if c.Identity.Handle() != "" {
			fmt.Printf("your bot is %s — that is who you will message from your phone.\n", c.Identity.Handle())
		}
		if c.Status == doctorPass {
			printSetupBotNext(channel, c.Identity)
		}
		if c.Status != doctorFail || !env.tty || credsFromFlags(fields, args) {
			return false
		}
		return promptYes(env.in, "enter the credentials again?", true)
	}
}

// credsFromFlags reports whether every secret field was given as a flag.
func credsFromFlags(fields []credField, args []string) bool {
	for _, f := range fields {
		if f.EnvName != "" && flagVal(args, "--"+strings.ReplaceAll(f.Key, "_", "-")+"-env") == "" {
			return false
		}
	}
	return true
}

// channelConfigFromFields renders the collected fields through the same
// SKILL.md builder the profile uses and parses the channel block back, so the
// pre-write check sees exactly the config that will be written.
func channelConfigFromFields(channel string, fields []credField, values []string) (msg.ChannelConfig, error) {
	content, err := buildSkillMarkdown(skillSpec{Name: assistantName, Channel: channel, Fields: fields, Values: values})
	if err != nil {
		return msg.ChannelConfig{}, err
	}
	fmRaw, _, ok := splitSkillFrontmatter(content)
	if !ok {
		return msg.ChannelConfig{}, errors.New("internal: rendered profile has no frontmatter")
	}
	var fm skillFileFrontmatter
	if err := yamlUnmarshal([]byte(fmRaw), &fm); err != nil {
		return msg.ChannelConfig{}, err
	}
	return fm.Contacts[channel], nil
}

// refuseSecretFlags rejects a secret passed as a flag value — the provider's
// --key and every preset's secret fields — before anything is written. The
// value is never echoed back.
func refuseSecretFlags(args []string) error {
	if flagVal(args, "--key") != "" {
		return errors.New("rysh " + setupCommand + " does not take the API key as a flag value (argv is visible to every process): use --key-env <NAME>, or type it at the TTY prompt")
	}
	seen := map[string]bool{}
	for _, p := range assistantPresets() {
		for _, f := range channelCredFields(p.Channel) {
			flag := "--" + strings.ReplaceAll(f.Key, "_", "-")
			if f.EnvName == "" || seen[flag] {
				continue
			}
			seen[flag] = true
			if flagVal(args, flag) != "" {
				return fmt.Errorf("rysh %s does not take %s as a flag value (argv is visible to every process): use %s-env <NAME>, or type it at the hidden TTY prompt", setupCommand, flag, flag)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Slice A2: what an already-running daemon can resolve
// ---------------------------------------------------------------------------

// channelRefNames lists the ${NAME} references in the profile's channel block.
func channelRefNames(profilePath, channel string) []string {
	fm, _, err := parseSkillFile(profilePath)
	if err != nil || fm == nil {
		return nil
	}
	raw, err := yaml.Marshal(fm.Contacts[channel])
	if err != nil {
		return nil
	}
	return config.EnvRefNames(string(raw))
}

// storeSetupSecrets is --store-secrets: each named reference that this
// process's environment holds is written to .rysh/secrets/default/<NAME>
// (0600) through persistFieldSecrets, so a daemon started from any
// environment resolves it. Called only for references whose check did not
// FAIL. Names are printed; values never are.
func storeSetupSecrets(cfg config.Config, names []string, deps doctorDeps) ([]string, error) {
	var stored []string
	for _, name := range names {
		v := strings.TrimSpace(deps.getenv(name))
		if v == "" {
			continue // not in this environment: nothing to store
		}
		if _, err := persistFieldSecrets(cfg.RyshDir, map[string]string{name: v}); err != nil {
			return stored, err
		}
		fmt.Printf("stored ${%s} from this environment in %s (0600)\n", name, filepath.Join(cfg.RyshDir, "secrets", "default", name))
		stored = append(stored, name)
	}
	sort.Strings(stored)
	return stored, nil
}

// liveDaemonCaveat downgrades a PASS to WARN when the session's daemon is
// already running and a reference resolves only from setup's own
// environment: the daemon resolves ${NAME} from .rysh/secrets or ITS own
// environment, and setup cannot see the latter, so PASS would be a claim
// nothing checked — the silent-bot case.
func liveDaemonCaveat(c *channelCheck, names []string, sessionName string, deps doctorDeps) {
	if c == nil || c.Status == doctorFail {
		return
	}
	var unseen []string
	for _, name := range names {
		if _, ok := deps.lookupStored(name); !ok {
			unseen = append(unseen, name)
		}
	}
	if len(unseen) == 0 {
		return
	}
	c.Status = doctorWarn
	c.Detail += fmt.Sprintf("; session %q's daemon is already running and resolves ${%s} only from .rysh/secrets or its own environment, which setup cannot see",
		sessionName, strings.Join(unseen, "}, ${"))
	for _, name := range unseen {
		c.Fixes = append(c.Fixes, fmt.Sprintf(progname.Rewrite("re-run rysh %s with --store-secrets, or ##secret new %s <value>"), setupCommand, name))
	}
}

// setupStepsJSON is `rysh setup --steps-only --json`: the bot-creation guides,
// nothing else. Version 1; fields are only ever added.
type setupStepsJSON struct {
	Version int          `json:"version"`
	OK      bool         `json:"ok"`
	Error   string       `json:"error,omitempty"`
	Guides  []setupGuide `json:"guides"`
}

// setupGuide is one chat app's guide. The Discord invite URL needs the
// application id validation learns, so it is not part of a guide.
type setupGuide struct {
	Channel   string   `json:"channel"`
	Steps     []string `json:"steps"`
	Manifest  string   `json:"manifest,omitempty"`   // Slack app manifest (YAML)
	CreateURL string   `json:"create_url,omitempty"` // Slack: create the app from it
}

// setupGuideChannels are the chat apps with a guide, in setup's order.
var setupGuideChannels = []string{"telegram", "discord", "slack", "mattermost", "matrix", "line"}

// runSetupStepsOnly is --steps-only: print the guides for --channel (or every
// chat app with one) and write nothing.
func runSetupStepsOnly(args []string, env assistantEnv) error {
	chs := setupGuideChannels
	if ch := flagVal(args, "--channel"); ch != "" {
		if _, ok := channels.BotSetupSteps(ch, channels.BotIdentity{}); !ok {
			err := fmt.Errorf("no bot-creation guide for %q (have: %s)", ch, strings.Join(setupGuideChannels, ", "))
			if env.jsonOut != nil {
				_ = json.NewEncoder(env.jsonOut).Encode(setupStepsJSON{Version: 1, Error: err.Error(), Guides: []setupGuide{}})
			}
			return err
		}
		chs = []string{ch}
	}
	if env.jsonOut == nil {
		for _, ch := range chs {
			printSetupBotSteps(ch)
		}
		return nil
	}
	out := setupStepsJSON{Version: 1, OK: true}
	for _, ch := range chs {
		steps, _ := channels.BotSetupSteps(ch, channels.BotIdentity{})
		g := setupGuide{Channel: ch, Steps: steps}
		if ch == "slack" {
			g.Manifest, g.CreateURL = channels.SlackAppManifest(""), channels.SlackCreateFromManifestURL("")
		}
		out.Guides = append(out.Guides, g)
	}
	enc := json.NewEncoder(env.jsonOut)
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}
