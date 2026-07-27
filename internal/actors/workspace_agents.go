package actors

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/worktree"
)

// ---------------------------------------------------------------------------
// Autonomous agent handlers
// ---------------------------------------------------------------------------

// handleAgentPrompt processes @agent-name <prompt> input.
func (w *WorkspaceActor) handleAgentPrompt(sourcePaneID, text string) {
	trimmed := strings.TrimPrefix(text, "@")
	parts := strings.SplitN(trimmed, " ", 2)
	if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
		_ = w.pub.SendPaneRyshOutput(sourcePaneID,
			"\n[agents] usage: @agent-name <prompt>\n")
		return
	}

	agentName := parts[0]
	prompt := parts[1]

	if w.agentRegistryPID == nil {
		_ = w.pub.SendPaneRyshOutput(sourcePaneID,
			"\n[agents] agent registry not available (agentic mode disabled?)\n")
		return
	}

	// Echo the prompt in the source pane.
	_ = w.pub.SendPaneRyshOutput(sourcePaneID,
		fmt.Sprintf("\n[agents] sending prompt to @%s\n", agentName))

	w.actorSystem.Root.Send(w.agentRegistryPID, &msg.MsgAgentPrompt{
		AgentName:    agentName,
		Prompt:       prompt,
		SourcePaneID: sourcePaneID,
		// Resolve the invoking pane's scope chain so the agent inherits its scope
		// (the "execution point"). The agent registry re-points to this per prompt.
		ScopeHint: w.resolveScopeIDs(sourcePaneID).Hint(),
	})
}

// handleAgentCommand processes @@agent-name <command> input.
func (w *WorkspaceActor) handleAgentCommand(paneID, text string) {
	trimmed := strings.TrimPrefix(text, "@@")
	parts := strings.Fields(trimmed)
	if len(parts) < 2 {
		_ = w.pub.SendPaneRyshOutput(paneID,
			"\n[agents] usage: @@agent-name <command>\n"+
				"  commands: stop, continue, activate, deactivate\n")
		return
	}

	agentName := parts[0]
	subCmd := parts[1]

	if w.agentRegistryPID == nil {
		_ = w.pub.SendPaneRyshOutput(paneID,
			"\n[agents] agent registry not available\n")
		return
	}

	switch subCmd {
	case "stop":
		w.actorSystem.Root.Send(w.agentRegistryPID, &msg.MsgAgentStop{Name: agentName})
		_ = w.pub.SendPaneRyshOutput(paneID,
			fmt.Sprintf("\n[agents] interrupting agent %q — state preserved; `@@%s continue` resumes\n", agentName, agentName))

	case "continue":
		w.actorSystem.Root.Send(w.agentRegistryPID, &msg.MsgAgentContinue{Name: agentName})
		_ = w.pub.SendPaneRyshOutput(paneID,
			fmt.Sprintf("\n[agents] resuming agent %q from its checkpoint\n", agentName))

	case "activate":
		w.actorSystem.Root.Send(w.agentRegistryPID, &msg.MsgAgentActivate{Name: agentName})
		_ = w.pub.SendPaneRyshOutput(paneID,
			fmt.Sprintf("\n[agents] activating agent %q\n", agentName))

	case "deactivate":
		w.actorSystem.Root.Send(w.agentRegistryPID, &msg.MsgAgentDeactivate{Name: agentName})
		_ = w.pub.SendPaneRyshOutput(paneID,
			fmt.Sprintf("\n[agents] deactivating agent %q\n", agentName))

	default:
		_ = w.pub.SendPaneRyshOutput(paneID,
			fmt.Sprintf("\n[agents] unknown command: %q\n"+
				"  commands: stop, continue, activate, deactivate\n", subCmd))
	}
}

// handleAgentSubcommand processes ##agent subcommands.
func (w *WorkspaceActor) handleAgentSubcommand(out *strings.Builder, paneID string, args []string) {
	if len(args) == 0 || args[0] == "help" {
		w.agentHelp(out)
		return
	}

	if w.agentRegistryPID == nil {
		fmt.Fprintf(out, "\n[agents] agent registry not available (agentic mode disabled?)\n")
		return
	}

	sub := args[0]
	switch sub {
	case "spawn":
		// --worktree provisions a per-agent git worktree (design 008), same as
		// `isolation: worktree` in the skill frontmatter.
		spawnArgs, forceWorktree := stripWorktreeFlag(args[1:])
		if len(spawnArgs) == 0 {
			fmt.Fprintf(out, "\n[agents] usage:\n")
			fmt.Fprintf(out, "  ##agent spawn <name> [--worktree]      load .rysh/agents/<name>/SKILL.md\n")
			fmt.Fprintf(out, "  ##agent spawn <path-to-file.md>        load explicit skill file\n")
			fmt.Fprintf(out, "  ##agent spawn <name> <system-prompt>   create agent inline\n")
			return
		}
		// Single arg: skill lookup (bare name or explicit .md/path).
		if len(spawnArgs) == 1 || strings.HasSuffix(spawnArgs[0], ".md") {
			// Explicit file path with extra trailing args is ambiguous — favor file.
			w.createAgentFromFile(out, paneID, spawnArgs[0], forceWorktree)
		} else {
			// Multiple args without .md: inline definition.
			name := spawnArgs[0]
			systemPrompt := strings.Join(spawnArgs[1:], " ")
			var wtBranch, wtPath string
			if forceWorktree {
				// A single spawn retargets the caller's pane group: the point is
				// to work with this agent in its worktree right here.
				wtBranch, wtPath = w.isolateAgentWorktree(out, paneID, name, true)
			}
			w.actorSystem.Root.Send(w.agentRegistryPID, &msg.MsgAgentCreate{
				Name:           name,
				SystemPrompt:   systemPrompt,
				WorktreeBranch: wtBranch,
				WorktreePath:   wtPath,
			})
			fmt.Fprintf(out, "\n[agents] spawning agent %q\n", name)
		}

	case "spawn-all":
		spawnAllArgs, forceWorktree := stripWorktreeFlag(args[1:])
		dir := ""
		if len(spawnAllArgs) >= 1 {
			dir = spawnAllArgs[0]
		}
		w.createAgentsFromDir(out, paneID, dir, forceWorktree)

	case "register-output":
		if len(args) < 3 {
			fmt.Fprintf(out, "\n[agents] usage: ##agent register-output <agent-name> <pane-name>\n")
			return
		}
		agentName := args[1]
		paneName := args[2]
		targetPaneID := w.resolvePaneID(paneName)
		if targetPaneID == "" {
			fmt.Fprintf(out, "\n[agents] pane not found: %s\n", paneName)
			return
		}
		w.actorSystem.Root.Send(w.agentRegistryPID, &msg.MsgAgentRegisterPane{
			AgentName: agentName,
			PaneID:    targetPaneID,
			PaneName:  paneName,
		})
		fmt.Fprintf(out, "\n[agents] registered agent %q output to pane %q\n", agentName, paneName)

	case "unregister-output":
		if len(args) < 3 {
			fmt.Fprintf(out, "\n[agents] usage: ##agent unregister-output <agent-name> <pane-name>\n")
			return
		}
		agentName := args[1]
		paneName := args[2]
		targetPaneID := w.resolvePaneID(paneName)
		if targetPaneID == "" {
			fmt.Fprintf(out, "\n[agents] pane not found: %s\n", paneName)
			return
		}
		w.actorSystem.Root.Send(w.agentRegistryPID, &msg.MsgAgentUnregisterPane{
			AgentName: agentName,
			PaneID:    targetPaneID,
		})
		fmt.Fprintf(out, "\n[agents] unregistered agent %q output from pane %q\n", agentName, paneName)

	case "list":
		// ##agent list [instances|artefacts]. "instances" (the default) shows the
		// agents loaded into this workspace; "artefacts" shows the skill files on
		// disk under .rysh/agents and marks which are loaded.
		mode := "instances"
		if len(args) >= 2 {
			mode = args[1]
		}
		switch mode {
		case "instances", "instance", "loaded":
			w.agentListInstances(out)
		case "artefacts", "artifacts", "artefact", "artifact", "files", "disk":
			w.agentListArtefacts(out)
		default:
			fmt.Fprintf(out, "\n[agents] usage: ##agent list [instances|artefacts]\n")
			fmt.Fprintf(out, "  instances  loaded in this workspace (default)\n")
			fmt.Fprintf(out, "  artefacts  skill files under .rysh/agents, marking the loaded ones\n")
		}

	case "show":
		if len(args) < 2 {
			fmt.Fprintf(out, "\n[agents] usage: ##agent show <name>\n")
			return
		}
		w.agentShow(out, args[1])

	case "delete":
		if len(args) < 2 {
			fmt.Fprintf(out, "\n[agents] usage: ##agent delete <name>\n")
			return
		}
		w.actorSystem.Root.Send(w.agentRegistryPID, &msg.MsgAgentDelete{Name: args[1]})
		fmt.Fprintf(out, "\n[agents] deleting agent %q\n", args[1])

	case "activate":
		if len(args) < 2 {
			fmt.Fprintf(out, "\n[agents] usage: ##agent activate <name>\n")
			return
		}
		w.actorSystem.Root.Send(w.agentRegistryPID, &msg.MsgAgentActivate{Name: args[1]})
		fmt.Fprintf(out, "\n[agents] activating agent %q\n", args[1])

	case "deactivate":
		if len(args) < 2 {
			fmt.Fprintf(out, "\n[agents] usage: ##agent deactivate <name>\n")
			return
		}
		w.actorSystem.Root.Send(w.agentRegistryPID, &msg.MsgAgentDeactivate{Name: args[1]})
		fmt.Fprintf(out, "\n[agents] deactivating agent %q\n", args[1])

	case "reload-prompts":
		// Follow-up 2b: reload the layered prompt store + re-apply on Setup
		// + broadcast MsgReloadPrompts to every active LLM-execution actor.
		w.handleAgentReloadPrompts(out, paneID)

	case "metrics":
		// Follow-up 3b: dump the in-process metrics sink to the active
		// pane.
		w.handleAgentMetrics(out)

	default:
		fmt.Fprintf(out, "\n[agents] unknown subcommand: %q\n", sub)
		w.agentHelp(out)
	}
}

// queryAgents fetches the agents currently loaded into the workspace from the
// registry via a synchronous request/reply.
func (w *WorkspaceActor) queryAgents() ([]msg.AgentInfo, error) {
	future := w.actorSystem.Root.RequestFuture(w.agentRegistryPID,
		&msg.MsgAgentList{}, 2*time.Second)
	result, err := future.Result()
	if err != nil {
		return nil, err
	}
	reply, ok := result.(*msg.MsgAgentListReply)
	if !ok {
		return nil, fmt.Errorf("unexpected reply type")
	}
	return reply.Agents, nil
}

// agentShow prints an agent's recipe: the skill file it is defined by, exactly
// as written (frontmatter + system prompt), with literal credential values
// masked. ${VAR} references are NOT expanded — the recipe should read like the
// file, and expanding here would print secrets into the pane. An agent spawned
// inline (no file on disk) falls back to the system prompt the registry holds.
func (w *WorkspaceActor) agentShow(out *strings.Builder, name string) {
	path := skillFilePath("agents", name)
	if content, err := os.ReadFile(path); err == nil {
		display := deriveSkillName(path)
		if def, perr := parseSkillFile(path, envOnlyExpand); perr == nil && def.Name != "" {
			display = def.Name
		}
		renderRecipe(out, "agents", display, path, w.agentLoaded(display), string(content), 60)
		return
	}
	if a, ok := w.loadedAgent(name); ok {
		renderRecipe(out, "agents", a.Name, "", true, a.SystemPrompt, 60)
		return
	}
	fmt.Fprintf(out, "\n[agents] no recipe for %q: no skill file at %s, and no agent by that name is loaded\n",
		name, path)
}

// loadedAgent returns the loaded instance with this name, if any.
func (w *WorkspaceActor) loadedAgent(name string) (msg.AgentInfo, bool) {
	agents, err := w.queryAgents()
	if err != nil {
		return msg.AgentInfo{}, false
	}
	for _, a := range agents {
		if a.Name == name {
			return a, true
		}
	}
	return msg.AgentInfo{}, false
}

// agentLoaded reports whether an agent by this name is loaded (best effort: a
// registry that does not answer reads as "not loaded").
func (w *WorkspaceActor) agentLoaded(name string) bool {
	_, ok := w.loadedAgent(name)
	return ok
}

// agentListInstances prints the agents loaded into this workspace — the
// behaviour of "##agent list" and "##agent list instances".
func (w *WorkspaceActor) agentListInstances(out *strings.Builder) {
	agents, err := w.queryAgents()
	if err != nil {
		fmt.Fprintf(out, "\n[agents] failed to list agents: %v\n", err)
		return
	}
	if len(agents) == 0 {
		fmt.Fprintf(out, "\n[agents] no agents loaded in this workspace (spawn one with ##agent spawn <name>)\n")
		return
	}
	fmt.Fprintf(out, "\n[agents] %d instance(s) loaded in this workspace:\n", len(agents))
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
	for _, a := range agents {
		status := "active"
		if !a.Active {
			status = "inactive"
		}
		panes := "-"
		if len(a.RegisteredPanes) > 0 {
			panes = strings.Join(a.RegisteredPanes, ", ")
		}
		promptPreview := a.SystemPrompt
		if len(promptPreview) > 60 {
			promptPreview = promptPreview[:57] + "..."
		}
		fmt.Fprintf(out, "  %-20s [%s]  panes: %s\n", a.Name, status, panes)
		fmt.Fprintf(out, "    prompt: %s\n", promptPreview)
	}
	fmt.Fprintf(out, "\n")
}

// agentListArtefacts lists the agent skill files on disk under .rysh/agents
// (each a <name>/SKILL.md), marking the ones currently loaded into the
// workspace — the behaviour of "##agent list artefacts".
func (w *WorkspaceActor) agentListArtefacts(out *strings.Builder) {
	dir := resolveRyshPath("agents", "")
	// Listing only needs names/descriptions/models, so resolve ${VAR} from the
	// environment only (no secret-store lookups, no values shown).
	defs, err := parseSkillDir("", envOnlyExpand)
	if err != nil {
		fmt.Fprintf(out, "\n[agents] no artefacts under %s (%v)\n", dir, err)
		return
	}
	if len(defs) == 0 {
		fmt.Fprintf(out, "\n[agents] no agent artefacts found under %s\n", dir)
		return
	}

	// Best-effort set of loaded names so each artefact can be marked.
	loaded := map[string]bool{}
	if agents, aerr := w.queryAgents(); aerr == nil {
		for _, a := range agents {
			loaded[a.Name] = true
		}
	}

	renderAgentArtefacts(out, dir, defs, loaded)
}

// renderAgentArtefacts writes the artefact table: one row per skill file, sorted
// by name, each marked [loaded]/[not loaded] against the loaded set, with its
// model and a truncated description. Kept pure (no registry/actor dependency) so
// it can be tested directly.
func renderAgentArtefacts(out *strings.Builder, dir string, defs []*agentDefinition, loaded map[string]bool) {
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	loadedCount := 0
	fmt.Fprintf(out, "\n[agents] %d artefact(s) under %s:\n", len(defs), dir)
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
	for _, d := range defs {
		mark := "not loaded"
		if loaded[d.Name] {
			mark = "loaded"
			loadedCount++
		}
		model := d.Model
		if model == "" {
			model = "-"
		}
		desc := d.Description
		if len(desc) > 38 {
			desc = desc[:35] + "..."
		}
		fmt.Fprintf(out, "  %-20s [%-10s] model: %-22s %s\n", d.Name, mark, model, desc)
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
	fmt.Fprintf(out, "  %d of %d loaded in this workspace; spawn an artefact with ##agent spawn <name>\n\n",
		loadedCount, len(defs))
}

// createAgentFromFile parses a skill .md file and spawns an agent, honouring
// `isolation: worktree` frontmatter (or the --worktree flag override).
func (w *WorkspaceActor) createAgentFromFile(out *strings.Builder, paneID, path string, forceWorktree bool) {
	def, err := parseSkillFile(path, w.namedExpandFunc(paneID))
	if err != nil {
		fmt.Fprintf(out, "\n[agents] failed to parse skill file %q: %v\n", path, err)
		return
	}
	if def.Name == "" {
		fmt.Fprintf(out, "\n[agents] skill file %q has no name\n", path)
		return
	}
	if def.SystemPrompt == "" {
		fmt.Fprintf(out, "\n[agents] skill file %q has no system prompt\n", path)
		return
	}

	var wtBranch, wtPath string
	if forceWorktree || def.Isolation == "worktree" {
		wtBranch, wtPath = w.isolateAgentWorktree(out, paneID, def.Name, true)
	}
	w.actorSystem.Root.Send(w.agentRegistryPID, &msg.MsgAgentCreate{
		Name:           def.Name,
		SystemPrompt:   def.SystemPrompt,
		WorktreeBranch: wtBranch,
		WorktreePath:   wtPath,
		AutoApprove:    def.AutoApprove,
	})
	fmt.Fprintf(out, "\n[agents] spawning agent %q from %s\n", def.Name, path)
}

// createAgentsFromDir spawns agents from all .md files in a directory. With
// forceWorktree (##agent spawn-all <dir> --worktree) — or per-agent
// `isolation: worktree` frontmatter — each agent gets its own worktree, but no
// pane group is retargeted: N agents cannot share the caller's group, so the
// worktrees are provisioned and reported with their `##worktree cwd` handles.
func (w *WorkspaceActor) createAgentsFromDir(out *strings.Builder, paneID, dirPath string, forceWorktree bool) {
	defs, err := parseSkillDir(dirPath, w.namedExpandFunc(paneID))
	if err != nil {
		fmt.Fprintf(out, "\n[agents] failed to read directory %q: %v\n", dirPath, err)
		return
	}
	if len(defs) == 0 {
		fmt.Fprintf(out, "\n[agents] no .md files found in %q\n", dirPath)
		return
	}

	for _, def := range defs {
		if def.Name == "" || def.SystemPrompt == "" {
			fmt.Fprintf(out, "\n[agents] skipping invalid definition in %q\n", dirPath)
			continue
		}
		var wtBranch, wtPath string
		if forceWorktree || def.Isolation == "worktree" {
			wtBranch, wtPath = w.isolateAgentWorktree(out, paneID, def.Name, false)
		}
		w.actorSystem.Root.Send(w.agentRegistryPID, &msg.MsgAgentCreate{
			Name:           def.Name,
			SystemPrompt:   def.SystemPrompt,
			WorktreeBranch: wtBranch,
			WorktreePath:   wtPath,
			AutoApprove:    def.AutoApprove,
		})
		fmt.Fprintf(out, "\n[agents] spawning agent %q\n", def.Name)
	}
	fmt.Fprintf(out, "\n[agents] spawned %d agent(s) from %s\n", len(defs), dirPath)
}

// stripWorktreeFlag removes --worktree from args, reporting whether it was
// present. Order of the remaining args is preserved.
func stripWorktreeFlag(args []string) ([]string, bool) {
	out := make([]string, 0, len(args))
	found := false
	for _, a := range args {
		if a == "--worktree" {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}

// isolateAgentWorktree provisions the per-agent worktree (design 008):
// branch agent/<name>, directory .rysh/worktrees/<sanitized>, reused when it
// already exists so re-spawning after a skill edit is idempotent. With
// retarget it points the caller's pane group at the worktree (the single-
// spawn flow); otherwise it prints the `##worktree cwd` handle. Returns the
// branch+path for the registry's KV record, or empty strings when isolation
// could not be provisioned — the spawn still proceeds on the shared checkout,
// SAYING so, rather than failing the agent outright.
func (w *WorkspaceActor) isolateAgentWorktree(out *strings.Builder, paneID, name string, retarget bool) (string, string) {
	base := w.baseDir()
	if !worktree.IsGitRepo(base) {
		fmt.Fprintf(out, "\n[agents] %s: worktree isolation SKIPPED — %s is not a git repository\n", name, base)
		return "", ""
	}
	root, err := worktree.RepoRoot(base)
	if err != nil {
		fmt.Fprintf(out, "\n[agents] %s: worktree isolation SKIPPED — %v\n", name, err)
		return "", ""
	}
	branch := "agent/" + worktree.SanitizeBranch(name)
	path := worktree.Dir(root, branch)
	if info, statErr := os.Stat(path); statErr != nil || !info.IsDir() {
		if err := worktree.Add(root, path, branch); err != nil {
			fmt.Fprintf(out, "\n[agents] %s: worktree isolation SKIPPED — %v\n", name, err)
			return "", ""
		}
		fmt.Fprintf(out, "\n[agents] %s: created worktree %s (branch %s)\n", name, path, branch)
	} else {
		fmt.Fprintf(out, "\n[agents] %s: reusing worktree %s (branch %s)\n", name, path, branch)
	}

	if retarget {
		if groupID := w.groupIDForPane(paneID); groupID != "" {
			_ = w.pub.Send(msg.T("pane-group", groupID, "inbox"), &msg.MsgSetWorkingDir{Dir: path})
			// Record the group as a worktree user for cleanup-on-close
			// (workspace_pane_worktree.go). The agent's own registry record
			// keeps the worktree alive while the agent exists.
			w.trackGroupWorktree(groupID, path, branch)
			fmt.Fprintf(out, "[agents] this pane group now starts new panes in the worktree\n")
		} else {
			fmt.Fprintf(out, "[agents] point a pane group at it with: ##worktree cwd %s\n", branch)
		}
	} else {
		fmt.Fprintf(out, "[agents] point a pane group at it with: ##worktree cwd %s\n", branch)
	}
	return branch, path
}

// agentHelp writes agent command help text.
func (w *WorkspaceActor) agentHelp(out *strings.Builder) {
	fmt.Fprintf(out, "\n[agents] usage:\n")
	fmt.Fprintf(out, "  ##agent spawn <name> [--worktree]             load .rysh/agents/<name>/SKILL.md\n")
	fmt.Fprintf(out, "  ##agent spawn <path-to-file.md>               load explicit skill file\n")
	fmt.Fprintf(out, "  ##agent spawn <name> <system-prompt>          create agent inline\n")
	fmt.Fprintf(out, "  ##agent spawn-all [directory] [--worktree]     spawn all agents (default: .rysh/agents)\n")
	fmt.Fprintf(out, "    --worktree (or `isolation: worktree` frontmatter): per-agent git worktree,\n")
	fmt.Fprintf(out, "    branch agent/<name>; single spawn also points this pane group at it\n")
	fmt.Fprintf(out, "    `auto_approve:` frontmatter (default true): run tool calls without an\n")
	fmt.Fprintf(out, "    approval dialog. Set false to be asked first — policy always_gate /\n")
	fmt.Fprintf(out, "    bash.deny still gate regardless.\n")
	fmt.Fprintf(out, "  ##agent list [instances]                       list agents loaded in this workspace (default)\n")
	fmt.Fprintf(out, "  ##agent list artefacts                         list skill files under .rysh/agents, marking loaded ones\n")
	fmt.Fprintf(out, "  ##agent show <name>                            print an agent's recipe (skill file: frontmatter + prompt)\n")
	fmt.Fprintf(out, "  ##agent delete <name>                          delete an agent\n")
	fmt.Fprintf(out, "  ##agent activate <name>                        activate an agent\n")
	fmt.Fprintf(out, "  ##agent deactivate <name>                      deactivate an agent\n")
	fmt.Fprintf(out, "  ##agent register-output <agent> <pane>         route agent output to pane chat\n")
	fmt.Fprintf(out, "  ##agent unregister-output <agent> <pane>       stop routing output to pane\n")
	fmt.Fprintf(out, "  ##agent reload-prompts                         re-read rysh-cli-agent-prompts/ (override dir wins; effective next prompt)\n")
	fmt.Fprintf(out, "  ##agent metrics                                dump per-tool / LLM / compaction metrics\n")
	fmt.Fprintf(out, "\n")
	fmt.Fprintf(out, "  skill lookup: bare names resolve to <base>/agents/<name>/SKILL.md, where\n")
	fmt.Fprintf(out, "    <base> is ./.rysh if a project .rysh exists, else $HOME/.rysh.\n")
	fmt.Fprintf(out, "    Use ./, ../, /, ~/ for explicit paths (legacy *.md files supported).\n")
	fmt.Fprintf(out, "\n")
	fmt.Fprintf(out, "  @agent-name <prompt>                           send prompt to agent\n")
	fmt.Fprintf(out, "  @@agent-name stop                              stop agent\n")
	fmt.Fprintf(out, "  @@agent-name activate                          activate agent\n")
	fmt.Fprintf(out, "  @@agent-name deactivate                        deactivate agent\n")
	fmt.Fprintf(out, "\n")
}

// ---------------------------------------------------------------------------
// Humanoid commands (##humanoid)
// ---------------------------------------------------------------------------

// extractEntityName extracts the name from a @name or @@name prefix.
func extractEntityName(text, prefix string) string {
	trimmed := strings.TrimPrefix(text, prefix)
	parts := strings.Fields(trimmed)
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}
