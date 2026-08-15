// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

type Provider interface {
	Name() string
	Complete(ctx context.Context, prompt string) (string, error)
}

// New builds the simple completion provider (the legacy Complete()-only seam,
// used by the loop-engineering judge seat).
//
// Its selector MIRRORS NewAgenticProvider deliberately. The two used to be
// inverted: the agentic selector treated everything except openai/ollama as
// Claude, while this one matched only the exact string "claude" and sent
// everything else — including "anthropic", the name the onboarding flow writes,
// and the empty default — to a StaticProvider that returned canned text as a
// SUCCESSFUL completion. The judge seat therefore scored loop iterations
// against a mock string under ollama, under openai, and under "anthropic",
// with no error and no log. Any divergence between these two switches
// reintroduces that bug, so keep them in lockstep.
func New(cfg config.Config) Provider {
	switch strings.ToLower(strings.TrimSpace(cfg.ProviderName)) {
	case "openai", "ollama", "gemini":
		// Same OpenAI-compatible provider the agentic path uses; it already
		// implements Complete(), so there is no second HTTP client to keep in
		// sync. A missing key surfaces as a real 401 rather than a fake answer.
		return NewOpenAIAgenticProvider(cfg)
	case "claude-cli":
		// Explicit CLI selection: honored even when an API key is configured,
		// unlike the default branch's key-based auto-select below.
		return NewClaudeCLIAgenticProvider(cfg)
	default: // "", "claude", "anthropic", "claude-agentic"
		// When an API key is configured, use the HTTP API directly.
		// Otherwise fall back to the CLI adapter.
		if cfg.APIKey != "" {
			return NewClaudeAPI(cfg)
		}
		return &ClaudeCLI{
			command:      cfg.ClaudeCommand,
			systemPrompt: cfg.SystemPrompt,
			model:        cfg.DefaultModel,
		}
	}
}

type ClaudeCLI struct {
	command      string
	systemPrompt string
	model        string
}

func (c *ClaudeCLI) Name() string {
	return "claude"
}

func (c *ClaudeCLI) Complete(ctx context.Context, prompt string) (string, error) {
	systemPrompt, err := c.loadSystemPrompt()
	if err != nil {
		return "", err
	}
	return c.completeWithSystem(ctx, systemPrompt, prompt)
}

// completeWithSystem runs the CLI with an explicit system prompt string. The
// agentic adapter (ClaudeCLIAgentic) needs this seam: the orchestrator hands it
// the fully assembled system prompt (project memory, env block), which must not
// be silently replaced by the static file Complete() loads.
func (c *ClaudeCLI) completeWithSystem(ctx context.Context, systemPrompt, prompt string) (string, error) {
	args := []string{"--print", "--system-prompt", systemPrompt}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, c.command, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errText := strings.TrimSpace(stderr.String())
		if errText == "" {
			errText = err.Error()
		}
		return "", fmt.Errorf("claude command failed: %s", errText)
	}

	return strings.TrimSpace(stdout.String()), nil
}

func (c *ClaudeCLI) loadSystemPrompt() (string, error) {
	path := c.systemPrompt
	if !filepath.IsAbs(path) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		path = filepath.Join(wd, path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read system prompt: %w", err)
	}
	return string(data), nil
}

type StaticProvider struct {
	name     string
	response string
}

func (s *StaticProvider) Name() string {
	return s.name
}

func (s *StaticProvider) Complete(_ context.Context, prompt string) (string, error) {
	return fmt.Sprintf("%s\n\nprompt:\n%s", s.response, prompt), nil
}

// NewOverride builds the simple completion provider with per-use model and
// effort overrides — the seam behind the loop-engineering judge seat
// (loop.while.model / loop.while.effort). Empty overrides keep the session
// defaults; effort applies to the HTTP API provider only (the CLI adapter
// has no effort control and ignores it).
func NewOverride(cfg config.Config, model, effort string) Provider {
	if strings.TrimSpace(model) != "" {
		cfg.DefaultModel = strings.TrimSpace(model)
	}
	p := New(cfg)
	if api, ok := p.(*ClaudeAPI); ok && strings.TrimSpace(effort) != "" {
		api.effort = strings.TrimSpace(effort)
	}
	return p
}

// NewSanitized wraps a simple Provider so every outbound prompt passes
// through sanitize before leaving the process — the SecretNAT / ReSet seam
// for the single-turn provider path (judge/loop seats). The response is
// returned as-is: single-turn callers have no tool execution, so nothing
// needs restoring. Identity when either argument is nil.
func NewSanitized(p Provider, sanitize func(string) string) Provider {
	if p == nil || sanitize == nil {
		return p
	}
	return &sanitizedProvider{inner: p, sanitize: sanitize}
}

type sanitizedProvider struct {
	inner    Provider
	sanitize func(string) string
}

func (s *sanitizedProvider) Name() string { return s.inner.Name() }

func (s *sanitizedProvider) Complete(ctx context.Context, prompt string) (string, error) {
	return s.inner.Complete(ctx, s.sanitize(prompt))
}
