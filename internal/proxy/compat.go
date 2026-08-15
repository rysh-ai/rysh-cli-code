// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Ungoverned-CLI detection and per-CLI adapters (design 022 §4.4).
//
// Design 001 §2 listed "CLIs that ignore base-URL env vars" as a non-goal and
// §7 promised an unproxied warning that was never built. The gap matters
// because the failure is SILENT: the pane keeps working, the CLI talks to the
// provider directly, and nothing in rysh says the traffic escaped. A user
// reasonably reads "governance proxy: ON" as "this pane is governed".
//
// Two halves:
//
//  1. DETECT. When a known agent CLI is the pane's foreground process and no
//     governed request arrives within a grace window, warn once. This is
//     NEGATIVE evidence and is treated as such: an idle CLI is indistinguishable
//     from an escaped one, so it warns and never blocks.
//
//  2. CLOSE. Env injection alone does not govern every CLI, and we already knew
//     it: the wire harness has to pin a model_provider and a scratch CODEX_HOME
//     to stop a ChatGPT-authenticated machine falling back to ungoverned egress.
//     That knowledge lived only in cmd/wire-harness. It lives here now, so the
//     harness, `##proxy check` and pane spawn all read it from one place and
//     cannot drift apart.

// CLIProfile is what rysh knows about one third-party agent CLI.
type CLIProfile struct {
	// Binary is the executable name as it appears in the process table.
	Binary string
	// Dialect is which proxy dialect this CLI's traffic speaks.
	Dialect string
	// BaseURLEnv is the environment variable it reads its endpoint from.
	BaseURLEnv string
	// KeyEnv is the environment variable it reads its credential from.
	KeyEnv string
	// EnvIsEnough reports whether exporting BaseURLEnv actually governs this
	// CLI. False means the CLI needs an adapter (see AdaptSpawn) and that
	// base-URL injection alone will silently leave it ungoverned.
	EnvIsEnough bool
	// Note explains a false EnvIsEnough, and is what the pane warning says.
	Note string

	// probeArgs builds the argv for a one-shot governed probe (`##proxy check`
	// and the wire harness). scratch is a caller-owned temp dir.
	probeArgs func(bin, baseURL, prompt, scratch string) []string
	// adaptEnv returns the extra environment that forces this CLI through
	// baseURL. dir is a rysh-owned directory the adapter may populate.
	adaptEnv func(baseURL, dir string) ([]string, error)
}

// profiles is the known-CLI table, keyed by binary name.
var profiles = map[string]CLIProfile{
	"claude": {
		Binary: "claude", Dialect: "anthropic",
		BaseURLEnv: "ANTHROPIC_BASE_URL", KeyEnv: "ANTHROPIC_API_KEY",
		EnvIsEnough: true,
		probeArgs: func(bin, _, prompt, _ string) []string {
			return []string{bin, "--print", prompt}
		},
	},
	"codex": {
		Binary: "codex", Dialect: "openai",
		BaseURLEnv: "OPENAI_BASE_URL", KeyEnv: "OPENAI_API_KEY",
		EnvIsEnough: false,
		Note: "codex can fall back to ChatGPT auth and egress directly; " +
			"OPENAI_BASE_URL alone does not govern it",
		probeArgs: func(bin, baseURL, prompt, scratch string) []string {
			// Pinning a custom provider AT the proxy URL (rather than only
			// exporting OPENAI_BASE_URL) is what guarantees no fallback egress
			// on a ChatGPT-authenticated machine.
			return []string{bin, "exec",
				"--skip-git-repo-check",
				"-c", "model_provider=rysh",
				"-c", "model_providers.rysh.name=rysh governance proxy",
				"-c", "model_providers.rysh.base_url=" + baseURL + "/v1",
				"-c", "model_providers.rysh.env_key=OPENAI_API_KEY",
				"-c", "model=gpt-4o",
				prompt}
		},
		adaptEnv: codexAdaptEnv,
	},
	"aider": {
		Binary: "aider", Dialect: "openai",
		BaseURLEnv: "OPENAI_BASE_URL", KeyEnv: "OPENAI_API_KEY",
		EnvIsEnough: true,
		probeArgs: func(bin, _, prompt, _ string) []string {
			return []string{bin, "--no-git", "--yes", "--message", prompt}
		},
	},
	"openclaw": {
		Binary: "openclaw", Dialect: "anthropic",
		BaseURLEnv: "ANTHROPIC_BASE_URL", KeyEnv: "ANTHROPIC_API_KEY",
		EnvIsEnough: true,
		probeArgs: func(bin, _, prompt, _ string) []string {
			return []string{bin, "--print", prompt}
		},
	},
	"gemini": {
		Binary: "gemini", Dialect: "gemini",
		BaseURLEnv: "GEMINI_BASE_URL", KeyEnv: "GEMINI_API_KEY",
		EnvIsEnough: true,
		probeArgs: func(bin, _, prompt, _ string) []string {
			return []string{bin, "-p", prompt}
		},
	},
}

// ProfileFor returns what rysh knows about a binary. The lookup tolerates a
// path and a .exe suffix, because a process table reports whatever was exec'd.
func ProfileFor(binary string) (CLIProfile, bool) {
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(strings.TrimSpace(binary))), ".exe")
	p, ok := profiles[name]
	return p, ok
}

// KnownCLIs lists the binaries rysh recognises, sorted for stable output.
func KnownCLIs() []string {
	out := make([]string, 0, len(profiles))
	for name := range profiles {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ProbeCommand builds the argv and environment for a one-shot run of this CLI
// against baseURL. Used by `##proxy check` and by cmd/wire-harness, so the
// answer to "how do you force this CLI through a base URL" has ONE home.
//
// dummyKey is what lands in the CLI's credential variable: the probe must never
// need a working key, since the point is to observe the wire, not to complete.
func (p CLIProfile) ProbeCommand(bin, baseURL, prompt, scratch, dummyKey string) ([]string, []string, error) {
	argv := p.probeArgs(bin, baseURL, prompt, scratch)
	env := []string{
		p.BaseURLEnv + "=" + baseURL,
		p.KeyEnv + "=" + dummyKey,
	}
	if p.adaptEnv != nil {
		extra, err := p.adaptEnv(baseURL, scratch)
		if err != nil {
			return nil, nil, err
		}
		env = append(env, extra...)
	}
	return argv, env, nil
}

// AdaptSpawn returns the extra environment that forces this CLI through baseURL
// at pane-spawn time, where rysh controls the environment but NOT the argv.
//
// dir is a rysh-owned directory the adapter may write into. Returns nil when
// the CLI needs no adaptation.
func (p CLIProfile) AdaptSpawn(baseURL, dir string) ([]string, error) {
	if p.adaptEnv == nil {
		return nil, nil
	}
	return p.adaptEnv(baseURL, dir)
}

// codexAdaptEnv points CODEX_HOME at a rysh-owned directory holding a config
// that pins the provider at the proxy.
//
// The directory deliberately contains NO auth.json. That is the whole
// mechanism: with a ChatGPT session available, codex can egress directly and
// the pane is silently ungoverned — which is exactly what the wire harness had
// to defeat. A governed pane therefore uses the API-key path through the proxy
// and does not use an existing ChatGPT login. That is a real behavioural change,
// which is why the adapter is opt-in ([proxy] adapters) rather than automatic.
func codexAdaptEnv(baseURL, dir string) ([]string, error) {
	if dir == "" {
		return nil, fmt.Errorf("codex adapter: no directory to write config into")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("codex adapter: %w", err)
	}
	cfg := "" +
		"# Written by rysh (design 022 §4.4). Points codex at the governance proxy.\n" +
		"# This home deliberately carries no auth.json: a ChatGPT session would let\n" +
		"# codex egress directly, leaving the pane silently ungoverned.\n" +
		"model_provider = \"rysh\"\n\n" +
		"[model_providers.rysh]\n" +
		"name = \"rysh governance proxy\"\n" +
		"base_url = \"" + baseURL + "/v1\"\n" +
		"env_key = \"OPENAI_API_KEY\"\n"
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(cfg), 0o600); err != nil {
		return nil, fmt.Errorf("codex adapter: %w", err)
	}
	return []string{"CODEX_HOME=" + dir}, nil
}

// ---- governance observation ----

// GraceWindow is how long a known CLI may run without a governed request before
// rysh says so. Long enough that a cold start is not accused, short enough that
// an escaped CLI is reported while the user is still watching.
const GraceWindow = 20 * time.Second

// govWatch tracks, per pane, when a known CLI came to the foreground and
// whether a governed request has been seen since.
//
// The mutex guards only this map: same documented data-plane exception as the
// audit ring and the budget cache.
type govWatch struct {
	mu      sync.Mutex
	started map[string]watchEntry // paneID → current observation
	seen    map[string]time.Time  // paneID → last governed request
}

type watchEntry struct {
	binary string
	at     time.Time
	warned bool
}

func newGovWatch() *govWatch {
	return &govWatch{
		started: map[string]watchEntry{},
		seen:    map[string]time.Time{},
	}
}

// noteGoverned records that a pane's traffic reached the proxy. This is the
// POSITIVE evidence that clears any pending suspicion.
func (g *govWatch) noteGoverned(paneID string) {
	if g == nil || paneID == "" {
		return
	}
	g.mu.Lock()
	g.seen[paneID] = time.Now()
	if e, ok := g.started[paneID]; ok {
		e.warned = true // governed: never warn about this run
		g.started[paneID] = e
	}
	g.mu.Unlock()
}

// noteForeground records that binary became the pane's foreground process.
// A change resets the observation, so each run of a CLI is judged on its own.
func (g *govWatch) noteForeground(paneID, binary string, now time.Time) {
	if g == nil || paneID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if binary == "" {
		delete(g.started, paneID)
		return
	}
	if e, ok := g.started[paneID]; ok && e.binary == binary {
		return // same program still running
	}
	g.started[paneID] = watchEntry{binary: binary, at: now}
}

// forget drops every observation for a pane that has gone away — both halves.
//
// noteForeground(paneID, "") alone was not enough: it clears `started` but
// leaves `seen`, so a long-lived daemon accumulates one timestamp per pane it
// has ever proxied, and a recycled pane ID inherits a governed state it did
// not earn.
func (g *govWatch) forget(paneID string) {
	if g == nil || paneID == "" {
		return
	}
	g.mu.Lock()
	delete(g.started, paneID)
	delete(g.seen, paneID)
	g.mu.Unlock()
}

// dueWarning reports a pane whose known CLI has run past the grace window with
// no governed request, and latches so the warning is emitted exactly once per
// run of that program.
func (g *govWatch) dueWarning(paneID string, now time.Time) (string, bool) {
	if g == nil || paneID == "" {
		return "", false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.started[paneID]
	if !ok || e.warned || now.Sub(e.at) < GraceWindow {
		return "", false
	}
	if last, ok := g.seen[paneID]; ok && last.After(e.at) {
		return "", false // governed since this program started
	}
	e.warned = true
	g.started[paneID] = e
	return e.binary, true
}

// state reports how a pane looks right now, for ##proxy status.
func (g *govWatch) state(paneID string, now time.Time) string {
	if g == nil {
		return "unknown"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if last, ok := g.seen[paneID]; ok && now.Sub(last) < 5*time.Minute {
		return "governed"
	}
	e, ok := g.started[paneID]
	if !ok {
		return "idle"
	}
	if now.Sub(e.at) >= GraceWindow {
		return "unproxied?"
	}
	return "starting"
}

// StrictBlockNotice is the pane line for a CLI that strict mode is about to
// stop. It names what is being stopped, why, and how to get the deterministic
// answer — because strict mode acts on negative evidence and will occasionally
// stop something innocent.
func StrictBlockNotice(binary string) string {
	env := "the base-URL env var"
	if p, ok := ProfileFor(binary); ok {
		env = p.BaseURLEnv
	}
	return fmt.Sprintf(
		"[proxy] strict: stopping '%s' — it ran past the grace window with no "+
			"governed request, so its provider traffic is not being redacted, "+
			"metered or capped. Confirm with `##proxy check %s`, or clear "+
			"[proxy] strict / policy proxy.strict to go back to warning only. "+
			"(It ignores %s.)\n", binary, binary, env)
}

// UnproxiedWarning is the pane line for a CLI that looks ungoverned. It says
// what was observed and what to do, and is explicit that this is a suspicion —
// an idle CLI looks the same from here.
func UnproxiedWarning(binary string) string {
	p, ok := ProfileFor(binary)
	detail := ""
	if ok && p.Note != "" {
		detail = " " + p.Note + "."
	}
	env := "the base-URL env var"
	if ok {
		env = p.BaseURLEnv
	}
	return fmt.Sprintf(
		"[proxy] warning: '%s' has been running with no governed request — "+
			"it may be ignoring %s.%s Confirm with `##proxy check %s`; "+
			"see docs/proxy-compat-matrix.md.\n", binary, env, detail, binary)
}
