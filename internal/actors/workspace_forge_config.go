package actors

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/agentic"
	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/forge"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/forgecmd"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/runtime"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// forgeConfigStartDelay is how long after session start the declarative forge
// config is applied — enough for the initial tabs/panes to exist (scope
// resolution) and the upstream to connect.
const forgeConfigStartDelay = 2 * time.Second

// forgeConfigSubscribeRetries bounds the startup subscribe retry (× the actor's
// forgeSubscribeRetryDelay, ~3s) so a subscriber that starts before the source
// has shared still mounts the API once it appears (~36s window).
const forgeConfigSubscribeRetries = 12

// msgApplyForgeConfig triggers applyForgeConfig on the workspace mailbox.
type msgApplyForgeConfig struct{}

// applyForgeConfig kicks off the declarative `forge:` config. It runs on the
// workspace mailbox only long enough to snapshot the mailbox-only state it needs
// (active pane + resolved scope ids), then hands the actual work — spec ingest,
// tool-pack generation, enable, share/subscribe — to a GOROUTINE so it never
// blocks the workspace actor or slows the session. The goroutine touches only
// the captured values and internally-thread-safe services (forge manager, scope
// registries, publisher, actor system), so it is race-free.
func (w *WorkspaceActor) applyForgeConfig(ctx actor.Context) {
	if w.agSetup == nil || w.agSetup.Forge == nil || w.agSetup.Scopes == nil {
		return
	}
	fc := w.cfg.Forge
	if len(fc.Integrations) == 0 && len(fc.Subscribe) == 0 {
		return
	}
	paneID := w.activePaneID
	job := &forgeConfigJob{
		fc:       fc,
		workDir:  w.agSetup.Forge.WorkDir(),
		paneID:   paneID,
		ids:      w.resolveScopeIDs(paneID), // brief snapshot query; resolved on the mailbox (race-free)
		forge:    w.agSetup.Forge,
		scopes:   w.agSetup.Scopes,
		pub:      w.pub,
		system:   w.system(),
		forgePID: w.forgeSharePID,
	}
	go job.run()
}

// forgeConfigJob is an off-mailbox snapshot of everything applyForgeConfig needs.
// All fields are either immutable captured values or internally-thread-safe
// services, so run() is safe to execute in a goroutine.
type forgeConfigJob struct {
	fc       config.ForgeConfig
	workDir  string
	paneID   string
	ids      agentic.ScopeIDs
	forge    *forge.Manager
	scopes   *agentic.ScopeRegistries
	pub      *msg.NATSPublisher
	system   *actor.ActorSystem
	forgePID *actor.PID
}

func (j *forgeConfigJob) out(s string) {
	if j.paneID == "" || j.pub == nil {
		return
	}
	_ = j.pub.SendPaneOutput(j.paneID, s)
	_ = j.pub.SendPaneRyshOutput(j.paneID, s)
}

// run performs the (potentially slow) forge-config work off the workspace
// mailbox. Idempotent: an integration already in the store is not re-added; an
// already-subscribed API is skipped by the ForgeShareActor.
func (j *forgeConfigJob) run() {
	for _, ig := range j.fc.Integrations {
		if strings.TrimSpace(ig.Name) == "" {
			continue
		}
		// add to the store if not already present (idempotent across restarts).
		defs, _ := forge.LoadStore(j.workDir)
		inStore := false
		for _, d := range defs {
			if d.Name == ig.Name {
				inStore = true
				break
			}
		}
		if !inStore {
			if strings.TrimSpace(ig.Spec) == "" {
				j.out(fmt.Sprintf("[forge] config: %q has no spec — skipping\n", ig.Name))
				continue
			}
			args := []string{"add", ig.Name, config.ExpandHome(ig.Spec), "--targets", "rysh-toolpack"}
			if ig.BaseURL != "" {
				args = append(args, "--base-url", ig.BaseURL)
			}
			if ig.Source != "" {
				args = append(args, "--source", ig.Source)
			}
			if ig.CredentialEnv != "" {
				args = append(args, "--cred-env", ig.CredentialEnv)
			}
			args = appendForgeAuthArgs(args, ig.Auth)
			var buf strings.Builder
			if err := forgecmd.Run(j.workDir, args, &buf); err != nil {
				j.out(fmt.Sprintf("[forge] config: add %q failed: %v\n", ig.Name, err))
				slog.Warn("forge-config: add failed", "name", ig.Name, "err", err)
				continue
			}
			j.out(fmt.Sprintf("[forge] config: added integration %q\n", ig.Name))
		}

		// enable at the configured scope.
		if strings.TrimSpace(ig.EnableScope) != "" {
			kind, ok := agentic.ParseScope(ig.EnableScope)
			if !ok {
				kind = agentic.ScopeTab
			}
			target := forge.ScopeTarget{Key: agentic.ScopeKey(kind, j.ids), Registry: j.scopes.RegistryFor(kind, j.ids)}
			n, _, err := j.forge.EnableByName(context.Background(), ig.Name, target)
			if err != nil {
				j.out(fmt.Sprintf("[forge] config: enable %q failed: %v\n", ig.Name, err))
				continue
			}
			j.out(fmt.Sprintf("[forge] config: enabled %q at scope %s (%d tool(s))\n", ig.Name, kind.String(), n))
		}

		// share via the upstream.
		if ig.Share {
			if j.forgePID == nil {
				j.out(fmt.Sprintf("[forge] config: cannot share %q — upstream not enabled\n", ig.Name))
				continue
			}
			j.system.Root.Send(j.forgePID, &msgForgeShareAPI{Name: ig.Name, PaneID: j.paneID})
		}
	}

	for _, sub := range j.fc.Subscribe {
		if strings.TrimSpace(sub.API) == "" {
			continue
		}
		if j.forgePID == nil {
			j.out(fmt.Sprintf("[forge] config: cannot subscribe %q — upstream not enabled\n", sub.API))
			continue
		}
		kind, ok := agentic.ParseScope(sub.Scope)
		if !ok {
			kind = agentic.ScopeTab
		}
		j.system.Root.Send(j.forgePID, &msgForgeSubscribe{
			Name: sub.API, Kind: kind, IDs: j.ids, PaneID: j.paneID, RetriesLeft: forgeConfigSubscribeRetries,
			Auth: toRuntimeAuth(sub.Auth),
		})
	}
}

// appendForgeAuthArgs maps a YAML auth block to `##forge add` flags (Phase A,
// owner identity). No-op when ac is nil or declares no token flow.
func appendForgeAuthArgs(args []string, ac *config.ForgeAuthConfig) []string {
	if ac == nil || strings.TrimSpace(ac.Type) == "" || ac.Type == "none" {
		return args
	}
	add := func(flag, val string) {
		if val != "" {
			args = append(args, flag, val)
		}
	}
	add("--auth", ac.Type)
	add("--token-url", ac.TokenURL)
	add("--refresh-url", ac.RefreshURL)
	add("--auth-content-type", ac.ContentType)
	add("--user-env", ac.UsernameEnv)
	add("--pass-env", ac.PasswordEnv)
	add("--client-id-env", ac.ClientIDEnv)
	add("--client-secret-env", ac.ClientSecretEnv)
	add("--access-token-env", ac.AccessTokenEnv)
	add("--refresh-token-env", ac.RefreshTokenEnv)
	add("--scope", ac.Scope)
	add("--access-token-field", ac.AccessField)
	add("--refresh-token-field", ac.RefreshField)
	add("--expires-in-field", ac.ExpiresInField)
	add("--auth-header", ac.Header)
	add("--auth-scheme", ac.Scheme)
	add("--expiry-margin", ac.ExpiryMargin)
	return args
}

// toRuntimeAuth maps the YAML auth block to a runtime.AuthConfig (Phase B,
// subscriber identity). Returns nil when no token flow is declared.
func toRuntimeAuth(ac *config.ForgeAuthConfig) *runtime.AuthConfig {
	if ac == nil || strings.TrimSpace(ac.Type) == "" || ac.Type == "none" {
		return nil
	}
	return &runtime.AuthConfig{
		Type:              ac.Type,
		TokenURL:          ac.TokenURL,
		RefreshURL:        ac.RefreshURL,
		ContentType:       ac.ContentType,
		UsernameEnv:       ac.UsernameEnv,
		PasswordEnv:       ac.PasswordEnv,
		ClientIDEnv:       ac.ClientIDEnv,
		ClientSecretEnv:   ac.ClientSecretEnv,
		AccessTokenEnv:    ac.AccessTokenEnv,
		RefreshTokenEnv:   ac.RefreshTokenEnv,
		Scope:             ac.Scope,
		AccessTokenField:  ac.AccessField,
		RefreshTokenField: ac.RefreshField,
		ExpiresInField:    ac.ExpiresInField,
		Header:            ac.Header,
		Scheme:            ac.Scheme,
		ExpiryMargin:      ac.ExpiryMargin,
	}
}
