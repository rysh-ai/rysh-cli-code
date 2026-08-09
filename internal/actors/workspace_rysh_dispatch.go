package actors

import (
	"fmt"
	"sort"
	"strings"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/progname"
)

// ---------------------------------------------------------------------------
// The ## command table
//
// handleRyshCommand used to be a 1074-line switch over 42 cases, with the help
// text written out separately by hand in ryshHelp. Nothing tied the two
// together, so a command could be added, renamed or aliased without its help
// changing — and the reverse. The aliases were the clearest symptom: ##model,
// ##pg, ##stack, ##rst, ##var and friends dispatched correctly but appeared in
// the help only as prose inside another command's description, if at all.
//
// This table is the single declaration of what a ## command is: its canonical
// name, its aliases, its help, and what runs it. handleRyshCommand looks a
// command up here; ryshHelp renders this; the unknown-command message is built
// from this. Adding a command without help, or documenting one that does not
// dispatch, is now a compile-time or test-time error rather than a slow drift.
// ---------------------------------------------------------------------------

// ryshCmd is the invocation being dispatched. Handlers historically took
// whichever subset of these they needed — most take (out, paneID, args), some
// also need the actor context, ##h needs the pane's input mode, and ##cron
// needs the raw untokenised text because its schedule is a quoted multi-field
// cron spec that strings.Fields would shred. Carrying all of it in one struct
// is what lets every table entry share a single signature.
type ryshCmd struct {
	ctx       actor.Context
	out       *strings.Builder
	paneID    string
	inputMode string
	// args is everything after the command word, so args[0] is the subcommand.
	args []string
	// rawText is the full command line including the ## prefix.
	rawText string
	// err is the failure the handler reported. Nil means success. Migrated
	// handlers return an error and their table entry records it here
	// (run: func(...) { c.err = w.handleX(...) }); the message is for the
	// caller's stderr / exit path, and a handler that also wants it on screen
	// still writes to out.
	//
	// Every handler used to communicate purely by writing prose into out, so a
	// caller — the CLI, and now `rysh script` — could not tell "worked" from
	// "refused". Handlers that have been audited for their failure modes set
	// this instead of (or as well as) writing an error line, and their table
	// entry sets statusAware so a script can trust the resulting exit code.
	err error
}

// firstArgOr returns the first argument, or def when there is none.
func (c *ryshCmd) firstArgOr(def string) string {
	if len(c.args) > 0 && c.args[0] != "" {
		return c.args[0]
	}
	return def
}

// ryshCommand is one entry in the table.
type ryshCommand struct {
	// name is the canonical spelling — the one the help lists.
	name string
	// aliases dispatch to the same handler. Declaring them here is what makes
	// them visible to the help and to the unknown-command message.
	aliases []string
	// help is the block of lines this command contributes to ##help, copied
	// verbatim from the hand-written original.
	help []string
	// helpRewrite marks a help line that must go through progname.Rewrite
	// because it names the binary.
	helpRewrite []bool
	// statusAware records that this command's handler has been audited for its
	// failure modes and reports every one of them into c.err, so a non-zero exit
	// code from it means something.
	//
	// The 41 handlers predate any notion of an exit status: they report trouble
	// by writing prose into out and returning normally. Migrating them is
	// mechanical but must be done per-command, and a script silently trusting a
	// 0 from an unmigrated command is worse than one that knows it cannot. This
	// flag is what `rysh exec --json` reports as "status_aware", so the
	// difference is visible rather than assumed.
	statusAware bool
	// run dispatches the command.
	run func(w *WorkspaceActor, c *ryshCmd)
}

// ryshCommands is ordered: the help renders in this order, which is the order
// the hand-written help used.
//
// Populated in init() rather than as a var initialiser. The entries close over
// handlers, and ##cron can fire an input that routes back through this table,
// so a direct initialiser is an initialisation cycle the compiler rejects.
var ryshCommands []ryshCommand

// ryshCommandIndex maps every name and alias to its entry. A duplicate name is
// a programming error and panics at startup rather than silently shadowing a
// command.
var ryshCommandIndex map[string]*ryshCommand

func init() {
	ryshCommands = []ryshCommand{
		{
			name: "help",
			help: []string{
				"  ##help                       show this help\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.ryshHelp(c.out) },
		},
		{
			name:    "h",
			aliases: []string{"history"},
			help: []string{
				"  ##h, ##history               shell or AI prompt history (based on current mode)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleHistoryCommand(c.out, c.paneID, c.inputMode, c.args) },
		},
		{
			name: "native",
			help: []string{
				"  ##native [on|off]            native pass-through shell: bash owns the terminal (Esc Esc exits)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleNativeCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "new",
			help: []string{
				"  ##new tab                    create a new tab (also: ##rysh new tab)\n",
				"  ##new lane [tab]             create a new lane (default: active tab)\n",
				"  ##new pane [tab] [lane]      create a pane at the bottom of a lane (default: active tab+lane)\n",
				"  ##new grid <N>               stack N panes vertically in the active lane (e.g. ##new grid 4)\n",
				"  ##new grid <L>x<P>           build an L x P grid in the active tab (e.g. ##new grid 3x4)\n",
				"  ##new grid <T>x<L>x<P>       create T tabs x L lanes x P panes (e.g. ##new grid 2x3x4)\n",
				"  ##new stack <N>              add N stacked panes to the active pane group (aliases: ##new pg|panegroup <N>)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleNewInstance(c.ctx, c.out, c.paneID, c.args) },
		},
		{
			name: "cmd",
			help: []string{
				"  ##cmd <scope> [sel] <cmd>    run a bash cmd in every pane of a scope (pane|pg/stack|lane|tab|ws)\n",
				"                               selectors: --ws/--tab/--lane/--pg/--pane <id|name|index>; e.g. ##cmd stack pwd\n",
				"                               --running <program|shell> targets only panes running that program\n",
				"                               --capture writes each pane's output to a file and prints where\n",
				"                               (shared panes and panes in a pipeline tab are excluded)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleCmdBroadcast(c.ctx, c.out, c.paneID, c.args) },
		},
		{
			name: "prompt",
			help: []string{
				"  ##prompt <text>              (rysh script only) send a prompt and WAIT for the turn\n",
				"                               to finish; result in $RYSH_OUT, status in $RYSH_STATUS\n",
				"                               at the keyboard use prompt mode; from a shell: rysh prompt\n",
			},
			helpRewrite: []bool{false, false, true},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { c.err = w.handlePromptCommandInfo(c.out) },
		},
		{
			name:    "llm",
			aliases: []string{"model"},
			help: []string{
				"  ##llm [list]                 list LLM providers/models from .rysh/llms (session default marked)\n",
				"                               (##model is an alias for ##llm)\n",
				"  ##llm add <p>/<name> [id]    declare a new model in .rysh/llms\n",
				"  ##llm use <provider>/<name>  set the session default LLM model (persists to .rysh/llms)\n",
				"  ##llm enable|disable <p>/<n> allow / refuse activation of a model (stays listed)\n",
				"  ##llm info <provider>/<name> show a model's properties\n",
				"  ##llm status                 show the model currently in effect\n",
				"  ##llm scopes                 show the model hierarchy and which level wins\n",
				"  ##<scope> model [<p>/<name>] bind an LLM model at any level of the hierarchy:\n",
				"                               session > workspace > tab > lane > stack > pane\n",
				"                               a narrower binding always wins; \\\"default\\\" clears one level\n",
				"                               e.g. ##lane model anthropic/fable5, ##pane model openai/gpt-4o\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleLLMCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "session",
			help: []string{
				"  ##session                    show current session details (alias: ##session info)\n",
				"  ##session list               list all known sessions (current marked with >)\n",
				"  ##session switch <name>      start another session's daemon + how to attach\n",
				"  ##session reload             flush this session's state to KV and refresh its record\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { c.err = w.handleSessionSubcommand(c.out, c.paneID, c.args) },
		},
		{
			name:    "ws",
			aliases: []string{"workspace"},
			help: []string{
				"  ##ws, ##workspace            list workspaces (default: list)\n",
				"  ##ws create <name> <api_key> create a workspace (working_directory ~/, upstream key) + persist it\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleWorkspaceCommand(c.ctx, c.out, c.paneID, c.args) },
		},
		{
			name: "tab",
			help: []string{
				"  ##tab                        list all tabs (default: list)\n",
				"  ##tab  list                  list all tabs\n",
				"  ##tab  list-panes [--tab <tab-id>]  list panes of a tab (default: active tab)\n",
				"  ##tab  name <tab-name>       rename the active tab (also: ##rysh tab name, ctrl+t r)\n",
				"  ##tab  name <tab-id> <name>  rename the tab with that id (see ##tab list)\n",
				"  ##tab  pipeline enable       enable pipeline mode for the active tab\n",
				"  ##tab  pipeline disable      disable pipeline mode for the active tab\n",
				"  ##tab  delete <tab-id>       delete the tab with that id (see ##tab list)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleTabCommand(c.ctx, c.out, c.paneID, c.args) },
		},
		{
			name: "pane",
			help: []string{
				"  ##pane                       show active pane details (default: info)\n",
				"  ##pane info                  show active pane details\n",
				"  ##pane info <pane>           show that pane (id, title or given-name)\n",
				"  ##pane new [--worktree [branch]]  new pane; --worktree runs it in its own git worktree\n",
				"                               (branch pane/<alias>; removed on close if clean, KEPT if dirty)\n",
				"  ##pane new --claude [prompt] new pane running an interactive claude, session id recorded\n",
				"                               in ##pane meta (claude.session_id); the prompt is passed at launch\n",
				"  ##pane meta [--pane <ref>] list|get <k>|set <k> <v>|delete <k>   per-pane notes for whoever\n",
				"                               is driving it (rysh stores them, never reads them)\n",
				"  ##pane list [--tab <tab-id>] list panes of a tab (default: active tab)\n",
				"  ##pane name <name>           set a given-name for the active pane (unique per lane)\n",
				"  ##pane name <pane-id> <name> set a given-name for the pane with that id (see ##pane list)\n",
				"  ##pane listen <id|alias>     listen to another pane's shared output\n",
				"  ##pane unlisten              stop listening\n",
				"  ##pane provider [name [model]] show or override the active pane's LLM provider (persisted; \\\"default\\\" clears)\n",
				"  ##pane model [<p>/<name>]     show or switch the active pane's model via .rysh/llms (also: list, scopes, default)\n",
				"  ##pane delete <pane-id>      delete the pane with that id (see ##pane list)\n",
				"  ##pane share start           start sharing pane to remote upstream\n",
				"  ##pane share stop            stop sharing pane\n",
				"  ##pane share status          show sharing status\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handlePaneCommand(c.ctx, c.out, c.paneID, c.args) },
		},
		{
			name: "share",
			help: []string{
				"  ##share pane [view|control]   share the active pane to upstream\n",
				"  ##share panegroup [view|ctrl] share the active pane group\n",
				"  ##share lane [view|control]   share the active lane\n",
				"  ##share tab [view|control]    share the active tab\n",
				"  ##share list                  list all active shares\n",
				"  ##share status                show share status for active pane\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleShareCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "unshare",
			help: []string{
				"  ##unshare pane                stop sharing the active pane\n",
				"  ##unshare panegroup           stop sharing the active pane group\n",
				"  ##unshare lane                stop sharing the active lane\n",
				"  ##unshare tab                 stop sharing the active tab\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleUnshareCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "upstream",
			help: []string{
				"  ##upstream connect <url> <api-key>  connect to a rysh server (resolves the workspace id, writes the config)\n",
				"  ##upstream status             show upstream configuration and status\n",
				"  ##upstream my-shares          list shares published from this session\n",
				"  ##upstream list-remote        list all shares in the workspace (from server)\n",
				"  ##upstream subscribe <id>     subscribe to a remote share\n",
				"  ##upstream unsubscribe        stop subscribing to remote share\n",
				"  ##upstream send <text>        send command to active remote share\n",
				"  ####<command>                 run ##<command> on the shared source pane (control subscriber)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleUpstreamCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "lane",
			help: []string{
				"  ##lane list                  list lanes in the active tab\n",
				"  ##lane info                  show active lane details\n",
				"  ##lane name <lane-name>      rename the active lane (also: ##rysh lane name)\n",
				"  ##lane delete <lane-id>      delete the lane with that id (see ##lane list)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleLaneCommand(c.out, c.paneID, c.args) },
		},
		{
			name:    "panegroup",
			aliases: []string{"pg", "stack"},
			help: []string{
				"  ##panegroup, ##pg            show active pane group details (default: info)\n",
				"  ##panegroup info             show active pane group details\n",
				"  ##panegroup list             list pane groups in the active tab\n",
				"  ##panegroup layout           show lane layout overview\n",
				"  ##panegroup delete <group-id> delete the pane group with that id (see ##panegroup list)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handlePaneGroupCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "public",
			help: []string{
				"  ##public pane print          print redacted (public) output of the active pane\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handlePublicCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "private",
			help: []string{
				"  ##private pane print         print raw (private) output of the active pane\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handlePrivateCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "snap",
			help: []string{
				"  ##snap                       copy private buffer to clipboard (default)\n",
				"  ##snap private               copy private buffer to clipboard\n",
				"  ##snap public                copy public (redacted) buffer to clipboard\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.cmdSnap(c.out, c.paneID, c.firstArgOr("private")) },
		},
		{
			name:    "pipe",
			aliases: []string{"pipeline"},
			help: []string{
				"  ##pipe, ##pipeline           pipeline commands (default: help)\n",
				"  ##pipe list                  list loaded pipelines\n",
				"  ##pipe load <file>           load a pipeline from .rysh/pipelines/<file>\n",
				"  ##pipe unload <name>         remove a loaded pipeline\n",
				"  ##pipe run [name]            run a pipeline (default: first loaded)\n",
				"  ##pipe status                show pipeline execution status\n",
				"  ##pipe clear                 clear pipeline output\n",
				"  ##pipe name <name>           set the pipeline name label\n",
				"  ##pipe placeholder add       add a new pipeline placeholder (lane)\n",
				"  ##pipe placeholder list      list current placeholders\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handlePipelineCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "rysh",
			help: []string{
				"  ##rysh web start [--bind <addr>] [--port <n>] [--username <u> --password <p>]  start the web UI server\n",
				"  ##rysh web stop              stop the web UI server\n",
				"  ##rysh web status            show bind address, port + login\n",
				"  ##rysh web auth username=<u> password=<p>  set the web UI login (30-day token)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleRyshSelfCommand(c.ctx, c.out, c.paneID, c.args) },
		},
		{
			name: "mode",
			help: []string{
				"  ##mode list                  list enabled + available input modes for the active pane\n",
				"  ##mode new <mode>            enable a mode (shell|prompt(ai)|rysh|chat|external|web)\n",
				"  ##mode new web [--profile N] [url]  enable web mode bound to a browser profile\n",
				"  ##mode delete <mode>         disable a mode (shell cannot be disabled)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleModeSubcommand(c.out, c.paneID, c.args) },
		},
		{
			name: "webai",
			help: []string{
				"  ##webai [--pane <id|name>] <prompt>  prompt a pane's web AI (default: this pane; --pane targets another)\n",
				"  ##webai [--pane <id|name>] history [print] [N]  print a pane's last N Ask Rysh turns (default 5)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.sendWebAIPrompt(c.out, c.paneID, strings.Join(c.args, " ")) },
		},
		{
			name: "auto",
			help: []string{
				"  ##auto web|task|agent|humanoid|code save|run|list|show|delete  reusable prompt automations (web recipes, plain tasks, named agents/humanoids, project code)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleAutoCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "web",
			help: []string{
				"  ##web headless on|off|status|login    CLI-owned headless Chromium (runs without the desktop app)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleWebCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "hop",
			help: []string{
				"  ##hop <pane-name|pane-id>    hop output + agent memory to another pane (session fork)\n",
				"  ##hop resume                 resume the AI with the hopped session\n",
				"  ##hop status                 show hop state for active pane\n",
				"  ##hop clear                  clear hopped content\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleHopCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "grounding",
			help: []string{
				"  ##grounding                  show grounding state for active pane\n",
				"  ##grounding off|prompt|enforced  override grounding mode (persisted per pane)\n",
				"  ##grounding reset            clear the override, revert to default\n",
				"  ##grounding report           show the last grounding report\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleGroundingCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "cron",
			help: []string{
				"  ##cron add|list|run|rm|logs  schedule rysh inputs (##auto web, @agent, prompts) in-daemon\n",
				"  ##>...                       event pass-through (forwarded to NATS as-is)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleCronCommand(c.ctx, c.out, c.paneID, c.rawText) },
		},
		{
			name: "agent",
			help: []string{
				"  ##agent spawn <name>          load .rysh/agents/<name>/SKILL.md\n",
				"  ##agent spawn <name> <prompt> spawn agent inline\n",
				"  ##agent spawn-all [dir]       spawn all (default: .rysh/agents)\n",
				"  ##agent list                  list all autonomous agents\n",
				"  ##agent show <name>           print an agent's recipe (its skill file)\n",
				"  ##agent delete <name>         delete an agent\n",
				"  ##agent register-output       route agent output to pane chat\n",
				"  ##agent unregister-output      stop routing output to pane\n",
				"  ##agent reload-prompts        reload rysh-cli-agent-prompts/ + override dir (effective next prompt)\n",
				"  ##agent metrics               dump per-tool / LLM / compaction metrics\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleAgentSubcommand(c.out, c.paneID, c.args) },
		},
		{
			name: "humanoid",
			help: []string{
				"  ##humanoid spawn <name>       load .rysh/humanoids/<name>/SKILL.md (agent + external channels)\n",
				"  ##humanoid spawn-all [dir]    spawn all (default: .rysh/humanoids)\n",
				"  ##humanoid stop <name>        stop a running humanoid (its skill file is kept)\n",
				"  ##humanoid list               running + paused + stopped humanoids, with channel status\n",
				"  ##humanoid show <name>        print a humanoid's recipe (its skill file)\n",
				"  ##humanoid channels <name>    show a humanoid's configured channels\n",
				"  ##humanoid channel start|stop <name> <channel>  connect/disconnect one channel\n",
				"  ##humanoid governance <name> ai|human  autonomous, or draft-and-confirm before each reply\n",
				"  ##humanoid pair list|approve  review and approve inbound contact pairing requests\n",
				"  ##humanoid activate|deactivate <name>  bench or revive a humanoid\n",
				"  ##humanoid register-output|unregister-output  route humanoid output to pane chat\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleHumanoidSubcommand(c.out, c.paneID, c.args) },
		},
		{
			name: "image",
			help: []string{
				"  ##image <path>                attach an image to the next prompt in this pane\n",
				"  ##image clear                 clear any pending image\n",
				"  @agent-name <prompt>          send prompt to autonomous agent or humanoid\n",
				"  @@agent-name stop             stop autonomous agent or humanoid\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleImageCommand(c.out, c.paneID, c.args) },
		},
		{
			name:    "secret",
			aliases: []string{"secrets"},
			help: []string{
				"  ##secret new <NAME> <VALUE> [--no-persist] [--tab <tab>]  workspace secret (--tab for a tab); persisted to .rysh/secrets/<scope>/<NAME> by default\n",
				"  ##secret list [--tab <tab>]   list the workspace's (or a tab's) secrets\n",
				"  ##secret get <NAME> [--tab <tab>]   resolve a secret as a skill here would (tab→workspace→env)\n",
				"  ##secret delete <NAME> [--tab <tab>]  remove a session + persisted secret (alias: ##secrets)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleSecretSubcommand(c.out, c.paneID, c.args) },
		},
		{
			name:    "variable",
			aliases: []string{"variables", "var"},
			help: []string{
				"  ##variable new <NAME> <VALUE> [--no-persist] [--tab <tab>]  env variable (.rysh/variables); like ##secret but visible to the LLM (aliases: ##var, ##variables)\n",
				"  ##variable list|get|delete [--tab <tab>]   list/resolve/remove workspace (or tab) variables\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleVariableSubcommand(c.out, c.paneID, c.args) },
		},
		{
			name:    "snat",
			aliases: []string{"rst"},
			help: []string{
				"  ##snat status|on|off|reset|mode|list  SecretNAT/ReSet: reversible secret translation — the LLM provider\n",
				"                                never sees real secrets; tools still get real values (alias: ##rst)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleSnatSubcommand(c.out, c.paneID, c.args) },
		},
		{
			name: "proxy",
			help: []string{
				"  ##proxy [status]              governance proxy: route wrapped agent CLIs through rysh\n",
				"  ##proxy on|off                enable/disable the proxy for this session (default: off)\n",
				"  ##proxy audit                 show the proxied-request audit log\n",
				"  ##proxy check <cli>           run a CLI once and report whether its traffic was governed\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleProxyCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "cost",
			help: []string{
				"  ##cost                        token + dollar spend for this session\n",
				"  ##cost week|7d                spend over the last 7 days\n",
				"  ##cost budget <n>             set a spend ceiling (e.g. ##cost budget 500k)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleCostCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "policy",
			help: []string{
				"  ##policy                      show the resolved policy (.rysh/policy.yaml + org policy; fail-closed)\n",
				"  ##policy reload               re-read the policy file(s)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handlePolicyCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "replay",
			help: []string{
				"  ##replay status               session capture state (opt-in per session)\n",
				"  ##replay export [--pane <id>] [-o <file>]  export captured output as asciicast v2\n",
				"  ##replay play [--pane <id>] [--from <dur|ts>] [--speed <n|max>]  replay into a dedicated read-only pane\n",
				"                                (focused replay pane: space pause, ←/→ seek ∓10s, +/- speed, q close)\n",
				"  ##replay play --here          v1 behavior: replay into this pane instead\n",
				"  ##replay stop                 cancel an in-progress replay\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleReplayCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "worktree",
			help: []string{
				"  ##worktree new <branch>       create a git worktree for isolated agent work\n",
				"  ##worktree list|status        list worktrees / show the active one\n",
				"  ##worktree cwd <branch>       run this pane group in the worktree (new panes start there)\n",
				"  ##worktree merge <branch> [--confirm]  gated merge back (shows a diff first)\n",
				"  ##worktree remove <branch>    remove a worktree (dirty trees are preserved)\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleWorktreeCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "board",
			help: []string{
				"  ##board open                  open the agents board pane\n",
				"  ##board post <text>           post a milestone to the agents board\n",
				"  ##board reply <thread> <text> reply under an existing thread\n",
				"  (agents post silently instead: rysh board post --as <pane-id> <text>)\n",
			},
			helpRewrite: []bool{false, false, false, true},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { c.err = w.handleBoardCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "ansa",
			help: []string{
				"  ##ansa send <@name|id> <text> deliver a shell line to another pane (agent nervous system)\n",
				"  ##ansa prompt <@name|id> <t>  deliver a prompt to another pane\n",
				"  ##ansa who                    list addressable panes + ids (duplicate names flagged)\n",
				"  (agents route silently instead: rysh ansa send --to <@name|id> <text>)\n",
			},
			helpRewrite: []bool{false, false, false, true},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { c.err = w.handleAnsaCommand(c.out, c.paneID, c.args) },
		},
		{
			name: "mcp",
			help: []string{
				"  ##mcp add <name> http <url>   connect an MCP server (tools→agents)\n",
				"  ##mcp add <name> stdio <cmd>  spawn a stdio MCP server\n",
				"  ##mcp list                    list MCP servers + status\n",
				"  ##mcp tools <name>            list a server's tools\n",
				"  ##mcp remove <name>           disconnect + forget a server\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleMCPSubcommand(c.out, c.paneID, c.args) },
		},
		{
			name: "forge",
			help: []string{
				"  ##forge add <name> <spec>     ingest an API spec + generate artifacts\n",
				"  ##forge list                  list configured integrations\n",
				"  ##forge diff <name> <spec>    operation changes vs the stored spec\n",
				"  ##forge targets               list available generator targets\n",
			},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleForgeSubcommand(c.out, c.paneID, c.args) },
		},
		{
			name:    "integration",
			aliases: []string{"int"},
			help: []string{
				"  ##integration list            list Forge API integrations (alias: ##int)\n",
				"  ##integration enable <name>   register a generated integration's tools\n",
				"  ##integration tools <name>    list an integration's tools\n",
				"  (forge artifacts also build from the shell: rysh forge add <name> <spec-file>)\n",
			},
			helpRewrite: []bool{false, false, false, true},
			statusAware: true,
			run:         func(w *WorkspaceActor, c *ryshCmd) { w.handleIntegrationSubcommand(c.out, c.paneID, c.args) },
		},
	}

	ryshCommandIndex = make(map[string]*ryshCommand, len(ryshCommands)*2)
	for i := range ryshCommands {
		c := &ryshCommands[i]
		for _, n := range append([]string{c.name}, c.aliases...) {
			if prev, dup := ryshCommandIndex[n]; dup {
				panic(fmt.Sprintf("rysh command %q declared by both %q and %q", n, prev.name, c.name))
			}
			ryshCommandIndex[n] = c
		}
	}
}

// lookupRyshCommand resolves a command word, following aliases.
func lookupRyshCommand(name string) (*ryshCommand, bool) {
	c, ok := ryshCommandIndex[name]
	return c, ok
}

// RyshCommandWords returns every dispatchable ## command word — canonical names
// and aliases alike — sorted.
//
// It exists so cmd/rysh can stop hand-maintaining its own list. The CLI's
// "rysh --<name> ..." forms were gated on a literal map that had drifted to 31
// of these 52 words, leaving ##secret, ##var, ##mode, ##policy, ##worktree and
// eleven others reachable only by typing them into a pane. A list derived from
// the table cannot drift.
func RyshCommandWords() []string {
	words := make([]string, 0, len(ryshCommandIndex))
	for w := range ryshCommandIndex {
		words = append(words, w)
	}
	sort.Strings(words)
	return words
}

// RyshCommandIsStatusAware reports whether a command word's handler has been
// audited to report failure (see ryshCommand.statusAware). Unknown words report
// false.
func RyshCommandIsStatusAware(word string) bool {
	c, ok := ryshCommandIndex[word]
	return ok && c.statusAware
}

// ryshHelp renders the ## command help from the table.
func (w *WorkspaceActor) ryshHelp(out *strings.Builder) {
	fmt.Fprintf(out, "\navailable ## commands:\n")
	for i := range ryshCommands {
		c := &ryshCommands[i]
		for j, line := range c.help {
			if j < len(c.helpRewrite) && c.helpRewrite[j] {
				fmt.Fprint(out, progname.Rewrite(line))
				continue
			}
			fmt.Fprint(out, line)
		}
	}
	fmt.Fprintf(out, "\n")
}

// ryshUnknownCommand reports a command word that is not in the table. It offers
// the closest matches by prefix before falling back to the full help, so a
// typo does not bury the user in 180 lines.
func (w *WorkspaceActor) ryshUnknownCommand(out *strings.Builder, cmd string) {
	fmt.Fprintf(out, "\n[rysh] unknown command: %q\n", cmd)
	w.failRysh("unknown command: %q", cmd)
	if near := nearestRyshCommands(cmd); len(near) > 0 {
		fmt.Fprintf(out, "  did you mean: %s\n", strings.Join(near, ", "))
		fmt.Fprintf(out, "  (##help lists every command)\n\n")
		return
	}
	w.ryshHelp(out)
}

// nearestRyshCommands returns names and aliases that share a prefix with the
// typed word, in either direction, so both "##pan" and "##panex" suggest
// "##pane". Empty when nothing is close enough to be worth guessing.
func nearestRyshCommands(cmd string) []string {
	if cmd == "" {
		return nil
	}
	var near []string
	for name := range ryshCommandIndex {
		if strings.HasPrefix(name, cmd) || strings.HasPrefix(cmd, name) {
			near = append(near, "##"+name)
		}
	}
	sort.Strings(near)
	return near
}
