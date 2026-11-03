// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/memory"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// ##memory — cross-session memory (design 010 §3a). Entries live in
// <RyshDir>/memory/entries, one file each; the user lists, shows, adds and
// forgets them here or edits the files directly. Writes go through the
// memory package's credential scrubber and the policy's memory: rules, which
// fail closed.

func (w *WorkspaceActor) memoryStore() *memory.Store {
	if w.cfg.RyshDir == "" {
		return nil
	}
	return memory.Open(w.cfg.RyshDir)
}

// memoryRules is the memory policy in force for this workspace.
func (w *WorkspaceActor) memoryRules() policy.MemoryRules {
	if w.policy == nil {
		return policy.MemoryRules{Reason: "the policy has not loaded yet"}
	}
	return w.policy.MemoryRules()
}

// knownSecretValues returns the workspace's registered secret values for the
// scrubber. In-process only: never printed, logged or compared outside it.
func (w *WorkspaceActor) knownSecretValues() []string {
	scope, _ := w.secretWorkspaceScope()
	return knownSecretValuesIn(w.secrets, scope)
}

func knownSecretValuesIn(store *namedStore, scope string) []string {
	if store == nil {
		return nil
	}
	var out []string
	for _, info := range store.List(scope) {
		if v, _, ok := store.Get(scope, info.Name); ok && v != "" {
			out = append(out, v)
		}
	}
	return out
}

func (w *WorkspaceActor) handleMemoryCommand(out *strings.Builder, paneID string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = strings.ToLower(strings.TrimSpace(args[0]))
	}
	store := w.memoryStore()
	if store == nil {
		fmt.Fprintf(out, "memory: no rysh directory in this session\n")
		w.failRysh("memory: no rysh directory")
		return
	}
	rules := w.memoryRules()
	known := w.knownSecretValues()
	switch sub {
	case "", "list", "ls":
		w.memoryList(out, store, rules, known)
	case "show":
		if len(args) < 2 {
			w.memoryUsage(out, "##memory show needs an id")
			return
		}
		e, err := store.Get(args[1], known)
		if err != nil {
			fmt.Fprintf(out, "memory: %v\n", err)
			w.failRysh("memory show: %v", err)
			return
		}
		fmt.Fprintf(out, "%s  (%s, %s, %s)\n", e.ID, e.Kind, e.Source, e.Created.Format("2006-01-02 15:04"))
		if e.Withheld != "" {
			fmt.Fprintf(out, "  withheld: %s — this entry looks like it holds a credential, so it is never shown or injected. Remove it with ##memory forget %s.\n", e.Withheld, e.ID)
			return
		}
		fmt.Fprintf(out, "%s\n", e.Text)
	case "forget", "rm", "delete":
		if len(args) < 2 {
			w.memoryUsage(out, "##memory forget needs an id or --all")
			return
		}
		if args[1] == "--all" {
			n, err := store.ForgetAll()
			fmt.Fprintf(out, "memory: forgot %d memor%s\n", n, map[bool]string{true: "y", false: "ies"}[n == 1])
			if err != nil {
				fmt.Fprintf(out, "memory: some entries could not be removed: %v\n", err)
				w.failRysh("memory forget --all: %v", err)
			}
			w.afterMemoryChange()
			return
		}
		if err := store.Forget(args[1]); err != nil {
			fmt.Fprintf(out, "memory: %v\n", err)
			w.failRysh("memory forget: %v", err)
			return
		}
		fmt.Fprintf(out, "memory: forgot %s\n", args[1])
		w.afterMemoryChange()
	case "add", "remember":
		kind := memory.KindNote
		rest := args[1:]
		if len(rest) >= 2 && rest[0] == "--kind" {
			kind = memory.Kind(strings.ToLower(rest[1]))
			rest = rest[2:]
		}
		text := strings.Join(rest, " ")
		if strings.TrimSpace(text) == "" {
			w.memoryUsage(out, "##memory add needs the text to remember")
			return
		}
		if !rules.Enabled {
			fmt.Fprintf(out, "memory: refused — memory is off: %s\n", memoryOffReason(rules))
			w.failRysh("memory add refused: memory is off")
			return
		}
		// rysh cannot tell who is behind input nobody typed (rysh_command,
		// `rysh exec`, a cron job), so only typed input is source: user.
		source := memory.SourceAgent
		if w.inputTyped {
			source = memory.SourceUser
		}
		e, err := store.Add(kind, text, source, known)
		if err != nil {
			if errors.Is(err, memory.ErrCredentialShaped) {
				fmt.Fprintf(out, "memory: %v\n", err)
			} else {
				fmt.Fprintf(out, "memory: not remembered: %v\n", err)
			}
			w.failRysh("memory add: not remembered")
			return
		}
		fmt.Fprintf(out, "memory: remembered %s [%s]\n", e.Kind, e.ID)
		if !rules.Inject {
			fmt.Fprintf(out, "memory: note — policy has memory.inject: false, so agents do not see it\n")
		}
		w.afterMemoryChange()
	case "path":
		fmt.Fprintf(out, "%s\n", store.Dir())
	case "help":
		w.memoryHelp(out)
	default:
		w.memoryUsage(out, fmt.Sprintf("unknown ##memory subcommand: %q", sub))
	}
}

func (w *WorkspaceActor) memoryList(out *strings.Builder, store *memory.Store, rules policy.MemoryRules, known []string) {
	list, err := store.List(known)
	if err != nil {
		fmt.Fprintf(out, "memory: could not read %s: %v\n", store.Dir(), err)
		w.failRysh("memory list: %v", err)
		return
	}
	switch {
	case !rules.Enabled:
		fmt.Fprintf(out, "memory: OFF — %s (entries are kept but not injected; no new ones)\n", memoryOffReason(rules))
	case !rules.Inject:
		fmt.Fprintf(out, "memory: on, injection OFF by policy (memory.inject: false)\n")
	default:
		fmt.Fprintf(out, "memory: on — injected into rysh agents, up to %d bytes\n", rules.MaxInjectBytes)
	}
	if len(list) == 0 {
		fmt.Fprintf(out, "  (nothing remembered yet — ##memory add <text>, or files in %s)\n", store.Dir())
		return
	}
	for _, e := range list {
		if e.Withheld != "" {
			fmt.Fprintf(out, "  %-40s %-10s withheld: %s\n", e.ID, e.Kind, e.Withheld)
			continue
		}
		first, _, _ := strings.Cut(e.Text, "\n")
		if len(first) > 80 {
			first = first[:77] + "..."
		}
		fmt.Fprintf(out, "  %-40s %-10s %s\n", e.ID, e.Kind, first)
	}
}

func memoryOffReason(r policy.MemoryRules) string {
	if r.Reason != "" {
		return r.Reason
	}
	return "disabled by policy (memory.enabled: false)"
}

func (w *WorkspaceActor) memoryUsage(out *strings.Builder, why string) {
	fmt.Fprintf(out, "%s\n", why)
	w.memoryHelp(out)
	w.failRyshUsage("%s", why)
}

func (w *WorkspaceActor) memoryHelp(out *strings.Builder) {
	fmt.Fprintf(out, "usage: ##memory [list] | show <id> | add [--kind fact|preference|note] <text> | forget <id>|--all | path\n")
}

// afterMemoryChange rebuilds the agents' system prompt and broadcasts it, so
// an add or forget applies on the next prompt.
func (w *WorkspaceActor) afterMemoryChange() {
	if w.agSetup == nil {
		return
	}
	w.broadcastIfPromptChanged(w.agSetup.RebuildMemory)
}

// wireMemoryScrubber installs the scrubber's registered-secret lookup on the
// agent setup and rebuilds the prompt with it. NewWorkspaceActor calls it, so
// the first prompt any pane gets is already scrubbed; it is installed once,
// and the scope is fixed at install like the store it reads.
func (w *WorkspaceActor) wireMemoryScrubber() {
	if w.agSetup == nil || w.agSetup.KnownSecrets != nil {
		return
	}
	scope, _ := w.secretWorkspaceScope()
	store := w.secrets
	w.agSetup.KnownSecrets = func() []string { return knownSecretValuesIn(store, scope) }
	w.agSetup.RebuildMemory()
}

// syncMemoryPolicy hands the freshly loaded memory rules to the agent setup.
// Called on every policy load and every ##secret change (syncPolicyGate), so
// it broadcasts only when the prompt actually changed.
func (w *WorkspaceActor) syncMemoryPolicy() {
	if w.agSetup == nil {
		return
	}
	w.wireMemoryScrubber()
	rules := w.memoryRules()
	w.broadcastIfPromptChanged(func() { w.agSetup.SetMemoryRules(rules) })
}

// broadcastIfPromptChanged runs rebuild and broadcasts the system prompt only
// when it changed: the broadcast queries every tab, which is not free.
func (w *WorkspaceActor) broadcastIfPromptChanged(rebuild func()) {
	before, beforeNoEnv := w.agSetup.SystemPrompt, w.agSetup.SystemPromptNoEnv
	rebuild()
	if w.agSetup.SystemPrompt != before || w.agSetup.SystemPromptNoEnv != beforeNoEnv {
		w.broadcastSystemPrompt()
	}
}

// broadcastSystemPrompt sends the current system prompt to every pane's
// LLM-execution actor, the message ##agent reload-prompts uses.
func (w *WorkspaceActor) broadcastSystemPrompt() {
	if w.agSetup == nil || w.pub == nil {
		return
	}
	var email string
	if w.agSetup.Prompts != nil {
		email = w.agSetup.Prompts.EmailGovernance
	}
	// Panes start from the env-block-free prompt and add a live env block per
	// turn (Setup.SystemPromptNoEnv); send that one so the block is not doubled.
	prompt := w.agSetup.SystemPromptNoEnv
	if prompt == "" {
		prompt = w.agSetup.SystemPrompt
	}
	for _, pid := range w.collectAllPaneIDs() {
		_ = w.pub.Send(
			msg.T("pane", pid, "llm_prompt_execution", "inbox"),
			&msg.MsgReloadPrompts{SystemPrompt: prompt, EmailGovernance: email},
		)
	}
}
