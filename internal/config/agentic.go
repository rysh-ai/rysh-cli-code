// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// AgenticConfig holds configuration for the agentic coding assistant.
type AgenticConfig struct {
	Enabled              bool
	MaxIterations        int
	MaxParallelTasks     int
	ContextWindowToks    int
	BashTimeout          time.Duration
	BlockedCommands      []string
	BlockedPaths         []string
	MaxFileSize          int64
	AutoApproveReads     bool
	AutoApproveBash      bool
	RequireApproveWrites bool
	// Grounding selects the grounding protocol ("off" | "prompt" |
	// "enforced"): read the codebase like a human (search → read → iterate)
	// before acting, asking the user for the location when searching cannot
	// find the target. Empty means the built-in defaults: panes run
	// "enforced", agents/humanoids and sub-agents run "prompt" (advisory).
	// RYSH_GROUNDING overrides.
	Grounding string
}

// DefaultAgenticConfig returns sensible defaults for the agentic system.
func DefaultAgenticConfig() AgenticConfig {
	return AgenticConfig{
		Enabled:           true,
		MaxIterations:     50,
		MaxParallelTasks:  4,
		ContextWindowToks: 100000,
		BashTimeout:       2 * time.Minute,
		BlockedCommands: []string{
			"rm -rf /",
			"dd if=",
			"mkfs",
			"> /dev/sd",
		},
		BlockedPaths: []string{
			".env",
			"*.pem",
			"*.key",
			"credentials*",
		},
		MaxFileSize:          1048576, // 1MB
		AutoApproveReads:     true,
		AutoApproveBash:      true,
		RequireApproveWrites: true,
	}
}

// LoadAgenticConfig loads agentic configuration from environment variables.
// It starts from defaults and applies overrides.
func LoadAgenticConfig() AgenticConfig {
	cfg := DefaultAgenticConfig()

	if v := strings.TrimSpace(os.Getenv("RYSH_AGENTIC_ENABLED")); v != "" {
		cfg.Enabled = v == "true" || v == "1"
	}
	if v := strings.TrimSpace(os.Getenv("RYSH_AGENTIC_MAX_ITERATIONS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.MaxIterations = n
		}
	}
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("RYSH_GROUNDING"))); v != "" {
		switch v {
		case "off", "prompt", "enforced":
			cfg.Grounding = v
		}
	}
	if v := strings.TrimSpace(os.Getenv("RYSH_AGENTIC_BASH_TIMEOUT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.BashTimeout = time.Duration(n) * time.Millisecond
		}
	}
	if v := strings.TrimSpace(os.Getenv("RYSH_AGENTIC_AUTO_APPROVE_READS")); v != "" {
		cfg.AutoApproveReads = v == "true" || v == "1"
	}
	if v := strings.TrimSpace(os.Getenv("RYSH_AGENTIC_AUTO_APPROVE_BASH")); v != "" {
		cfg.AutoApproveBash = v == "true" || v == "1"
	}

	return cfg
}
