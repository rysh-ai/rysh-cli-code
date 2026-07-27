package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"github.com/rysh-ai/rysh-cli-code/internal/progname"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/rysh-ai/rysh-cli-code/internal/webauto"
)

// AutomationConfig maps the top-level `automation` section: config-level
// defaults for the ##auto command family, one child per subcommand
// (web/task/agent/humanoid/code), each carrying its own step defaults.
//
//	automation:
//	  web:
//	    step: {...}
//	  task:
//	    step: {...}
//	  agent:
//	    step: {...}
//	  humanoid:
//	    step: {...}
type AutomationConfig struct {
	Web      AutomationKindConfig `yaml:"web"`
	Task     AutomationKindConfig `yaml:"task"`
	Agent    AutomationKindConfig `yaml:"agent"`
	Humanoid AutomationKindConfig `yaml:"humanoid"`
	Code     AutomationKindConfig `yaml:"code"`
	// Loop is the kind-INDEPENDENT loop defaults tier, one below the per-kind
	// blocks: `automation.loop.do` seeds the internal-loop executor seat
	// (model/effort of the do/step legs) and `automation.loop.while` seeds
	// the until-judge seat, for every kind that doesn't declare its own.
	// Built-ins below this tier: do → webauto.DefaultStepModel (Sonnet),
	// while → webauto.DefaultJudgeModel (Opus 4.8).
	//
	//	automation:
	//	  loop:
	//	    do:    {model: claude-sonnet-5}
	//	    while: {model: claude-opus-4-8}
	Loop *webauto.LoopConfig `yaml:"loop"`
}

// AutomationKindConfig maps one `automation.<kind>` child: config-level
// defaults for that kind's recipes. Step mirrors the recipe `step`
// frontmatter block (interval, max_iterations, max_duration, auto_continue,
// auto_approve, budget: {size, watch: {takeover_when, takeover_prompt,
// floor}}). Per-field precedence: recipe frontmatter > these defaults >
// built-ins.
//
//	automation:
//	  web:
//	    step:
//	      interval: 30
//	      max_iterations: 300
//	      max_duration: 7m
//	      auto_continue: true
//	      auto_approve: true
//	      budget:
//	        size: 3b
//	        watch:
//	          takeover_when: 90
//	          floor:
//	            trigger_iterations: 50
//	            trigger_duration: 1m
//	            trigger_size: 60p
//	            iterations: 100
//	            duration: 5m
//	            size: 1b
type AutomationKindConfig struct {
	// Step is the config-level default for the per-pass budget (the recipe's
	// `step` / `loop.do` block). Loop carries the full loop-engineering
	// defaults ({do, while}); when Loop.Do is set it wins over Step, the same
	// aliasing rule the recipe uses.
	Step *webauto.StepConfig `yaml:"step"`
	Loop *webauto.LoopConfig `yaml:"loop"`
	// Record is the config-level default for run recording. Only the web kind
	// reads it (the others have no browser to capture):
	//
	//	automation:
	//	  web:
	//	    record:
	//	      enabled: true
	//	      interval: 500ms
	//	      format: jpeg
	//	      quality: 60
	Record *webauto.RecordConfig `yaml:"record"`
}

// AutomationWebConfig is the former name of AutomationKindConfig, kept as an
// alias for compatibility (the web kind was the first ##auto child).
type AutomationWebConfig = AutomationKindConfig

// NATSConfig holds NATS-specific settings.
type NATSConfig struct {
	Mode    string `yaml:"mode"`
	URL     string `yaml:"url"`
	DataDir string `yaml:"data_dir"`
	Port    int    `yaml:"port"`
}

// UpstreamConfig holds settings for the remote upstream server connection.
type UpstreamConfig struct {
	Enabled                bool     `yaml:"enabled"`
	URL                    string   `yaml:"url"`
	APIKey                 string   `yaml:"api_key"`
	Workspace              string   `yaml:"workspace"` // upstream NATS namespace: MUST be the server workspace ID (UUID), shown in the dashboard. Workspace names are unique per user, so the globally unique id is the wire namespace (ws.{id}.*). All sessions using the same id see the same shares.
	AutoShare              bool     `yaml:"auto_share"`
	ReconnectInterval      string   `yaml:"reconnect_interval"`
	MaxReconnectAttempts   int      `yaml:"max_reconnect_attempts"`
	DefaultShareMode       string   `yaml:"default_share_mode"`
	AllowedCommands        []string `yaml:"allowed_commands"`
	CommandApproval        bool     `yaml:"command_approval"`
	CommandBlocklist       []string `yaml:"command_blocklist"`
	MaxSubscribersPerShare int      `yaml:"max_subscribers_per_share"`
	// ForgedAPIAllow / ForgedAPIBlock are glob lists governing which forge-origin
	// operations a remote subscriber may invoke on a forged-API share (Task 2,
	// phase 2b). The block list always wins; a non-empty allow list requires a
	// match; an empty allow list passes reads but denies mutating ops
	// (default-deny mutation). They apply ONLY to forge-generated operations —
	// never built-in tools, which are never shared.
	ForgedAPIAllow []string `yaml:"forged_api_allow"`
	ForgedAPIBlock []string `yaml:"forged_api_block"`
	// ForgedAPIDelegatedAuth opts this share into Model B (delegated subscriber
	// identity, forged-API auth plan §4/§8): when true, a subscriber-supplied
	// access token is injected as the bearer for that one invocation so the
	// backend enforces the subscriber's authorization. Off by default — Model A
	// (owner identity) is used unless explicitly enabled. Gated on the §8 review.
	ForgedAPIDelegatedAuth bool `yaml:"forged_api_delegated_auth"`
	// PredictiveEcho: Mosh-style local echo when typing in a control-mode mirror
	// (shared) tab — typed characters render immediately from a local prediction
	// overlay and are dropped as the source's authoritative echo confirms them, so
	// typing no longer waits a full network round-trip. This is now ALWAYS enabled
	// for control-mode mirrors (it only predicts printable chars and self-corrects
	// against the authoritative stream), so this flag no longer needs to be set; it
	// is retained for backward compatibility.
	PredictiveEcho bool `yaml:"predictive_echo"`
}

// SNATConfig configures SecretNAT — alias ReSet (Reversible Secret
// Translation) — the reversible secret translation layer between rysh and
// the LLM provider. Outbound prompts/conversations/tool traffic have real
// secrets replaced with synthetic tokens; tokens are restored to real values
// only for local tool execution. Command surface: ##snat (alias ##rst).
type SNATConfig struct {
	// Enabled is the session-wide default (per-pane override via ##snat
	// on|off). Defaults to true.
	Enabled bool
	// Mode selects token style: "semantic" (type-preserving, e.g.
	// sk_live_SNAT000001 — default) or "private" (SECRET_TOKEN_001).
	Mode string
	// RestoreDisplay, when true, restores real values into DISPLAYED LLM
	// output. Off by default: the pane display buffer is persisted to KV and
	// forwarded to listeners/shares, so restoring would re-leak secrets.
	RestoreDisplay bool
	// MappingTTL expires idle per-conversation mappings (Go duration string);
	// empty/0 = conversation lifetime.
	MappingTTL string
	// DisableDetectors removes built-in detectors by name (e.g. ["google"]).
	DisableDetectors []string
	// CustomDetectors adds user-defined regex detectors.
	CustomDetectors []SNATCustomDetector
}

// SNATCustomDetector is one user-defined regex detector for the snat section.
type SNATCustomDetector struct {
	Name    string `yaml:"name"`
	Pattern string `yaml:"pattern"`
	Prefix  string `yaml:"prefix"`
}

// WorkspaceName returns the upstream NATS namespace token, defaulting to
// "default". Despite the name, this is the server workspace ID (UUID), not the
// human workspace name: names are unique per user, so the globally unique id is
// what namespaces NATS subjects (ws.{id}.*) on the upstream server. Set
// [upstream] workspace to the workspace id from the dashboard.
func (u UpstreamConfig) WorkspaceName() string {
	if u.Workspace != "" {
		return u.Workspace
	}
	return "default"
}

// ProfileConfig holds per-user identity settings used in collaborative chat.
type ProfileConfig struct {
	Name  string `yaml:"name"`  // display name shown as "Name: message" in chat
	Color string `yaml:"color"` // ANSI color name for the name prefix (e.g. "green", "cyan")
}

// VoiceControlConfig holds the speech-to-text provider settings for voice
// prompting (the [voice_control] section).
type VoiceControlConfig struct {
	TTSProviderName string `yaml:"tts_provider_name"` // "deepgram" (default) | "whisper" (alias: "openai")
	APIKey          string `yaml:"api_key"`           // API key for the selected provider
}

// VoiceConfig holds the recording / TUI behaviour for voice prompting (the
// [voice] section).
type VoiceConfig struct {
	Enabled     bool   `yaml:"enabled"`      // master switch
	Hotkey      string `yaml:"hotkey"`       // toggle record key (Bubble Tea key string), default "ctrl+r"
	Recorder    string `yaml:"recorder"`     // "auto" | "sox" | "ffmpeg" | "afrecord" | "arecord"
	RecorderCmd string `yaml:"recorder_cmd"` // optional full recorder command override (uses {output})
	MaxSeconds  int    `yaml:"max_seconds"`  // hard cap on a single recording (0 = no cap)
	Language    string `yaml:"language"`     // optional BCP-47 language hint; empty = auto
}

// WorkspaceConfig defines a single workspace within a session. A session may
// host multiple isolated workspaces; each one gets its own WorkspaceActor
// subtree under the WorkspaceFarmActor. When no [[workspace]] entries are
// declared in rysh.config.yaml, a single workspace is synthesized from the
// top-level settings (see Config.ResolvedWorkspaces).
type WorkspaceConfig struct {
	Name             string `yaml:"name"`
	InitialTabs      int    `yaml:"initial_tabs"`
	InitialPanes     int    `yaml:"initial_panes"`
	WorkingDirectory string `yaml:"working_directory"`
	// Upstream optionally overrides the session-wide [upstream] block for this
	// workspace (parsed from a [workspace.upstream] sub-table). When nil, the
	// workspace inherits the top-level [upstream]. See ResolveUpstream.
	Upstream *UpstreamConfig `yaml:"upstream"`
	// Forge declares this workspace's forge integrations (add/enable/share) and
	// subscriptions to remote shared APIs. Forge config is WORKSPACE-scoped only —
	// there is no global forge section above the workspace.
	Forge ForgeConfig `yaml:"forge"`
}

// ResolveUpstream computes the effective upstream config for this workspace,
// given the session-wide upstream block. A workspace that declares its own
// [workspace.upstream] uses it, inheriting connection details (url/api_key/
// reconnect/share-mode) from the session block for any field it leaves empty.
// A workspace with no override inherits the session block wholesale.
//
// The upstream namespace (workspace) is the server workspace ID (UUID), which
// is what namespaces NATS subjects on the upstream server (ws.{id}.*) — server
// workspace names are unique per user, not globally, so the id is the wire key.
// It MUST be set explicitly (via [upstream] workspace or [workspace.upstream]
// workspace) for sharing to work. As a last-resort fallback it defaults to the
// local workspace name when left unset, but that only matches an upstream share
// if the server workspace id happens to equal the local name (it normally does
// not), so always set the id from the dashboard.
func (wc WorkspaceConfig) ResolveUpstream(session UpstreamConfig) UpstreamConfig {
	up := session
	if wc.Upstream != nil {
		up = *wc.Upstream
		if up.URL == "" {
			up.URL = session.URL
		}
		if up.APIKey == "" {
			up.APIKey = session.APIKey
		}
		if up.ReconnectInterval == "" {
			up.ReconnectInterval = session.ReconnectInterval
		}
		if up.MaxReconnectAttempts == 0 {
			up.MaxReconnectAttempts = session.MaxReconnectAttempts
		}
		if up.DefaultShareMode == "" {
			up.DefaultShareMode = session.DefaultShareMode
		}
		// Inherit the session-wide predictive-echo setting unless this workspace
		// explicitly turned it on.
		if !up.PredictiveEcho {
			up.PredictiveEcho = session.PredictiveEcho
		}
		// Inherit the forged-API allow/deny policy from the session unless this
		// workspace sets its own — otherwise a top-level [upstream] forged_api_allow
		// is silently dropped for any workspace with a [workspace.upstream] block.
		if len(up.ForgedAPIAllow) == 0 {
			up.ForgedAPIAllow = session.ForgedAPIAllow
		}
		if len(up.ForgedAPIBlock) == 0 {
			up.ForgedAPIBlock = session.ForgedAPIBlock
		}
		if !up.ForgedAPIDelegatedAuth {
			up.ForgedAPIDelegatedAuth = session.ForgedAPIDelegatedAuth
		}
	}
	if up.Workspace == "" {
		up.Workspace = wc.Name
	}
	return up
}

// Config is the main configuration for Rysh.
type Config struct {
	NATS          NATSConfig
	Upstream      UpstreamConfig
	SNAT          SNATConfig
	Profile       ProfileConfig
	Voice         VoiceConfig
	VoiceControl  VoiceControlConfig
	Automation    AutomationConfig
	Usage         UsageConfig
	Proxy         ProxyConfig
	Replay        ReplayConfig
	Registry      RegistryConfig
	Policy        PolicyConfig
	Workspaces    []WorkspaceConfig
	ProviderName  string
	ClaudeCommand string
	SystemPrompt  string
	// PairingDefault is the session-wide admission default for humanoid
	// channels that declare neither an allowlist nor a pairing_policy:
	// "open" (default, pre-WS3 behaviour) or "closed" (design 003 G5).
	PairingDefault string
	DefaultShell   string
	// InteractiveShell, when true, launches each pane's shell with the
	// interactive flag (e.g. `bash -i`) so it sources the user's rc files
	// (~/.bashrc, ~/.zshrc) and uses the user's real prompt (PS1). rysh then
	// streams the shell's native prompt and command echo through verbatim
	// instead of synthesizing a "$ <cmd>" line and stripping prompts. When
	// false, the legacy minimal-prompt behavior is used. Defaults to true.
	InteractiveShell bool
	// ShellColorOutput, when true (the default), preserves ANSI color/style
	// (SGR) sequences in shell-mode pane output so `ls --color`, git diffs,
	// etc. keep their colors in the pane scrollback. Cursor-movement, OSC and
	// other non-styling escapes are still removed, and carriage-return line
	// rewrites are collapsed to their final state. Only effective together
	// with InteractiveShell; when false the legacy strip-everything behavior
	// is used. Config `[ui] shell_color_output` or RYSH_SHELL_COLOR.
	ShellColorOutput bool
	// ShellBufferBytes caps each pane's text output buffers (merged and
	// per-mode) in bytes. <= 0 falls back to the built-in default (256 KiB).
	// Config `[ui] shell_buffer_bytes` or RYSH_SHELL_BUFFER_BYTES.
	ShellBufferBytes int
	// ShellReadlineKeys, when true (the default), enables the bash-style
	// extras rysh adds to shell mode: Ctrl+R incremental reverse-i-search
	// over shell history, prefix history search (Up with a typed draft) and
	// PS2 multi-line continuation. Ctrl+R is the ONLY Ctrl key this takes —
	// every other Ctrl shortcut remains a rysh multiplexer chord (pane/tab/
	// layout/... modes), in shell mode exactly like everywhere else.
	// Config `[ui] shell_readline_keys` or RYSH_SHELL_READLINE.
	ShellReadlineKeys bool
	// ShellHistorySize bounds the persistent shell history file (and the
	// number of entries seeded into a fresh pane) — bash HISTSIZE/HISTFILESIZE.
	// <= 0 falls back to 1000. `[ui] shell_history_size` / RYSH_SHELL_HISTORY_SIZE.
	ShellHistorySize int
	// ShellHistoryIgnoreDups skips recording a shell command identical to the
	// previous entry — bash HISTCONTROL=ignoredups. Default true.
	// `[ui] shell_history_ignore_dups`.
	ShellHistoryIgnoreDups bool
	// ShellHistoryIgnoreSpace skips recording shell commands that start with a
	// space — bash HISTCONTROL=ignorespace. Default false.
	// `[ui] shell_history_ignore_space`.
	ShellHistoryIgnoreSpace bool
	// ShellBashCompletion, when true (the default), delegates Tab completion
	// in argument position to a bash sidecar running the command's
	// programmable completion spec (bash-completion) — git subcommands and
	// branches, ssh hosts, docker/kubectl verbs, command flags. Falls back
	// to built-in file completion when no spec exists or the sidecar fails.
	// `[ui] shell_bash_completion` or RYSH_SHELL_BASH_COMPLETION.
	ShellBashCompletion bool
	// ShellPrompt is the template for the shell-mode input prompt. Supported
	// placeholders: {dir} (basename of the shell's live cwd, "~" at home),
	// {cwd} (full ~-abbreviated path), {session} (session name). The live cwd
	// comes from OSC 7 reporting; when unknown the prompt falls back to "> ".
	// Default "{dir} > ". `[ui] shell_prompt` or RYSH_SHELL_PROMPT.
	ShellPrompt  string
	DefaultModel string
	APIKey       string
	APIURL       string
	BraveAPIKey  string
	MaxTokens    int
	// Thinking enables extended (adaptive) thinking on the agentic provider.
	// On Claude 4.6+ models the model decides when/how much to reason before
	// answering; thinking blocks are recorded on assistant turns and replayed
	// so tool-use loops keep working. Config `[provider] thinking = true` or
	// RYSH_THINKING=1.
	Thinking    bool
	SessionName string
	SessionDir  string
	// SessionSource identifies the front-end that owns sessions this process
	// creates/opens: "cli" (rysh command line, the default) or "app" (rysh
	// desktop app, set via RYSH_SESSION_SOURCE=app by the app sidecar). The
	// daemon stamps it onto the session record; the CLI refuses to open "app"
	// sessions and the app refuses to open "cli" sessions.
	SessionSource    string
	WorkingDirectory string // working directory assigned to newly created panes
	// ConfigFile is the absolute path of the rysh.config.yaml that was loaded,
	// or "" when no config file was found. RyshDir is the rysh directory
	// implied by where that config was found (see resolveConfig): the
	// project-local ".rysh" that lives in (or beside) the working directory.
	// rysh state is ALWAYS project-local — when NO config file is found, RyshDir
	// falls back to "<cwd>/.rysh" (defaultRyshDir). There is no global state root
	// (no ~/.rysh, ~/.local/state/rysh, or ~/.config/rysh). Both are recorded so
	// the running session is aware of where its configuration and rysh state
	// live. RyshDir is always populated, and the session registry (SessionDir =
	// <RyshDir>/sessions) and NATS JetStream data (NATS.DataDir =
	// <RyshDir>/nats) are derived from it unless explicitly overridden.
	ConfigFile        string
	RyshDir           string
	InitialTabs       int
	InitialPanes      int
	LogMessages       bool
	LogLevel          string // "off", "debug", "info", "warn", "error" (default: "off" — logging disabled)
	PrivateBufferSize int    // max bytes for ##private pane output buffer (per pane)
	PublicBufferSize  int    // max bytes for ##public pane output buffer (per pane)
	WebPort           int    // web UI server port (default 23232)
	WebHost           string // web UI bind IP (default "" = web.DefaultHost, 127.0.0.1; set 0.0.0.0 to expose on the network)
	WebToken          string // web UI access token (default "" = no token auth)
	WebAutoStart      bool   // auto-start web server on daemon startup
	// Prometheus metrics exporter (follow-up 3b). When MetricsEnabled, the
	// daemon serves the agentic MetricsSink as Prometheus text on
	// MetricsListen (/metrics). Disabled by default; the in-process sink
	// (##agent metrics) is always available.
	MetricsEnabled bool   // expose Prometheus /metrics endpoint
	MetricsListen  string // listen address for the /metrics server (default 127.0.0.1:9095)
	// ShareFirstTab, when true, auto-shares the very first tab to the upstream
	// workspace as soon as the initial tabs/panes are bootstrapped. Set by the
	// global "--shared" CLI flag (not read from rysh.config.yaml). Requires a
	// configured, enabled [upstream] block to take effect.
	ShareFirstTab bool
	// UpgradeOnAttach controls what happens when "rysh attach" (or the bare
	// "rysh <session>") connects to a live daemon that was launched from a
	// different rysh binary than the one now attaching (detected via the session
	// record's BinHash/Version). Valid values:
	//   "off"    — ignore the mismatch and attach to the old daemon as-is.
	//   "warn"   — print a one-line notice, then attach to the old daemon (default).
	//   "prompt" — ask interactively whether to restart the daemon (upgrade).
	//   "auto"   — restart the daemon from the current binary automatically.
	// The explicit "--upgrade" CLI flag always forces a restart regardless of
	// this setting. Set via [session] upgrade_on_attach or RYSH_UPGRADE_ON_ATTACH.
	UpgradeOnAttach string

	// Forge declares forge integrations to ingest/enable/share and remote forged
	// Forge is the WORKSPACE-scoped forge config for this workspace. It is NOT a
	// global/top-level YAML section — it is parsed from each `workspace:` entry's
	// `forge:` block (WorkspaceConfig.Forge) and copied here per-workspace by the
	// workspace farm. Applied a couple of seconds after start (applyForgeConfig).
	Forge ForgeConfig

	// Secrets are scope-namespaced named secrets declared in rysh.config.yaml
	// under [secrets], nested by scope: secrets[<scope>][<NAME>] = value. A scope
	// is a workspace name (the default storage scope) or a tab ID (for per-tab
	// secrets). They are a resolution tier between the persisted .rysh/secrets
	// files and the process environment, and fill ${NAME} placeholders when
	// agent/humanoid skill files are loaded from a pane, resolving tab → workspace
	// → environment.
	Secrets map[string]map[string]string

	// Variables mirror Secrets for plain environment variables, declared under
	// [variables] and nested by scope: variables[<scope>][<NAME>] = value. Unlike
	// secrets they are NOT SecretNAT-protected (the LLM may see them). They form
	// the config tier between the persisted .rysh/variables files and the process
	// environment, and fill ${NAME} placeholders alongside secrets when
	// agent/humanoid skill files are loaded (secrets take precedence on a clash).
	Variables map[string]map[string]string
}

// ForgeConfig is the declarative startup config for forged-API sharing, declared
// per-workspace under `workspace[].forge`.
type ForgeConfig struct {
	Integrations []ForgeIntegrationConfig `yaml:"integrations"` // (source) add → enable → optionally share
	Subscribe    []ForgeSubscribeConfig   `yaml:"subscribe"`    // (subscriber) subscribe to remote shared APIs
}

// ForgeIntegrationConfig declares one integration to make live on startup.
type ForgeIntegrationConfig struct {
	Name          string           `yaml:"name"`           // integration name (tool prefix)
	Spec          string           `yaml:"spec"`           // path to the OpenAPI/GraphQL spec (~ expanded at apply time)
	BaseURL       string           `yaml:"base_url"`       // absolute base URL (required when the spec server is relative)
	Source        string           `yaml:"source"`         // "openapi" | "graphql" (inferred when empty)
	CredentialEnv string           `yaml:"credential_env"` // env var holding the API key (optional)
	Auth          *ForgeAuthConfig `yaml:"auth"`           // JWT/OAuth2 token flow (owner identity, Model A)
	EnableScope   string           `yaml:"enable_scope"`   // pane|panegroup|lane|tab; empty ⇒ added but not enabled
	Share         bool             `yaml:"share"`          // auto-share via the upstream
}

// ForgeSubscribeConfig declares one remote shared API to mount on startup.
type ForgeSubscribeConfig struct {
	API   string           `yaml:"api"`   // shared API name
	Scope string           `yaml:"scope"` // pane|panegroup|lane|tab (default tab)
	Auth  *ForgeAuthConfig `yaml:"auth"`  // subscriber identity (Model B, delegated) — opt-in
}

// ForgeAuthConfig is the YAML surface for a forged-API token-auth flow (Phase A
// owner identity / Phase B subscriber identity). It mirrors runtime.AuthConfig
// and, like it, holds ONLY env-var names + URLs — never secret values.
type ForgeAuthConfig struct {
	Type            string `yaml:"type"`         // static|jwt_login|oauth2_password|oauth2_client_credentials
	TokenURL        string `yaml:"token_url"`    //
	RefreshURL      string `yaml:"refresh_url"`  // defaults to token_url
	ContentType     string `yaml:"content_type"` // form|json
	UsernameEnv     string `yaml:"username_env"`
	PasswordEnv     string `yaml:"password_env"`
	ClientIDEnv     string `yaml:"client_id_env"`
	ClientSecretEnv string `yaml:"client_secret_env"`
	AccessTokenEnv  string `yaml:"access_token_env"`
	RefreshTokenEnv string `yaml:"refresh_token_env"`
	Scope           string `yaml:"scope"`
	AccessField     string `yaml:"access_token_field"`
	RefreshField    string `yaml:"refresh_token_field"`
	ExpiresInField  string `yaml:"expires_in_field"`
	Header          string `yaml:"header"`
	Scheme          string `yaml:"scheme"`
	ExpiryMargin    string `yaml:"expiry_margin"`
}

// ResolvedWorkspaces returns the effective list of workspaces for the session.
// When rysh.config.yaml declares [[workspace]] entries, they are returned with any
// unset per-workspace fields filled in from the top-level defaults. Otherwise a
// single workspace is synthesized so existing single-workspace configs keep
// working unchanged. The result always contains at least one workspace.
func (c Config) ResolvedWorkspaces() []WorkspaceConfig {
	if len(c.Workspaces) == 0 {
		name := c.SessionName
		if name == "" {
			name = "default"
		}
		return []WorkspaceConfig{{
			Name:             name,
			InitialTabs:      c.InitialTabs,
			InitialPanes:     c.InitialPanes,
			WorkingDirectory: c.WorkingDirectory,
		}}
	}
	out := make([]WorkspaceConfig, len(c.Workspaces))
	for i, ws := range c.Workspaces {
		if ws.Name == "" {
			ws.Name = fmt.Sprintf("ws-%d", i+1)
		}
		if ws.InitialTabs <= 0 {
			ws.InitialTabs = c.InitialTabs
		}
		if ws.InitialPanes <= 0 {
			ws.InitialPanes = c.InitialPanes
		}
		if ws.WorkingDirectory == "" {
			ws.WorkingDirectory = c.WorkingDirectory
		}
		out[i] = ws
	}
	return out
}

// ryshConfigFile is the YAML structure that maps to rysh.config.yaml.
type ryshConfigFile struct {
	Rysh         ryshRysh                     `yaml:"rysh"`
	NATS         ryshNATS                     `yaml:"nats"`
	Upstream     ryshUpstream                 `yaml:"upstream"`
	SNAT         ryshSNAT                     `yaml:"snat"`
	Profile      ryshProfile                  `yaml:"profile"`
	HumanoidCfg  ryshHumanoidDefaults         `yaml:"humanoid_defaults"`
	Provider     ryshProvider                 `yaml:"provider"`
	UI           ryshUI                       `yaml:"ui"`
	Pane         ryshPane                     `yaml:"pane"`
	Web          ryshWeb                      `yaml:"web"`
	Metrics      ryshMetrics                  `yaml:"metrics"`
	Usage        ryshUsage                    `yaml:"usage"`
	Proxy        ryshProxy                    `yaml:"proxy"`
	Replay       ryshReplay                   `yaml:"replay"`
	Registry     ryshRegistry                 `yaml:"registry"`
	Policy       ryshPolicy                   `yaml:"policy"`
	Logging      ryshLogging                  `yaml:"logging"`
	Voice        ryshVoice                    `yaml:"voice"`
	VoiceControl ryshVoiceControl             `yaml:"voice_control"`
	Automation   AutomationConfig             `yaml:"automation"`
	Secrets      map[string]map[string]string `yaml:"secrets"`
	Variables    map[string]map[string]string `yaml:"variables"`
	Workspaces   []WorkspaceConfig            `yaml:"workspace"`
}

type ryshVoiceControl struct {
	TTSProviderName string `yaml:"tts_provider_name"`
	APIKey          string `yaml:"api_key"`
}

type ryshVoice struct {
	Enabled     bool   `yaml:"enabled"`
	Hotkey      string `yaml:"hotkey"`
	Recorder    string `yaml:"recorder"`
	RecorderCmd string `yaml:"recorder_cmd"`
	MaxSeconds  int    `yaml:"max_seconds"`
	Language    string `yaml:"language"`
}

// ryshRysh maps the top-level [rysh] section.
type ryshRysh struct {
	// WorkingDirectory is the inherited default a workspace uses when its own
	// working_directory is empty. When this is also empty it is derived from the
	// location of rysh.config.yaml (see resolveWorkingDirectory). The actual pane
	// working directory is per-workspace (workspace[i].working_directory).
	WorkingDirectory string `yaml:"working_directory"`
	// SessionName is the default session name (the NATS prefix / KV bucket suffix
	// / registry key). Overridden by the CLI/daemon argument and RYSH_SESSION env;
	// defaults to "default" when unset. Moved here from the removed [session]
	// section so sessions stay identified by (config path + name).
	SessionName string `yaml:"session_name"`
	// UpgradeOnAttach controls daemon-version-mismatch behavior on attach
	// (off|warn|prompt|auto). Moved here from the removed [session] section.
	UpgradeOnAttach string `yaml:"upgrade_on_attach"`
}

type ryshProfile struct {
	Name  string `yaml:"name"`
	Color string `yaml:"color"`
}

// ryshHumanoidDefaults carries session-wide humanoid policy (openclaw_roadmap
// R7). PairingDefault decides how a channel that declares NEITHER an allowlist
// NOR a pairing_policy is treated:
//
//	"open"   (default) — ungated; every sender is admitted. Pre-WS3 behaviour.
//	"closed"           — gated with policy "request"; unknown senders become
//	                     pending pairing requests instead of being answered.
//
// Design 003 G5 wants fail-closed, but flipping the default outright would
// silently stop answering for every existing deployment, so it is a documented
// switch and `rysh doctor` WARNs per ungated channel until it is set.
type ryshHumanoidDefaults struct {
	PairingDefault string `yaml:"pairing_default"`
}

type ryshNATS struct {
	Mode    string `yaml:"mode"`
	URL     string `yaml:"url"`
	DataDir string `yaml:"data_dir"`
	Port    int    `yaml:"port"`
}

type ryshUpstream struct {
	Enabled                bool     `yaml:"enabled"`
	URL                    string   `yaml:"url"`
	APIKey                 string   `yaml:"api_key"`
	Workspace              string   `yaml:"workspace"`
	AutoShare              bool     `yaml:"auto_share"`
	ReconnectInterval      string   `yaml:"reconnect_interval"`
	MaxReconnectAttempts   int      `yaml:"max_reconnect_attempts"`
	DefaultShareMode       string   `yaml:"default_share_mode"`
	AllowedCommands        []string `yaml:"allowed_commands"`
	CommandApproval        bool     `yaml:"command_approval"`
	CommandBlocklist       []string `yaml:"command_blocklist"`
	MaxSubscribersPerShare int      `yaml:"max_subscribers_per_share"`
	ForgedAPIAllow         []string `yaml:"forged_api_allow"`
	ForgedAPIBlock         []string `yaml:"forged_api_block"`
	ForgedAPIDelegatedAuth bool     `yaml:"forged_api_delegated_auth"`
	PredictiveEcho         bool     `yaml:"predictive_echo"`
}

// ryshSNAT maps the [snat] section (SecretNAT / ReSet). Enabled and
// RestoreDisplay are pointers so "unset" (nil → keep the default) is
// distinguishable from an explicit false.
type ryshSNAT struct {
	Enabled          *bool                `yaml:"enabled"`
	Mode             string               `yaml:"mode"`
	RestoreDisplay   *bool                `yaml:"restore_display"`
	MappingTTL       string               `yaml:"mapping_ttl"`
	DisableDetectors []string             `yaml:"disable_detectors"`
	CustomDetectors  []SNATCustomDetector `yaml:"custom_detectors"`
}

type ryshProvider struct {
	Name         string `yaml:"name"`
	Command      string `yaml:"command"`
	SystemPrompt string `yaml:"system_prompt"`
	Model        string `yaml:"model"`
	APIKey       string `yaml:"api_key"`
	APIURL       string `yaml:"api_url"`
	BraveAPIKey  string `yaml:"brave_api_key"`
	MaxTokens    int    `yaml:"max_tokens"`
	// Thinking is a pointer so "unset" (nil → keep default false) is
	// distinguishable from an explicit `thinking: false`.
	Thinking *bool `yaml:"thinking"`
}

type ryshUI struct {
	Shell string `yaml:"shell"`
	// InteractiveShell is a pointer so we can tell "unset" (nil → keep the
	// default of true) apart from an explicit `interactive_shell: false`.
	InteractiveShell *bool `yaml:"interactive_shell"`
	// ShellColorOutput is a pointer so "unset" (nil → keep the default of
	// true) is distinguishable from an explicit `shell_color_output: false`.
	ShellColorOutput *bool `yaml:"shell_color_output"`
	ShellBufferBytes int   `yaml:"shell_buffer_bytes"`
	// ShellReadlineKeys is a pointer so "unset" (nil → keep the default of
	// true) is distinguishable from an explicit `shell_readline_keys: false`.
	ShellReadlineKeys *bool `yaml:"shell_readline_keys"`
	ShellHistorySize  int   `yaml:"shell_history_size"`
	// Pointer for the same unset-vs-false reason (default true).
	ShellHistoryIgnoreDups  *bool `yaml:"shell_history_ignore_dups"`
	ShellHistoryIgnoreSpace bool  `yaml:"shell_history_ignore_space"`
	// Pointer for the same unset-vs-false reason (default true).
	ShellBashCompletion *bool  `yaml:"shell_bash_completion"`
	ShellPrompt         string `yaml:"shell_prompt"`
	InitialTabs         int    `yaml:"initial_tabs"`
	InitialPanes        int    `yaml:"initial_panes"`
}

type ryshPane struct {
	PrivateBufferSize int `yaml:"private_buffer_size"`
	PublicBufferSize  int `yaml:"public_buffer_size"`
}

type ryshWeb struct {
	Port      int    `yaml:"port"`
	Host      string `yaml:"host"`
	Token     string `yaml:"token"`
	AutoStart bool   `yaml:"auto_start"`
}

// ryshMetrics maps the [metrics] section (follow-up 3b).
//
//	[metrics]
//	prometheus = true
//	listen     = "127.0.0.1:9095"
type ryshMetrics struct {
	Prometheus bool   `yaml:"prometheus"`
	Listen     string `yaml:"listen"`
}

// ProxyConfig configures the Universal Agent Governance Proxy (design 001).
type ProxyConfig struct {
	// Enabled turns on env injection + the loopback proxy. Default false
	// (safe: existing panes are untouched until opted in via config or ##proxy on).
	Enabled bool
	// InjectKey injects a dummy provider key into pane env; the real key is
	// injected proxy-side from config.APIKey. Off ⇒ the pane's own key is used.
	InjectKey bool
	// AuditContent, when true, would persist redacted request bodies for
	// debugging (never pre-redaction). Metadata-only by default.
	AuditContent bool
	// Upstreams overrides the default dialect → base-URL map (corp gateways).
	Upstreams map[string]string
	// Keys maps a dialect (anthropic|openai|gemini) to the REAL upstream API
	// key the proxy injects server-side.
	//
	// The proxy is multi-dialect by design — a pane can run Claude Code and
	// Codex side by side — so a single key cannot serve it. Before this
	// existed, one key was applied to both the Anthropic and OpenAI dialects,
	// which meant enabling the proxy sent an Anthropic key as the OpenAI
	// bearer and broke every wrapped OpenAI CLI.
	//
	// A dialect with no key here is NOT rewritten: the caller's own
	// credentials pass through untouched, so the pane keeps working (and is
	// still redacted, metered and audited).
	Keys map[string]string
}

// UsageConfig configures the cost-observability ledger (design 003).
type UsageConfig struct {
	// RetentionDays bounds how long day aggregates are kept (default 90).
	RetentionDays int
	// Pricing maps a lowercased model prefix to a USD-per-1M-token override
	// (also the µUSD-per-token cost). Applied over the built-in table.
	Pricing map[string]UsagePriceOverride
}

// UsagePriceOverride is a per-model pricing override (USD per 1M tokens).
type UsagePriceOverride struct {
	In         float64
	Out        float64
	CacheRead  float64
	CacheWrite float64
}

type ryshUsage struct {
	RetentionDays int                            `yaml:"retention_days"`
	Pricing       map[string]ryshUsagePriceEntry `yaml:"pricing"`
}

// ProxyDummyKey is the placeholder written into a pane's provider env var when
// [proxy] inject_key is on. It must never be forwarded upstream or mistaken for
// a real key — the proxy swaps it for the real one server-side.
const ProxyDummyKey = "rysh-proxy-dummy"

// ProxyKeyFor resolves the real upstream API key for a proxy dialect.
//
// "" means "we hold no key for this dialect" — the caller's own credentials
// must then pass through untouched rather than being replaced by a key that
// belongs to a different provider.
func (c Config) ProxyKeyFor(dialect string) string {
	d := strings.ToLower(strings.TrimSpace(dialect))
	if k := strings.TrimSpace(c.Proxy.Keys[d]); k != "" && k != ProxyDummyKey {
		return k
	}
	// Back-compat: the single top-level api_key serves whichever dialect the
	// configured provider speaks, and only that one.
	if c.APIKey != "" && c.APIKey != ProxyDummyKey && d == c.providerDialect() {
		return c.APIKey
	}
	return ""
}

// providerDialect maps the configured provider name onto a proxy dialect.
// Ollama is deliberately absent: it is local and needs no key.
func (c Config) providerDialect() string {
	switch strings.ToLower(strings.TrimSpace(c.ProviderName)) {
	case "openai":
		return "openai"
	case "gemini":
		return "gemini"
	case "ollama":
		return ""
	default: // "", claude, anthropic, claude-agentic
		return "anthropic"
	}
}

type ryshProxy struct {
	Keys         map[string]string `yaml:"keys"`
	Enabled      bool              `yaml:"enabled"`
	InjectKey    bool              `yaml:"inject_key"`
	AuditContent bool              `yaml:"audit_content"`
	Upstreams    map[string]string `yaml:"upstreams"`
}

// ReplayConfig configures session replay capture (design 006).
// RegistryConfig points `rysh install @ns/name` at a package index. Empty URL
// means the first-party default (registry.DefaultIndexURL); set it to host an
// internal or air-gapped mirror. The index format is open — see
// internal/registry/index.go.
type RegistryConfig struct {
	URL string
}

type ryshRegistry struct {
	URL string `yaml:"url"`
}

// PolicyConfig wires policy-as-code (design 013) to an org-level policy file
// merged over .rysh/policy.yaml, strictest-wins (see internal/policy.Merge).
// Empty OrgFile means no org policy is configured — project policy only. A
// configured org file that is missing OR unparseable fails closed at load.
type PolicyConfig struct {
	// OrgFile is the org policy file path ("~" expanded, made absolute at
	// load). `[policy] org_file` or RYSH_ORG_POLICY (env wins).
	OrgFile string
}

type ryshPolicy struct {
	OrgFile string `yaml:"org_file"`
}

type ReplayConfig struct {
	// Enabled turns on in-session output capture for asciicast export.
	Enabled bool
	// Retention bounds the durable JetStream replay stream's age (design 006
	// §3.1). Zero means the replay package default (7d).
	Retention time.Duration
	// MaxBytes caps the durable replay stream's size. Zero means the replay
	// package default (1 GiB).
	MaxBytes int64
}

type ryshReplay struct {
	Enabled   bool   `yaml:"enabled"`
	Retention string `yaml:"retention"` // duration, day suffix allowed: "7d", "36h"
	MaxBytes  string `yaml:"max_bytes"` // byte size: "1GB", "512MB", or a plain byte count
}

// parseDurationDays parses a positive duration, accepting a "d" (day) suffix
// on top of time.ParseDuration's units — retention windows are naturally
// written in days ("7d") but Go's parser stops at hours.
func parseDurationDays(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if strings.HasSuffix(s, "d") {
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil || n <= 0 {
			return 0, false
		}
		return time.Duration(n * 24 * float64(time.Hour)), true
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// parseByteSize parses a positive byte count with an optional KB/MB/GB suffix
// (binary multiples, case-insensitive, "KiB"-style spellings accepted).
func parseByteSize(s string) (int64, bool) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0, false
	}
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{
		{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30},
		{"KB", 1 << 10}, {"MB", 1 << 20}, {"GB", 1 << 30}, {"B", 1},
	} {
		if strings.HasSuffix(s, u.suffix) {
			mult = u.mult
			s = strings.TrimSpace(strings.TrimSuffix(s, u.suffix))
			break
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return int64(n * float64(mult)), true
}

type ryshUsagePriceEntry struct {
	In         float64 `yaml:"in"`
	Out        float64 `yaml:"out"`
	CacheRead  float64 `yaml:"cache_read"`
	CacheWrite float64 `yaml:"cache_write"`
}

type ryshLogging struct {
	LogMessages bool   `yaml:"log_messages"`
	LogLevel    string `yaml:"log_level"`
}

// Load reads configuration from rysh.config.yaml, then applies environment
// variable overrides. The config file is searched in the current directory
// first, then <cwd>/.rysh/rysh.config.yaml — never any home-directory
// location (rysh has no global state root; see resolveConfig).
//
// When the config file exists but cannot be parsed, a warning is written to
// stderr. This avoids the silent trap where a single malformed value (for
// example bad YAML indentation or a string where a number is expected) causes
// the entire configuration -- session name, provider, upstream,
// working_directory -- to be silently discarded in favor of defaults.
func Load() Config {
	cfg, _ := load(os.Stderr)
	return cfg
}

// LoadFrom reads configuration from an explicit file path, then applies
// environment variable overrides. When path is empty it behaves exactly like
// Load (searching the current directory, then <cwd>/.rysh/rysh.config.yaml).
// This backs the global "--config <path>" CLI flag.
func LoadFrom(path string) Config {
	cfg, _ := loadFrom(path, os.Stderr)
	return cfg
}

// load is the testable core of Load. Diagnostics about an unparsable config
// file are written to w, and the parse error (if any) is returned.
func load(w io.Writer) (Config, error) {
	return loadFrom("", w)
}

// loadFrom is the testable core of LoadFrom. When explicitPath is non-empty it
// is used verbatim; otherwise the default search locations are consulted.
// Diagnostics about an unparsable config file are written to w, and the parse
// error (if any) is returned.
func loadFrom(explicitPath string, w io.Writer) (Config, error) {
	var file, ryshDir string
	if explicitPath != "" {
		file = explicitPath
		ryshDir = ryshDirForConfig(explicitPath)
	} else {
		file, ryshDir = resolveConfig()
	}
	if ryshDir == "" {
		ryshDir = defaultRyshDir()
	}
	// An explicit RYSH_DIR overrides the resolved rysh dir for both the
	// recorded RyshDir and the storage paths derived from it below. This is
	// resolved here (rather than in applyEnvOverrides) so the override flows
	// into SessionDir / NATS.DataDir before those defaults are computed.
	if v := env("RYSH_DIR"); v != "" {
		ryshDir = absOrEmpty(expandHome(v))
	}

	cfg := applyDefaults()
	cfg.ConfigFile = absOrEmpty(file)
	cfg.RyshDir = ryshDir
	// The session registry and NATS JetStream data live under the rysh dir so
	// each rysh dir is a self-contained state root (<rysh-dir>/sessions and
	// <rysh-dir>/nats). Explicit config-file values (session.dir /
	// nats.data_dir) and env overrides (RYSH_SESSION_DIR / RYSH_NATS_DATA_DIR),
	// applied below, still take precedence over these derived defaults.
	cfg.SessionDir = filepath.Join(ryshDir, "sessions")
	cfg.NATS.DataDir = filepath.Join(ryshDir, "nats")
	var loadErr error
	if file != "" {
		var err error
		cfg, err = loadFromFile(file, cfg)
		if err != nil {
			loadErr = err
			fmt.Fprintf(w, progname.Rewrite("rysh: warning: failed to parse config file %s: %v\n"), file, err)
			fmt.Fprint(w, progname.Rewrite("rysh: warning: the config file is being ignored; defaults and environment overrides apply instead. ")+
				"Note that YAML is indentation-sensitive (use spaces, not tabs) and values must have the expected type, e.g. working_directory: \"~/projects/app\".\n")
		}
	}
	// The provider api_key may be a ${ENV} secret reference written by
	// `rysh onboard` (design 004 G4: references in YAML, never literals).
	// Resolve it through the config-level tiers (.rysh/secrets files, then the
	// environment) BEFORE applyEnvOverrides: an unresolved reference degrades
	// to "" so the ANTHROPIC_API_KEY fallback below still applies, instead of
	// the literal "${VAR}" string being sent to the provider as a key.
	if EnvRefPattern.MatchString(cfg.APIKey) {
		expanded, missing := ExpandEnvRefs(cfg.APIKey, func(name string) (string, bool) {
			return LookupSecretRef(ryshDir, name)
		})
		if len(missing) > 0 {
			cfg.APIKey = ""
		} else {
			cfg.APIKey = expanded
		}
	}
	cfg = applyEnvOverrides(cfg)
	// Fold the automation model-seat defaults into every kind (recipe >
	// automation.<kind> > automation.loop > built-ins), so the internal loop
	// (do/step legs) and the until-judge always have a resolved model.
	applyAutomationLLMDefaults(&cfg.Automation)
	return cfg, loadErr
}

// applyAutomationLLMDefaults resolves the two automation model seats for every
// kind, per-field: the kind's own value wins, then the global `automation.loop`
// block from rysh.config.yaml, then the built-ins (internal loop / do legs →
// webauto.DefaultStepModel, until-judge → webauto.DefaultJudgeModel).
//
// The executor seat is seeded into the exact block EffectiveStepDef will
// select (loop.do when the kind declares one, step otherwise) so block-level
// aliasing can't shadow it. The judge seat is seeded into loop.while; creating
// an empty while block is inert — a loop only runs when a recipe (or config)
// supplies an `until` prompt.
func applyAutomationLLMDefaults(a *AutomationConfig) {
	doModel, doEffort := webauto.DefaultStepModel, ""
	judgeModel, judgeEffort := webauto.DefaultJudgeModel, ""
	if g := a.Loop; g != nil {
		if g.Do != nil {
			if m := strings.TrimSpace(g.Do.Model); m != "" {
				doModel = m
			}
			if e := strings.TrimSpace(g.Do.Effort); e != "" {
				doEffort = e
			}
		}
		if g.While != nil {
			if m := strings.TrimSpace(g.While.Model); m != "" {
				judgeModel = m
			}
			if e := strings.TrimSpace(g.While.Effort); e != "" {
				judgeEffort = e
			}
		}
	}
	for _, k := range []*AutomationKindConfig{&a.Web, &a.Task, &a.Agent, &a.Humanoid, &a.Code} {
		// Executor seat (internal loop legs).
		step := k.Step
		if k.Loop != nil && k.Loop.Do != nil {
			step = k.Loop.Do
		} else if step == nil {
			step = &webauto.StepConfig{}
			k.Step = step
		}
		if strings.TrimSpace(step.Model) == "" {
			step.Model = doModel
		}
		if strings.TrimSpace(step.Effort) == "" && doEffort != "" {
			step.Effort = doEffort
		}
		// Judge seat.
		if k.Loop == nil {
			k.Loop = &webauto.LoopConfig{}
		}
		if k.Loop.While == nil {
			k.Loop.While = &webauto.WhileConfig{}
		}
		if strings.TrimSpace(k.Loop.While.Model) == "" {
			k.Loop.While.Model = judgeModel
		}
		if strings.TrimSpace(k.Loop.While.Effort) == "" && judgeEffort != "" {
			k.Loop.While.Effort = judgeEffort
		}
	}
}

func applyDefaults() Config {
	return Config{
		NATS: NATSConfig{
			Mode: "embedded",
			URL:  "nats://localhost:4222",
			Port: 24242,
		},
		Upstream: UpstreamConfig{
			Enabled:              false,
			ReconnectInterval:    "5s",
			MaxReconnectAttempts: -1, // -1 = unlimited reconnect attempts
		},
		SNAT: SNATConfig{
			Enabled: true, // privacy protection is on out of the box
			Mode:    "semantic",
		},
		Voice: VoiceConfig{
			Enabled:    false,
			Hotkey:     "ctrl+r",
			Recorder:   "auto",
			MaxSeconds: 120,
		},
		VoiceControl: VoiceControlConfig{
			TTSProviderName: "deepgram",
		},
		LogLevel:                "off",
		ProviderName:            "claude",
		ClaudeCommand:           "claude",
		SystemPrompt:            "system-prompt.md",
		DefaultShell:            shellDefault(),
		InteractiveShell:        true,
		ShellColorOutput:        true,
		ShellBufferBytes:        262144,
		ShellReadlineKeys:       true,
		ShellHistorySize:        1000,
		ShellHistoryIgnoreDups:  true,
		ShellHistoryIgnoreSpace: false,
		ShellBashCompletion:     true,
		ShellPrompt:             "{dir} > ",
		DefaultModel:            "",
		APIKey:                  "",
		APIURL:                  "https://api.anthropic.com",
		MaxTokens:               4096,
		SessionName:             "default",
		SessionDir:              defaultSessionDir(),
		SessionSource:           "cli",
		UpgradeOnAttach:         "warn",
		InitialTabs:             1,
		InitialPanes:            1,
		PrivateBufferSize:       10240,
		PublicBufferSize:        10240,
		WebPort:                 23232,
		MetricsListen:           "127.0.0.1:9095",
	}
}

func loadFromFile(path string, cfg Config) (Config, error) {
	var f ryshConfigFile
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		// If the file cannot be parsed, fall back to defaults and surface the
		// error so callers can warn the user instead of silently ignoring it.
		return cfg, err
	}

	// NATS section
	if f.NATS.Mode != "" {
		cfg.NATS.Mode = f.NATS.Mode
	}
	if f.NATS.URL != "" {
		cfg.NATS.URL = f.NATS.URL
	}
	if f.NATS.DataDir != "" {
		cfg.NATS.DataDir = f.NATS.DataDir
	}
	if f.NATS.Port > 0 {
		cfg.NATS.Port = f.NATS.Port
	}

	// Automation section: config-level defaults for the ##auto command family,
	// one step block per kind (web/task/agent/humanoid/code).
	if f.Automation.Web.Step != nil {
		cfg.Automation.Web.Step = f.Automation.Web.Step
	}
	if f.Automation.Task.Step != nil {
		cfg.Automation.Task.Step = f.Automation.Task.Step
	}
	if f.Automation.Agent.Step != nil {
		cfg.Automation.Agent.Step = f.Automation.Agent.Step
	}
	if f.Automation.Humanoid.Step != nil {
		cfg.Automation.Humanoid.Step = f.Automation.Humanoid.Step
	}
	if f.Automation.Code.Step != nil {
		cfg.Automation.Code.Step = f.Automation.Code.Step
	}
	if f.Automation.Web.Loop != nil {
		cfg.Automation.Web.Loop = f.Automation.Web.Loop
	}
	if f.Automation.Task.Loop != nil {
		cfg.Automation.Task.Loop = f.Automation.Task.Loop
	}
	if f.Automation.Agent.Loop != nil {
		cfg.Automation.Agent.Loop = f.Automation.Agent.Loop
	}
	if f.Automation.Humanoid.Loop != nil {
		cfg.Automation.Humanoid.Loop = f.Automation.Humanoid.Loop
	}
	if f.Automation.Code.Loop != nil {
		cfg.Automation.Code.Loop = f.Automation.Code.Loop
	}
	if f.Automation.Loop != nil {
		cfg.Automation.Loop = f.Automation.Loop
	}

	// Provider section
	if f.Provider.Name != "" {
		cfg.ProviderName = f.Provider.Name
	}
	if f.Provider.Command != "" {
		cfg.ClaudeCommand = f.Provider.Command
	}
	if f.Provider.SystemPrompt != "" {
		cfg.SystemPrompt = f.Provider.SystemPrompt
	}
	if f.Provider.Model != "" {
		cfg.DefaultModel = f.Provider.Model
	}
	if f.Provider.APIKey != "" {
		cfg.APIKey = f.Provider.APIKey
	}
	if f.Provider.APIURL != "" {
		cfg.APIURL = f.Provider.APIURL
	}
	if f.Provider.BraveAPIKey != "" {
		cfg.BraveAPIKey = f.Provider.BraveAPIKey
	}
	if f.Provider.MaxTokens > 0 {
		cfg.MaxTokens = f.Provider.MaxTokens
	}
	if f.Provider.Thinking != nil {
		cfg.Thinking = *f.Provider.Thinking
	}

	// Session identity / behavior moved from the removed [session] section into
	// [rysh]. The session name is a default only — the CLI/daemon argument and
	// RYSH_SESSION env still take precedence (applied later). The registry is
	// always <RyshDir>/sessions (overridable via RYSH_SESSION_DIR env only).
	if f.Rysh.SessionName != "" {
		cfg.SessionName = f.Rysh.SessionName
	}
	if v := normalizeUpgradeMode(f.Rysh.UpgradeOnAttach); v != "" {
		cfg.UpgradeOnAttach = v
	}

	// Humanoid defaults (R7)
	if v := strings.ToLower(strings.TrimSpace(f.HumanoidCfg.PairingDefault)); v == "open" || v == "closed" {
		cfg.PairingDefault = v
	}

	// UI section
	if f.UI.Shell != "" {
		cfg.DefaultShell = f.UI.Shell
	}
	if f.UI.InteractiveShell != nil {
		cfg.InteractiveShell = *f.UI.InteractiveShell
	}
	if f.UI.ShellColorOutput != nil {
		cfg.ShellColorOutput = *f.UI.ShellColorOutput
	}
	if f.UI.ShellBufferBytes > 0 {
		cfg.ShellBufferBytes = f.UI.ShellBufferBytes
	}
	if f.UI.ShellReadlineKeys != nil {
		cfg.ShellReadlineKeys = *f.UI.ShellReadlineKeys
	}
	if f.UI.ShellHistorySize > 0 {
		cfg.ShellHistorySize = f.UI.ShellHistorySize
	}
	if f.UI.ShellHistoryIgnoreDups != nil {
		cfg.ShellHistoryIgnoreDups = *f.UI.ShellHistoryIgnoreDups
	}
	if f.UI.ShellHistoryIgnoreSpace {
		cfg.ShellHistoryIgnoreSpace = true
	}
	if f.UI.ShellBashCompletion != nil {
		cfg.ShellBashCompletion = *f.UI.ShellBashCompletion
	}
	if f.UI.ShellPrompt != "" {
		cfg.ShellPrompt = f.UI.ShellPrompt
	}
	if f.UI.InitialTabs > 0 {
		cfg.InitialTabs = f.UI.InitialTabs
	}
	if f.UI.InitialPanes > 0 {
		cfg.InitialPanes = f.UI.InitialPanes
	}

	// Workspace sections ([[workspace]]). Each entry defines an isolated
	// workspace hosted by the WorkspaceFarmActor. A non-empty per-workspace
	// working_directory is resolved relative to the config file; an empty one
	// is left blank so it inherits the top-level working directory at runtime
	// (see Config.ResolvedWorkspaces). A configured directory that does not
	// exist is blanked too, so it falls back through the same inherit chain
	// (rysh.working_directory -> config dir) instead of stranding panes in $HOME.
	if len(f.Workspaces) > 0 {
		ws := make([]WorkspaceConfig, len(f.Workspaces))
		for i, w := range f.Workspaces {
			if strings.TrimSpace(w.WorkingDirectory) != "" {
				resolved := resolveWorkingDirectory(path, w.WorkingDirectory)
				if isExistingDir(resolved) {
					w.WorkingDirectory = resolved
				} else {
					w.WorkingDirectory = "" // inherit rysh.working_directory at runtime
				}
			}
			ws[i] = w
		}
		cfg.Workspaces = ws
	}

	// Pane section
	if f.Pane.PrivateBufferSize > 0 {
		cfg.PrivateBufferSize = f.Pane.PrivateBufferSize
	}
	if f.Pane.PublicBufferSize > 0 {
		cfg.PublicBufferSize = f.Pane.PublicBufferSize
	}

	// Web section
	if f.Web.Port > 0 {
		cfg.WebPort = f.Web.Port
	}
	if v := strings.TrimSpace(f.Web.Host); v != "" {
		cfg.WebHost = v
	}
	if v := strings.TrimSpace(f.Web.Token); v != "" {
		cfg.WebToken = v
	}
	if f.Web.AutoStart {
		cfg.WebAutoStart = true
	}

	// Metrics section (follow-up 3b)
	if f.Usage.RetentionDays > 0 {
		cfg.Usage.RetentionDays = f.Usage.RetentionDays
	}
	if len(f.Usage.Pricing) > 0 {
		cfg.Usage.Pricing = make(map[string]UsagePriceOverride, len(f.Usage.Pricing))
		for k, v := range f.Usage.Pricing {
			cfg.Usage.Pricing[k] = UsagePriceOverride{In: v.In, Out: v.Out, CacheRead: v.CacheRead, CacheWrite: v.CacheWrite}
		}
	}
	if f.Proxy.Enabled {
		cfg.Proxy.Enabled = true
	}
	if f.Proxy.InjectKey {
		cfg.Proxy.InjectKey = true
	}
	if f.Proxy.AuditContent {
		cfg.Proxy.AuditContent = true
	}
	if len(f.Proxy.Upstreams) > 0 {
		cfg.Proxy.Upstreams = f.Proxy.Upstreams
	}
	if len(f.Proxy.Keys) > 0 {
		if cfg.Proxy.Keys == nil {
			cfg.Proxy.Keys = map[string]string{}
		}
		for dialect, key := range f.Proxy.Keys {
			cfg.Proxy.Keys[strings.ToLower(strings.TrimSpace(dialect))] = key
		}
	}
	if f.Replay.Enabled {
		cfg.Replay.Enabled = true
	}
	if d, ok := parseDurationDays(f.Replay.Retention); ok {
		cfg.Replay.Retention = d
	}
	if b, ok := parseByteSize(f.Replay.MaxBytes); ok {
		cfg.Replay.MaxBytes = b
	}
	if v := strings.TrimSpace(f.Registry.URL); v != "" {
		cfg.Registry.URL = v
	}
	if v := strings.TrimSpace(f.Policy.OrgFile); v != "" {
		cfg.Policy.OrgFile = absOrEmpty(expandHome(v))
	}
	if f.Metrics.Prometheus {
		cfg.MetricsEnabled = true
	}
	if s := strings.TrimSpace(f.Metrics.Listen); s != "" {
		cfg.MetricsListen = s
	}

	// Upstream section
	if f.Upstream.Enabled {
		cfg.Upstream.Enabled = true
	}
	if f.Upstream.URL != "" {
		cfg.Upstream.URL = f.Upstream.URL
	}
	if f.Upstream.APIKey != "" {
		cfg.Upstream.APIKey = f.Upstream.APIKey
	}
	if f.Upstream.Workspace != "" {
		cfg.Upstream.Workspace = f.Upstream.Workspace
	}
	if f.Upstream.AutoShare {
		cfg.Upstream.AutoShare = true
	}
	if f.Upstream.ReconnectInterval != "" {
		cfg.Upstream.ReconnectInterval = f.Upstream.ReconnectInterval
	}
	if f.Upstream.MaxReconnectAttempts > 0 {
		cfg.Upstream.MaxReconnectAttempts = f.Upstream.MaxReconnectAttempts
	}
	if f.Upstream.DefaultShareMode != "" {
		cfg.Upstream.DefaultShareMode = f.Upstream.DefaultShareMode
	}
	if len(f.Upstream.AllowedCommands) > 0 {
		cfg.Upstream.AllowedCommands = f.Upstream.AllowedCommands
	}
	if f.Upstream.CommandApproval {
		cfg.Upstream.CommandApproval = true
	}
	if len(f.Upstream.CommandBlocklist) > 0 {
		cfg.Upstream.CommandBlocklist = f.Upstream.CommandBlocklist
	}
	if len(f.Upstream.ForgedAPIAllow) > 0 {
		cfg.Upstream.ForgedAPIAllow = f.Upstream.ForgedAPIAllow
	}
	if len(f.Upstream.ForgedAPIBlock) > 0 {
		cfg.Upstream.ForgedAPIBlock = f.Upstream.ForgedAPIBlock
	}
	if f.Upstream.ForgedAPIDelegatedAuth {
		cfg.Upstream.ForgedAPIDelegatedAuth = true
	}
	if f.Upstream.MaxSubscribersPerShare > 0 {
		cfg.Upstream.MaxSubscribersPerShare = f.Upstream.MaxSubscribersPerShare
	}
	if f.Upstream.PredictiveEcho {
		cfg.Upstream.PredictiveEcho = true
	}

	// SNAT section (SecretNAT / ReSet). Enabled/RestoreDisplay are pointers
	// so an explicit `enabled: false` can turn the default-on feature off.
	if f.SNAT.Enabled != nil {
		cfg.SNAT.Enabled = *f.SNAT.Enabled
	}
	if f.SNAT.Mode != "" {
		cfg.SNAT.Mode = f.SNAT.Mode
	}
	if f.SNAT.RestoreDisplay != nil {
		cfg.SNAT.RestoreDisplay = *f.SNAT.RestoreDisplay
	}
	if f.SNAT.MappingTTL != "" {
		cfg.SNAT.MappingTTL = f.SNAT.MappingTTL
	}
	if len(f.SNAT.DisableDetectors) > 0 {
		cfg.SNAT.DisableDetectors = f.SNAT.DisableDetectors
	}
	if len(f.SNAT.CustomDetectors) > 0 {
		cfg.SNAT.CustomDetectors = f.SNAT.CustomDetectors
	}

	// Profile section
	if f.Profile.Name != "" {
		cfg.Profile.Name = f.Profile.Name
	}
	if f.Profile.Color != "" {
		cfg.Profile.Color = f.Profile.Color
	}

	// Logging section
	cfg.LogMessages = f.Logging.LogMessages
	if f.Logging.LogLevel != "" {
		cfg.LogLevel = f.Logging.LogLevel
	}

	// Voice control section (transcription provider).
	if f.VoiceControl.TTSProviderName != "" {
		cfg.VoiceControl.TTSProviderName = f.VoiceControl.TTSProviderName
	}
	if f.VoiceControl.APIKey != "" {
		cfg.VoiceControl.APIKey = f.VoiceControl.APIKey
	}

	// Voice section (recording / TUI behaviour).
	if f.Voice.Enabled {
		cfg.Voice.Enabled = true
	}
	if f.Voice.Hotkey != "" {
		cfg.Voice.Hotkey = f.Voice.Hotkey
	}
	if f.Voice.Recorder != "" {
		cfg.Voice.Recorder = f.Voice.Recorder
	}
	if f.Voice.RecorderCmd != "" {
		cfg.Voice.RecorderCmd = f.Voice.RecorderCmd
	}
	if f.Voice.MaxSeconds > 0 {
		cfg.Voice.MaxSeconds = f.Voice.MaxSeconds
	}
	if f.Voice.Language != "" {
		cfg.Voice.Language = f.Voice.Language
	}

	// Secrets section ([secrets]): scope-namespaced name → value maps nested by
	// scope (a workspace name, or a tab ID for per-tab secrets), a resolution tier
	// between .rysh/secrets and the environment.
	if len(f.Secrets) > 0 {
		cfg.Secrets = f.Secrets
	}

	// Variables section ([variables]): the same scope-namespaced shape as
	// [secrets] but for plain environment variables — a resolution tier between
	// .rysh/variables and the environment.
	if len(f.Variables) > 0 {
		cfg.Variables = f.Variables
	}

	// The inherited default working directory that a workspace uses when its own
	// working_directory is empty (see ResolvedWorkspaces). [rysh] working_directory
	// is the source; when it is empty — or points at a directory that does not
	// exist — fall back to the directory of rysh.config.yaml
	// (resolveWorkingDirectory("")), so a typo'd path doesn't strand panes in $HOME.
	explicitWD := strings.TrimSpace(f.Rysh.WorkingDirectory)
	wd := resolveWorkingDirectory(path, explicitWD)
	if explicitWD != "" && !isExistingDir(wd) {
		wd = resolveWorkingDirectory(path, "")
	}
	cfg.WorkingDirectory = wd

	return cfg, nil
}

func applyEnvOverrides(cfg Config) Config {
	if v := env("RYSH_NATS_MODE"); v != "" {
		cfg.NATS.Mode = v
	}
	if v := env("RYSH_NATS_URL"); v != "" {
		cfg.NATS.URL = v
	}
	if v := env("RYSH_NATS_DATA_DIR"); v != "" {
		cfg.NATS.DataDir = v
	}
	if v := envInt("RYSH_NATS_PORT"); v > 0 {
		cfg.NATS.Port = v
	}
	if v := envInt("RYSH_WEB_PORT"); v > 0 {
		cfg.WebPort = v
	}
	if v := env("RYSH_WEB_HOST"); v != "" {
		cfg.WebHost = v
	}
	if v := env("RYSH_WEB_TOKEN"); v != "" {
		cfg.WebToken = v
	}
	if v := env("RYSH_WEB_AUTO_START"); v == "true" || v == "1" {
		cfg.WebAutoStart = true
	}
	if v := env("RYSH_METRICS_PROMETHEUS"); v == "true" || v == "1" {
		cfg.MetricsEnabled = true
	}
	if v := env("RYSH_METRICS_LISTEN"); v != "" {
		cfg.MetricsListen = v
	}
	// Per-dialect proxy keys. Explicit RYSH_PROXY_* wins; otherwise the
	// conventional provider variable in the daemon's own environment is used,
	// so `export OPENAI_API_KEY=...` before `rysh` is enough to govern a
	// wrapped OpenAI CLI without any rysh-specific configuration.
	for _, k := range []struct {
		dialect, ryshEnv string
		fallbacks        []string
	}{
		{"anthropic", "RYSH_PROXY_ANTHROPIC_KEY", []string{"ANTHROPIC_API_KEY"}},
		{"openai", "RYSH_PROXY_OPENAI_KEY", []string{"OPENAI_API_KEY"}},
		{"gemini", "RYSH_PROXY_GEMINI_KEY", []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}},
	} {
		v := env(k.ryshEnv)
		if v == "" {
			for _, fb := range k.fallbacks {
				if v = env(fb); v != "" {
					break
				}
			}
		}
		// A dummy injected into a pane must never be mistaken for a real key.
		if v == "" || v == ProxyDummyKey {
			continue
		}
		if cfg.Proxy.Keys == nil {
			cfg.Proxy.Keys = map[string]string{}
		}
		if _, explicit := cfg.Proxy.Keys[k.dialect]; !explicit {
			cfg.Proxy.Keys[k.dialect] = v
		}
	}
	if b, ok := envBool("RYSH_PROXY_ENABLED"); ok {
		cfg.Proxy.Enabled = b
	}
	if b, ok := envBool("RYSH_PROXY_INJECT_KEY"); ok {
		cfg.Proxy.InjectKey = b
	}
	if b, ok := envBool("RYSH_REPLAY_ENABLED"); ok {
		cfg.Replay.Enabled = b
	}
	if v := env("RYSH_REGISTRY_URL"); v != "" {
		cfg.Registry.URL = v
	}
	if v := env("RYSH_ORG_POLICY"); v != "" {
		cfg.Policy.OrgFile = absOrEmpty(expandHome(v))
	}
	if v := env("RYSH_PROVIDER"); v != "" {
		cfg.ProviderName = v
	}
	if v := env("RYSH_CLAUDE_CMD"); v != "" {
		cfg.ClaudeCommand = v
	}
	if v := env("RYSH_SYSTEM_PROMPT"); v != "" {
		cfg.SystemPrompt = v
	}
	if v := env("RYSH_MODEL"); v != "" {
		cfg.DefaultModel = v
	}
	if v := env("RYSH_API_KEY"); v != "" {
		cfg.APIKey = v
	} else if cfg.APIKey == "" {
		// Provider-conventional fallback, keyed by the provider selected above
		// (RYSH_PROVIDER wins over the config file at this point). Gemini takes
		// GEMINI_API_KEY, falling back to GOOGLE_API_KEY — so
		// `RYSH_PROVIDER=gemini GEMINI_API_KEY=... rysh` needs no rysh-specific
		// key var. An ANTHROPIC_API_KEY must NOT flow to Gemini: a foreign key
		// would only produce a confusing upstream 401.
		switch strings.ToLower(strings.TrimSpace(cfg.ProviderName)) {
		case "gemini":
			if v := env("GEMINI_API_KEY"); v != "" {
				cfg.APIKey = v
			} else if v := env("GOOGLE_API_KEY"); v != "" {
				cfg.APIKey = v
			}
		default:
			if v := env("ANTHROPIC_API_KEY"); v != "" {
				cfg.APIKey = v
			}
		}
	}
	if v := env("RYSH_API_URL"); v != "" {
		cfg.APIURL = v
	}
	if v := env("RYSH_BRAVE_API_KEY"); v != "" {
		cfg.BraveAPIKey = v
	}
	if v := envInt("RYSH_MAX_TOKENS"); v > 0 {
		cfg.MaxTokens = v
	}
	if v := env("RYSH_THINKING"); v != "" {
		cfg.Thinking = v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
	}
	if v := env("RYSH_SESSION"); v != "" {
		cfg.SessionName = v
	}
	if v := env("RYSH_SESSION_DIR"); v != "" {
		cfg.SessionDir = v
	}
	if v := env("RYSH_SESSION_SOURCE"); v != "" {
		cfg.SessionSource = v
	}
	// Note: RYSH_DIR is resolved in loadFrom (before SessionDir / NATS.DataDir
	// are derived), not here, so it can influence those storage paths too.
	if v := normalizeUpgradeMode(env("RYSH_UPGRADE_ON_ATTACH")); v != "" {
		cfg.UpgradeOnAttach = v
	}
	if v := env("RYSH_WORKING_DIRECTORY"); v != "" {
		cfg.WorkingDirectory = resolveWorkingDirectory("", v)
	}
	// Shell precedence: RYSH_SHELL (explicit override) > `ui.shell` in the
	// config file > $SHELL (ambient baseline, applied by shellDefault() before
	// the file is read) > /bin/bash.
	//
	// This used to re-apply $SHELL *after* the file, which silently clobbered
	// `ui.shell` on every machine where $SHELL is set — i.e. always — making a
	// documented config key dead. `rysh onboard` writes that key (design 008
	// TO2), so the override now has to be explicit to win.
	if v := strings.ToLower(strings.TrimSpace(env("RYSH_PAIRING_DEFAULT"))); v == "open" || v == "closed" {
		cfg.PairingDefault = v
	}
	if v := env("RYSH_SHELL"); v != "" {
		cfg.DefaultShell = v
	}
	if b, ok := envBool("RYSH_INTERACTIVE_SHELL"); ok {
		cfg.InteractiveShell = b
	}
	if b, ok := envBool("RYSH_SHELL_COLOR"); ok {
		cfg.ShellColorOutput = b
	}
	if b, ok := envBool("RYSH_SHELL_READLINE"); ok {
		cfg.ShellReadlineKeys = b
	}
	if v := envInt("RYSH_SHELL_HISTORY_SIZE"); v > 0 {
		cfg.ShellHistorySize = v
	}
	if b, ok := envBool("RYSH_SHELL_BASH_COMPLETION"); ok {
		cfg.ShellBashCompletion = b
	}
	if v := env("RYSH_SHELL_PROMPT"); v != "" {
		cfg.ShellPrompt = v
	}
	if v := envInt("RYSH_SHELL_BUFFER_BYTES"); v > 0 {
		cfg.ShellBufferBytes = v
	}
	if v := envInt("RYSH_INITIAL_TABS"); v > 0 {
		cfg.InitialTabs = v
	}
	if v := envInt("RYSH_INITIAL_PANES"); v > 0 {
		cfg.InitialPanes = v
	}
	if v := envInt("RYSH_PRIVATE_BUFFER_SIZE"); v > 0 {
		cfg.PrivateBufferSize = v
	}
	if v := envInt("RYSH_PUBLIC_BUFFER_SIZE"); v > 0 {
		cfg.PublicBufferSize = v
	}
	if v := env("RYSH_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := env("RYSH_LOG_MESSAGES"); v == "true" || v == "1" {
		cfg.LogMessages = true
	} else if v == "false" || v == "0" {
		cfg.LogMessages = false
	}
	if v := env("RYSH_UPSTREAM_ENABLED"); v == "true" || v == "1" {
		cfg.Upstream.Enabled = true
	} else if v == "false" || v == "0" {
		cfg.Upstream.Enabled = false
	}
	if v := env("RYSH_UPSTREAM_URL"); v != "" {
		cfg.Upstream.URL = v
	}
	if v := env("RYSH_UPSTREAM_API_KEY"); v != "" {
		cfg.Upstream.APIKey = v
	}
	if v := env("RYSH_UPSTREAM_WORKSPACE"); v != "" {
		cfg.Upstream.Workspace = v
	}
	if v := env("RYSH_UPSTREAM_AUTO_SHARE"); v == "true" || v == "1" {
		cfg.Upstream.AutoShare = true
	} else if v == "false" || v == "0" {
		cfg.Upstream.AutoShare = false
	}
	// SecretNAT / ReSet (snat section).
	if v := env("RYSH_SNAT_ENABLED"); v == "true" || v == "1" {
		cfg.SNAT.Enabled = true
	} else if v == "false" || v == "0" {
		cfg.SNAT.Enabled = false
	}
	if v := env("RYSH_SNAT_MODE"); v != "" {
		cfg.SNAT.Mode = v
	}
	if v := env("RYSH_SNAT_RESTORE_DISPLAY"); v == "true" || v == "1" {
		cfg.SNAT.RestoreDisplay = true
	} else if v == "false" || v == "0" {
		cfg.SNAT.RestoreDisplay = false
	}
	if v := env("RYSH_SNAT_MAPPING_TTL"); v != "" {
		cfg.SNAT.MappingTTL = v
	}
	if v := env("RYSH_PROFILE_NAME"); v != "" {
		cfg.Profile.Name = v
	}
	if v := env("RYSH_PROFILE_COLOR"); v != "" {
		cfg.Profile.Color = v
	}
	if v := env("RYSH_VOICE_CONTROL_TTS_PROVIDER_NAME"); v != "" {
		cfg.VoiceControl.TTSProviderName = v
	}
	if v := env("RYSH_VOICE_CONTROL_API_KEY"); v != "" {
		cfg.VoiceControl.APIKey = v
	}
	if v := env("RYSH_VOICE_ENABLED"); v == "true" || v == "1" {
		cfg.Voice.Enabled = true
	} else if v == "false" || v == "0" {
		cfg.Voice.Enabled = false
	}
	if v := env("RYSH_VOICE_HOTKEY"); v != "" {
		cfg.Voice.Hotkey = v
	}
	if v := env("RYSH_VOICE_RECORDER"); v != "" {
		cfg.Voice.Recorder = v
	}
	if v := envInt("RYSH_VOICE_MAX_SECONDS"); v > 0 {
		cfg.Voice.MaxSeconds = v
	}
	if v := env("RYSH_VOICE_LANGUAGE"); v != "" {
		cfg.Voice.Language = v
	}
	return cfg
}

// resolveConfig finds the rysh.config.yaml to load and the rysh directory that
// the location of that config implies. rysh state is ALWAYS project-local: the
// ".rysh" directory (holding sessions/ and nats/) is colocated with the config.
// There are no global locations (no ~/.rysh, no ~/.local/state/rysh, no
// ~/.config/rysh). The search order, and the rysh dir each location maps to:
//
//  1. <cwd>/rysh.config.yaml       -> rysh dir = <cwd>/.rysh   (sibling .rysh)
//  2. <cwd>/.rysh/rysh.config.yaml -> rysh dir = <cwd>/.rysh   (the .rysh dir itself)
//
// Both return values are absolute. When no config file is found, ("", "") is
// returned and callers fall back to defaultRyshDir() (<cwd>/.rysh).
func resolveConfig() (configFile, ryshDir string) {
	if cwd, err := os.Getwd(); err == nil {
		// 1. <cwd>/rysh.config.yaml -> sibling .rysh
		if p := filepath.Join(cwd, "rysh.config.yaml"); isRegularFile(p) {
			return p, filepath.Join(cwd, ".rysh")
		}
		// 2. <cwd>/.rysh/rysh.config.yaml -> the .rysh dir itself
		if p := filepath.Join(cwd, ".rysh", "rysh.config.yaml"); isRegularFile(p) {
			return p, filepath.Join(cwd, ".rysh")
		}
	}
	return "", ""
}

// findConfigFile returns just the resolved config file path (see resolveConfig).
// Retained for callers (ConfigPath) that only need the path.
func findConfigFile() string {
	f, _ := resolveConfig()
	return f
}

// ryshDirForConfig derives the rysh dir for an explicitly supplied config file
// path (the "--config <path>" flag). When the config lives inside a ".rysh"
// directory, that directory is the rysh dir; otherwise the rysh dir is a
// ".rysh" sibling of the directory containing the config file. Returns "" for
// an empty path.
func ryshDirForConfig(configFile string) string {
	if strings.TrimSpace(configFile) == "" {
		return ""
	}
	dir := configFile
	if abs, err := filepath.Abs(configFile); err == nil {
		dir = abs
	}
	dir = filepath.Dir(dir)
	if filepath.Base(dir) == ".rysh" {
		return dir
	}
	return filepath.Join(dir, ".rysh")
}

// defaultRyshDir is the rysh dir used when no config file is found in the search
// path. rysh state is always project-local, so this anchors to "<cwd>/.rysh" —
// the same ".rysh" a rysh.config.yaml in the cwd would imply. There is NO global
// fallback (no ~/.rysh, ~/.local/state/rysh, or ~/.config/rysh): everything is
// stored relative to the working directory. Set RYSH_DIR to override explicitly.
func defaultRyshDir() string {
	if cwd, err := os.Getwd(); err == nil {
		return filepath.Join(cwd, ".rysh")
	}
	return ".rysh"
}

// isRegularFile reports whether p exists and is a regular file (not a directory).
func isRegularFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

// absOrEmpty returns the absolute form of p, or "" when p is empty.
func absOrEmpty(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// ConfigPath returns the config file to read/write: the explicit path when
// non-empty, otherwise the first existing rysh.config.yaml (CWD, then CWD/.rysh
// — see resolveConfig), falling back to "rysh.config.yaml" in the CWD.
func ConfigPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if p := findConfigFile(); p != "" {
		return p
	}
	return "rysh.config.yaml"
}

// AppendWorkspace adds a new workspace entry (name + working_directory "~/" +
// upstream{enabled, api_key}) to the YAML config at path, preserving existing
// comments and formatting. The entry is appended to the top-level `workspace:`
// sequence, which is created if absent. Used by the ##ws create command.
func AppendWorkspace(path, name, apiKey string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read config: %w", err)
	}

	var root yaml.Node
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
	}
	if root.Kind == 0 {
		root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("config root is not a mapping")
	}
	doc := root.Content[0]

	// Find (or create) the top-level `workspace` sequence.
	var seq *yaml.Node
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value == "workspace" {
			seq = doc.Content[i+1]
			break
		}
	}
	if seq == nil {
		seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		doc.Content = append(doc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "workspace"}, seq)
	}
	if seq.Kind != yaml.SequenceNode {
		return fmt.Errorf("config `workspace` is not a list")
	}

	str := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }
	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
		str("name"), str(name),
		str("working_directory"), str("~/"),
		str("upstream"), &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
			str("enabled"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"},
			str("api_key"), str(apiKey),
		}},
	}}
	seq.Content = append(seq.Content, entry)

	out, err := yaml.Marshal(&root)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// scalarNode builds a plain string scalar YAML node.
func scalarNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// mappingValue returns the value node for key in a mapping node, or nil.
func mappingValue(mapNode *yaml.Node, key string) *yaml.Node {
	if mapNode == nil {
		return nil
	}
	for i := 0; i+1 < len(mapNode.Content); i += 2 {
		if mapNode.Content[i].Value == key {
			return mapNode.Content[i+1]
		}
	}
	return nil
}

// setMappingKey sets key=val in mapNode: updates the existing value node in place
// (preserving its surrounding comments) or appends the pair when absent.
func setMappingKey(mapNode *yaml.Node, key, val string) {
	if v := mappingValue(mapNode, key); v != nil {
		v.Kind, v.Tag, v.Value = yaml.ScalarNode, "!!str", val
		return
	}
	mapNode.Content = append(mapNode.Content, scalarNode(key), scalarNode(val))
}

// loadConfigDoc reads and parses the YAML config at path into its root document
// node and top-level mapping (creating an empty mapping when the file is
// absent/empty). Comments and formatting are preserved for round-trip edits.
func loadConfigDoc(path string) (root *yaml.Node, doc *yaml.Node, err error) {
	data, rerr := os.ReadFile(path)
	if rerr != nil && !os.IsNotExist(rerr) {
		return nil, nil, fmt.Errorf("read config: %w", rerr)
	}
	root = &yaml.Node{}
	if len(data) > 0 {
		if perr := yaml.Unmarshal(data, root); perr != nil {
			return nil, nil, fmt.Errorf("parse config: %w", perr)
		}
	}
	if root.Kind == 0 {
		*root = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("config root is not a mapping")
	}
	return root, root.Content[0], nil
}

// writeConfigDoc marshals root and writes it back to path.
func writeConfigDoc(path string, root *yaml.Node) error {
	out, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// SetWorkspaceWorkingDir sets the working_directory of a workspace entry in the
// YAML config at path, preserving comments and formatting. Backs the persistence
// half of `##workspace cwd <path>`, so the chosen directory survives a restart.
//
// The workspace is matched by INDEX (workspaceIdx) into the `workspace:`
// sequence: that order is preserved across the YAML, Config.Workspaces,
// ResolvedWorkspaces and the farm's children, and a synthesized name ("ws-N")
// has no `name` key in the file to match on. workspaceName is only a defensive
// cross-check / fallback.
//
//   - workspace: sequence present, index in range -> update that entry.
//   - sequence present but index out of range / name mismatch -> match by raw
//     name, else append a {name, working_directory} entry (defensive).
//   - no workspace: section (a single synthesized default) -> set the inherited
//     default rysh.working_directory instead of fabricating a section.
func SetWorkspaceWorkingDir(path string, workspaceIdx int, workspaceName, dir string) error {
	root, doc, err := loadConfigDoc(path)
	if err != nil {
		return err
	}

	seq := mappingValue(doc, "workspace")

	// No usable workspace: section -> the lone synthesized workspace inherits
	// rysh.working_directory; set that rather than creating a section.
	if seq == nil || seq.Kind != yaml.SequenceNode || len(seq.Content) == 0 {
		ryshMap := mappingValue(doc, "rysh")
		if ryshMap == nil || ryshMap.Kind != yaml.MappingNode {
			ryshMap = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			doc.Content = append(doc.Content, scalarNode("rysh"), ryshMap)
		}
		setMappingKey(ryshMap, "working_directory", dir)
		return writeConfigDoc(path, root)
	}

	// Locate the target entry by index, with a defensive name guard.
	var entry *yaml.Node
	if workspaceIdx >= 0 && workspaceIdx < len(seq.Content) && seq.Content[workspaceIdx].Kind == yaml.MappingNode {
		cand := seq.Content[workspaceIdx]
		nameNode := mappingValue(cand, "name")
		if nameNode == nil || nameNode.Value == "" || workspaceName == "" || nameNode.Value == workspaceName {
			entry = cand
		}
	}
	if entry == nil && workspaceName != "" {
		// Index drifted -> match by raw (unsanitized) name.
		for _, e := range seq.Content {
			if e.Kind != yaml.MappingNode {
				continue
			}
			if nameNode := mappingValue(e, "name"); nameNode != nil && nameNode.Value == workspaceName {
				entry = e
				break
			}
		}
	}
	if entry == nil {
		// Defensive: no matching entry -> append a new one.
		entry = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map",
			Content: []*yaml.Node{scalarNode("name"), scalarNode(workspaceName)}}
		seq.Content = append(seq.Content, entry)
	}

	setMappingKey(entry, "working_directory", dir)
	return writeConfigDoc(path, root)
}

// resolveWorkingDirectory determines the working directory for newly created
// panes.
//
//   - If explicit (the [rysh] working_directory value or RYSH_WORKING_DIRECTORY)
//     is set, it wins. A relative explicit path is resolved against the
//     directory containing configFile (or the process CWD when configFile is
//     empty), and a leading "~" is expanded to the user's home directory.
//   - Otherwise the directory is derived from the location of configFile:
//     1. If rysh.config.yaml lives inside a ".rysh" directory, the parent of that
//     ".rysh" directory is used.
//     2. Otherwise the directory directly containing rysh.config.yaml is used.
//
// An empty string is returned when no decision can be made, in which case panes
// inherit the daemon process's working directory.
//
// This is pure path resolution: it does NOT check that an explicit directory
// exists. The session working-dir loader (loadFromFile) adds the
// "fall back to the config dir when the directory is missing" rule on top.
func resolveWorkingDirectory(configFile, explicit string) string {
	configDir := ""
	if configFile != "" {
		if abs, err := filepath.Abs(configFile); err == nil {
			configDir = filepath.Dir(abs)
		} else {
			configDir = filepath.Dir(configFile)
		}
	}

	if explicit = strings.TrimSpace(explicit); explicit != "" {
		dir := expandHome(explicit)
		if !filepath.IsAbs(dir) {
			base := configDir
			if base == "" {
				if cwd, err := os.Getwd(); err == nil {
					base = cwd
				}
			}
			dir = filepath.Join(base, dir)
		}
		if abs, err := filepath.Abs(dir); err == nil {
			return abs
		}
		return dir
	}

	if configDir == "" {
		return ""
	}
	if filepath.Base(configDir) == ".rysh" {
		return filepath.Dir(configDir)
	}
	return configDir
}

// isExistingDir reports whether path names an existing directory.
func isExistingDir(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// ExpandHome replaces a leading "~" with the user's home directory. Exported
// for callers that build a WorkspaceConfig at runtime (e.g. ##ws create), since
// config loaded from disk is expanded during parsing but runtime-built configs
// are not.
func ExpandHome(path string) string { return expandHome(path) }

// expandHome replaces a leading "~" with the user's home directory.
func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path[1:], "/"))
		}
	}
	return path
}

func shellDefault() string {
	if s := strings.TrimSpace(os.Getenv("SHELL")); s != "" {
		return s
	}
	return "/bin/bash"
}

// normalizeUpgradeMode validates and canonicalizes an upgrade-on-attach mode
// string. It returns the lower-cased value when it is one of the recognized
// modes (off|warn|prompt|auto), or "" when the input is empty or unrecognized
// (so the caller keeps the existing/default value rather than applying garbage).
func normalizeUpgradeMode(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "warn", "prompt", "auto":
		return strings.ToLower(strings.TrimSpace(v))
	default:
		return ""
	}
}

// defaultSessionDir is the session registry directory used when a Config is
// built via applyDefaults without going through loadFrom (e.g. direct
// construction). It derives from the default rysh dir; loadFrom recomputes
// SessionDir from the rysh dir it actually resolved. When no config file is
// found this is "<cwd>/.rysh/sessions" — the legacy global location
// ($XDG_STATE_HOME/rysh/sessions) is gone; when a config IS found, storage
// follows that config's rysh dir. Set RYSH_DIR or RYSH_SESSION_DIR to relocate.
func defaultSessionDir() string {
	return filepath.Join(defaultRyshDir(), "sessions")
}

func env(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

func envInt(key string) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// envBool parses a boolean environment variable. The second return value is
// false when the variable is unset or cannot be parsed, so callers can leave
// the existing default untouched in that case.
func envBool(key string) (bool, bool) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return false, false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, false
	}
	return b, true
}
