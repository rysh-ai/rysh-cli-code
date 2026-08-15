// SPDX-License-Identifier: Apache-2.0

package provider

// Tests for the capability table (design 002 §3.3, R3). The property that
// matters: a provider reports Tools=true exactly when it can structurally
// accept tool specs, because design 006 §4.4's degradation hangs off it.

import (
	"context"
	"testing"

	sharedprovider "github.com/rysh-ai/rysh-cli-shared/provider"
)

// completeOnly models ClaudeCLI / ClaudeAPI / StaticProvider: no tool calling.
type completeOnly struct{}

func (completeOnly) Name() string { return "complete-only" }
func (completeOnly) Complete(context.Context, string) (string, error) {
	return "", nil
}

// declaring models a provider that reports its own capabilities.
type declaring struct{ completeOnly }

func (declaring) Caps() Capabilities {
	return Capabilities{Tools: true, MaxContextTokens: 4096}
}

// decorator models WithSessionDefaults' wrapper.
type decorator struct {
	completeOnly
	inner Provider
}

func (d decorator) Unwrap() Provider { return d.inner }

func TestCapsNilIsZero(t *testing.T) {
	if got := Caps(nil); got.Tools || got.Streaming {
		t.Errorf("nil provider should report the zero value, got %+v", got)
	}
	if SupportsTools(nil) {
		t.Error("nil provider must not report tool support")
	}
}

func TestCapsCompleteOnlyHasNoTools(t *testing.T) {
	// This is the case design 006 §4.4 exists for: a humanoid on such a
	// provider must NOT be given draft-and-confirm tools.
	if SupportsTools(completeOnly{}) {
		t.Error("a Complete-only provider must report Tools=false")
	}
}

func TestCapsAgenticProviderHasTools(t *testing.T) {
	// The real agentic providers satisfy AgenticProvider; the shared mock is a
	// faithful stand-in for that shape.
	var p Provider = &sharedprovider.StaticAgenticProvider{}
	if !SupportsTools(p) {
		t.Error("an AgenticProvider must report Tools=true")
	}
	c := Caps(p)
	if !c.ParallelTools {
		t.Error("agentic providers should report ParallelTools")
	}
	if len(c.ToolChoiceModes) == 0 {
		t.Error("agentic providers should declare tool-choice modes")
	}
}

func TestCapsSelfReportWins(t *testing.T) {
	c := Caps(declaring{})
	if !c.Tools || c.MaxContextTokens != 4096 {
		t.Errorf("a Reporter's own capabilities must be believed, got %+v", c)
	}
}

// legacyStreamer models the pre-002 streaming shape: it implements the
// legacy StreamingProvider seam and nothing ChatProvider-shaped.
type legacyStreamer struct{ completeOnly }

func (legacyStreamer) CompleteWithToolsStream(
	context.Context,
	[]sharedprovider.ConversationTurn,
	[]sharedprovider.ToolSpec,
	string,
	sharedprovider.StreamCallback,
) (*sharedprovider.AgenticResponse, error) {
	return nil, nil
}

// chatNative models a ChatProvider-native implementation (design 002 A2):
// streaming happens via ChatStream + the ChatStreamSupported self-report,
// with NO legacy StreamingProvider seam.
type chatNative struct {
	completeOnly
	streams bool
}

func (chatNative) Chat(context.Context, sharedprovider.ChatRequest) (*sharedprovider.ChatResponse, error) {
	return &sharedprovider.ChatResponse{}, nil
}

func (chatNative) ChatStream(context.Context, sharedprovider.ChatRequest, sharedprovider.StreamCallback) (*sharedprovider.ChatResponse, error) {
	return &sharedprovider.ChatResponse{}, nil
}

func (c chatNative) ChatStreamSupported() bool { return c.streams }

// TestCapsStreamingDetection: streaming is reported when EITHER the legacy
// StreamingProvider type-assert succeeds OR the provider is a ChatProvider
// whose ChatStreamSupported self-report says it streams — and stays false
// when neither holds.
func TestCapsStreamingDetection(t *testing.T) {
	if !Caps(legacyStreamer{}).Streaming {
		t.Error("legacy StreamingProvider must still report Streaming=true")
	}

	native := chatNative{streams: true}
	if _, ok := Provider(native).(sharedprovider.StreamingProvider); ok {
		t.Fatal("fixture invalid: chatNative must not implement the legacy seam")
	}
	if !Caps(native).Streaming {
		t.Error("a ChatProvider-native streamer must report Streaming=true")
	}

	if Caps(chatNative{streams: false}).Streaming {
		t.Error("a ChatProvider self-reporting no streaming must stay false")
	}
	if Caps(completeOnly{}).Streaming {
		t.Error("a provider with neither seam must report Streaming=false")
	}

	// The ChatProvider check runs on the unwrapped provider, same as the
	// legacy type-assert.
	if !Caps(decorator{inner: chatNative{streams: true}}).Streaming {
		t.Error("Caps must see ChatProvider streaming through decorators")
	}
}

// TestCapsSeesThroughDecorator is the regression guard for WithSessionDefaults:
// without Unwrap, the wrapper's own type would mask the real provider and every
// humanoid would look tool-less.
func TestCapsSeesThroughDecorator(t *testing.T) {
	inner := &sharedprovider.StaticAgenticProvider{}
	if !SupportsTools(decorator{inner: inner}) {
		t.Error("Caps must unwrap decorators to see the real provider")
	}
	if SupportsTools(decorator{inner: completeOnly{}}) {
		t.Error("unwrapping must not invent capabilities the inner provider lacks")
	}
}
