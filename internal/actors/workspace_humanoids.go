package actors

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

// isHumanoid checks if a name belongs to a humanoid (vs an agent).
// We use a synchronous request to the humanoid registry.
func (w *WorkspaceActor) isHumanoid(name string) bool {
	if w.humanoidRegistryPID == nil {
		return false
	}
	future := w.actorSystem.Root.RequestFuture(w.humanoidRegistryPID,
		&msg.MsgHumanoidList{}, 2*time.Second)
	result, err := future.Result()
	if err != nil {
		return false
	}
	reply, ok := result.(*msg.MsgHumanoidListReply)
	if !ok {
		return false
	}
	for _, h := range reply.Humanoids {
		if h.Name == name {
			return true
		}
	}
	return false
}

// handleHumanoidPrompt processes @humanoid-name <prompt> input.
func (w *WorkspaceActor) handleHumanoidPrompt(sourcePaneID, text string) {
	trimmed := strings.TrimPrefix(text, "@")
	parts := strings.SplitN(trimmed, " ", 2)
	if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
		_ = w.pub.SendPaneRyshOutput(sourcePaneID,
			"\n[humanoids] usage: @humanoid-name <prompt>\n")
		return
	}

	humanoidName := parts[0]
	prompt := parts[1]

	if w.humanoidRegistryPID == nil {
		_ = w.pub.SendPaneRyshOutput(sourcePaneID,
			"\n[humanoids] humanoid registry not available (agentic mode disabled?)\n")
		return
	}

	// Echo the prompt in the source pane.
	_ = w.pub.SendPaneRyshOutput(sourcePaneID,
		fmt.Sprintf("\n[humanoids] sending prompt to @%s\n", humanoidName))

	w.actorSystem.Root.Send(w.humanoidRegistryPID, &msg.MsgHumanoidPrompt{
		HumanoidName: humanoidName,
		Prompt:       prompt,
		SourcePaneID: sourcePaneID,
		ScopeHint:    w.resolveScopeIDs(sourcePaneID).Hint(),
	})
}

// handleHumanoidCommand processes @@humanoid-name <command> input.
func (w *WorkspaceActor) handleHumanoidCommand(paneID, text string) {
	trimmed := strings.TrimPrefix(text, "@@")
	parts := strings.Fields(trimmed)
	if len(parts) < 2 {
		_ = w.pub.SendPaneRyshOutput(paneID,
			"\n[humanoids] usage: @@humanoid-name <command>\n"+
				"  commands: stop, continue, activate, deactivate\n"+
				"  (stop interrupts the current run; ##humanoid stop <name> ends the humanoid)\n")
		return
	}

	humanoidName := parts[0]
	subCmd := parts[1]

	if w.humanoidRegistryPID == nil {
		_ = w.pub.SendPaneRyshOutput(paneID,
			"\n[humanoids] humanoid registry not available\n")
		return
	}

	switch subCmd {
	case "stop":
		w.actorSystem.Root.Send(w.humanoidRegistryPID, &msg.MsgHumanoidStop{Name: humanoidName})
		_ = w.pub.SendPaneRyshOutput(paneID,
			fmt.Sprintf("\n[humanoids] interrupting humanoid %q — state preserved; `@@%s continue` resumes\n", humanoidName, humanoidName))

	case "continue":
		w.actorSystem.Root.Send(w.humanoidRegistryPID, &msg.MsgHumanoidContinue{Name: humanoidName})
		_ = w.pub.SendPaneRyshOutput(paneID,
			fmt.Sprintf("\n[humanoids] resuming humanoid %q from its checkpoint\n", humanoidName))

	case "activate":
		w.actorSystem.Root.Send(w.humanoidRegistryPID, &msg.MsgHumanoidActivate{Name: humanoidName})
		_ = w.pub.SendPaneRyshOutput(paneID,
			fmt.Sprintf("\n[humanoids] activating humanoid %q\n", humanoidName))

	case "deactivate":
		w.actorSystem.Root.Send(w.humanoidRegistryPID, &msg.MsgHumanoidDeactivate{Name: humanoidName})
		_ = w.pub.SendPaneRyshOutput(paneID,
			fmt.Sprintf("\n[humanoids] deactivating humanoid %q\n", humanoidName))

	default:
		_ = w.pub.SendPaneRyshOutput(paneID,
			fmt.Sprintf("\n[humanoids] unknown command: %q\n"+
				"  commands: stop, continue, activate, deactivate\n", subCmd))
	}
}

// handleHumanoidSubcommand processes ##humanoid subcommands.
func (w *WorkspaceActor) handleHumanoidSubcommand(out *strings.Builder, paneID string, args []string) {
	if len(args) == 0 || args[0] == "help" {
		w.humanoidHelp(out)
		return
	}

	if w.humanoidRegistryPID == nil {
		fmt.Fprintf(out, "\n[humanoids] humanoid registry not available (agentic mode disabled?)\n")
		w.failRysh("humanoid registry not available (agentic mode disabled?)")
		return
	}

	sub := args[0]
	switch sub {
	case "spawn":
		if len(args) < 2 {
			ryshWriter(out).UsageIn("humanoids",
				"##humanoid spawn <name>                   load .rysh/humanoids/<name>/SKILL.md",
				"##humanoid spawn <path-to-file.md>        load explicit skill file",
				"##humanoid spawn <name> <system-prompt>   create humanoid inline",
			)
			return
		}
		// Single arg: skill lookup (bare name or explicit .md/path).
		if len(args) == 2 {
			w.createHumanoidFromFile(out, paneID, args[1])
		} else if strings.HasSuffix(args[1], ".md") {
			w.createHumanoidFromFile(out, paneID, args[1])
		} else {
			name := args[1]
			systemPrompt := strings.Join(args[2:], " ")
			w.actorSystem.Root.Send(w.humanoidRegistryPID, &msg.MsgHumanoidCreate{
				Name:         name,
				SystemPrompt: systemPrompt,
			})
			fmt.Fprintf(out, "\n[humanoids] spawning humanoid %q\n", name)
		}

	case "spawn-all":
		dir := ""
		if len(args) >= 2 {
			dir = args[1]
		}
		w.createHumanoidsFromDir(out, paneID, dir)

	case "register-output":
		if len(args) < 3 {
			ryshWriter(out).UsageLineIn("humanoids", "##humanoid register-output <humanoid-name> <pane-name>")
			w.failRyshUsage("usage: %s", "##humanoid register-output <humanoid-name> <pane-name>")
			return
		}
		humanoidName := args[1]
		paneName := args[2]
		targetPaneID := w.resolvePaneID(paneName)
		if targetPaneID == "" {
			fmt.Fprintf(out, "\n[humanoids] pane not found: %s\n", paneName)
			w.failRysh("pane not found: %s", paneName)
			return
		}
		groupID := w.findPaneGroupID(targetPaneID)
		w.actorSystem.Root.Send(w.humanoidRegistryPID, &msg.MsgHumanoidRegisterPane{
			HumanoidName: humanoidName,
			PaneID:       targetPaneID,
			PaneName:     paneName,
			PaneGroupID:  groupID,
		})
		fmt.Fprintf(out, "\n[humanoids] registered humanoid %q output to pane %q\n", humanoidName, paneName)

	case "unregister-output":
		if len(args) < 3 {
			ryshWriter(out).UsageLineIn("humanoids", "##humanoid unregister-output <humanoid-name> <pane-name>")
			w.failRyshUsage("usage: %s", "##humanoid unregister-output <humanoid-name> <pane-name>")
			return
		}
		humanoidName := args[1]
		paneName := args[2]
		targetPaneID := w.resolvePaneID(paneName)
		if targetPaneID == "" {
			fmt.Fprintf(out, "\n[humanoids] pane not found: %s\n", paneName)
			w.failRysh("pane not found: %s", paneName)
			return
		}
		w.actorSystem.Root.Send(w.humanoidRegistryPID, &msg.MsgHumanoidUnregisterPane{
			HumanoidName: humanoidName,
			PaneID:       targetPaneID,
		})
		fmt.Fprintf(out, "\n[humanoids] unregistered humanoid %q output from pane %q\n", humanoidName, paneName)

	case "list":
		// ##humanoid list [all|instances|artefacts]. "all" (the default) merges
		// both sources into one roster: the humanoids running in this workspace
		// AND the skill files on disk that are not spawned. "instances" and
		// "artefacts" keep the narrower single-source views.
		mode := "all"
		if len(args) >= 2 {
			mode = args[1]
		}
		switch mode {
		case "all", "":
			w.humanoidListAll(out)
		case "instances", "instance", "loaded", "running":
			w.humanoidListInstances(out)
		case "artefacts", "artifacts", "artefact", "artifact", "files", "disk":
			w.humanoidListArtefacts(out)
		default:
			ryshWriter(out).UsageLineIn("humanoids", "##humanoid list [all|instances|artefacts]")
			w.failRyshUsage("usage: %s", "##humanoid list [all|instances|artefacts]")
			fmt.Fprintf(out, "  all        running humanoids + unspawned skill files (default)\n")
			fmt.Fprintf(out, "  instances  only the humanoids loaded in this workspace\n")
			fmt.Fprintf(out, "  artefacts  only the skill files under .rysh/humanoids\n")
		}

	case "show":
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("humanoids", "##humanoid show <name>")
			w.failRyshUsage("usage: %s", "##humanoid show <name>")
			return
		}
		w.humanoidShow(out, args[1])

	case "stop", "delete":
		// The inverse of ##humanoid spawn: tear the instance down and drop it
		// from the workspace. The skill file on disk is untouched, so the
		// humanoid reappears as "stopped" in ##humanoid list and re-spawns by
		// name. ("delete" is the old spelling, kept working for scripts.)
		//
		// Not to be confused with `@@<name> stop`, which only interrupts the
		// in-flight run and keeps the humanoid alive (`@@<name> continue`).
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("humanoids", "##humanoid stop <name>")
			w.failRyshUsage("usage: %s", "##humanoid stop <name>")
			return
		}
		name := args[1]
		// The registry only logs a miss, so a typo used to ack as a success.
		// Check first and point at the roster instead.
		if !w.humanoidLoaded(name) {
			fmt.Fprintf(out, "\n[humanoids] no running humanoid named %q — see ##humanoid list\n", name)
			return
		}
		w.actorSystem.Root.Send(w.humanoidRegistryPID, &msg.MsgHumanoidDelete{Name: name})
		fmt.Fprintf(out, "\n[humanoids] stopping humanoid %q — its skill file stays on disk; ##humanoid spawn %s starts it again\n",
			name, name)

	case "activate":
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("humanoids", "##humanoid activate <name>")
			w.failRyshUsage("usage: %s", "##humanoid activate <name>")
			return
		}
		w.actorSystem.Root.Send(w.humanoidRegistryPID, &msg.MsgHumanoidActivate{Name: args[1]})
		fmt.Fprintf(out, "\n[humanoids] activating humanoid %q\n", args[1])

	case "deactivate":
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("humanoids", "##humanoid deactivate <name>")
			w.failRyshUsage("usage: %s", "##humanoid deactivate <name>")
			return
		}
		w.actorSystem.Root.Send(w.humanoidRegistryPID, &msg.MsgHumanoidDeactivate{Name: args[1]})
		fmt.Fprintf(out, "\n[humanoids] deactivating humanoid %q\n", args[1])

	case "channels":
		if len(args) < 2 {
			ryshWriter(out).UsageLineIn("humanoids", "##humanoid channels <name>")
			w.failRyshUsage("usage: %s", "##humanoid channels <name>")
			return
		}
		w.humanoidChannels(out, args[1])

	case "channel":
		if len(args) < 3 {
			ryshWriter(out).UsageIn("humanoids",
				"##humanoid channel start <name> <channel-type>",
				"##humanoid channel stop <name> <channel-type>",
			)
			return
		}
		action := args[1]
		if len(args) < 4 {
			ryshWriter(out).UsageLineIn("humanoids", fmt.Sprintf("##humanoid channel %s <name> <channel-type>", action))
			w.failRyshUsage("usage: %s", fmt.Sprintf("##humanoid channel %s <name> <channel-type>", action))
			return
		}
		humanoidName := args[2]
		channelType := args[3]
		switch action {
		case "start":
			_ = w.pub.Send(msg.T("humanoid", humanoidName, "inbox"),
				&msg.MsgHumanoidChannelStart{ChannelType: channelType})
			fmt.Fprintf(out, "\n[humanoids] starting channel %q on humanoid %q\n",
				channelType, humanoidName)
		case "stop":
			_ = w.pub.Send(msg.T("humanoid", humanoidName, "inbox"),
				&msg.MsgHumanoidChannelStop{ChannelType: channelType})
			fmt.Fprintf(out, "\n[humanoids] stopping channel %q on humanoid %q\n",
				channelType, humanoidName)
		default:
			fmt.Fprintf(out, "\n[humanoids] unknown channel action: %q (use start or stop)\n", action)
			w.failRysh("unknown channel action: %q (use start or stop)", action)
		}

	case "reply-to":
		// ##humanoid reply-to <name> messages
		// ##humanoid reply-to <name> mentions
		if len(args) < 3 {
			ryshWriter(out).UsageLineIn("humanoids", "##humanoid reply-to <name> messages|mentions")
			w.failRyshUsage("usage: %s", "##humanoid reply-to <name> messages|mentions")
			return
		}
		humanoidName := args[1]
		mode := args[2]
		if mode != "messages" && mode != "mentions" {
			fmt.Fprintf(out, "\n[humanoids] invalid reply mode %q — use \"messages\" or \"mentions\"\n", mode)
			w.failRysh("invalid reply mode %q — use \"messages\" or \"mentions\"", mode)
			return
		}
		// Default channel type to "slack" — this is the only channel where
		// the distinction between messages and mentions is meaningful.
		channelType := "slack"
		_ = w.pub.Send(msg.T("humanoid", humanoidName, "inbox"),
			&msg.MsgHumanoidSetReplyMode{ChannelType: channelType, Mode: mode})
		fmt.Fprintf(out, "\n[humanoids] %s reply mode set to %q\n", humanoidName, mode)

	case "governance":
		// ##humanoid governance <name> ai|human
		if len(args) < 3 {
			ryshWriter(out).UsageLineIn("humanoids", "##humanoid governance <name> ai|human")
			w.failRyshUsage("usage: %s", "##humanoid governance <name> ai|human")
			return
		}
		humanoidName := args[1]
		mode := args[2]
		if mode != "ai" && mode != "human" {
			fmt.Fprintf(out, "\n[humanoids] invalid governance mode %q — use \"ai\" or \"human\"\n", mode)
			w.failRysh("invalid governance mode %q — use \"ai\" or \"human\"", mode)
			return
		}
		_ = w.pub.Send(msg.T("humanoid", humanoidName, "inbox"),
			&msg.MsgHumanoidSetGovernance{Mode: mode})
		// The humanoid reports which channels actually switched (its pane echo);
		// this ack only confirms the request was sent.
		fmt.Fprintf(out, "\n[humanoids] requested governance %q for %s\n", mode, humanoidName)

	case "provider":
		// ##humanoid provider <name> <provider> [model]
		// design 006 §4.3 step 2 (R4). Valid providers: provider.KnownProviderNames.
		if len(args) < 3 {
			ryshWriter(out).UsageLineIn("humanoids", fmt.Sprintf(
				"##humanoid provider <name> <%s> [model]",
				strings.Join(provider.KnownProviderNames(), "|")))
			return
		}
		humanoidName := args[1]
		providerName := strings.ToLower(args[2])
		model := ""
		if len(args) > 3 {
			model = args[3]
		}
		if !provider.IsKnownProviderName(providerName) {
			fmt.Fprintf(out, "\n[humanoids] unknown provider %q — use %s\n",
				providerName, strings.Join(provider.KnownProviderNames(), ", "))
			w.failRyshUsage("unknown provider %q", providerName)
			return
		}
		_ = w.pub.Send(msg.T("humanoid", humanoidName, "inbox"),
			&msg.MsgHumanoidSetProvider{Provider: providerName, Model: model})
		fmt.Fprintf(out, "\n[humanoids] %s provider set to %q", humanoidName, providerName)
		if model != "" {
			fmt.Fprintf(out, " (model %s)", model)
		}
		fmt.Fprintf(out, "\n[humanoids] applies on the next executor spawn — deactivate/activate to apply now.\n")
		fmt.Fprintf(out, "[humanoids] in-memory only: a restart returns to the skill file.\n")

	case "pair":
		// ##humanoid pair list <name> [channel]
		// ##humanoid pair approve <name> <code>
		// ##humanoid pair link <name> <channel> [force]
		// All ride the humanoid.{name}.pairing subject — the same messages the
		// dashboard drives, so terminal and web share one code path (WS3,
		// design 003 §4.4).
		if len(args) < 3 {
			ryshWriter(out).UsageIn("humanoids",
				"##humanoid pair list <name> [channel]",
				"##humanoid pair approve <name> <code>",
				"##humanoid pair link <name> <channel> [force]",
			)
			return
		}
		action := args[1]
		humanoidName := args[2]
		switch action {
		case "list":
			channel := ""
			if len(args) >= 4 {
				channel = args[3]
			}
			w.humanoidPairList(out, humanoidName, channel)
		case "approve":
			if len(args) < 4 {
				ryshWriter(out).UsageLineIn("humanoids", "##humanoid pair approve <name> <code>")
				w.failRyshUsage("usage: %s", "##humanoid pair approve <name> <code>")
				return
			}
			code := args[3]
			_ = w.pub.Send(msg.T("humanoid", humanoidName, "pairing"),
				&msg.MsgChannelPairApprove{HumanoidName: humanoidName, Code: code})
			fmt.Fprintf(out, "\n[humanoids] approving pairing code %q on humanoid %q\n",
				code, humanoidName)
		case "link":
			// Explicit device-link trigger (X4, design 009 §3.4): never runs
			// implicitly — re-provisioning a linked number is the §6 hazard.
			// "force" overrides the adapter's re-link guard.
			if len(args) < 4 {
				ryshWriter(out).UsageLineIn("humanoids", "##humanoid pair link <name> <channel> [force]")
				w.failRyshUsage("usage: %s", "##humanoid pair link <name> <channel> [force]")
				return
			}
			channel := args[3]
			force := len(args) >= 5 && args[4] == "force"
			_ = w.pub.Send(msg.T("humanoid", humanoidName, "pairing"),
				&msg.MsgChannelPairLink{HumanoidName: humanoidName, Channel: channel, Force: force})
			fmt.Fprintf(out, "\n[humanoids] starting %s device-link on humanoid %q", channel, humanoidName)
			if force {
				fmt.Fprintf(out, " (force: re-link guard overridden)")
			}
			fmt.Fprintf(out, "\n[humanoids] the QR renders in the humanoid's chat pane when ready\n")
		default:
			fmt.Fprintf(out, "\n[humanoids] unknown pair action: %q (use list, approve or link)\n", action)
			w.failRysh("unknown pair action: %q (use list, approve or link)", action)
		}

	case "allow":
		// ##humanoid allow <name> <sender> [channel] — allowlist a sender
		// directly, skipping the pairing-code flow.
		if len(args) < 3 {
			ryshWriter(out).UsageLineIn("humanoids", "##humanoid allow <name> <sender> [channel]")
			w.failRyshUsage("usage: %s", "##humanoid allow <name> <sender> [channel]")
			return
		}
		humanoidName := args[1]
		sender := args[2]
		channel := ""
		if len(args) >= 4 {
			channel = args[3]
		}
		_ = w.pub.Send(msg.T("humanoid", humanoidName, "pairing"),
			&msg.MsgChannelAllow{HumanoidName: humanoidName, Channel: channel, SenderID: sender})
		scope := "all channels"
		if channel != "" {
			scope = channel
		}
		fmt.Fprintf(out, "\n[humanoids] allowlisting %q on humanoid %q (%s)\n",
			sender, humanoidName, scope)

	case "attention":
		if len(args) < 3 {
			ryshWriter(out).UsageIn("humanoids",
				"##humanoid attention enable <name>",
				"##humanoid attention disable <name>",
			)
			return
		}
		action := args[1]
		humanoidName := args[2]
		switch action {
		case "enable":
			_ = w.pub.Send(msg.T("humanoid", humanoidName, "inbox"),
				&msg.MsgAttentionEnable{HumanoidName: humanoidName})
			fmt.Fprintf(out, "\n[humanoids] attention enabled for %s\n", humanoidName)
		case "disable":
			_ = w.pub.Send(msg.T("humanoid", humanoidName, "inbox"),
				&msg.MsgAttentionDisable{HumanoidName: humanoidName})
			fmt.Fprintf(out, "\n[humanoids] attention disabled for %s\n", humanoidName)
		default:
			fmt.Fprintf(out, "\n[humanoids] unknown action: %s (use enable or disable)\n", action)
			w.failRysh("unknown action: %s (use enable or disable)", action)
		}

	default:
		ryshWriter(out).UnknownIn("humanoids", sub)
		w.failRyshUsage("unknown %s subcommand: %q", "humanoids", sub)
		w.humanoidHelp(out)
	}
}

// humanoidPairList fetches and prints a humanoid's pending pairing requests
// and allowlist via NATS request/reply on the pairing subject (the humanoid
// answers from its RequestEnvelope case, sweeping expired codes first).
func (w *WorkspaceActor) humanoidPairList(out *strings.Builder, humanoidName, channel string) {
	reply, err := w.pub.Request(msg.T("humanoid", humanoidName, "pairing"),
		&msg.MsgChannelPairList{HumanoidName: humanoidName, Channel: channel},
		2*time.Second)
	if err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "\n[humanoids] no pairing reply from %q (is the humanoid spawned?): %v\n",
			humanoidName, err)
		return
	}
	lr, ok := reply.(*msg.MsgChannelPairListReply)
	if !ok {
		fmt.Fprintf(out, "\n[humanoids] unexpected pairing reply type %T\n", reply)
		return
	}

	scope := "all channels"
	if channel != "" {
		scope = channel
	}
	fmt.Fprintf(out, "\n[pairing] %s (%s)\n", humanoidName, scope)
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 70))
	if len(lr.Pending) == 0 {
		fmt.Fprintf(out, "  pending: none\n")
	} else {
		fmt.Fprintf(out, "  pending (%d):\n", len(lr.Pending))
		for _, p := range lr.Pending {
			expires := time.Unix(p.ExpiresAt, 0).Format("15:04:05")
			fmt.Fprintf(out, "    %-8s %s (%s) on %s — expires %s\n",
				p.Code, p.SenderName, p.SenderID, p.Channel, expires)
			if p.FirstMsg != "" {
				fmt.Fprintf(out, "             first message: %s\n", p.FirstMsg)
			}
			fmt.Fprintf(out, "             approve: ##humanoid pair approve %s %s\n",
				humanoidName, p.Code)
		}
	}
	if len(lr.Allowlist) == 0 {
		fmt.Fprintf(out, "  allowlist: empty\n")
	} else {
		fmt.Fprintf(out, "  allowlist (%d):\n", len(lr.Allowlist))
		for _, sender := range lr.Allowlist {
			fmt.Fprintf(out, "    %s\n", sender)
		}
	}
	fmt.Fprintf(out, "\n")
}

// humanoidShow prints a humanoid's recipe: the skill file it is defined by,
// exactly as written (frontmatter + system prompt), with literal credential
// values masked. ${VAR} references are NOT expanded — the recipe should read
// like the file, and expanding here would print secrets into the pane.
// A humanoid spawned inline (no file on disk) falls back to the system prompt
// the registry holds.
func (w *WorkspaceActor) humanoidShow(out *strings.Builder, name string) {
	path := skillFilePath("humanoids", name)
	if content, err := os.ReadFile(path); err == nil {
		display := deriveSkillName(path)
		if def, perr := parseHumanoidFile(path, envOnlyExpand); perr == nil && def.Name != "" {
			display = def.Name
		}
		renderRecipe(out, "humanoids", display, path, w.humanoidLoaded(display), string(content), 70)
		return
	}
	if h, ok := w.loadedHumanoid(name); ok {
		renderRecipe(out, "humanoids", h.Name, "", true, h.SystemPrompt, 70)
		return
	}
	fmt.Fprintf(out, "\n[humanoids] no recipe for %q: no skill file at %s, and no humanoid by that name is loaded\n",
		name, path)
	w.failRysh("no recipe for %q", name)
}

// loadedHumanoid returns the loaded instance with this name, if any.
func (w *WorkspaceActor) loadedHumanoid(name string) (msg.HumanoidInfo, bool) {
	humanoids, err := w.queryHumanoids()
	if err != nil {
		return msg.HumanoidInfo{}, false
	}
	for _, h := range humanoids {
		if h.Name == name {
			return h, true
		}
	}
	return msg.HumanoidInfo{}, false
}

// humanoidLoaded reports whether a humanoid by this name is loaded (best
// effort: a registry that does not answer reads as "not loaded").
func (w *WorkspaceActor) humanoidLoaded(name string) bool {
	_, ok := w.loadedHumanoid(name)
	return ok
}

// humanoidChannels shows channel details for a specific humanoid.
func (w *WorkspaceActor) humanoidChannels(out *strings.Builder, name string) {
	future := w.actorSystem.Root.RequestFuture(w.humanoidRegistryPID,
		&msg.MsgHumanoidList{}, 2*time.Second)
	result, err := future.Result()
	if err != nil {
		fmt.Fprintf(out, "\n[humanoids] failed to query humanoids: %v\n", err)
		w.failRysh("failed to query humanoids: %v", err)
		return
	}
	reply, ok := result.(*msg.MsgHumanoidListReply)
	if !ok {
		fmt.Fprintf(out, "\n[humanoids] unexpected reply type\n")
		return
	}

	for _, h := range reply.Humanoids {
		if h.Name == name {
			if len(h.Channels) == 0 {
				fmt.Fprintf(out, "\n[humanoids] %q has no configured channels\n", name)
				return
			}
			fmt.Fprintf(out, "\n[humanoids] channels for %q:\n", name)
			fmt.Fprintf(out, "%s\n", strings.Repeat("-", 50))
			for _, ch := range h.Channels {
				connStr := "disconnected"
				if ch.Connected {
					connStr = "connected"
				}
				fmt.Fprintf(out, "  %-10s [%s]\n", ch.Type, connStr)
				if ch.Details != "" {
					fmt.Fprintf(out, "    details: %s\n", ch.Details)
				}
				if ch.Error != "" {
					fmt.Fprintf(out, "    error:   %s\n", ch.Error)
				}
			}
			fmt.Fprintf(out, "\n")
			return
		}
	}
	fmt.Fprintf(out, "\n[humanoids] humanoid not found: %q\n", name)
	w.failRysh("humanoid not found: %q", name)
}

// queryHumanoids fetches the humanoids currently loaded into the workspace from
// the registry via a synchronous request/reply.
func (w *WorkspaceActor) queryHumanoids() ([]msg.HumanoidInfo, error) {
	future := w.actorSystem.Root.RequestFuture(w.humanoidRegistryPID,
		&msg.MsgHumanoidList{}, 2*time.Second)
	result, err := future.Result()
	if err != nil {
		return nil, err
	}
	reply, ok := result.(*msg.MsgHumanoidListReply)
	if !ok {
		return nil, fmt.Errorf("unexpected reply type")
	}
	return reply.Humanoids, nil
}

// The three states "##humanoid list" reports. They fold the two axes a humanoid
// varies on — spawned into the workspace or not, and active or deactivated —
// into a single status column:
//
//	running  spawned and active: it answers on its channels
//	paused   spawned but deactivated (##humanoid activate resumes it)
//	stopped  a skill file on disk that is not spawned (##humanoid spawn starts it)
//
// "stopped" is exactly the state ##humanoid stop leaves a humanoid in, so the
// listing and the commands share one vocabulary.
const (
	humanoidStatusRunning = "running"
	humanoidStatusPaused  = "paused"
	humanoidStatusStopped = "stopped"
)

// humanoidStatusRank orders the roster: running first, then paused, then the
// stopped skill files — what is live matters more than what is merely on disk.
func humanoidStatusRank(status string) int {
	switch status {
	case humanoidStatusRunning:
		return 0
	case humanoidStatusPaused:
		return 1
	default:
		return 2
	}
}

// humanoidNameColWidth sizes the roster's name column. Sized to fit the longest
// skill names shipped in .rysh/humanoids (e.g. "tangomingo-sales-concierge").
const humanoidNameColWidth = 26

// humanoidRosterRow is one line of the merged "##humanoid list" table: a
// humanoid known as a running instance, as an on-disk skill file, or as both.
type humanoidRosterRow struct {
	Name     string
	Status   string // running | paused | stopped
	Channels string // channel types, "-" when none
	Detail   string // panes/prompt for spawned rows, description for stopped ones
	OnDisk   bool   // a skill file exists, so the row survives ##humanoid stop
}

// humanoidListAll prints the full roster: every humanoid spawned into this
// workspace plus every skill file under .rysh/humanoids that is not spawned.
// This is the default behaviour of "##humanoid list" — one table answering
// "what is running, and what could I run?" without a second command.
func (w *WorkspaceActor) humanoidListAll(out *strings.Builder) {
	loaded, lerr := w.queryHumanoids()
	if lerr != nil {
		// A dead registry is worth reporting, but the on-disk artefacts are
		// still listable — degrade to the skill files rather than printing
		// nothing.
		fmt.Fprintf(out, "\n[humanoids] registry unavailable (%v) — listing skill files only\n", lerr)
	}
	// Listing only needs names/descriptions/channel types, so resolve ${VAR}
	// from the environment only (no secret-store lookups, no values shown).
	defs, derr := parseHumanoidDir("", envOnlyExpand)
	if derr != nil {
		defs = nil
	}

	dir := resolveRyshPath("humanoids", "")
	rows := buildHumanoidRoster(loaded, defs)
	if len(rows) == 0 {
		fmt.Fprintf(out, "\n[humanoids] none: no humanoids spawned, and no skill files under %s\n", dir)
		fmt.Fprintf(out, "  write one at %s/<name>/SKILL.md, then ##humanoid spawn <name>\n\n", dir)
		return
	}
	renderHumanoidRoster(out, dir, rows)
}

// buildHumanoidRoster merges the two sources by humanoid name. A name present in
// both (the normal case: a spawned skill file) yields one row carrying the live
// status; a name only on disk is "stopped"; a name only in the registry was
// spawned inline (##humanoid spawn <name> <prompt>) and has no file to return
// to. Kept pure so the merge is testable without an actor system.
func buildHumanoidRoster(loaded []msg.HumanoidInfo, defs []*humanoidDefinition) []humanoidRosterRow {
	onDisk := make(map[string]*humanoidDefinition, len(defs))
	for _, d := range defs {
		if d != nil && d.Name != "" {
			onDisk[d.Name] = d
		}
	}

	rows := make([]humanoidRosterRow, 0, len(loaded)+len(defs))
	spawned := make(map[string]bool, len(loaded))
	for _, h := range loaded {
		spawned[h.Name] = true
		status := humanoidStatusRunning
		if !h.Active {
			status = humanoidStatusPaused
		}
		detail := "no output pane registered"
		if len(h.RegisteredPanes) > 0 {
			detail = "panes: " + strings.Join(h.RegisteredPanes, ", ")
		}
		rows = append(rows, humanoidRosterRow{
			Name:     h.Name,
			Status:   status,
			Channels: liveChannelSummary(h.Channels),
			Detail:   detail,
			OnDisk:   onDisk[h.Name] != nil,
		})
	}

	for _, d := range defs {
		if d == nil || d.Name == "" || spawned[d.Name] {
			continue
		}
		rows = append(rows, humanoidRosterRow{
			Name:     d.Name,
			Status:   humanoidStatusStopped,
			Channels: definedChannelSummary(d.Contacts),
			Detail:   d.Description,
			OnDisk:   true,
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		ri, rj := humanoidStatusRank(rows[i].Status), humanoidStatusRank(rows[j].Status)
		if ri != rj {
			return ri < rj
		}
		return rows[i].Name < rows[j].Name
	})
	return rows
}

// liveChannelSummary renders a spawned humanoid's channels as "slack,email!",
// where "!" marks a channel that is configured but not connected — the roster
// has one column for this, and a silent humanoid is usually a dead channel.
func liveChannelSummary(channels []msg.ChannelStatus) string {
	if len(channels) == 0 {
		return "-"
	}
	types := make([]string, 0, len(channels))
	for _, ch := range channels {
		label := ch.Type
		if !ch.Connected {
			label += "!"
		}
		types = append(types, label)
	}
	sort.Strings(types)
	return strings.Join(types, ",")
}

// definedChannelSummary renders the channel types a skill file declares. These
// are configured-but-not-running, so no connection marker applies.
func definedChannelSummary(contacts map[string]msg.ChannelConfig) string {
	if len(contacts) == 0 {
		return "-"
	}
	types := make([]string, 0, len(contacts))
	for ct := range contacts {
		types = append(types, ct)
	}
	sort.Strings(types)
	return strings.Join(types, ",")
}

// renderHumanoidRoster writes the merged table plus a summary counting each
// state. Kept pure (no registry/actor dependency) so it can be tested directly.
func renderHumanoidRoster(out *strings.Builder, dir string, rows []humanoidRosterRow) {
	var running, paused, stopped int
	for _, r := range rows {
		switch r.Status {
		case humanoidStatusRunning:
			running++
		case humanoidStatusPaused:
			paused++
		default:
			stopped++
		}
	}

	fmt.Fprintf(out, "\n[humanoids] %d humanoid(s): %d running, %d paused, %d stopped\n",
		len(rows), running, paused, stopped)
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 80))
	for _, r := range rows {
		// Real skill names run long ("tangomingo-sales-concierge"), so the name
		// column is sized for them and truncates past that rather than letting
		// one row shift every column after it.
		name := r.Name
		if len(name) > humanoidNameColWidth {
			name = name[:humanoidNameColWidth-3] + "..."
		}
		detail := r.Detail
		if len(detail) > 34 {
			detail = detail[:31] + "..."
		}
		fmt.Fprintf(out, "  %-*s [%-7s] channels: %-18s %s\n",
			humanoidNameColWidth, name, r.Status, r.Channels, detail)
		// An inline humanoid has no skill file, so ##humanoid stop is final —
		// flag it rather than let the user discover it after stopping.
		if !r.OnDisk && r.Status != humanoidStatusStopped {
			fmt.Fprintf(out, "  %-*s   inline (no skill file — stopping it cannot be undone)\n",
				humanoidNameColWidth, "")
		}
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 80))
	fmt.Fprintf(out, "  skill files: %s   ·   \"!\" on a channel means configured but not connected\n", dir)
	fmt.Fprintf(out, "  ##humanoid spawn <name> starts a stopped one · ##humanoid stop <name> stops a running one\n\n")
}

// humanoidListInstances prints the humanoids loaded into this workspace — the
// behaviour of "##humanoid list instances".
func (w *WorkspaceActor) humanoidListInstances(out *strings.Builder) {
	humanoids, err := w.queryHumanoids()
	if err != nil {
		fmt.Fprintf(out, "\n[humanoids] failed to list humanoids: %v\n", err)
		w.failRysh("failed to list humanoids: %v", err)
		return
	}
	if len(humanoids) == 0 {
		fmt.Fprintf(out, "\n[humanoids] no humanoids loaded in this workspace (spawn one with ##humanoid spawn <name>)\n")
		return
	}
	fmt.Fprintf(out, "\n[humanoids] %d instance(s) loaded in this workspace:\n", len(humanoids))
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 70))
	for _, h := range humanoids {
		status := "active"
		if !h.Active {
			status = "inactive"
		}
		panes := "-"
		if len(h.RegisteredPanes) > 0 {
			panes = strings.Join(h.RegisteredPanes, ", ")
		}
		promptPreview := h.SystemPrompt
		if len(promptPreview) > 60 {
			promptPreview = promptPreview[:57] + "..."
		}
		fmt.Fprintf(out, "  %-20s [%s]  panes: %s\n", h.Name, status, panes)
		fmt.Fprintf(out, "    prompt: %s\n", promptPreview)
		if len(h.Channels) > 0 {
			fmt.Fprintf(out, "    channels:\n")
			for _, ch := range h.Channels {
				connStr := "disconnected"
				if ch.Connected {
					connStr = "connected"
				}
				detail := ""
				if ch.Details != "" {
					detail = " (" + ch.Details + ")"
				}
				errStr := ""
				if ch.Error != "" {
					errStr = " error: " + ch.Error
				}
				fmt.Fprintf(out, "      %-10s [%s]%s%s\n", ch.Type, connStr, detail, errStr)
			}
		}
	}
	fmt.Fprintf(out, "\n")
}

// humanoidListArtefacts lists the humanoid skill files on disk under
// .rysh/humanoids (each a <name>/SKILL.md), marking the ones currently loaded
// into the workspace — the behaviour of "##humanoid list artefacts".
func (w *WorkspaceActor) humanoidListArtefacts(out *strings.Builder) {
	dir := resolveRyshPath("humanoids", "")
	// Listing only needs names/descriptions/channel types, so resolve ${VAR}
	// from the environment only (no secret-store lookups, no values shown).
	defs, err := parseHumanoidDir("", envOnlyExpand)
	if err != nil {
		w.failRysh("%v", err)
		fmt.Fprintf(out, "\n[humanoids] no artefacts under %s (%v)\n", dir, err)
		return
	}
	if len(defs) == 0 {
		fmt.Fprintf(out, "\n[humanoids] no humanoid artefacts found under %s\n", dir)
		return
	}

	// Best-effort set of loaded names so each artefact can be marked.
	loaded := map[string]bool{}
	if humanoids, herr := w.queryHumanoids(); herr == nil {
		for _, h := range humanoids {
			loaded[h.Name] = true
		}
	}

	renderHumanoidArtefacts(out, dir, defs, loaded)
}

// renderHumanoidArtefacts writes the artefact table: one row per skill file,
// sorted by name, each marked [loaded]/[not loaded] against the loaded set, with
// its channel types and a truncated description. Kept pure (no registry/actor
// dependency) so it can be tested directly.
func renderHumanoidArtefacts(out *strings.Builder, dir string, defs []*humanoidDefinition, loaded map[string]bool) {
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	loadedCount := 0
	fmt.Fprintf(out, "\n[humanoids] %d artefact(s) under %s:\n", len(defs), dir)
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 70))
	for _, d := range defs {
		mark := "not loaded"
		if loaded[d.Name] {
			mark = "loaded"
			loadedCount++
		}
		channels := "-"
		if len(d.Contacts) > 0 {
			types := make([]string, 0, len(d.Contacts))
			for ct := range d.Contacts {
				types = append(types, ct)
			}
			sort.Strings(types)
			channels = strings.Join(types, ",")
		}
		desc := d.Description
		if len(desc) > 38 {
			desc = desc[:35] + "..."
		}
		fmt.Fprintf(out, "  %-20s [%-10s] channels: %-18s %s\n", d.Name, mark, channels, desc)
	}
	fmt.Fprintf(out, "%s\n", strings.Repeat("-", 70))
	fmt.Fprintf(out, "  %d of %d loaded in this workspace; spawn an artefact with ##humanoid spawn <name>\n\n",
		loadedCount, len(defs))
}

// createHumanoidFromFile parses a humanoid skill .md file and spawns a humanoid.
func (w *WorkspaceActor) createHumanoidFromFile(out *strings.Builder, paneID, path string) {
	def, err := parseHumanoidFile(path, w.namedExpandFunc(paneID))
	if err != nil {
		fmt.Fprintf(out, "\n[humanoids] failed to parse skill file %q: %v\n", path, err)
		w.failRysh("failed to parse skill file %q: %v", path, err)
		return
	}
	if def.Name == "" {
		fmt.Fprintf(out, "\n[humanoids] skill file %q has no name\n", path)
		return
	}
	if def.SystemPrompt == "" {
		fmt.Fprintf(out, "\n[humanoids] skill file %q has no system prompt\n", path)
		return
	}

	w.actorSystem.Root.Send(w.humanoidRegistryPID, &msg.MsgHumanoidCreate{
		Name:         def.Name,
		SystemPrompt: def.SystemPrompt,
		Contacts:     def.Contacts,
		Provider:     def.Provider,
		Model:        def.Model,
		Profile:      def.Profile,
		AutoApprove:  def.AutoApprove,
	})

	channelCount := len(def.Contacts)
	fmt.Fprintf(out, "\n[humanoids] spawning humanoid %q from %s (%d channel(s))\n",
		def.Name, path, channelCount)
}

// createHumanoidsFromDir spawns humanoids from all .md files in a directory.
func (w *WorkspaceActor) createHumanoidsFromDir(out *strings.Builder, paneID, dirPath string) {
	defs, err := parseHumanoidDir(dirPath, w.namedExpandFunc(paneID))
	if err != nil {
		fmt.Fprintf(out, "\n[humanoids] failed to read directory %q: %v\n", dirPath, err)
		w.failRysh("failed to read directory %q: %v", dirPath, err)
		return
	}
	if len(defs) == 0 {
		fmt.Fprintf(out, "\n[humanoids] no .md files found in %q\n", dirPath)
		return
	}

	for _, def := range defs {
		if def.Name == "" || def.SystemPrompt == "" {
			fmt.Fprintf(out, "\n[humanoids] skipping invalid definition in %q\n", dirPath)
			w.failRysh("skipping invalid definition in %q", dirPath)
			continue
		}
		w.actorSystem.Root.Send(w.humanoidRegistryPID, &msg.MsgHumanoidCreate{
			Name:         def.Name,
			SystemPrompt: def.SystemPrompt,
			Contacts:     def.Contacts,
			Provider:     def.Provider,
			Model:        def.Model,
			Profile:      def.Profile,
			AutoApprove:  def.AutoApprove,
		})
		fmt.Fprintf(out, "\n[humanoids] spawning humanoid %q (%d channel(s))\n",
			def.Name, len(def.Contacts))
	}
	fmt.Fprintf(out, "\n[humanoids] spawned %d humanoid(s) from %s\n", len(defs), dirPath)
}

// humanoidHelp writes humanoid command help text.
func (w *WorkspaceActor) humanoidHelp(out *strings.Builder) {
	ryshWriter(out).UsageIn("humanoids",
		"##humanoid spawn <name>                              load .rysh/humanoids/<name>/SKILL.md",
		"##humanoid spawn <path-to-file.md>                   load explicit skill file",
		"##humanoid spawn <name> <system-prompt>              create humanoid inline",
		"##humanoid spawn-all [directory]                     spawn all (default: .rysh/humanoids)",
		"##humanoid stop <name>                                stop a running humanoid (skill file kept; re-spawn by name)",
		"##humanoid list [all]                                 running + paused + stopped (unspawned skill files) (default)",
		"##humanoid list instances                             only the humanoids spawned in this workspace",
		"##humanoid list artefacts                             only the skill files under .rysh/humanoids",
		"##humanoid show <name>                                print a humanoid's recipe (skill file: frontmatter + prompt)",
		"##humanoid activate <name>                            activate a humanoid",
		"##humanoid deactivate <name>                          deactivate a humanoid",
		"##humanoid register-output <humanoid> <pane>          route output to pane chat",
		"##humanoid unregister-output <humanoid> <pane>        stop routing output to pane",
		"##humanoid channels <name>                            show channel details",
		"##humanoid channel start <name> <type>                start a channel",
		"##humanoid channel stop <name> <type>                 stop a channel",
		"##humanoid reply-to <name> messages                   reply to all messages",
		"##humanoid reply-to <name> mentions                   reply only to @mentions",
		"##humanoid governance <name> ai|human                 ai: auto-reply · human: draft-and-confirm",
		"##humanoid provider <name> <provider> [model]         override the model provider (until restart)",
		"##humanoid pair list <name> [channel]                 show pending pairings + allowlist",
		"##humanoid pair approve <name> <code>                 approve a pending contact",
		"##humanoid pair link <name> <channel> [force]         run a device-link (QR) flow now",
		"##humanoid allow <name> <sender> [channel]            allowlist a sender directly",
		"##humanoid attention enable <name>                   enable attention alerts",
		"##humanoid attention disable <name>                  disable attention alerts",
		"",
		"channel types: whatsapp, slack, email, phone, chatbot",
		"",
		"skill lookup: bare names resolve to <base>/humanoids/<name>/SKILL.md, where",
		"  <base> is ./.rysh if a project .rysh exists, else $HOME/.rysh.",
		"  Use ./, ../, /, ~/ for explicit paths (legacy *.md files supported).",
		"",
		"@humanoid-name <prompt>                               send prompt to humanoid",
		"@@humanoid-name stop                                  interrupt the current run only (@@name continue resumes)",
		"                                                      — to end the humanoid itself use ##humanoid stop <name>",
		"@@humanoid-name activate                              activate humanoid",
		"@@humanoid-name deactivate                            deactivate humanoid",
		"",
	)
}
