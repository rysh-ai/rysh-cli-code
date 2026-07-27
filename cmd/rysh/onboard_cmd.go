package main

// rysh onboard — guided first-run setup for using rysh AT the keyboard
// (design 004 WS4, re-scoped by design 008 WS8/TO1-TO3).
//
// Three steps: (1) pick a provider + API key with live validation and a
// structured write of the `provider:` block into rysh.config.yaml; (2) terminal
// preferences — shell and starting layout — written to the `ui:` block; (3)
// open the session, so the wizard ends with the user INSIDE rysh rather than
// reading instructions about how to get there.
//
// Deliberately NOT here: channels, humanoids, Slack tokens. Design 008 splits
// first-run by audience — remote setup ("drive this session from a chat app")
// belongs to `rysh assistant`. The scaffolding helpers both commands share live
// in skill_scaffold.go.
//
// Secrets are NEVER written into YAML: a literal the user types is persisted to
// the .rysh/secrets tier (0600) and the config gets a ${ENV} reference resolved
// by the existing session → .rysh/secrets → environment precedence.
//
// A flag-driven fallback makes the provider step scriptable without a TTY (and
// never launches a session, which would hang a CI caller):
//
//	rysh onboard --provider anthropic --key-env ANTHROPIC_API_KEY [--model m] [--base-url u] [--no-validate]

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/session"
)

// ---------------------------------------------------------------------------
// Provider registry (step 1)
// ---------------------------------------------------------------------------

// onboardProvider describes one selectable provider. The list mirrors the
// runtime provider seam (provider.NewAgenticProvider): anthropic is rysh's
// shipped default; openai and ollama route through the OpenAI-compatible
// dialect. It grows with WS6 as new providers land in that switch.
type onboardProvider struct {
	Name           string // config `provider.name`
	DefaultModel   string
	DefaultBaseURL string // validation/base URL default; not written unless overridden
	KeyEnv         string // conventional env var for the API key
	KeyOptional    bool   // e.g. local ollama needs no key
}

// onboardProviders returns the selectable providers, default first.
func onboardProviders() []onboardProvider {
	return []onboardProvider{
		{Name: "anthropic", DefaultModel: "claude-sonnet-5", DefaultBaseURL: "https://api.anthropic.com", KeyEnv: "ANTHROPIC_API_KEY"},
		{Name: "openai", DefaultModel: "gpt-4o", DefaultBaseURL: "https://api.openai.com/v1", KeyEnv: "OPENAI_API_KEY"},
		{Name: "ollama", DefaultModel: "llama3.1", DefaultBaseURL: "http://127.0.0.1:11434/v1", KeyEnv: "OLLAMA_API_KEY", KeyOptional: true},
	}
}

// providerByName resolves a provider name (accepting the runtime aliases
// "claude"/"claude-agentic" for anthropic) to its registry entry.
func providerByName(name string) (onboardProvider, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "claude", "claude-agentic", "anthropic":
		return onboardProviders()[0], true
	case "openai":
		return onboardProviders()[1], true
	case "ollama":
		return onboardProviders()[2], true
	}
	return onboardProvider{}, false
}

// ---------------------------------------------------------------------------
// Secret-reference handling (G4): pure, testable
// ---------------------------------------------------------------------------

// keySpec is the normalized form of whatever the user typed into the key
// field: Ref is what goes into YAML (always a ${ENV} reference, never a
// literal), EnvName the reference name, and Literal the raw secret to persist
// into the .rysh/secrets tier ("" when the user supplied a reference).
type keySpec struct {
	Ref     string
	EnvName string
	Literal string
}

// normalizeKeyInput converts raw key input into a keySpec. An exact "${NAME}"
// (or any input containing a reference) passes through as a reference; a bare
// literal is mapped to the provider's conventional env var and remembered for
// persistence. Empty input yields the zero spec (no key).
func normalizeKeyInput(p onboardProvider, raw string) keySpec {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return keySpec{}
	}
	if names := config.EnvRefNames(raw); len(names) > 0 {
		return keySpec{Ref: raw, EnvName: names[0]}
	}
	return keySpec{Ref: "${" + p.KeyEnv + "}", EnvName: p.KeyEnv, Literal: raw}
}

// resolveKeyForValidation returns the literal key value to use for the live
// validation ping: the typed literal, or the reference resolved through the
// config-level tiers (.rysh/secrets files, then the environment).
func resolveKeyForValidation(ryshDir string, k keySpec) (string, bool) {
	if k.Literal != "" {
		return k.Literal, true
	}
	if k.EnvName == "" {
		return "", false
	}
	return config.LookupSecretRef(ryshDir, k.EnvName)
}

// ---------------------------------------------------------------------------
// Live provider validation (shared with rysh doctor)
// ---------------------------------------------------------------------------

// validateProviderTimeout bounds the validation ping (design: short timeout,
// decoded errors, and a "skip validation" escape).
const validateProviderTimeout = 6 * time.Second

// validateProvider performs a minimal, cheap reachability + credential check
// against the named provider — a models-list GET via the same endpoints the
// runtime clients use. On success it returns a short human detail string
// (e.g. "anthropic reachable (claude-…)"). Errors are decoded into friendly
// messages: 401/403 → key rejected, dial/timeout → network. Shared by
// `rysh onboard` step 1 and the `rysh doctor` provider check, so there is one
// validation code path.
func validateProvider(ctx context.Context, name, model, key, baseURL string) (string, error) {
	p, ok := providerByName(name)
	if !ok {
		// Unknown names route through the runtime's default (Claude) branch;
		// validate the same way so the check matches what would execute.
		p = onboardProviders()[0]
	}
	if key == "" && !p.KeyOptional {
		return "", fmt.Errorf("no API key configured for provider %q", p.Name)
	}
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		base = strings.TrimRight(p.DefaultBaseURL, "/")
	}

	var reqURL string
	req, err := (func() (*http.Request, error) {
		switch p.Name {
		case "anthropic":
			reqURL = base + "/v1/models?limit=1"
			r, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
			if err != nil {
				return nil, err
			}
			r.Header.Set("x-api-key", key)
			r.Header.Set("anthropic-version", "2023-06-01")
			return r, nil
		default: // openai, ollama — OpenAI-compatible dialect
			reqURL = base + "/models"
			r, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
			if err != nil {
				return nil, err
			}
			if key != "" {
				r.Header.Set("Authorization", "Bearer "+key)
			}
			return r, nil
		}
	})()
	if err != nil {
		return "", fmt.Errorf("build validation request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", decodeProviderNetErr(p.Name, reqURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		detail := fmt.Sprintf("%s reachable", p.Name)
		if id := firstModelID(body); id != "" {
			detail += fmt.Sprintf(" (%s)", id)
		} else if model != "" {
			detail += fmt.Sprintf(" (model %s)", model)
		}
		return detail, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", fmt.Errorf("%d — key rejected by %s (check the key value%s)",
			resp.StatusCode, p.Name, keyEnvHint(p))
	case resp.StatusCode == http.StatusNotFound:
		return "", fmt.Errorf("404 from %s — endpoint not found (is base URL %q right?)", p.Name, base)
	default:
		return "", fmt.Errorf("%s returned HTTP %d: %s", p.Name, resp.StatusCode, apiErrorMessage(body))
	}
}

// decodeProviderNetErr converts transport-level failures into friendly text.
func decodeProviderNetErr(name, reqURL string, err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		if uerr.Timeout() {
			return fmt.Errorf("timeout reaching %s (%s) — slow or no network", name, reqURL)
		}
		return fmt.Errorf("cannot reach %s (%v) — no network, DNS, or a wrong base URL", name, uerr.Err)
	}
	return fmt.Errorf("cannot reach %s: %v", name, err)
}

// keyEnvHint names the conventional env var in error text, when known.
func keyEnvHint(p onboardProvider) string {
	if p.KeyEnv == "" {
		return ""
	}
	return "; export " + p.KeyEnv + progname.Rewrite(" or re-run `rysh onboard`")
}

// firstModelID extracts data[0].id from a models-list response (both the
// Anthropic and OpenAI shapes), best-effort.
func firstModelID(body []byte) string {
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && len(parsed.Data) > 0 {
		return parsed.Data[0].ID
	}
	return ""
}

// apiErrorMessage extracts error.message from a provider error body, falling
// back to a trimmed body snippet.
func apiErrorMessage(body []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	if s == "" {
		s = "(empty body)"
	}
	return s
}

// ---------------------------------------------------------------------------
// Provider-block config write (structured, preserves unrelated keys)
// ---------------------------------------------------------------------------

// writeProviderConfig persists step-1 results: a typed literal key goes to
// the .rysh/secrets tier (0600) and rysh.config.yaml receives the provider
// block with a ${ENV} reference via a structured YAML round-trip. Returns the
// config path written plus human-readable notes.
func writeProviderConfig(configPath, ryshDir string, p onboardProvider, model string, key keySpec, baseURL string) (string, []string, error) {
	var notes []string
	if key.Literal != "" {
		secretPath, err := config.WriteSecret(ryshDir, "default", key.EnvName, key.Literal)
		if err != nil {
			return "", nil, fmt.Errorf("store secret: %w", err)
		}
		notes = append(notes, fmt.Sprintf("stored the literal key in %s (0600); the config only holds the reference %s", secretPath, key.Ref))
	}
	path := config.ConfigPath(configPath)
	// Only persist a base URL the user explicitly set; provider defaults stay
	// implicit so the runtime's own per-provider defaults keep applying.
	writeURL := baseURL
	if writeURL == p.DefaultBaseURL {
		writeURL = ""
	}
	if err := config.SetProviderBlock(path, p.Name, model, key.Ref, writeURL); err != nil {
		return "", nil, err
	}
	return path, notes, nil
}

// ---------------------------------------------------------------------------
// Entry point + headless (flag-driven) path
// ---------------------------------------------------------------------------

// runOnboard dispatches `rysh onboard`: flag-driven when provider flags are
// given (or stdin is not a TTY), otherwise the Bubble Tea wizard.
func runOnboard(cfg config.Config, configPath string, args []string, logger *slog.Logger) error {
	if hasFlag(args, "--help", "-h") {
		printOnboardUsage()
		return nil
	}
	flagDriven := flagVal(args, "--provider") != "" || flagVal(args, "--key-env") != "" || flagVal(args, "--key") != ""
	if flagDriven {
		// The scriptable path never launches a session: it exists for CI and
		// provisioning, where attaching a TUI would hang the caller.
		return runOnboardHeadless(cfg, configPath, args)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("onboard needs a terminal for the wizard; use the flag form instead:\n" +
			progname.Rewrite("  rysh onboard --provider anthropic --key-env ANTHROPIC_API_KEY [--model <m>] [--base-url <u>] [--no-validate]"))
	}
	return runOnboardWizard(cfg, configPath, logger, !hasFlag(args, "--no-launch"))
}

func printOnboardUsage() {
	fmt.Println(progname.Rewrite("usage: rysh onboard                                   interactive wizard (TTY)"))
	fmt.Println(progname.Rewrite("       rysh onboard --provider <name> [flags]         scriptable, no TTY needed"))
	fmt.Println()
	fmt.Println("Sets up rysh for use AT the keyboard: provider + key (validated), terminal")
	fmt.Println("preferences, then opens your session. To drive a session remotely from")
	fmt.Println(progname.Rewrite("Slack/Telegram/WhatsApp instead, run \"rysh assistant\"."))
	fmt.Println()
	fmt.Println("wizard flags:")
	fmt.Println("  --no-launch           write the config but do not open a session")
	fmt.Println()
	fmt.Println("flags (scriptable form — provider step only, never launches):")
	fmt.Println("  --provider <name>     anthropic (default) | openai | ollama")
	fmt.Println("  --key-env <VAR>       env var holding the API key; written as ${VAR} reference")
	fmt.Println("  --key <literal>       literal key; stored in .rysh/secrets (0600), config gets ${VAR}")
	fmt.Println("  --model <model>       model to write (default: provider default)")
	fmt.Println("  --base-url <url>      override the provider base URL")
	fmt.Println("  --no-validate         skip the live validation ping")
	fmt.Println()
	fmt.Println("Config is PROJECT-LOCAL: rysh.config.yaml + .rysh/ live in the current")
	fmt.Println("directory (there is no global ~/.rysh); onboarding another directory starts fresh.")
}

// runOnboardHeadless is the non-TTY provider step: same writes as the wizard,
// driven by flags. Validation runs unless --no-validate.
func runOnboardHeadless(cfg config.Config, configPath string, args []string) error {
	name := flagVal(args, "--provider")
	if name == "" {
		name = "anthropic"
	}
	p, ok := providerByName(name)
	if !ok {
		return fmt.Errorf("unknown provider %q (valid: anthropic, openai, ollama)", name)
	}
	model := flagVal(args, "--model")
	if model == "" {
		model = p.DefaultModel
	}
	baseURL := flagVal(args, "--base-url")

	var key keySpec
	switch {
	case flagVal(args, "--key-env") != "":
		env := flagVal(args, "--key-env")
		key = normalizeKeyInput(p, "${"+env+"}")
		if key.EnvName == "" {
			return fmt.Errorf("invalid --key-env name %q", env)
		}
	case flagVal(args, "--key") != "":
		key = normalizeKeyInput(p, flagVal(args, "--key"))
	default:
		key = normalizeKeyInput(p, "${"+p.KeyEnv+"}")
	}

	if !hasFlag(args, "--no-validate") {
		literal, found := resolveKeyForValidation(cfg.RyshDir, key)
		if !found && !p.KeyOptional {
			return fmt.Errorf("key reference %s does not resolve (not in .rysh/secrets or the environment); export it or pass --no-validate", key.Ref)
		}
		ctx, cancel := context.WithTimeout(context.Background(), validateProviderTimeout)
		detail, err := validateProvider(ctx, p.Name, model, literal, baseURL)
		cancel()
		if err != nil {
			return fmt.Errorf("provider validation failed: %v (pass --no-validate to save anyway)", err)
		}
		fmt.Printf("validated: %s\n", detail)
	}

	path, notes, err := writeProviderConfig(configPath, cfg.RyshDir, p, model, key, baseURL)
	if err != nil {
		return err
	}
	fmt.Printf("wrote provider block (name: %s, model: %s, api_key: %s) to %s\n", p.Name, model, key.Ref, path)
	for _, n := range notes {
		fmt.Println(n)
	}
	fmt.Println("note: rysh config is project-local — this setup applies to the current directory only.")
	fmt.Println(progname.Rewrite("next: \"rysh doctor\" to verify, \"rysh onboard\" in a terminal to finish setup and"))
	fmt.Println(progname.Rewrite("open a session, or \"rysh assistant\" to drive a session from Slack/Telegram/WhatsApp."))
	return nil
}

// ---------------------------------------------------------------------------
// The Bubble Tea wizard
// ---------------------------------------------------------------------------

type onboardStep int

// The steps of the terminal-onboarding wizard (design 008 §4.1). The channel /
// humanoid steps that used to sit between the provider and the finish line
// moved to `rysh assistant` (remote onboarding) — a user setting up their own
// terminal is never asked about Slack tokens now.
const (
	stepProvider onboardStep = iota
	stepModel
	stepKey
	stepValidating
	stepValidateResult
	stepShell  // TO2 — terminal preferences
	stepLayout // TO2 — initial tabs × panes
	stepLaunch // TO3 — open the session now?
	stepDone
)

// onboardOutcome records the decisions the wizard collected; runOnboardWizard
// executes the launch after the TUI exits, so process/network work never runs
// inside the tea loop.
type onboardOutcome struct {
	ConfigPath  string
	LaunchNow   bool // user consented to open a session (design 004 §8: prompt before spawning)
	SessionLive bool
	SessionName string
}

type valResultMsg struct {
	detail string
	err    error
}

type onboardModel struct {
	cfg        config.Config
	configPath string

	step   onboardStep
	cursor int

	providers []onboardProvider
	provider  onboardProvider
	model     string
	key       keySpec
	baseURL   string

	input textinput.Model

	valDetail    string
	valErr       string
	skippedCheck bool

	// Terminal preferences (TO2).
	shell        string
	initialTabs  int
	initialPanes int

	outcome onboardOutcome
	err     error
	notes   []string
	// sessionNote explains a session name the wizard had to move off (a record
	// owned by the other front-end). Shown at the launch step, where the name
	// is what the user is consenting to.
	sessionNote string
}

func newOnboardModel(cfg config.Config, configPath string, sessionLive bool, sessionName, sessionNote string) onboardModel {
	ti := textinput.New()
	ti.Prompt = "> "
	ti.CharLimit = 512
	ti.Width = 64
	ti.Focus()
	m := onboardModel{
		cfg:          cfg,
		configPath:   configPath,
		providers:    onboardProviders(),
		input:        ti,
		shell:        cfg.DefaultShell,
		initialTabs:  cfg.InitialTabs,
		initialPanes: cfg.InitialPanes,
	}
	// Idempotent pre-fill (G3): fall back to sane defaults only when config
	// carries nothing, so a re-run shows what is currently in effect.
	if m.initialTabs <= 0 {
		m.initialTabs = 1
	}
	if m.initialPanes <= 0 {
		m.initialPanes = 1
	}
	m.outcome.SessionLive = sessionLive
	m.outcome.SessionName = sessionName
	m.sessionNote = sessionNote
	// Idempotent pre-fill (G3): an existing config selects its provider.
	if p, ok := providerByName(cfg.ProviderName); ok {
		for i, cand := range m.providers {
			if cand.Name == p.Name {
				m.cursor = i
			}
		}
	}
	return m
}

func (m onboardModel) Init() tea.Cmd { return textinput.Blink }

// moveCursor is the pure list-navigation helper shared by the picker steps.
func moveCursor(cur, delta, n int) int {
	if n <= 0 {
		return 0
	}
	cur += delta
	if cur < 0 {
		return 0
	}
	if cur >= n {
		return n - 1
	}
	return cur
}

// startTextStep switches the model into a text-input step with a prefill.
func (m *onboardModel) startTextStep(step onboardStep, prefill, placeholder string) {
	m.step = step
	m.input.SetValue(prefill)
	m.input.Placeholder = placeholder
	m.input.CursorEnd()
}

func (m onboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case valResultMsg:
		if m.step != stepValidating {
			return m, nil
		}
		if msg.err != nil {
			m.valErr, m.valDetail = msg.err.Error(), ""
		} else {
			m.valDetail, m.valErr = msg.detail, ""
		}
		m.step = stepValidateResult
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc":
			m.outcome = onboardOutcome{SessionLive: m.outcome.SessionLive, SessionName: m.outcome.SessionName}
			m.err = errors.New("onboard cancelled")
			m.step = stepDone
			return m, tea.Quit
		}
		return m.updateKey(msg)
	}
	// Forward everything else (blink ticks, …) to the focused input.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// updateKey routes a key press to the current step. Each branch delegates to
// small helpers so the step logic stays testable without a tea program.
func (m onboardModel) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	switch m.step {
	case stepProvider:
		switch key {
		case "up", "k":
			m.cursor = moveCursor(m.cursor, -1, len(m.providers))
		case "down", "j":
			m.cursor = moveCursor(m.cursor, +1, len(m.providers))
		case "enter":
			m.provider = m.providers[m.cursor]
			prefill := m.cfg.DefaultModel
			if prefill == "" || !strings.EqualFold(m.cfg.ProviderName, m.provider.Name) {
				prefill = m.provider.DefaultModel
			}
			m.startTextStep(stepModel, prefill, m.provider.DefaultModel)
		}
		return m, nil

	case stepModel:
		if key == "enter" {
			m.model = strings.TrimSpace(m.input.Value())
			if m.model == "" {
				m.model = m.provider.DefaultModel
			}
			// Pre-fill the key from existing config when it is already a
			// reference; literal keys are never echoed back.
			prefill := "${" + m.provider.KeyEnv + "}"
			m.startTextStep(stepKey, prefill, prefill)
			return m, nil
		}

	case stepKey:
		if key == "enter" {
			m.key = normalizeKeyInput(m.provider, m.input.Value())
			m.step = stepValidating
			m.valErr, m.valDetail = "", ""
			return m, m.validateCmd()
		}

	case stepValidating:
		if key == "s" { // skip validation escape while the ping is in flight
			m.skippedCheck = true
			return m.finishProviderStep()
		}
		return m, nil

	case stepValidateResult:
		switch key {
		case "enter":
			if m.valErr == "" {
				return m.finishProviderStep()
			}
			// Failed validation: enter retypes the key.
			m.startTextStep(stepKey, "", "${"+m.provider.KeyEnv+"}")
		case "s": // skip validation, save anyway (design risk table)
			m.skippedCheck = true
			return m.finishProviderStep()
		case "r": // retry
			m.step = stepValidating
			return m, m.validateCmd()
		case "b": // back to provider pick
			m.step = stepProvider
		}
		return m, nil

	case stepShell:
		if key == "enter" {
			if v := strings.TrimSpace(m.input.Value()); v != "" {
				m.shell = v
			}
			m.startTextStep(stepLayout, fmt.Sprintf("%dx%d", m.initialTabs, m.initialPanes), "1x1")
			return m, nil
		}

	case stepLayout:
		if key == "enter" {
			tabs, panes := parseLayoutSpec(m.input.Value(), m.initialTabs, m.initialPanes)
			m.initialTabs, m.initialPanes = tabs, panes
			if err := m.writeTerminalPrefs(); err != nil {
				m.err = err
				m.step = stepDone
				return m, tea.Quit
			}
			m.step = stepLaunch
			m.cursor = 0
			return m, nil
		}

	case stepLaunch:
		switch key {
		case "up", "k":
			m.cursor = moveCursor(m.cursor, -1, 2)
		case "down", "j":
			m.cursor = moveCursor(m.cursor, +1, 2)
		case "enter":
			m.outcome.LaunchNow = m.cursor == 0
			m.step = stepDone
			return m, tea.Quit
		}
		return m, nil
	}

	// Default: feed the key into the focused text input.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	// Mask literal secrets as they are typed; references stay readable.
	if m.step == stepKey {
		if v := m.input.Value(); v != "" && !strings.HasPrefix(v, "$") {
			m.input.EchoMode = textinput.EchoPassword
		} else {
			m.input.EchoMode = textinput.EchoNormal
		}
	} else {
		m.input.EchoMode = textinput.EchoNormal
	}
	return m, cmd
}

// validateCmd runs the live provider ping off the UI thread.
func (m onboardModel) validateCmd() tea.Cmd {
	provider, model, baseURL := m.provider, m.model, m.baseURL
	key := m.key
	ryshDir := m.cfg.RyshDir
	return func() tea.Msg {
		literal, found := resolveKeyForValidation(ryshDir, key)
		if !found && !provider.KeyOptional {
			return valResultMsg{err: fmt.Errorf("reference %s does not resolve (not in .rysh/secrets or the environment) — press s to save anyway and export it later", key.Ref)}
		}
		ctx, cancel := context.WithTimeout(context.Background(), validateProviderTimeout)
		defer cancel()
		detail, err := validateProvider(ctx, provider.Name, model, literal, baseURL)
		return valResultMsg{detail: detail, err: err}
	}
}

// finishProviderStep writes the provider block (+ secret tier) and advances to
// the terminal-preferences step.
func (m onboardModel) finishProviderStep() (tea.Model, tea.Cmd) {
	path, notes, err := writeProviderConfig(m.configPath, m.cfg.RyshDir, m.provider, m.model, m.key, m.baseURL)
	if err != nil {
		m.err = err
		m.step = stepDone
		return m, tea.Quit
	}
	m.outcome.ConfigPath = path
	m.notes = append(m.notes, notes...)
	shell := m.shell
	if shell == "" {
		shell = shellFallback()
	}
	m.startTextStep(stepShell, shell, shell)
	return m, nil
}

// writeTerminalPrefs writes the `ui:` block (TO2). Like the provider block it
// is a yaml.Node delta write, so unrelated keys and comments survive.
func (m *onboardModel) writeTerminalPrefs() error {
	path := m.outcome.ConfigPath
	if path == "" {
		path = m.configPath
	}
	if err := config.SetUIBlock(path, m.shell, m.initialTabs, m.initialPanes); err != nil {
		return err
	}
	m.notes = append(m.notes, fmt.Sprintf("terminal prefs: shell %s, %d tab(s) × %d pane(s)",
		m.shell, m.initialTabs, m.initialPanes))
	return nil
}

// parseLayoutSpec reads "TABSxPANES" (also "TABS PANES", "TABS,PANES" or a bare
// "TABS"). Anything unparseable keeps the current value rather than resetting
// the user's layout to a default they did not ask for.
func parseLayoutSpec(in string, curTabs, curPanes int) (tabs, panes int) {
	tabs, panes = curTabs, curPanes
	fields := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(in)), func(r rune) bool {
		return r == 'x' || r == ' ' || r == ',' || r == '*'
	})
	if len(fields) > 0 {
		if v, err := strconv.Atoi(strings.TrimSpace(fields[0])); err == nil && v > 0 {
			tabs = v
		}
	}
	if len(fields) > 1 {
		if v, err := strconv.Atoi(strings.TrimSpace(fields[1])); err == nil && v > 0 {
			panes = v
		}
	}
	return tabs, panes
}

// shellFallback mirrors the daemon's own default so the wizard prefills what
// rysh would actually use if the key were left unset.
func shellFallback() string {
	if s := strings.TrimSpace(os.Getenv("SHELL")); s != "" {
		return s
	}
	return "/bin/bash"
}

func (m onboardModel) View() string {
	var b strings.Builder
	b.WriteString(progname.Rewrite("rysh onboard — project-local setup (rysh.config.yaml + .rysh/ in this directory)\n\n"))
	switch m.step {
	case stepProvider:
		b.WriteString("Step 1/3 — pick a provider (↑/↓, enter):\n")
		for i, p := range m.providers {
			marker := "  "
			if i == m.cursor {
				marker = "> "
			}
			def := ""
			if i == 0 {
				def = "  (default)"
			}
			fmt.Fprintf(&b, "%s%s%s\n", marker, p.Name, def)
		}
	case stepModel:
		fmt.Fprintf(&b, "Step 1/3 — model for %s (enter accepts):\n%s\n", m.provider.Name, m.input.View())
	case stepKey:
		fmt.Fprintf(&b, "Step 1/3 — API key for %s\n", m.provider.Name)
		fmt.Fprintf(&b, "Enter a ${ENV} reference (recommended) or paste the key; a pasted literal is\nstored in .rysh/secrets (0600) — the config file only ever gets the reference.\n%s\n", m.input.View())
	case stepValidating:
		fmt.Fprintf(&b, "Step 1/3 — validating %s… (s skips validation)\n", m.provider.Name)
	case stepValidateResult:
		if m.valErr == "" {
			fmt.Fprintf(&b, "✓ %s\n\nenter: save and continue   s: skip-validate save   b: back\n", m.valDetail)
		} else {
			fmt.Fprintf(&b, "✗ %s\n\nenter: re-enter key   r: retry   s: save anyway   b: back\n", m.valErr)
		}
	case stepShell:
		fmt.Fprintf(&b, "Provider saved to %s\n", m.outcome.ConfigPath)
		for _, n := range m.notes {
			fmt.Fprintf(&b, "  %s\n", n)
		}
		b.WriteString("\nStep 2/3 — shell for new panes (enter accepts):\n")
		fmt.Fprintf(&b, "%s\n", m.input.View())
	case stepLayout:
		b.WriteString("Step 2/3 — starting layout, tabs × panes (enter accepts):\n")
		fmt.Fprintf(&b, "%s\n", m.input.View())
	case stepLaunch:
		b.WriteString("Step 3/3 — open your rysh session now?\n")
		for _, n := range m.notes {
			fmt.Fprintf(&b, "  %s\n", n)
		}
		if m.sessionNote != "" {
			fmt.Fprintf(&b, "  %s\n", m.sessionNote)
		}
		options := [2]string{}
		if m.outcome.SessionLive {
			options[0] = fmt.Sprintf("attach to the live session %q", m.outcome.SessionName)
		} else {
			options[0] = fmt.Sprintf("start session %q and attach", m.outcome.SessionName)
		}
		options[1] = "not now — just print the command"
		for i, o := range options {
			marker := "  "
			if i == m.cursor {
				marker = "> "
			}
			fmt.Fprintf(&b, "%s%s\n", marker, o)
		}
	case stepDone:
		b.WriteString("…\n")
	}
	b.WriteString("\n(esc cancels)\n")
	return b.String()
}

// runOnboardWizard runs the Bubble Tea program, then executes the start-step
// actions the user consented to.
func runOnboardWizard(cfg config.Config, configPath string, logger *slog.Logger, allowLaunch bool) error {
	sessionName := cfg.SessionName
	if sessionName == "" {
		sessionName = "default"
	}
	sessionLive := false
	var sessionNote string
	store, storeErr := session.NewStore(cfg)
	if storeErr == nil {
		// Settle the name BEFORE the wizard runs: step 3 shows it in the launch
		// prompt, so consent has to be collected for the session we will
		// actually open. A "default" record owned by the desktop app would
		// otherwise be spawned into and refused (see ownableSessionName).
		sessionName, sessionNote = ownableSessionName(store, sessionName, session.NormalizeSource(cfg.SessionSource))
		if rec, err := store.Get(sessionName); err == nil && rec.PID > 0 && session.ProcessAlive(rec.PID) && rec.NATSPort > 0 {
			sessionLive = true
		}
	}

	prog := tea.NewProgram(newOnboardModel(cfg, configPath, sessionLive, sessionName, sessionNote))
	final, err := prog.Run()
	if err != nil {
		return fmt.Errorf("run onboard wizard: %w", err)
	}
	m, ok := final.(onboardModel)
	if !ok {
		return nil
	}
	if m.err != nil {
		return m.err
	}

	// Post-TUI summary + step 3 launch (process work never runs in the tea loop).
	if m.outcome.ConfigPath != "" {
		fmt.Printf("provider block written to %s (api_key: %s)\n", m.outcome.ConfigPath, m.key.Ref)
	}
	for _, n := range m.notes {
		fmt.Println(n)
	}
	if m.sessionNote != "" {
		fmt.Println(m.sessionNote)
	}
	out := m.outcome
	if !allowLaunch {
		out.LaunchNow = false // --no-launch overrides the wizard's answer
	}
	return launchOnboardSession(cfg, logger, store, storeErr, out)
}

// absConfigPath makes a config path absolute so the spawned daemon resolves the
// same file regardless of its working directory. An empty path stays empty —
// the daemon then falls back to its own search order.
func absConfigPath(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// launchOnboardSession is TO3: the wizard's whole point is that the user ends
// up INSIDE a session, not reading instructions about how to get there. It
// reuses the existing attach machinery — spawnDaemon + waitForSession +
// runAttachUI — and only ever spawns with the consent collected in stepLaunch
// (design 004 §8: prompt before starting a background process).
func launchOnboardSession(cfg config.Config, logger *slog.Logger, store *session.Store, storeErr error, out onboardOutcome) error {
	if !out.LaunchNow {
		fmt.Printf(progname.Rewrite("\nwhen you are ready:\n  rysh attach %s\n"), out.SessionName)
		fmt.Println(progname.Rewrite("verify anytime with: rysh doctor"))
		return nil
	}
	if storeErr != nil {
		return fmt.Errorf("open session store: %w", storeErr)
	}
	if !out.SessionLive {
		fmt.Printf("starting session %q…\n", out.SessionName)
		// Hand the daemon the config the wizard just wrote. Leaving it empty
		// made the daemon re-resolve from its cwd, which silently ignored an
		// explicit `rysh onboard --config <path>` — and since RyshDir is derived
		// from the config location, the daemon then registered in a different
		// registry than the one polled below.
		h, err := spawnDaemon(out.SessionName, cfg.LogLevel, absConfigPath(out.ConfigPath), false)
		if err != nil {
			return err
		}
		defer h.cleanup()
		if _, err := waitForSession(store, out.SessionName, 10*time.Second, h); err != nil {
			return daemonStartError(out.SessionName, err)
		}
	}
	rec, err := store.Get(out.SessionName)
	if err != nil {
		return fmt.Errorf("session %q not found after start: %w", out.SessionName, err)
	}
	cfg.SessionName = out.SessionName
	return runAttachUI(cfg, logger, store, rec)
}
