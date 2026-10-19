// SPDX-License-Identifier: Apache-2.0

// Package agentenv is the ONE definition of which environment variables an
// agent process (claude, codex) may inherit (root-0472; the ORDER-63 design).
//
// WHY AT THE EXEC LINE. A pane shell is an interactive shell, and the user's rc
// file can export credentials into it after the daemon has started it — that is
// how AWS keys reached every agent while neither the daemon nor the pane's own
// exec env carried them (ORDER-62). Scrubbing the daemon cannot fix that; only
// the exec of the agent itself can. So every place rysh starts an agent runs it
// through Filter — typed lines via `rysh exec-clean`, daemon-side exec.Command
// via cmd.Env = Filter(os.Environ(), kind).
//
// THE RULE, in precedence order (tested):
//
//  1. exact KEEP for the agent (its own login names) — kept, whatever matches below;
//  2. DENY — the exact names, then the credential-shaped patterns — dropped;
//  3. everything else passes through, the prefix KEEPs (RYSH_*, GIT_*,
//     *_BASE_URL) included: a prefix KEEP never rescues a DENY match, which is
//     why RYSH_CLI_PAT is dropped (by *_PAT) while RYSH_PANE is kept.
//
// There is deliberately no bare *_KEY pattern: it would take SSH_AUTH_SOCK-style
// and ordinary config names with it. Names only: nothing here reads, logs or
// compares a value, except that a provider switch (CLAUDE_CODE_USE_BEDROCK /
// CLAUDE_CODE_USE_VERTEX) is checked for being non-empty.
package agentenv

import (
	"sort"
	"strings"
)

// Kind is the agent being started.
type Kind string

const (
	Claude Kind = "claude"
	Codex  Kind = "codex"
)

// ParseKind maps a CLI/meta spelling to a Kind; ok is false for anything else.
func ParseKind(s string) (Kind, bool) {
	switch strings.TrimSpace(s) {
	case string(Claude):
		return Claude, true
	case string(Codex):
		return Codex, true
	}
	return "", false
}

// denyExact are dropped by exact name: root-0349 as widened by rr-msg-193, the
// release/publish tokens, the GIT_* names that can carry auth (root-0472 (b)),
// and the markers a parent claude exports — without those, a claude started
// from inside another claude comes up as a nested child of the conversation that
// launched it.
var denyExact = []string{
	// root-0349 widened (rr-msg-193)
	"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_DEFAULT_REGION", "AWS_SESSION_TOKEN", "S3_ENDPOINT_URL",
	"GEMINI_API_KEY", "GIT_USERNAME",
	"TANGOLIFE_BLOG_AUTOMATION_TOKEN", "TANGOLIFE_INSTAGRAM_PASSWORD", "TANGOLIFE_INSTAGRAM_USERNAME",
	// release / publish credentials (rr-msg-193's token unsets)
	"GH_TOKEN", "GITHUB_TOKEN", "RYSH_CLI_PAT", "NPM_TOKEN", "NODE_AUTH_TOKEN",
	"HOMEBREW_TAP_TOKEN", "GORELEASER_GITHUB_TOKEN", "GPG_KEY_FILE", "GPG_FINGERPRINT",
	// GIT_* that can carry auth (root-0472 (b)); GIT_CONFIG_KEY_*/VALUE_* are prefixes below
	"GIT_ASKPASS", "GIT_CONFIG_COUNT",
	// a parent claude's nesting markers
	"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_EXECPATH",
	"CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID", "CLAUDE_SESSION_ID",
	"CLAUDE_PID", "CLAUDE_EFFORT",
}

// denyPrefixes / denySuffixes / denyContains are the credential-shaped patterns:
// AWS_*, GIT_CONFIG_KEY_*, GIT_CONFIG_VALUE_*; *_PASSWORD, *_TOKEN, *_API_KEY,
// *_PAT, *_CREDENTIALS, *_PRIVATE_KEY, *_PASSPHRASE; *_SECRET*.
var (
	denyPrefixes = []string{"AWS_", "GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_"}
	denySuffixes = []string{"_PASSWORD", "_TOKEN", "_API_KEY", "_PAT", "_CREDENTIALS", "_PRIVATE_KEY", "_PASSPHRASE"}
	denyContains = []string{"_SECRET"}
)

// keepClaude are the names Claude Code itself reads to authenticate and to
// reach the API (from the claude binary's strings, names only; ORDER-63). Many
// match *_TOKEN/*_API_KEY, so without these exact KEEPs the scrub would log out
// every claude that authenticates through the environment — including the
// proxy's dummy ANTHROPIC_API_KEY (pane_shell.go ProxyDummyKey).
var keepClaude = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
	"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_REFRESH_TOKEN", "CLAUDE_CODE_SESSION_ACCESS_TOKEN",
	"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
	"CLAUDE_CODE_WEBSOCKET_AUTH_FILE_DESCRIPTOR", "CLAUDE_CODE_CLIENT_KEY",
	"ANTHROPIC_IDENTITY_TOKEN_FILE", "ANTHROPIC_FOUNDRY_API_KEY", "ANTHROPIC_AWS_API_KEY",
}

// keepClaudeBedrock / keepClaudeVertex are kept for claude only while the
// matching provider switch is set: those users authenticate claude WITH them.
var (
	keepClaudeBedrock = []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_REGION",
		"AWS_DEFAULT_REGION", "AWS_PROFILE", "AWS_BEARER_TOKEN_BEDROCK", "AWS_SHARED_CREDENTIALS_FILE",
	}
	keepClaudeVertex = []string{"GOOGLE_APPLICATION_CREDENTIALS"}
)

// keepCodex are the names rysh's own codex adapter wires (proxy/compat.go:
// env_key OPENAI_API_KEY, OPENAI_BASE_URL, CODEX_HOME). NOT yet verified against
// a codex binary (none on the build host), which is why the codex launch sites
// still use the static StaticPrefix — see the package report (ORDER-64).
var keepCodex = []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_HOME"}

// keepFor returns the exact KEEP set for kind, given the environment (only the
// provider switches are looked at, for being non-empty).
func keepFor(kind Kind, env []string) map[string]bool {
	keep := map[string]bool{}
	add := func(names []string) {
		for _, n := range names {
			keep[n] = true
		}
	}
	switch kind {
	case Claude:
		add(keepClaude)
		if switchOn(env, "CLAUDE_CODE_USE_BEDROCK") {
			add(keepClaudeBedrock)
		}
		if switchOn(env, "CLAUDE_CODE_USE_VERTEX") {
			add(keepClaudeVertex)
		}
	case Codex:
		add(keepCodex)
	}
	return keep
}

func switchOn(env []string, name string) bool {
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if k == name {
			return strings.TrimSpace(v) != ""
		}
	}
	return false
}

// Denied reports whether name is dropped by DENY (exact or pattern), before any
// KEEP is applied.
func Denied(name string) bool {
	for _, n := range denyExact {
		if name == n {
			return true
		}
	}
	for _, p := range denyPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	for _, s := range denySuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	for _, c := range denyContains {
		if strings.Contains(name, c) {
			return true
		}
	}
	return false
}

// Filter returns env without the variables kind must not inherit. env is the
// KEY=VALUE form of os.Environ; order is preserved.
func Filter(env []string, kind Kind) []string {
	keep := keepFor(kind, env)
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if keep[name] || !Denied(name) {
			out = append(out, kv)
		}
	}
	return out
}

// Dropped returns the sorted NAMES Filter would remove (for --explain and tests;
// never values).
func Dropped(env []string, kind Kind) []string {
	keep := keepFor(kind, env)
	var names []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !keep[name] && Denied(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// StaticNames are the exact DENY names, the only part of the rule a static
// `env -u` list can express. It is the source of StaticPrefix and of nothing
// else, so the fallback can never drift from the definition (root-0472 (d)).
func StaticNames() []string {
	out := make([]string, len(denyExact))
	copy(out, denyExact)
	return out
}

// StaticPrefix is `env -u NAME …` over StaticNames: the fallback for a pane
// whose host has no rysh binary with exec-clean (and, until codex is verified,
// the codex launch prefix). Names only — no value can appear in it.
func StaticPrefix() string {
	var b strings.Builder
	b.WriteString("env")
	for _, n := range denyExact {
		b.WriteString(" -u ")
		b.WriteString(n)
	}
	return b.String()
}

// FilterStatic drops the exact DENY names only — what StaticPrefix does in a
// typed line, for a daemon-side exec of an agent whose patterns are not yet
// verified (codex, ORDER-64).
func FilterStatic(env []string) []string {
	deny := map[string]bool{}
	for _, n := range denyExact {
		deny[n] = true
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !deny[name] {
			out = append(out, kv)
		}
	}
	return out
}

// ForRunner is the env for a daemon-side exec of runner ("claude" → Filter;
// anything else, codex included until verified → FilterStatic).
func ForRunner(env []string, runner string) []string {
	if k, ok := ParseKind(runner); ok && k == Claude {
		return Filter(env, Claude)
	}
	return FilterStatic(env)
}
