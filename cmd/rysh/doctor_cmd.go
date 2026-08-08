package main

// rysh doctor — diagnostics (design 004 §4.3, WS4 OB3).
//
// Runs an ordered set of read-mostly checks — provider, channels, daemon/NATS,
// config — each emitting PASS / WARN / FAIL plus a one-line fix on anything
// but PASS. The provider check shares validateProvider with `rysh onboard`, so
// there is one validation code path. Channel checks prefer the optional,
// non-binding channels.Validator interface (Slack auth.test, WhatsApp Graph
// GET); adapters without it are reported as not-validated rather than probed
// via Start(), which binds ports/webhooks. Exits non-zero iff any check FAILs.

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/rysh-ai/rysh-cli-code/internal/channels"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
	"github.com/rysh-ai/rysh-cli-code/internal/tui"
)

// doctor check statuses.
const (
	doctorPass = "PASS"
	doctorWarn = "WARN"
	doctorFail = "FAIL"
)

// doctorCheck is one line of doctor output.
type doctorCheck struct {
	Group  string // provider | channels | daemon | config
	Status string // PASS | WARN | FAIL
	Detail string
	Fix    string // one-line fix; empty on PASS
}

// doctorDeps injects the effectful edges so the check logic is table-testable
// against synthetic states without network or a live daemon.
type doctorDeps struct {
	// validateProvider is the shared onboard/doctor provider ping.
	validateProvider func(ctx context.Context, name, model, key, baseURL string) (string, error)
	// newAdapter constructs a channel adapter (channels.NewAdapter).
	newAdapter func(channelType string, cc msg.ChannelConfig) (channels.ChannelAdapter, error)
	// dialNATS probes the session's NATS client port.
	dialNATS func(port int, timeout time.Duration) error
	// lookupRef resolves a ${NAME} reference (config-level precedence).
	lookupRef func(name string) (string, bool)
	// getenv reads process environment (os.Getenv) — the clipboard check
	// sniffs SSH/tmux/screen markers from it.
	getenv func(name string) string
}

// defaultDoctorDeps wires the real implementations.
func defaultDoctorDeps(cfg config.Config) doctorDeps {
	return doctorDeps{
		validateProvider: validateProvider,
		newAdapter:       channels.NewAdapter,
		dialNATS: func(port int, timeout time.Duration) error {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), timeout)
			if err != nil {
				return err
			}
			return conn.Close()
		},
		lookupRef: func(name string) (string, bool) {
			// .rysh/secrets files first, then config [secrets], then env —
			// the precedence visible outside a running session.
			if v, ok := config.LookupSecretRef(cfg.RyshDir, name); ok {
				return v, true
			}
			for _, scope := range cfg.Secrets {
				if v, ok := scope[name]; ok {
					return v, true
				}
			}
			return "", false
		},
		getenv: os.Getenv,
	}
}

// runDoctor executes the checks and prints one line per result. Returns an
// error (→ non-zero exit) iff any check FAILed.
func runDoctor(cfg config.Config, configPath string, args []string) error {
	if hasFlag(args, "--help", "-h") {
		fmt.Println(progname.Rewrite("usage: rysh doctor [--copy-test]"))
		fmt.Println("checks provider reachability, channel credentials, daemon/NATS health, and")
		fmt.Println("config sanity, printing PASS/WARN/FAIL per check with a one-line fix.")
		fmt.Println("--copy-test  emit an OSC 52 clipboard copy of a known string so you can")
		fmt.Println("             verify mouse-copy works end-to-end (SSH/tmux/terminal).")
		return nil
	}
	if hasFlag(args, "--copy-test") {
		return runClipboardCopyTest()
	}

	// Users coming from tools with a global config expect ~/.rysh; rysh config
	// is project-local, so say exactly which file (or default) applies here.
	if cfg.ConfigFile != "" {
		fmt.Printf(progname.Rewrite("rysh doctor — config: %s\n"), cfg.ConfigFile)
	} else {
		fmt.Printf(progname.Rewrite("rysh doctor — config: (none found — defaults; project-local state in %s)\n"), cfg.RyshDir)
	}

	checks := doctorChecks(cfg, defaultDoctorDeps(cfg))
	fails := 0
	for _, c := range checks {
		line := fmt.Sprintf("%-4s %-8s %s", c.Status, c.Group, c.Detail)
		fmt.Println(line)
		if c.Fix != "" {
			fmt.Printf("     %-8s fix: %s\n", "", c.Fix)
		}
		if c.Status == doctorFail {
			fails++
		}
	}
	if fails > 0 {
		return fmt.Errorf("doctor: %d check(s) failed", fails)
	}
	return nil
}

// doctorChecks runs every check group in the design's order: provider,
// channels, daemon, config.
func doctorChecks(cfg config.Config, deps doctorDeps) []doctorCheck {
	var out []doctorCheck
	out = append(out, checkProvider(cfg, deps))
	out = append(out, checkChannels(cfg, deps)...)
	out = append(out, checkDaemon(cfg, deps)...)
	out = append(out, checkConfig(cfg, deps)...)
	out = append(out, checkClipboard(deps)...)
	return out
}

// ---------------------------------------------------------------------------
// clipboard
// ---------------------------------------------------------------------------

// checkClipboard reports how TUI mouse-copy will reach a clipboard from this
// environment. Inside SSH the only path is OSC 52 down the pty to the user's
// LOCAL terminal (host-side xclip/pbcopy only reach the remote host); tmux and
// screen in the chain additionally swallow OSC 52 unless it is wrapped and,
// for tmux, its clipboard options cooperate. Whether the local terminal
// actually applies OSC 52 is undetectable from here — that is what
// `rysh doctor --copy-test` verifies end to end.
func checkClipboard(deps doctorDeps) []doctorCheck {
	env := deps.getenv
	if env == nil {
		env = os.Getenv
	}
	term := strings.ToLower(env("TERM"))
	transport := "local session: mouse-copy uses OSC 52 + system clipboard fallback"
	if env("SSH_CONNECTION") != "" || env("SSH_TTY") != "" {
		transport = "remote (SSH) session: mouse-copy reaches your local clipboard via OSC 52"
	}
	switch {
	case env("TMUX") != "" || strings.HasPrefix(term, "tmux"):
		return []doctorCheck{{
			Group:  "clipboard",
			Status: doctorWarn,
			Detail: transport + "; tmux in the chain — OSC 52 emitted raw + passthrough-wrapped",
			Fix:    progname.Rewrite("tmux needs `set-clipboard external|on` (default) or `allow-passthrough on`; verify: rysh doctor --copy-test"),
		}}
	case strings.HasPrefix(term, "screen"):
		return []doctorCheck{{
			Group:  "clipboard",
			Status: doctorWarn,
			Detail: transport + "; GNU screen in the chain — OSC 52 emitted DCS-wrapped",
			Fix:    progname.Rewrite("verify end-to-end: rysh doctor --copy-test"),
		}}
	}
	return []doctorCheck{{
		Group:  "clipboard",
		Status: doctorPass,
		Detail: transport + progname.Rewrite(" (terminal must support OSC 52 — verify: rysh doctor --copy-test)"),
	}}
}

// runClipboardCopyTest emits an OSC 52 copy of a known string through the
// exact writer/wrapping path the TUI mouse-copy uses, so a user can verify
// end-to-end (SSH hop, tmux/screen wrapping, terminal support/permission)
// that a copy actually lands in their local clipboard.
func runClipboardCopyTest() error {
	const probe = "rysh-clipboard-ok"
	out, closeOut := tui.OSC52Output()
	err := tui.WriteOSC52(out, os.Getenv, probe)
	closeOut()
	if err != nil {
		return fmt.Errorf("doctor: clipboard copy test: %w", err)
	}
	fmt.Printf("sent an OSC 52 copy of %q to your terminal.\n", probe)
	fmt.Println("now paste (Cmd+V / Ctrl+Shift+V): if that string appears, mouse-copy works")
	fmt.Println("end-to-end. if not, a hop is blocking OSC 52 — Apple Terminal.app has no")
	fmt.Println("OSC 52 support, iTerm2 needs Prefs → General → Selection → 'Applications in")
	fmt.Println("terminal may access clipboard', mosh needs ≥ 1.4, tmux needs set-clipboard")
	fmt.Println("external|on or allow-passthrough on.")
	return nil
}

// ---------------------------------------------------------------------------
// provider
// ---------------------------------------------------------------------------

// checkProvider pings the configured provider with the shared onboard probe.
func checkProvider(cfg config.Config, deps doctorDeps) doctorCheck {
	p, _ := providerByName(cfg.ProviderName)
	if cfg.APIKey == "" && !p.KeyOptional {
		return doctorCheck{
			Group: "provider", Status: doctorFail,
			Detail: fmt.Sprintf("no API key configured for provider %q", p.Name),
			Fix:    fmt.Sprintf(progname.Rewrite("set provider.api_key (as a ${ENV} reference) or export %s; run `rysh onboard`"), p.KeyEnv),
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), validateProviderTimeout)
	defer cancel()
	detail, err := deps.validateProvider(ctx, cfg.ProviderName, cfg.DefaultModel, cfg.APIKey, providerBaseURL(cfg, p))
	if err != nil {
		return doctorCheck{
			Group: "provider", Status: doctorFail,
			Detail: err.Error(),
			Fix:    fmt.Sprintf(progname.Rewrite("check the key/network (export %s or fix provider.api_key); run `rysh onboard` to reconfigure"), p.KeyEnv),
		}
	}
	return doctorCheck{Group: "provider", Status: doctorPass, Detail: detail}
}

// providerBaseURL picks the base URL the runtime would use: the configured
// api_url unless it is the config default that only applies to Anthropic.
func providerBaseURL(cfg config.Config, p onboardProvider) string {
	if cfg.APIURL != "" && cfg.APIURL != config.DefaultAnthropicAPIURL {
		return cfg.APIURL
	}
	if p.Name == "anthropic" {
		return cfg.APIURL
	}
	return "" // let the probe use the provider's own default
}

// ---------------------------------------------------------------------------
// channels
// ---------------------------------------------------------------------------

// skillFileFrontmatter mirrors the frontmatter shape parseHumanoidFile
// (internal/actors/humanoid_skillfile.go) unmarshals, so doctor validates the
// same contract the runtime parses.
type skillFileFrontmatter struct {
	Name        string                       `yaml:"name"`
	Description string                       `yaml:"description"`
	Model       string                       `yaml:"model"`
	Profile     string                       `yaml:"profile"`
	AutoApprove *bool                        `yaml:"auto_approve"`
	Contacts    map[string]msg.ChannelConfig `yaml:"contacts"`
}

// parseSkillFile parses one SKILL.md the way the runtime does: a "---"
// delimited YAML frontmatter (closing delimiter only on its own line),
// followed by the system-prompt body. A file without frontmatter is valid
// (whole content = prompt).
func parseSkillFile(path string) (*skillFileFrontmatter, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	fmRaw, body, ok := splitSkillFrontmatter(string(data))
	if !ok {
		return &skillFileFrontmatter{Name: filepath.Base(filepath.Dir(path))}, string(data), nil
	}
	var fm skillFileFrontmatter
	if err := yaml.Unmarshal([]byte(fmRaw), &fm); err != nil {
		return nil, "", fmt.Errorf("parse humanoid frontmatter: %w", err)
	}
	if fm.Name == "" {
		fm.Name = filepath.Base(filepath.Dir(path))
	}
	return &fm, body, nil
}

// splitSkillFrontmatter mirrors actors.splitFrontmatter: the closing "---"
// must be alone on its line, so "---" inside a comment/value is not mistaken
// for the delimiter.
func splitSkillFrontmatter(content string) (frontmatter, body string, ok bool) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n"), true
		}
	}
	return "", "", false
}

// listSkillFiles returns the .rysh/humanoids/*/SKILL.md paths (project-local
// layout), sorted for stable output.
func listSkillFiles() []string {
	entries, err := os.ReadDir(filepath.Join(".rysh", "humanoids"))
	if err != nil {
		return nil
	}
	var paths []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(".rysh", "humanoids", e.Name(), "SKILL.md")
		if _, err := os.Stat(p); err == nil {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths
}

// resolveChannelConfigRefs expands every ${NAME} reference in a ChannelConfig
// by round-tripping it through YAML — the uniform equivalent of the runtime's
// per-field resolveChannelEnvVars. Unresolved names are returned so the check
// can report the exact missing ${VAR}.
func resolveChannelConfigRefs(cc msg.ChannelConfig, lookup func(string) (string, bool)) (msg.ChannelConfig, []string, error) {
	raw, err := yaml.Marshal(cc)
	if err != nil {
		return cc, nil, err
	}
	// Quote resolved values? Not needed: secrets are single-line scalars and
	// the marshal already quoted the ${NAME} strings they replace.
	expanded, missing := config.ExpandEnvRefs(string(raw), lookup)
	var resolved msg.ChannelConfig
	if err := yaml.Unmarshal([]byte(expanded), &resolved); err != nil {
		return cc, missing, err
	}
	return resolved, missing, nil
}

// validateChannelAdapter dispatches the optional non-binding Validator check.
// validated=false means the adapter offers none (doctor reports it as WARN
// rather than running a side-effectful Start probe).
func validateChannelAdapter(ctx context.Context, a channels.ChannelAdapter) (validated bool, err error) {
	if v, ok := a.(channels.Validator); ok {
		return true, v.Validate(ctx)
	}
	return false, nil
}

// checkChannels validates every humanoid × configured channel.
func checkChannels(cfg config.Config, deps doctorDeps) []doctorCheck {
	paths := listSkillFiles()
	if len(paths) == 0 {
		return []doctorCheck{{Group: "channels", Status: doctorPass, Detail: "no humanoids configured — nothing to check"}}
	}
	var out []doctorCheck
	for _, path := range paths {
		fm, _, err := parseSkillFile(path)
		if err != nil {
			// The config check reports the parse failure with its fix; skip here.
			continue
		}
		var chTypes []string
		for t := range fm.Contacts {
			chTypes = append(chTypes, t)
		}
		sort.Strings(chTypes)
		for _, chType := range chTypes {
			label := fmt.Sprintf("%s/%s", fm.Name, chType)

			// R7 / design 003 G5: a channel that declares neither an allowlist
			// nor a pairing_policy admits EVERY sender under the shipped
			// default. That is the pre-WS3 behaviour and is easy to arrive at
			// by omission rather than intent, so say so here — an open door is
			// otherwise indistinguishable from a configured one.
			if cc := fm.Contacts[chType]; pairingUngated(cc, cfg.PairingDefault) {
				out = append(out, doctorCheck{Group: "channels", Status: doctorWarn,
					Detail: fmt.Sprintf("%s: UNGATED — any sender can talk to this humanoid", label),
					Fix: fmt.Sprintf("add `allowlist:` or `pairing_policy: request` to the %s block in %s, "+
						"set `humanoid_defaults.pairing_default: closed` session-wide, "+
						"or `pairing_policy: open` to say it is deliberate", chType, path)})
			}

			resolved, missing, err := resolveChannelConfigRefs(fm.Contacts[chType], deps.lookupRef)
			if err != nil {
				out = append(out, doctorCheck{Group: "channels", Status: doctorFail,
					Detail: fmt.Sprintf("%s: cannot resolve channel config: %v", label, err),
					Fix:    fmt.Sprintf("fix the %s block in %s", chType, path)})
				continue
			}
			adapter, err := deps.newAdapter(chType, resolved)
			if err != nil {
				out = append(out, doctorCheck{Group: "channels", Status: doctorFail,
					Detail: fmt.Sprintf("%s: %v", label, err),
					Fix:    fmt.Sprintf("fix the %s block in %s", chType, path)})
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), validateProviderTimeout)
			validated, verr := validateChannelAdapter(ctx, adapter)
			cancel()
			switch {
			case !validated:
				out = append(out, doctorCheck{Group: "channels", Status: doctorWarn,
					Detail: fmt.Sprintf("%s: no non-binding validation for %s", label, chType),
					Fix:    fmt.Sprintf(progname.Rewrite("start the channel to fully verify: rysh attach, then ##humanoid channel start %s %s"), fm.Name, chType)})
			case verr != nil:
				fix := fmt.Sprintf("fix the %s credentials referenced in %s", chType, path)
				if len(missing) > 0 {
					fix = fmt.Sprintf("empty ${%s} — export it or add it to .rysh/secrets, then re-run doctor", strings.Join(missing, "}, ${"))
				}
				out = append(out, doctorCheck{Group: "channels", Status: doctorFail,
					Detail: fmt.Sprintf("%s: %v", label, verr), Fix: fix})
			default:
				out = append(out, doctorCheck{Group: "channels", Status: doctorPass,
					Detail: fmt.Sprintf("%s: credentials valid", label)})
			}
		}
	}
	if len(out) == 0 {
		out = append(out, doctorCheck{Group: "channels", Status: doctorPass, Detail: "no humanoid channels configured — nothing to check"})
	}
	return out
}

// ---------------------------------------------------------------------------
// daemon / NATS
// ---------------------------------------------------------------------------

// checkDaemon verifies the session record, daemon liveness, and NATS
// reachability for the configured session name.
func checkDaemon(cfg config.Config, deps doctorDeps) []doctorCheck {
	name := cfg.SessionName
	if name == "" {
		name = "default"
	}
	store, err := session.NewStore(cfg)
	if err != nil {
		return []doctorCheck{{Group: "daemon", Status: doctorFail,
			Detail: fmt.Sprintf("cannot open session store: %v", err),
			Fix:    "check permissions on " + cfg.SessionDir}}
	}
	rec, err := store.Get(name)
	if err != nil {
		return []doctorCheck{{Group: "daemon", Status: doctorWarn,
			Detail: fmt.Sprintf("no live session — no record for session %q", name),
			Fix:    fmt.Sprintf(progname.Rewrite("run \"rysh attach %s\" (or just \"rysh\") to start one"), name)}}
	}
	if rec.PID <= 0 || !session.ProcessAlive(rec.PID) {
		return []doctorCheck{{Group: "daemon", Status: doctorWarn,
			Detail: fmt.Sprintf("session %q is stopped (state: %s)", name, rec.State),
			Fix:    fmt.Sprintf(progname.Rewrite("run \"rysh attach %s\" to restart it"), name)}}
	}
	if rec.NATSPort <= 0 {
		return []doctorCheck{{Group: "daemon", Status: doctorFail,
			Detail: fmt.Sprintf("session %q daemon (PID %d) has no NATS port recorded", name, rec.PID),
			Fix:    fmt.Sprintf(progname.Rewrite("run \"rysh stop %s\" then \"rysh attach %s\""), name, name)}}
	}
	if err := deps.dialNATS(rec.NATSPort, 2*time.Second); err != nil {
		return []doctorCheck{{Group: "daemon", Status: doctorFail,
			Detail: fmt.Sprintf("session %q daemon alive (PID %d) but NATS port %d unreachable: %v", name, rec.PID, rec.NATSPort, err),
			Fix:    fmt.Sprintf(progname.Rewrite("the daemon is wedged — run \"rysh stop %s\" then \"rysh attach %s\""), name, name)}}
	}
	return []doctorCheck{{Group: "daemon", Status: doctorPass,
		Detail: fmt.Sprintf("session %q alive (PID %d, NATS port %d reachable)", name, rec.PID, rec.NATSPort)}}
}

// ---------------------------------------------------------------------------
// config
// ---------------------------------------------------------------------------

// checkConfig verifies rysh.config.yaml parses, every SKILL.md parses, and
// every ${ENV} reference resolves — naming the exact file and ${VAR} on
// failure.
func checkConfig(cfg config.Config, deps doctorDeps) []doctorCheck {
	var out []doctorCheck

	// rysh.config.yaml parses + its ${ENV} references resolve.
	if cfg.ConfigFile == "" {
		out = append(out, doctorCheck{Group: "config", Status: doctorWarn,
			Detail: "no rysh.config.yaml found (defaults apply; config is project-local)",
			Fix:    progname.Rewrite("run `rysh onboard` in this directory to create one")})
	} else if raw, err := os.ReadFile(cfg.ConfigFile); err != nil {
		out = append(out, doctorCheck{Group: "config", Status: doctorFail,
			Detail: fmt.Sprintf("cannot read %s: %v", cfg.ConfigFile, err),
			Fix:    "check the file permissions"})
	} else {
		var parsed map[string]any
		if err := yaml.Unmarshal(raw, &parsed); err != nil {
			out = append(out, doctorCheck{Group: "config", Status: doctorFail,
				Detail: fmt.Sprintf("%s does not parse: %v", cfg.ConfigFile, err),
				Fix:    "fix the YAML (indentation-sensitive; use spaces, not tabs)"})
		} else {
			out = append(out, doctorCheck{Group: "config", Status: doctorPass,
				Detail: fmt.Sprintf("%s parses", cfg.ConfigFile)})
		}
		out = append(out, checkEnvRefs(cfg.ConfigFile, string(raw), deps)...)
	}

	// Every humanoid SKILL.md parses + its ${ENV} references resolve.
	for _, path := range listSkillFiles() {
		if _, _, err := parseSkillFile(path); err != nil {
			out = append(out, doctorCheck{Group: "config", Status: doctorFail,
				Detail: fmt.Sprintf("%s does not parse: %v", path, err),
				Fix:    "fix the YAML frontmatter (--- delimited) in that file"})
			continue
		}
		out = append(out, doctorCheck{Group: "config", Status: doctorPass,
			Detail: fmt.Sprintf("%s parses", path)})
		if raw, err := os.ReadFile(path); err == nil {
			out = append(out, checkEnvRefs(path, string(raw), deps)...)
		}
	}
	return out
}

// checkEnvRefs resolves every ${NAME} reference found in content, reporting
// each unresolved name with the file it came from.
func checkEnvRefs(path, content string, deps doctorDeps) []doctorCheck {
	var out []doctorCheck
	for _, name := range config.EnvRefNames(content) {
		if _, ok := deps.lookupRef(name); !ok {
			out = append(out, doctorCheck{Group: "config", Status: doctorFail,
				Detail: fmt.Sprintf("${%s} in %s does not resolve", name, path),
				Fix:    fmt.Sprintf("export %s or add it to .rysh/secrets (##secret new %s <value>)", name, name)})
		}
	}
	return out
}

// pairingUngated mirrors HumanoidActor.channelPairingGate: it reports whether a
// channel admits every sender. Kept as a pure function so doctor and the actor
// cannot drift — if this answers differently from the runtime, the warning is
// worse than useless.
func pairingUngated(cc msg.ChannelConfig, pairingDefault string) bool {
	if strings.EqualFold(cc.PairingPolicy, "open") {
		return false // explicitly, deliberately open — not a surprise
	}
	if cc.PairingPolicy == "" && len(cc.Allowlist) == 0 {
		return !strings.EqualFold(pairingDefault, "closed")
	}
	return false
}
