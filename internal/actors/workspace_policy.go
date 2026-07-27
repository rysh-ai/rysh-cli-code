package actors

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	sharedagentic "github.com/rysh-ai/rysh-cli-shared/agentic"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
	"github.com/rysh-ai/rysh-cli-code/internal/policy"
)

// loadPolicy loads the project policy and merges the org policy over it
// (design 013 §1, strictest-wins). The single load path for session start and
// ##policy reload, so neither can skip the org file. LoadOrg fails closed on a
// configured-but-missing org file; Merge fails closed on either side's error.
func (w *WorkspaceActor) loadPolicy() *policy.Policy {
	return policy.Merge(policy.LoadOrg(w.cfg.Policy.OrgFile), policy.Load(w.cfg.RyshDir))
}

// handlePolicyCommand implements ##policy (design 013):
//
//	##policy          show the loaded policy (rules, budgets, proxy-required,
//	                  org policy provenance when one is active)
//	##policy reload   re-read .rysh/policy.yaml + the org policy file and
//	                  re-apply (fail-closed)
func (w *WorkspaceActor) handlePolicyCommand(out *strings.Builder, paneID string, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = strings.ToLower(strings.TrimSpace(args[0]))
	}
	switch sub {
	case "reload":
		w.policy = w.loadPolicy()
		w.syncPolicyGate()
		if w.policy.Err != nil {
			fmt.Fprintf(out, "%s", w.policy.Summary())
			return
		}
		w.applyPolicy(out)
		fmt.Fprintf(out, "%s", w.policy.Summary())
		w.renderRequiredSecretStatus(out)
	case "", "show", "status":
		if w.policy == nil {
			w.policy = policy.Empty()
		}
		fmt.Fprintf(out, "%s", w.policy.Summary())
		w.renderRequiredSecretStatus(out)
	default:
		fmt.Fprintf(out, "usage: ##policy [show] | ##policy reload\n")
	}
}

// policyGateBlocked is the shared fail-closed check for every actor that
// dispatches agentic work but cannot see WorkspaceActor state.
//
// It returns true when the caller must refuse. `where` names the dispatch site
// in the log so a refusal is traceable; `notifyPaneID` may be empty for
// headless dispatch (agents, autonomous loops) where there is no pane to write
// to — the refusal is still logged at error level.
func policyGateBlocked(pub *msg.NATSPublisher, notifyPaneID, where string) bool {
	reason, blocked := policy.Blocked()
	if !blocked {
		return false
	}
	slog.Error("policy: governed execution refused (fail-closed)",
		"where", where, "reason", reason)
	if pub != nil && notifyPaneID != "" {
		_ = pub.SendPaneRyshOutput(notifyPaneID, policy.BlockedMessage(reason))
	}
	return true
}

// syncPolicyGate publishes the loaded policy's posture to the process-global
// fail-closed gate, so dispatch sites that cannot see WorkspaceActor state
// (PaneActor, AgentRegistryActor, HumanoidActor — including the network-facing
// remote-exec and inbound-channel paths) refuse governed execution too.
//
// Called on initial load and on every ##policy reload.
func (w *WorkspaceActor) syncPolicyGate() {
	if w.policy != nil && w.policy.Err != nil {
		path := w.policy.Path
		if path == "" {
			path = ".rysh/policy.yaml"
		}
		slog.Error("policy: failed to load — governed execution blocked (fail-closed)",
			"err", w.policy.Err, "path", path)
		// "load", not "parse": a configured-but-missing org file (tamper
		// signal) blocks through the same path as a parse failure.
		policy.SetBlocked(fmt.Sprintf("%s failed to load: %v", path, w.policy.Err))
		// A broken policy applies no rules; the fail-closed gate above refuses
		// execution anyway, so clear any previously-installed approval policy.
		sharedagentic.SetApprovalPolicy(nil)
		return
	}
	// Required-secret presence (design 013 snat.required): fail-closed if any
	// required secret is not registered for redaction, so agents don't run while
	// a credential the operator flagged could leak in the clear. This clears
	// automatically once the secret is registered — afterSecretMutation re-runs
	// this on every ##secret change.
	if missing := w.missingRequiredSecrets(); len(missing) > 0 {
		slog.Error("policy: required secrets not registered — governed execution blocked (fail-closed)",
			"missing", missing)
		policy.SetBlocked("required secret(s) not registered for redaction: " +
			strings.Join(missing, ", ") + " — add with `##secret new <NAME> <VALUE>` (clears automatically)")
		sharedagentic.SetApprovalPolicy(nil)
		return
	}
	policy.ClearBlocked()
	w.installApprovalPolicy()
}

// renderRequiredSecretStatus appends the snat.required presence status to
// ##policy output, so the operator sees which required secrets are still
// missing (and thus blocking) versus registered.
func (w *WorkspaceActor) renderRequiredSecretStatus(out *strings.Builder) {
	if w.policy == nil || len(w.policy.SNATRequired) == 0 {
		return
	}
	if missing := w.missingRequiredSecrets(); len(missing) > 0 {
		fmt.Fprintf(out, "  snat required: BLOCKED — missing %v (add with `##secret new <NAME> <VALUE>`)\n", missing)
	} else {
		fmt.Fprintf(out, "  snat required: all registered ✓\n")
	}
}

// afterSecretMutation re-syncs after a ##secret change: it refreshes the
// SecretNAT known tier and re-evaluates the fail-closed gate, so registering a
// snat.required secret clears its block (and removing one re-imposes it) without
// a ##policy reload.
func (w *WorkspaceActor) afterSecretMutation() {
	w.pushKnownSecrets()
	w.syncPolicyGate()
}

// installApprovalPolicy publishes the loaded policy's bash-allow/deny and
// approval auto-approve/always-gate rules to the process-global approval hook
// (design 013), so the orchestrator's per-tool-call approval decision consumes
// them. Mirrors syncPolicyGate's cross-boundary approach: the orchestrator runs
// in rysh-shared and cannot import this package, so the matching runs here and
// the verdict is injected. Called on initial load and every ##policy reload.
func (w *WorkspaceActor) installApprovalPolicy() {
	if w.policy == nil || !w.policy.Loaded {
		sharedagentic.SetApprovalPolicy(nil)
		return
	}
	pol := w.policy
	sharedagentic.SetApprovalPolicy(func(toolName string, input json.RawMessage) (sharedagentic.PolicyDecision, string) {
		act, id := pol.EvaluateToolApproval(toolName, input)
		switch act {
		case policy.ApprovalAuto:
			return sharedagentic.PolicyAutoApprove, id
		case policy.ApprovalGate:
			return sharedagentic.PolicyGate, id
		default:
			return sharedagentic.PolicyDefault, ""
		}
	})
}

// policyBlocked reports whether governed execution must be refused because the
// policy (project file, or a configured org file) failed to load.
//
// Design 013 §1 makes this non-negotiable: a rule set that does not parse is
// not "no rules", it is an unknown posture, and rysh must not run agents under
// an unknown posture. Only the LLM-executing paths are blocked — shell input
// and ## commands stay live so the operator can repair the file in place and
// run ##policy reload without killing the session.
func (w *WorkspaceActor) policyBlocked() bool {
	return w.policy != nil && w.policy.Err != nil
}

// notifyPolicyBlocked tells the pane why its input was refused.
func (w *WorkspaceActor) notifyPolicyBlocked(paneID string) {
	path := ""
	if w.policy != nil {
		path = w.policy.Path
	}
	if path == "" {
		path = ".rysh/policy.yaml"
	}
	_ = w.pub.SendPaneRyshOutput(paneID, fmt.Sprintf(
		"policy: BLOCKED — %s failed to load, so the governance posture is unknown.\n"+
			"  %v\n"+
			"  Agent and prompt execution are refused (fail-closed, design 013).\n"+
			"  Fix the file, then run: ##policy reload\n",
		path, w.policy.Err))
}

// applyPolicy re-applies the enforceable parts of a (re)loaded policy: budget
// ceilings to the usage ledger and the proxy-required flag. Bash-allow/deny and
// approval auto-approve/always-gate are applied separately in syncPolicyGate →
// installApprovalPolicy (they must also apply on initial load, not just
// ##policy reload).
func (w *WorkspaceActor) applyPolicy(out *strings.Builder) {
	for id, ceiling := range w.policy.BudgetCeilings {
		_ = w.pub.Send(msg.UsageInboxSubject(), &msg.MsgUsageBudgetSet{PaneID: id, CeilingTokens: ceiling})
	}
	if w.policy.ProxyRequired && w.proxyServer == nil {
		w.startProxy()
		if w.proxyServer != nil {
			fmt.Fprintf(out, "policy: proxy required — started at %s\n", w.proxyServer.BaseURL())
		}
	}
}
