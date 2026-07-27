package actors

// SecretNAT / ReSet (Reversible Secret Translation) — workspace-side wiring.
//
// This file owns two things:
//
//  1. the known-secret bridge: pushing the ##secret store's (name, value)
//     pairs into the process-wide secretnat.Manager so registered secrets
//     translate to stable "${NAME}" tokens (restorable across restarts);
//
//  2. the ##snat command surface (alias: ##rst) — status / on / off /
//     reset / mode / list / help.
//
// The mapping table itself lives inside the Manager (rysh-shared/secretnat),
// strictly in memory: it is never persisted to pane KV, workspace KV, or
// disk, and ##snat list prints tokens and detector names only — never values.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rysh-ai/rysh-cli-shared/secretnat"

	"github.com/rysh-ai/rysh-cli-code/internal/provider"
)

// snatManager returns the process-wide SecretNAT manager, or nil when the
// agentic setup is unavailable (e.g. some tests).
func (w *WorkspaceActor) snatManager() *secretnat.Manager {
	if w.agSetup == nil {
		return nil
	}
	return w.agSetup.SecretNAT
}

// snatSimpleProvider wraps a simple single-turn provider (judge/loop seats)
// with SecretNAT outbound sanitization. Single-turn callers run no tools, so
// there is nothing to restore on the way back. Identity when SecretNAT is
// unavailable.
func (w *WorkspaceActor) snatSimpleProvider(p provider.Provider) provider.Provider {
	mgr := w.snatManager()
	if mgr == nil {
		return p
	}
	sess := mgr.Session("workspace-judge")
	return provider.NewSanitized(p, sess.Sanitize)
}

// snatCloseSession drops a closed pane's SecretNAT mapping table so its real
// values become unreachable immediately.
func (w *WorkspaceActor) snatCloseSession(paneID string) {
	if mgr := w.snatManager(); mgr != nil {
		mgr.CloseSession(paneID)
	}
}

// pushKnownSecrets snapshots every enumerable secret visible in this
// workspace — each tab scope (most specific first), then the workspace scope
// — and swaps it into the SecretNAT manager as the known tier. Called from
// the workspace Started handler and after every ##secret mutation; it runs
// inside the actor's Receive, so no actor state escapes to other goroutines.
// The environment tier is not enumerable and is intentionally excluded (use
// ##secret new to register a value for translation).
func (w *WorkspaceActor) pushKnownSecrets() {
	mgr := w.snatManager()
	if mgr == nil {
		return
	}
	mgr.UpdateKnownSecrets(w.enumerableSecrets())
}

// enumerableSecrets returns every registrable secret visible in this workspace
// — each tab scope (most specific first), then the workspace scope — deduped so
// the most-specific scope wins, and excluding empty values. This is exactly the
// set pushKnownSecrets loads into the SecretNAT known tier, so the SNATRequired
// presence check (missingRequiredSecrets) uses the same definition of
// "registered for redaction". The environment tier is not enumerable and is
// intentionally excluded (register with ##secret new).
func (w *WorkspaceActor) enumerableSecrets() []secretnat.KnownSecret {
	if w.secrets == nil {
		return nil
	}
	wsScope, _ := w.secretWorkspaceScope()
	scopes := make([]string, 0, len(w.tabs)+1)
	for _, t := range w.tabs {
		if t != nil {
			scopes = append(scopes, sanitizeScope(t.id))
		}
	}
	scopes = append(scopes, wsScope)

	var known []secretnat.KnownSecret
	seen := map[string]bool{}
	for _, scope := range scopes {
		for _, info := range w.secrets.List(scope) {
			if seen[info.Name] {
				continue // most-specific scope wins, matching skill resolution
			}
			if v, _, ok := w.secrets.Get(scope, info.Name); ok && v != "" {
				known = append(known, secretnat.KnownSecret{Name: info.Name, Value: v})
				seen[info.Name] = true
			}
		}
	}
	return known
}

// missingRequiredSecrets returns the names in the policy's snat.required list
// (design 013) that are NOT registered for redaction — i.e. absent from the
// SecretNAT known tier. Sorted, nil when the policy requires none. Governed
// execution is blocked while this is non-empty (fail-closed): a secret the
// operator flagged as required must be redacted before agents run, or it could
// leak in the clear.
func (w *WorkspaceActor) missingRequiredSecrets() []string {
	if w.policy == nil || len(w.policy.SNATRequired) == 0 {
		return nil
	}
	present := map[string]bool{}
	for _, s := range w.enumerableSecrets() {
		present[s.Name] = true
	}
	var missing []string
	for _, name := range w.policy.SNATRequired {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// handleSnatSubcommand implements ##snat / ##rst:
//
//	##snat status                 show state, mode, and value-free counters
//	##snat on|off                 per-pane override for the ACTIVE pane
//	##snat on|off --session       flip the session-wide default
//	##snat reset                  clear the active pane's override
//	##snat mode semantic|private  token style for newly minted tokens
//	##snat list                   this pane's mappings (tokens + detector + hits)
//	##snat get <token>            reveal ONE token's real value locally (never sent to the model)
//	##snat help                   usage
func (w *WorkspaceActor) handleSnatSubcommand(out *strings.Builder, paneID string, parts []string) {
	mgr := w.snatManager()
	if mgr == nil {
		fmt.Fprintf(out, "\n[snat] SecretNAT is unavailable (agentic setup not initialized)\n")
		return
	}
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "on", "off":
		enable := sub == "on"
		if snatHasFlag(parts[1:], "--session") {
			mgr.SetEnabled(enable)
			fmt.Fprintf(out, "\n[snat] session-wide default: %s\n", snatOnOff(enable))
			return
		}
		v := enable
		mgr.Session(paneID).SetOverride(&v)
		fmt.Fprintf(out, "\n[snat] this pane: %s  (session default: %s — ##snat reset clears the override)\n",
			snatOnOff(enable), snatOnOff(mgr.Enabled()))

	case "reset":
		mgr.Session(paneID).SetOverride(nil)
		fmt.Fprintf(out, "\n[snat] pane override cleared — following session default (%s)\n", snatOnOff(mgr.Enabled()))

	case "mode":
		if len(parts) < 2 {
			fmt.Fprintf(out, "\n[snat] usage: ##snat mode semantic|private   (current: %s)\n", mgr.Mode())
			return
		}
		mode := secretnat.ParseMode(parts[1])
		mgr.SetMode(mode)
		fmt.Fprintf(out, "\n[snat] token mode: %s (applies to newly minted tokens)\n", mode)

	case "list", "ls":
		sess := mgr.Session(paneID)
		st := sess.Stats()
		fmt.Fprintf(out, "\n[snat] this pane's translations (%d mappings — tokens only; ##snat get <token> reveals one locally)\n", st.Mappings)
		fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
		if len(st.Entries) == 0 {
			fmt.Fprintf(out, "  (none yet — mappings appear when a secret is translated)\n")
		} else {
			for _, e := range st.Entries {
				fmt.Fprintf(out, "  %-40s  %-10s  hits:%d\n", e.Token, e.Detector, e.Hits)
			}
		}
		fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
		fmt.Fprintf(out, "  known-tier secrets (##secret store) translate to ${NAME} and are not listed here\n")

	case "get", "show", "reveal":
		if len(parts) < 2 {
			fmt.Fprintf(out, "\n[snat] usage: ##snat get <token>   (e.g. sk_live_SNAT000001 or ${NAME})\n")
			return
		}
		token := parts[1]
		sess := mgr.Session(paneID)
		v, src, ok := sess.Reveal(token)
		if !ok {
			fmt.Fprintf(out, "\n[snat] no mapping for %q in this pane (see ##snat list)\n", token)
			return
		}
		// Explicit, local-only reveal: the real value is printed to the
		// owner's own pane and was never placed on the outbound path.
		fmt.Fprintf(out, "\n[snat] %s\n         =  %s\n         [%s-tier · revealed locally — the model only ever saw the token]\n", token, v, src)

	case "status", "":
		ms := mgr.Stats()
		sess := mgr.Session(paneID)
		st := sess.Stats()
		fmt.Fprintf(out, "\n[snat] SecretNAT / ReSet — reversible secret translation\n")
		fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
		fmt.Fprintf(out, "  session default : %s\n", snatOnOff(ms.Enabled))
		paneState := "(follows default)"
		if st.Override != nil {
			paneState = snatOnOff(*st.Override) + " (pane override)"
		}
		fmt.Fprintf(out, "  this pane       : %s\n", paneState)
		fmt.Fprintf(out, "  mode            : %s\n", ms.Mode)
		fmt.Fprintf(out, "  restore_display : %v\n", ms.RestoreDisplay)
		fmt.Fprintf(out, "  known secrets   : %d (from ##secret store)\n", ms.KnownSecrets)
		fmt.Fprintf(out, "  sessions        : %d   detected: %d   restored: %d\n", ms.Sessions, ms.Detected, ms.Restored)
		if len(ms.PerDetector) > 0 {
			names := make([]string, 0, len(ms.PerDetector))
			for k := range ms.PerDetector {
				names = append(names, k)
			}
			sort.Strings(names)
			fmt.Fprintf(out, "  detector hits   :")
			for _, k := range names {
				fmt.Fprintf(out, " %s:%d", k, ms.PerDetector[k])
			}
			fmt.Fprintf(out, "\n")
		}
		fmt.Fprintf(out, "  detectors       : %s\n", strings.Join(mgr.DetectorNames(), ", "))
		fmt.Fprintf(out, "%s\n", strings.Repeat("-", 60))
		fmt.Fprintf(out, "  the LLM provider only ever sees synthetic tokens; real values are\n")
		fmt.Fprintf(out, "  restored locally at tool execution. mappings live in memory only.\n")

	case "help":
		snatHelp(out)

	default:
		fmt.Fprintf(out, "\n[snat] unknown subcommand: %q\n", sub)
		snatHelp(out)
	}
}

// snatHelp prints the ##snat usage block.
func snatHelp(out *strings.Builder) {
	fmt.Fprintf(out, "\n[snat] SecretNAT / ReSet — the LLM provider never sees your real secrets\n")
	fmt.Fprintf(out, "  ##snat status                 state, mode, and value-free counters\n")
	fmt.Fprintf(out, "  ##snat on|off                 override for the active pane\n")
	fmt.Fprintf(out, "  ##snat on|off --session       flip the session-wide default\n")
	fmt.Fprintf(out, "  ##snat reset                  clear the active pane's override\n")
	fmt.Fprintf(out, "  ##snat mode semantic|private  semantic keeps the credential type (sk_live_SNAT000001);\n")
	fmt.Fprintf(out, "                                private hides it (SECRET_TOKEN_001)\n")
	fmt.Fprintf(out, "  ##snat list                   this pane's mappings (tokens + detector + hits — never values)\n")
	fmt.Fprintf(out, "  ##snat get <token>            reveal ONE token's real value locally (never sent to the model)\n")
	fmt.Fprintf(out, "  ##rst ...                     alias of ##snat (ReSet: Reversible Secret Translation)\n")
	fmt.Fprintf(out, "  config: snat section (enabled, mode, restore_display, disable_detectors, custom_detectors)\n")
}

func snatOnOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// snatHasFlag reports whether args contains the given flag token.
func snatHasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}
