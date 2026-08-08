// wire-harness — the reproducible, recordable end-to-end proof that a wrapped
// agent CLI's provider traffic is governed by rysh (design 001, Phase 0/1
// definition-of-done item 3).
//
// WHY THIS EXISTS, given internal/proxy already has a wire test
// -------------------------------------------------------------
// TestWireTest_PlantedSecretNeverLeaves posts to the proxy with http.Post. That
// proves the proxy REDACTS. It does not prove the thing the product actually
// claims: that pointing a third-party CLI at rysh via ANTHROPIC_BASE_URL causes
// that CLI's traffic to be governed. The env-injection path — the whole
// mechanism — is untested by it.
//
// This harness closes that gap. It:
//
//  1. starts a fake upstream that appends every request body it receives to
//     wire.log (this is the "wire": bytes that left the machine);
//  2. starts the REAL internal/proxy in front of it, with a REAL SecretNAT
//     manager;
//  3. exports ANTHROPIC_BASE_URL exactly as pane_shell.go does
//     (<base>/anthropic/<paneID>) and runs a client under it;
//  4. asserts on wire.log: the planted secret appears ZERO times, and a
//     sk_live_SNAT… token appears in its place;
//  5. writes an asciicast v2 recording of the run.
//
// WHAT IT DOES AND DOES NOT PROVE
// -------------------------------
// -client=builtin (default) exercises the full proxy + SNAT + env-injection
// path using a small client that reads ANTHROPIC_BASE_URL the way a CLI does.
// It proves the mechanism. It does NOT prove any particular vendor CLI honours
// that variable.
//
// -client=real runs an actual external binary (-cli, default "claude"). Only
// this mode proves the vendor CLI is governed. The report says which mode ran;
// do not quote a builtin run as "Claude Code was governed".
//
// Usage:
//
//	go run ./cmd/wire-harness                       # builtin client
//	go run ./cmd/wire-harness -client=real          # real `claude`
//	go run ./cmd/wire-harness -out ./evidence       # where wire.log + .cast land
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/proxy"

	"github.com/rysh-ai/rysh-cli-shared/secretnat"
)

// plantedSecret is shaped like a live Stripe key so the detector tier fires on
// it. It is synthetic: the point is that a real-SHAPED credential is what the
// heuristic must catch.
const plantedSecret = "sk_live_ABCD1234abcd5678EFGH9012"

const paneID = "wire-harness-pane"

func main() {
	var (
		clientMode = flag.String("client", "builtin", `"builtin" (proves the mechanism) or "real" (proves a vendor CLI is governed)`)
		dialect    = flag.String("dialect", "anthropic", `"anthropic" or "openai" — selects the upstream shape, injected env var, and default real CLI (B8 compat matrix)`)
		cliBin     = flag.String("cli", "", `external binary for -client=real (default: "claude" for anthropic, "codex" for openai)`)
		outDir     = flag.String("out", "", "directory for wire.log and the .cast recording (default: a temp dir)")
		timeout    = flag.Duration("timeout", 90*time.Second, "budget for the client step")
	)
	flag.Parse()

	if *dialect != "anthropic" && *dialect != "openai" {
		fmt.Fprintf(os.Stderr, "wire-harness: unknown -dialect %q (want anthropic|openai)\n", *dialect)
		os.Exit(2)
	}
	if *cliBin == "" {
		if *dialect == "openai" {
			*cliBin = "codex"
		} else {
			*cliBin = "claude"
		}
	}

	rec := newRecorder()
	code := run(rec, *clientMode, *dialect, *cliBin, *outDir, *timeout)

	if err := rec.finish(); err != nil {
		fmt.Fprintf(os.Stderr, "wire-harness: could not write recording: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func run(rec *recorder, clientMode, dialect, cliBin, outDir string, timeout time.Duration) int {
	dir := outDir
	if dir == "" {
		d, err := os.MkdirTemp("", "rysh-wire-*")
		if err != nil {
			rec.failf("cannot create temp dir: %v", err)
			return 1
		}
		dir = d
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		rec.failf("cannot create %s: %v", dir, err)
		return 1
	}
	wireLog := filepath.Join(dir, "wire.log")
	rec.castPath = filepath.Join(dir, "wire-test.cast")

	rec.step("rysh wire test — is a wrapped CLI's provider traffic actually governed?")
	rec.printf("evidence dir : %s", dir)
	rec.printf("client mode  : %s", clientMode)
	rec.printf("dialect      : %s", dialect)

	// 1. The "upstream": everything it receives is a byte that left the machine.
	// The response shape matches the dialect so a real CLI parses it as a
	// completed turn instead of retrying.
	upstreamBody := `{"id":"msg_wire","type":"message","role":"assistant",` +
		`"content":[{"type":"text","text":"ok"}],"model":"claude-opus-4-8",` +
		`"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
	if dialect == "openai" {
		upstreamBody = `{"id":"chatcmpl-wire","object":"chat.completion","model":"gpt-4o",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	}
	var wire bytes.Buffer
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		wire.Write(b)
		wire.WriteByte('\n')
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()
	rec.step("1. fake upstream listening — every request body is appended to wire.log")

	// 2. The real proxy, with a real SecretNAT manager.
	mgr, err := secretnat.NewManager(secretnat.Options{Enabled: true, Mode: secretnat.ModeSemantic})
	if err != nil {
		rec.failf("secretnat manager: %v", err)
		return 1
	}
	srv := proxy.New(nil, mgr, nil, map[string]string{dialect: upstream.URL}, false)
	if _, err := srv.Start(""); err != nil {
		rec.failf("start proxy: %v", err)
		return 1
	}
	defer srv.Stop()
	rec.step("2. rysh governance proxy started (real internal/proxy + SecretNAT)")

	// 3. Env injection, byte-identical to pane_shell.go.
	baseURL := srv.BaseURL() + "/" + dialect + "/" + paneID
	envVar := "ANTHROPIC_BASE_URL"
	if dialect == "openai" {
		envVar = "OPENAI_BASE_URL"
	}
	rec.step("3. %s=%s", envVar, baseURL)
	rec.printf("   (this is exactly what pane_shell.go exports into a pane's shell)")

	rec.step("4. planting a live-shaped credential in the prompt:")
	rec.printf("   %s", plantedSecret)

	var clientErr error
	switch clientMode {
	case "builtin":
		clientErr = runBuiltinClient(rec, dialect, baseURL, timeout)
	case "real":
		clientErr = runRealCLI(rec, dialect, cliBin, baseURL, timeout)
	default:
		rec.failf("unknown -client %q (want builtin|real)", clientMode)
		return 1
	}
	if clientErr != nil {
		rec.failf("client step failed: %v", clientErr)
		return 1
	}

	// 5. The assertions, made against the captured bytes.
	captured := wire.String()
	if err := os.WriteFile(wireLog, []byte(captured), 0o644); err != nil {
		rec.failf("write wire.log: %v", err)
		return 1
	}

	rec.step("5. grepping the wire")
	if strings.TrimSpace(captured) == "" {
		rec.failf("wire.log is EMPTY — the client never reached the proxy, so this run proves nothing")
		return 1
	}

	realHits := strings.Count(captured, plantedSecret)
	tokenHits := len(regexp.MustCompile(`sk_live_SNAT[0-9]+`).FindAllString(captured, -1))

	rec.printf("   grep -c '%s' wire.log   -> %d", plantedSecret, realHits)
	rec.printf("   grep -oE 'sk_live_SNAT[0-9]+' wire.log | wc -l -> %d", tokenHits)

	ok := true
	if realHits != 0 {
		rec.failf("SECURITY FAILURE: the planted secret reached the upstream %d time(s)", realHits)
		ok = false
	}
	if tokenHits == 0 {
		rec.failf("no sk_live_SNAT token on the wire — the secret was dropped, not translated; "+
			"the model would have lost the credential's meaning (mode=%s)", secretnat.ModeSemantic)
		ok = false
	}
	if !ok {
		return 1
	}

	rec.step("PASS — the real credential never left; the token stood in for it")
	if clientMode == "builtin" {
		rec.printf("scope: proves the proxy + SecretNAT + base-URL injection path.")
		rec.printf("       does NOT prove any specific vendor CLI honours ANTHROPIC_BASE_URL —")
		rec.printf("       re-run with -client=real for that claim.")
	} else {
		rec.printf("scope: %s ran under the injected base URL and was governed end to end.", cliBin)
	}
	rec.printf("evidence: %s", wireLog)
	rec.printf("recording: %s", rec.castPath)
	return 0
}

// runBuiltinClient mimics what an agent CLI does: read the injected base URL
// and POST a dialect-shaped completion request to it. Deliberately minimal —
// its job is to exercise the injection path, not to be an agent.
func runBuiltinClient(rec *recorder, dialect, baseURL string, timeout time.Duration) error {
	prompt := "In one sentence: is " + plantedSecret + " a live or test key?"
	path := "/v1/messages"
	body := map[string]any{
		"model":      "claude-opus-4-8",
		"messages":   []map[string]any{{"role": "user", "content": prompt}},
		"max_tokens": 64,
	}
	if dialect == "openai" {
		path = "/v1/chat/completions"
		body = map[string]any{
			"model":    "gpt-4o",
			"messages": []map[string]any{{"role": "user", "content": prompt}},
		}
	}
	rec.step("4a. builtin client POSTing to $BASE_URL%s", path)
	raw, _ := json.Marshal(body)

	client := &http.Client{Timeout: timeout}
	resp, err := client.Post(baseURL+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	rec.printf("   HTTP %d from the proxy", resp.StatusCode)
	return nil
}

// runRealCLI drives an external agent CLI under the injected env. This is the
// only mode that proves a vendor CLI is governed.
func runRealCLI(rec *recorder, dialect, bin, baseURL string, timeout time.Duration) error {
	path, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("%s not on PATH — install it or use -client=builtin: %w", bin, err)
	}
	rec.step("4a. running the real CLI under the injected base URL")
	rec.printf("   %s (%s)", bin, path)

	prompt := "In one sentence: is " + plantedSecret + " a live or test key?"

	// How to force a given CLI through a base URL lives in internal/proxy's
	// profile table, NOT here. It used to live here, which meant `##proxy check`
	// and this harness could disagree about whether codex needs a pinned
	// model_provider — and the compat matrix's claims rest on this harness.
	// One home, both callers.
	prof, ok := proxy.ProfileFor(bin)
	if !ok {
		return fmt.Errorf("no CLI profile for %q — add one to internal/proxy/compat.go", bin)
	}
	scratch, err := os.MkdirTemp("", "rysh-wire-cli-*")
	if err != nil {
		return fmt.Errorf("scratch dir: %w", err)
	}
	argv, extraEnv, err := prof.ProbeCommand(path, baseURL, prompt, scratch, "rysh-wire-harness-dummy")
	if err != nil {
		return err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), extraEnv...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", bin, err)
	}
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		trimmed := strings.TrimSpace(out.String())
		if trimmed != "" {
			rec.printf("   %s output: %s", bin, firstLine(trimmed))
		}
		// A non-zero exit is not fatal: the CLI may reject our stub response.
		// What matters is whether it PUT BYTES ON THE WIRE, which the grep
		// answers. Surfacing the error keeps that judgement visible.
		if err != nil {
			rec.printf("   %s exited with: %v (not fatal — the wire grep decides)", bin, err)
		}
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return fmt.Errorf("%s did not finish within %s", bin, timeout)
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}
