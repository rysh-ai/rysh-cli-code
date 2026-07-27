package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestForgedAPIProxyInert(t *testing.T) {
	p := NewInertForgedAPIProxy("share1", "weather_getWeather", "Get the weather", `{"type":"object"}`, false)

	if p.Spec().Name != "weather_getWeather" {
		t.Fatalf("spec name = %q", p.Spec().Name)
	}
	if !strings.Contains(p.Spec().Description, "shared-api") {
		t.Fatalf("description should tag it as shared-api: %q", p.Spec().Description)
	}
	if p.RequiresApproval(nil) {
		t.Fatalf("shared forged APIs must not require per-call approval")
	}

	out, err := p.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("execute returned err: %v", err)
	}
	if out.Error == "" || !strings.Contains(out.Error, "discovery-only") {
		t.Fatalf("expected an inert 'discovery-only' error, got %+v", out)
	}
	if out.ErrorKind != ErrKindPermissionDenied {
		t.Fatalf("error kind = %q, want %q", out.ErrorKind, ErrKindPermissionDenied)
	}

	// An empty schema defaults to a valid object schema.
	p2 := NewInertForgedAPIProxy("s", "x", "d", "", false)
	if len(p2.Spec().Parameters) == 0 {
		t.Fatalf("empty schema should default to a non-empty object schema")
	}
}

// TestForgedAPIProxyActive verifies Execute delegates to the invoke callback,
// passing the op name and args, and returns the callback's output.
func TestForgedAPIProxyActive(t *testing.T) {
	var gotOp string
	var gotArgs string
	p := NewForgedAPIProxy("share1", "weather_getWeather", "Get the weather", `{"type":"object"}`, false,
		func(_ context.Context, op string, args json.RawMessage) (*ToolOutput, error) {
			gotOp = op
			gotArgs = string(args)
			return &ToolOutput{Content: "72F"}, nil
		})

	out, err := p.Execute(context.Background(), json.RawMessage(`{"city":"SF"}`))
	if err != nil {
		t.Fatalf("execute returned err: %v", err)
	}
	if out.Content != "72F" {
		t.Fatalf("content = %q, want 72F", out.Content)
	}
	if gotOp != "weather_getWeather" {
		t.Fatalf("invoke got op %q, want weather_getWeather", gotOp)
	}
	if gotArgs != `{"city":"SF"}` {
		t.Fatalf("invoke got args %q", gotArgs)
	}
}
