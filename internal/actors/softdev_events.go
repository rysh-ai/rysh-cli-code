package actors

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/pipeline"
)

// Event prefix constants.
const (
	aiSoftdevPrefix = "##>event:ai:softdev:"
	shSoftdevPrefix = "##>event:sh:softdev:"
)

// softdevPrompts maps "language:phase" keys to agentic prompt templates.
// Two %s placeholders: source pane alias, accumulated context.
var softdevPrompts = map[string]string{
	"golang:planning": "You are working on a Go project. The planning phase has been triggered from pane '%s'.\n" +
		"Analyze the codebase and requirements, then create a detailed implementation plan.\n\n" +
		"Context from source pane:\n%s",

	"golang:development": "You are working on a Go project. The development phase has been triggered from pane '%s'.\n" +
		"Review the plan and context below, then start implementing the code changes.\n\n" +
		"Context from source pane:\n%s",

	"golang:linting": "You are working on a Go project. The linting phase has been triggered from pane '%s'.\n" +
		"Run the linter (go vet, staticcheck, or golangci-lint) and fix any issues found.\n\n" +
		"Context from source pane:\n%s",

	"golang:unit_testing": "You are working on a Go project. The unit testing phase has been triggered from pane '%s'.\n" +
		"Run the unit tests with `go test ./...` and fix any failures.\n\n" +
		"Context from source pane:\n%s",

	"golang:integration_testing": "You are working on a Go project. The integration testing phase has been triggered from pane '%s'.\n" +
		"Run the integration tests and fix any failures.\n\n" +
		"Context from source pane:\n%s",

	"golang:deployment": "You are working on a Go project. The deployment phase has been triggered from pane '%s'.\n" +
		"Prepare and execute the deployment based on the project configuration.\n\n" +
		"Context from source pane:\n%s",

	"golang:monitoring": "You are working on a Go project. The monitoring phase has been triggered from pane '%s'.\n" +
		"Set up monitoring and verify the deployment is healthy.\n\n" +
		"Context from source pane:\n%s",
}

// softdevCommands maps "language:phase" keys to shell commands.
var softdevCommands = map[string]string{
	"golang:planning":            "echo '[softdev] planning phase — use ##>event:ai:softdev:golang:planning for AI-driven planning'",
	"golang:development":         "go build ./...",
	"golang:linting":             "go vet ./...",
	"golang:unit_testing":        "go test -v ./...",
	"golang:integration_testing": "go test -v -tags=integration ./...",
	"golang:deployment":          "go build -o ./bin/ ./...",
	"golang:monitoring":          "echo '[softdev] monitoring phase — configure monitoring tools'",
}

// ParseAISoftdevEvent checks if a line matches ##>event:ai:softdev:<language>:<phase> [userText].
// Any text after the phase (separated by space) is returned as userText.
func ParseAISoftdevEvent(line string) (language, phase, userText string, ok bool) {
	return parseSoftdevEvent(line, aiSoftdevPrefix)
}

// ParseShSoftdevEvent checks if a line matches ##>event:sh:softdev:<language>:<phase> [userText].
// Any text after the phase (separated by space) is returned as userText.
func ParseShSoftdevEvent(line string) (language, phase, userText string, ok bool) {
	return parseSoftdevEvent(line, shSoftdevPrefix)
}

func parseSoftdevEvent(line, prefix string) (language, phase, userText string, ok bool) {
	if !strings.HasPrefix(line, prefix) {
		return "", "", "", false
	}
	rest := strings.TrimPrefix(line, prefix)
	rest = strings.TrimSpace(rest)
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", false
	}
	language = parts[0]
	phaseAndText := strings.TrimRight(parts[1], ": ")

	// Split phase from optional trailing user text on the first space.
	// e.g. "planning I want to refactor config" → phase="planning", userText="I want to refactor config"
	if idx := strings.IndexByte(phaseAndText, ' '); idx >= 0 {
		phase = phaseAndText[:idx]
		userText = strings.TrimSpace(phaseAndText[idx+1:])
	} else {
		phase = phaseAndText
	}
	if phase == "" {
		return "", "", "", false
	}
	return language, phase, userText, true
}

// BuildSoftdevPrompt constructs the agentic prompt for an AI softdev event.
// It checks the pipeline overlay first, falling back to the built-in softdevPrompts.
// userText is optional inline instructions from the event line (text after the phase).
func BuildSoftdevPrompt(language, phase, sourceAlias, userText, context string) (string, bool) {
	key := language + ":" + phase

	// Check runtime pipeline overlay first.
	tmpl, ok := pipeline.LookupPrompt(key)
	if !ok {
		// Fall back to built-in prompts.
		tmpl, ok = softdevPrompts[key]
		if !ok {
			keys := make([]string, 0, len(softdevPrompts))
			for k := range softdevPrompts {
				keys = append(keys, k)
			}
			slog.Error("BuildSoftdevPrompt: key not found",
				"key", key, "language", language, "phase", phase,
				"mapLen", len(softdevPrompts), "availableKeys", keys)
			return "", false
		}
	}

	if context == "" {
		context = "(no context accumulated yet)"
	}
	prompt := fmt.Sprintf(tmpl, sourceAlias, context)
	if userText != "" {
		prompt += "\n\nUser instructions:\n" + userText
	}
	return prompt, true
}

// LookupSoftdevCommand returns the shell command for a SH softdev event.
func LookupSoftdevCommand(language, phase string) (string, bool) {
	key := language + ":" + phase
	cmd, ok := softdevCommands[key]
	return cmd, ok
}
