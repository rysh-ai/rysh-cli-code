// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

var errBoom = errors.New("boom")

// fakeForgeRunner is a stand-in for *forge.Manager in the owner-side invoke path.
type fakeForgeRunner struct {
	out        *sharedtools.ToolOutput
	err        error
	called     int
	lastOp     string
	lastBearer string
}

func (f *fakeForgeRunner) RunOp(ctx context.Context, op string, args json.RawMessage) (*sharedtools.ToolOutput, error) {
	return f.RunOpWithAuth(ctx, op, args, "")
}

func (f *fakeForgeRunner) RunOpWithAuth(_ context.Context, op string, _ json.RawMessage, bearer string) (*sharedtools.ToolOutput, error) {
	f.called++
	f.lastOp = op
	f.lastBearer = bearer
	return f.out, f.err
}

func invokePayload(op, args string) string {
	b, _ := json.Marshal(msg.ForgedInvokeRequest{Op: op, Args: json.RawMessage(args)})
	return string(b)
}

var invokeOps = []msg.ForgedOpSpec{
	{Name: "weather_getWeather", Mutating: false},
	{Name: "weather_postAlert", Mutating: true},
}

// TestEvaluateInvokeGovernance covers every gate in the owner-side invoke path:
// share-api off, view mode, nil runner, forge-origin rejection, default-deny
// mutation, success, allow-opted mutation, and runner error.
func TestEvaluateInvokeGovernance(t *testing.T) {
	ok := &sharedtools.ToolOutput{Content: "72F"}

	cases := []struct {
		name        string
		shareAPI    bool
		mode        string
		op          string
		allow       []string
		block       []string
		nilRunner   bool
		runnerErr   error
		wantErrKind string // "" => success
		wantRun     bool   // whether the runner should have been invoked
	}{
		{name: "share-api off", shareAPI: false, mode: "control", op: "weather_getWeather", wantErrKind: sharedtools.ErrKindPermissionDenied},
		{name: "view mode", shareAPI: true, mode: "view", op: "weather_getWeather", wantErrKind: sharedtools.ErrKindPermissionDenied},
		{name: "nil runner", shareAPI: true, mode: "control", op: "weather_getWeather", nilRunner: true, wantErrKind: sharedtools.ErrKindPermissionDenied},
		{name: "non-forge op rejected", shareAPI: true, mode: "control", op: "bash", wantErrKind: sharedtools.ErrKindPermissionDenied},
		{name: "mutation default-denied", shareAPI: true, mode: "control", op: "weather_postAlert", wantErrKind: sharedtools.ErrKindPermissionDenied},
		{name: "read succeeds", shareAPI: true, mode: "control", op: "weather_getWeather", wantRun: true},
		{name: "mutation allowed by glob", shareAPI: true, mode: "control", op: "weather_postAlert", allow: []string{"weather_*"}, wantRun: true},
		{name: "block beats allow", shareAPI: true, mode: "control", op: "weather_getWeather", allow: []string{"weather_*"}, block: []string{"weather_get*"}, wantErrKind: sharedtools.ErrKindPermissionDenied},
		{name: "runner error", shareAPI: true, mode: "control", op: "weather_getWeather", runnerErr: errBoom, wantErrKind: sharedtools.ErrKindInternal, wantRun: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runner := &fakeForgeRunner{out: ok, err: c.runnerErr}
			var r forgeOpRunner = runner
			if c.nilRunner {
				r = nil
			}
			res := evaluateInvoke(context.Background(), invokePayload(c.op, `{}`), c.mode, c.shareAPI, invokeOps, c.allow, c.block, r, false, false)
			if res.ErrorKind != c.wantErrKind {
				t.Fatalf("ErrorKind = %q, want %q (err=%q)", res.ErrorKind, c.wantErrKind, res.Error)
			}
			if c.wantErrKind == "" && res.Content != "72F" {
				t.Fatalf("Content = %q, want 72F", res.Content)
			}
			if got := runner.called > 0; got != c.wantRun {
				t.Fatalf("runner called = %v, want %v", got, c.wantRun)
			}
		})
	}
}

// TestEvaluateInvokeRedaction verifies the result is scrubbed when redact is on
// and passed through raw when off.
func TestEvaluateInvokeRedaction(t *testing.T) {
	secret := "secret_key=sk-livesecret"
	runner := &fakeForgeRunner{out: &sharedtools.ToolOutput{Content: secret}}

	red := evaluateInvoke(context.Background(), invokePayload("weather_getWeather", `{}`), "control", true, invokeOps, nil, nil, runner, true, false)
	if strings.Contains(red.Content, "sk-livesecret") {
		t.Fatalf("redacted result still leaks the secret: %q", red.Content)
	}

	raw := evaluateInvoke(context.Background(), invokePayload("weather_getWeather", `{}`), "control", true, invokeOps, nil, nil, runner, false, false)
	if !strings.Contains(raw.Content, "sk-livesecret") {
		t.Fatalf("unredacted result should preserve content: %q", raw.Content)
	}
}

// TestInvokeWireRoundTrip locks the end-to-end wire contract between the
// subscriber (bridgeInvoke builds a shareCommandPayload of type "invoke_op") and
// the owner (subscribeRemoteCommands parses it into MsgUpstreamCommand and runs
// evaluateInvoke), plus the result's return marshaling — all without NATS.
func TestInvokeWireRoundTrip(t *testing.T) {
	// --- subscriber side: build the command exactly like bridgeInvoke ---
	reqBody, _ := json.Marshal(msg.ForgedInvokeRequest{Op: "weather_getWeather", Args: json.RawMessage(`{"city":"SF"}`)})
	wire, _ := json.Marshal(shareCommandPayload{
		ShareID:     "share1",
		CommandID:   "c1",
		CommandType: "invoke_op",
		Payload:     string(reqBody),
		SenderID:    "pane1",
	})

	// --- owner side: parse like subscribeRemoteCommands, then govern+run ---
	var cmd msg.MsgUpstreamCommand
	if err := json.Unmarshal(wire, &cmd); err != nil {
		t.Fatalf("owner failed to parse command: %v", err)
	}
	if cmd.CommandType != "invoke_op" {
		t.Fatalf("command_type = %q, want invoke_op", cmd.CommandType)
	}
	runner := &fakeForgeRunner{out: &sharedtools.ToolOutput{Content: "72F"}}
	res := evaluateInvoke(context.Background(), cmd.Payload, "control", true, invokeOps, nil, nil, runner, false, false)
	if res.Error != "" {
		t.Fatalf("unexpected error: %q", res.Error)
	}
	if res.Content != "72F" || runner.lastOp != "weather_getWeather" {
		t.Fatalf("round-trip mismatch: content=%q lastOp=%q", res.Content, runner.lastOp)
	}

	// --- return trip: the result marshals back for the subscriber proxy ---
	resWire, _ := json.Marshal(res)
	var back msg.ForgedInvokeResult
	if err := json.Unmarshal(resWire, &back); err != nil || back.Content != "72F" {
		t.Fatalf("result round-trip failed: err=%v result=%+v", err, back)
	}
}

// TestEvaluateInvokeDelegatedAuth verifies Model B gating: a subscriber-supplied
// token reaches the runner ONLY when the share opted into delegated auth; with
// delegation off (the default) the token is ignored (Model A / owner identity).
func TestEvaluateInvokeDelegatedAuth(t *testing.T) {
	payload := func() string {
		b, _ := json.Marshal(msg.ForgedInvokeRequest{
			Op: "weather_getWeather", Args: json.RawMessage(`{}`), Auth: "sub-token-xyz",
		})
		return string(b)
	}()

	// Delegation ON → the subscriber's bearer is forwarded to the runner.
	on := &fakeForgeRunner{out: &sharedtools.ToolOutput{Content: "ok"}}
	resOn := evaluateInvoke(context.Background(), payload, "control", true, invokeOps, nil, nil, on, false, true)
	if resOn.Error != "" {
		t.Fatalf("delegated-on: unexpected error %q", resOn.Error)
	}
	if on.lastBearer != "sub-token-xyz" {
		t.Fatalf("delegated-on: runner bearer = %q, want sub-token-xyz", on.lastBearer)
	}

	// Delegation OFF (default) → the bearer is dropped; Model A is used.
	off := &fakeForgeRunner{out: &sharedtools.ToolOutput{Content: "ok"}}
	resOff := evaluateInvoke(context.Background(), payload, "control", true, invokeOps, nil, nil, off, false, false)
	if resOff.Error != "" {
		t.Fatalf("delegated-off: unexpected error %q", resOff.Error)
	}
	if off.lastBearer != "" {
		t.Fatalf("delegated-off: runner bearer = %q, want empty (Model A)", off.lastBearer)
	}
}

// TestEvaluateInvokeBadPayload rejects malformed input as a validation error.
func TestEvaluateInvokeBadPayload(t *testing.T) {
	runner := &fakeForgeRunner{out: &sharedtools.ToolOutput{Content: "x"}}
	res := evaluateInvoke(context.Background(), "not-json", "control", true, invokeOps, nil, nil, runner, false, false)
	if res.ErrorKind != sharedtools.ErrKindValidation {
		t.Fatalf("bad payload: ErrorKind = %q, want validation", res.ErrorKind)
	}
	if runner.called != 0 {
		t.Fatalf("runner should not run on a bad payload")
	}
}
