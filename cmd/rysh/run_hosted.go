// SPDX-License-Identifier: Apache-2.0

package main

// `rysh run --hosted` (boat gap #5): the CI one-shot on a rysh.ai hosted
// runner. The server connection is the upstream: block of rysh.config.yaml
// (the same as ##hosted); the model key is read by NAME from the environment
// and goes in the start request body only. The flow and the exit codes are
// internal/hosted RunCI.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/hosted"
)

// hostedRunKeyEnv names the environment variable that holds each hosted
// provider's key (the providers the hosted runner accepts).
var hostedRunKeyEnv = map[string]string{
	"anthropic": "ANTHROPIC_API_KEY",
	"gemini":    "GEMINI_API_KEY",
	"openai":    "OPENAI_API_KEY",
}

// hostedRunSize is the one runner size the server offers today. The names
// follow the sandbox sizes (small, default, large, xlarge; rr-order-154).
const hostedRunSize = "default"

// checkHostedRunFlags refuses flag combinations --hosted cannot honour.
func checkHostedRunFlags(o runOptions) error {
	if !o.Hosted {
		switch {
		case o.Size != "":
			return errors.New("--size needs --hosted")
		case o.Model != "":
			return errors.New("--model needs --hosted")
		}
		return nil
	}
	for flag, set := range map[string]bool{
		"--worktree": o.Worktree, "--record": o.RecordDir != "", "--replay": o.ReplayDir != "",
		"--share": o.ShareDir != "", "--budget": o.Budget.set(), "--result-out": o.ResultOut != "",
		"--json-audit": o.JSONAudit != "",
	} {
		if set {
			return fmt.Errorf("%s is not available with --hosted", flag)
		}
	}
	if o.Provider == "" {
		return errors.New("--hosted needs --provider anthropic, gemini or openai (the run uses your own model key)")
	}
	if _, ok := hostedRunKeyEnv[o.Provider]; !ok {
		return fmt.Errorf("--hosted runs with anthropic, gemini or openai, not %q", o.Provider)
	}
	if o.Size != "" && o.Size != hostedRunSize {
		return fmt.Errorf("--size %q: the hosted runner offers one size today: %q", o.Size, hostedRunSize)
	}
	return nil
}

// hostedRunLLMKey reads the provider's key from its environment variable.
// The error names the variable, never a value.
func hostedRunLLMKey(provider string) (string, error) {
	name, ok := hostedRunKeyEnv[provider]
	if !ok {
		return "", fmt.Errorf("unsupported provider %q", provider)
	}
	k := os.Getenv(name)
	if k == "" {
		return "", fmt.Errorf("--hosted --provider %s needs %s in the environment (nothing was started)", provider, name)
	}
	return k, nil
}

// runHostedRun runs opts on the hosted runner and returns the exit code.
func runHostedRun(cfg config.Config, opts runOptions) (int, error) {
	if opts.Worktree {
		return runExitError, errors.New("the skill asks for isolation: worktree, which is not available with --hosted")
	}
	conn := hosted.Conn{URL: cfg.Upstream.URL, APIKey: cfg.Upstream.APIKey, WorkspaceID: cfg.Upstream.Workspace}
	if err := conn.Validate(); err != nil {
		return runExitError, fmt.Errorf("run --hosted: %w", err)
	}
	// The model key comes from the environment; it may go only to a server the
	// environment (or rysh itself) chose, never to one a config file named.
	if !config.EnvSecretMayGoTo(conn.URL) {
		return runExitError, fmt.Errorf("run --hosted: the server %s comes from a config file, so %s from your environment is not sent to it; set RYSH_UPSTREAM_URL to that server to confirm it (nothing was started)",
			hostDisplay(conn.URL), hostedRunKeyEnv[opts.Provider])
	}
	key, err := hostedRunLLMKey(opts.Provider)
	if err != nil {
		return runExitError, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	res := hosted.RunCI(ctx, &hosted.Client{Conn: conn}, hosted.RunOptions{
		Prompt: opts.Prompt, Timeout: opts.Timeout, Provider: opts.Provider, Model: opts.Model,
		LLMKey: key, Keep: opts.Keep, JSON: opts.JSON,
	}, os.Stdout, os.Stderr)
	return res.ExitCode, nil
}

// hostDisplay is the scheme and host of a URL, for messages.
func hostDisplay(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(unreadable url)"
	}
	return u.Scheme + "://" + u.Host
}
