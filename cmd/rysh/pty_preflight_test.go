package main

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/platform"
)

// TestRequirePTY_AllowsPtylessCommands must hold on every platform: the
// commands that never open a pane are always permitted.
func TestRequirePTY_AllowsPtylessCommands(t *testing.T) {
	for _, cmd := range []string{
		"list-sessions", "send", "install", "eval", "help", "--version", "doctor",
		// `rysh exec` and `rysh script` drive an existing session over NATS;
		// like `send`, they never open a pane of their own.
		"exec", "script",
	} {
		if err := requirePTY([]string{cmd}); err != nil {
			t.Errorf("requirePTY(%q) = %v, want nil — this command needs no pane", cmd, err)
		}
	}
}

// TestRequirePTY_SessionCommandsFollowPlatformSupport pins the behaviour to the
// build's actual capability, so the same test is meaningful on both a Unix host
// (where sessions must be allowed) and a Windows build (where they must be
// refused with guidance).
func TestRequirePTY_SessionCommandsFollowPlatformSupport(t *testing.T) {
	sessionCommands := [][]string{
		{},                 // bare `rysh`
		{"my-session"},     // bare session name
		{"attach", "work"}, // explicit attach
		{"create"},
	}

	for _, args := range sessionCommands {
		err := requirePTY(args)

		if platform.PTYSupported {
			if err != nil {
				t.Errorf("requirePTY(%v) = %v, want nil on a PTY-capable host", args, err)
			}
			continue
		}

		if err == nil {
			t.Errorf("requirePTY(%v) = nil, want a refusal on a host with no PTY", args)
			continue
		}
		// A refusal that does not tell the user what to do instead is a worse
		// failure than the crash it replaces.
		for _, want := range []string{"WSL", "docs/wsl.md"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal must mention %q, got: %v", want, err)
			}
		}
	}
}

// TestPTYUnsupportedReason_IsPopulatedWhenUnsupported guards the pairing of the
// two constants: an unsupported build with an empty reason would produce a
// blank, unactionable error.
func TestPTYUnsupportedReason_IsPopulatedWhenUnsupported(t *testing.T) {
	if !platform.PTYSupported && strings.TrimSpace(platform.PTYUnsupportedReason) == "" {
		t.Fatal("PTYSupported=false requires a non-empty PTYUnsupportedReason")
	}
	if platform.PTYSupported && platform.PTYUnsupportedReason != "" {
		t.Fatal("PTYSupported=true must not carry an unsupported reason")
	}
}
