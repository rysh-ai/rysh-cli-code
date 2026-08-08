// Package wirecheck answers one question about one CLI: if rysh points it at
// the governance proxy, does its provider traffic actually go there?
//
// It is the mechanism behind `##proxy check <binary>` (design 022 §4.4), and it
// exists because the alternative was a hand-maintained table. docs/proxy-compat-matrix.md
// can only say what someone verified on a verification host; this lets a user
// get the same answer for their own CLI, their own version, on their own
// machine — including CLIs rysh has never tested.
//
// The verdict is made on CAPTURED BYTES, never on an exit code. A CLI that
// exits non-zero against a stub upstream but put a governed request on the wire
// is governed; one that exits cleanly having talked to the real provider is not.
package wirecheck

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rysh-ai/rysh-cli-shared/secretnat"

	"github.com/rysh-ai/rysh-cli-code/internal/proxy"
)

// plantedSecret is a live-SHAPED but synthetic credential. Real shape is the
// point: it is what the SecretNAT detector tier must catch.
const plantedSecret = "sk_live_ABCD1234abcd5678EFGH9012"

// dummyKey is what the probed CLI gets as its credential. The probe must never
// need a working key — the stub upstream answers everything.
const dummyKey = "rysh-proxy-check-dummy"

// probePaneID keeps probe traffic out of any real pane's audit trail.
const probePaneID = "proxy-check"

var snatToken = regexp.MustCompile(`sk_live_SNAT[0-9]+`)

// Result is the verdict. Governed is true only when the CLI put at least one
// request on the proxy's wire AND the planted secret was translated rather than
// forwarded.
type Result struct {
	Binary   string
	Dialect  string
	Governed bool
	// Inconclusive marks a run that proves nothing either way: the CLI failed
	// before it could make any request, so its silence is not evidence about
	// whether it honours the injected base URL. Reporting that as NOT GOVERNED
	// would put a false ❌ in the compatibility matrix — a broken install read
	// as a governance failure.
	Inconclusive bool
	Requests     int // requests that reached the stub upstream through the proxy
	Planted      int // occurrences of the raw secret on the wire (must be 0)
	Tokens       int // SNAT stand-in tokens on the wire
	CLIError     string
	Reason       string
}

// String renders the verdict as the pane sees it.
func (r Result) String() string {
	head := fmt.Sprintf("[proxy] check %s (%s dialect): ", r.Binary, r.Dialect)
	switch {
	case r.Governed:
		return head + fmt.Sprintf("GOVERNED — %d request(s) through rysh, "+
			"planted secret 0x on the wire, %d SNAT token(s) in its place\n",
			r.Requests, r.Tokens)
	case r.Inconclusive:
		return head + "INCONCLUSIVE — " + r.Reason + "\n"
	default:
		return head + "NOT GOVERNED — " + r.Reason + "\n"
	}
}

// Run probes one CLI. It starts a stub upstream and a REAL proxy with a REAL
// SecretNAT manager, runs the binary under the injected environment, and greps
// what arrived.
//
// scratchBase is where the probe's throwaway working directory is created. It
// should be project-local (RyshDir); see makeScratch for why "" is a worse
// default than it looks.
func Run(ctx context.Context, binary string, timeout time.Duration, scratchBase string) (Result, error) {
	prof, ok := proxy.ProfileFor(binary)
	if !ok {
		return Result{Binary: binary}, fmt.Errorf(
			"unknown CLI %q — rysh knows: %s", binary, strings.Join(proxy.KnownCLIs(), ", "))
	}
	res := Result{Binary: prof.Binary, Dialect: prof.Dialect}

	bin, err := exec.LookPath(binary)
	if err != nil {
		return res, fmt.Errorf("%s is not on PATH", binary)
	}

	// The stub upstream IS the wire: everything it receives is a byte that
	// would have left the machine.
	var mu sync.Mutex
	var wire bytes.Buffer
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		// The query string is part of the wire too. Body-only capture would
		// score a CLI that smuggles context into a URL as clean, when those
		// bytes left the machine just the same.
		if r.URL.RawQuery != "" {
			wire.WriteString(r.URL.RawQuery)
			wire.WriteByte('\n')
		}
		wire.Write(b)
		wire.WriteByte('\n')
		res.Requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, stubBody(prof.Dialect))
	}))
	defer upstream.Close()

	mgr, err := secretnat.NewManager(secretnat.Options{Enabled: true, Mode: secretnat.ModeSemantic})
	if err != nil {
		return res, fmt.Errorf("secretnat: %w", err)
	}

	// A private proxy on its own port: probing must not disturb the session's
	// running proxy, its audit trail or its budgets. StartPrivate, not Start —
	// the probe runs inside the live daemon, and the publishing Start would
	// overwrite proxy.Endpoint()/Current() here and clear them on the deferred
	// Stop, leaving every pane spawned afterwards silently ungoverned.
	srv := proxy.New(nil, mgr, nil, map[string]string{prof.Dialect: upstream.URL}, false)
	if _, err := srv.StartPrivate(""); err != nil {
		return res, fmt.Errorf("start probe proxy: %w", err)
	}
	defer srv.Stop()

	scratch, err := makeScratch(scratchBase)
	if err != nil {
		return res, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	baseURL := srv.BaseURL() + "/" + prof.Dialect + "/" + probePaneID
	prompt := "In one sentence: is " + plantedSecret + " a live or test key?"
	argv, env, err := prof.ProbeCommand(bin, baseURL, prompt, scratch, dummyKey)
	if err != nil {
		return res, err
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Dir = scratch
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		// Not fatal, and deliberately so: a CLI may reject the stub response
		// and exit non-zero having already put a governed request on the wire.
		// The grep decides; the error is surfaced, not obeyed.
		res.CLIError = strings.TrimSpace(firstLine(out.String()))
		if res.CLIError == "" {
			res.CLIError = err.Error()
		}
	}

	mu.Lock()
	captured := wire.String()
	requests := res.Requests
	mu.Unlock()

	res.Planted = strings.Count(captured, plantedSecret)
	res.Tokens = len(snatToken.FindAllString(captured, -1))

	switch {
	case requests == 0 && res.CLIError != "":
		// The CLI fell over before it could call anything. Its silence says
		// nothing about base-URL handling, and calling that NOT GOVERNED would
		// record a broken install as a governance failure. Seen for real:
		// gemini-cli crashing on Node 18 (it needs the regex `v` flag from
		// Node 20) never reached the point of making a request.
		res.Inconclusive = true
		res.Reason = "the CLI did not run to completion and made no request, so this " +
			"proves nothing about whether it honours " + prof.BaseURLEnv +
			" — fix the CLI, then re-check (" + res.CLIError + ")"
	case requests == 0:
		res.Reason = "no request reached the proxy — the CLI ignored " +
			prof.BaseURLEnv + " and talked to the provider directly, or never called one"
		if prof.Note != "" {
			res.Reason += " (" + prof.Note + ")"
		}
	case res.Planted > 0:
		res.Reason = fmt.Sprintf("SECURITY FAILURE: the planted secret reached the "+
			"upstream %dx — traffic is proxied but NOT redacted", res.Planted)
	case res.Tokens == 0:
		res.Reason = "traffic was proxied but no SNAT token appeared — the secret " +
			"was dropped rather than translated"
	default:
		res.Governed = true
	}
	return res, nil
}

// makeScratch creates the probe's throwaway working directory under base.
//
// base should be project-local, NOT the system temp dir. Codex refuses to
// create its PATH-alias helper binaries when CODEX_HOME sits under /tmp and
// warns loudly about it — so a probe run from a temp dir prints a scary
// warning next to a verdict that is, in fact, correct. The verdict was never
// affected (it is made on captured bytes, not on the CLI's exit or stderr), but
// a passing check that looks broken is a bug in its own right.
//
// An empty base still falls back to the system temp dir, so a caller with no
// project directory — tests, or a session with no RyshDir — keeps working.
func makeScratch(base string) (string, error) {
	if base != "" {
		if err := os.MkdirAll(base, 0o700); err != nil {
			return "", fmt.Errorf("proxy check: scratch dir: %w", err)
		}
	}
	dir, err := os.MkdirTemp(base, "rysh-proxy-check-*")
	if err != nil {
		return "", fmt.Errorf("proxy check: scratch dir: %w", err)
	}
	return dir, nil
}

// stubBody is a minimal dialect-shaped success, so a real CLI parses the reply
// as a completed turn instead of retrying against it.
func stubBody(dialect string) string {
	switch dialect {
	case "openai":
		return `{"id":"chatcmpl-check","object":"chat.completion","model":"gpt-4o",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},` +
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	case "gemini":
		return `{"candidates":[{"content":{"parts":[{"text":"ok"}],"role":"model"},` +
			`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,` +
			`"candidatesTokenCount":5},"modelVersion":"gemini-2.0"}`
	default:
		return `{"id":"msg_check","type":"message","role":"assistant",` +
			`"content":[{"type":"text","text":"ok"}],"model":"claude-opus-4-8",` +
			`"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
