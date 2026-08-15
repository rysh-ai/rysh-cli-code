// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
)

// StartServerStream opens a gRPC server-streaming call through a
// grpc-gateway-style JSON-over-HTTP bridge and pumps its frames into a
// streaming session (design 015 §2.2).
//
// Wire contract: the request message is POSTed as JSON to the method's path;
// the response is newline-delimited JSON where each line is either
//
//	{"result": <response message>}    — one stream message
//	{"error":  {"message": …, …}}     — a terminal error
//
// (grpc-gateway's streaming envelope). Some bridges emit bare message objects
// per line; those are passed through as-is. Connect's enveloped binary framing
// is NOT supported — this consumer only speaks NDJSON.
//
// The HTTP client used here deliberately has NO overall timeout (a stream is
// long-lived by design); cancellation comes from the session context — Stop,
// idle expiry, max lifetime, or manager CloseAll. Each frame is capped at
// Options.MaxBody and redacted with Options.Redact before it enters the ring.
func (e *HTTPExecutor) StartServerStream(sm *StreamManager, op *ir.Operation, args map[string]any) (string, error) {
	reqURL, err := e.buildURL(op, args)
	if err != nil {
		return "", err
	}
	// gRPC methods carry no path/query params: the whole flat arg object IS the
	// request message (matching Operation.ToolInputSchema for a body-only op).
	body, _ := json.Marshal(args)
	if len(args) == 0 {
		body = []byte("{}")
	}

	// Resolve auth material up front, on the caller's context.
	bearer, err := e.resolveBearer(context.Background())
	if err != nil {
		return "", err
	}

	client := &http.Client{} // no Timeout: the session context governs lifetime

	return sm.Start("grpc-stream", op.ID, func(ctx context.Context, push func(string)) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		e.applyAuth(req)
		if bearer != "" {
			setBearer(req, e.opts.AuthHeader, e.opts.AuthScheme, bearer)
		}
		for k, v := range e.opts.ExtraHeaders {
			req.Header.Set(k, v)
		}

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("open stream %s: %w", op.ID, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return fmt.Errorf("stream %s -> HTTP %d: %s", op.ID, resp.StatusCode, truncate(e.redact(string(data)), 1024))
		}

		return e.consumeNDJSON(resp.Body, push)
	})
}

// consumeNDJSON reads newline-delimited JSON frames until EOF or a terminal
// error frame, pushing each decoded frame (redacted, pretty-printed).
func (e *HTTPExecutor) consumeNDJSON(r io.Reader, push func(string)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), int(e.opts.MaxBody))
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var env struct {
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		switch {
		case json.Unmarshal(line, &env) == nil && len(env.Error) > 0:
			// Terminal grpc-gateway error frame: surface it as the end reason.
			return fmt.Errorf("stream error frame: %s", truncate(e.redact(compactJSON(env.Error)), 1024))
		case json.Unmarshal(line, &env) == nil && len(env.Result) > 0:
			push(e.redact(prettyJSON(env.Result)))
		default:
			// Bare per-line message (bridge without the {"result":…} envelope).
			push(e.redact(prettyJSON(append([]byte(nil), line...))))
		}
	}
	if err := sc.Err(); err != nil {
		if err == bufio.ErrTooLong {
			return fmt.Errorf("stream frame exceeded the %d-byte cap", e.opts.MaxBody)
		}
		return err
	}
	return nil // clean EOF: server completed the stream
}

// redact applies the configured redactor, if any.
func (e *HTTPExecutor) redact(s string) string {
	if e.opts.Redact != nil {
		return e.opts.Redact(s)
	}
	return s
}

// compactJSON renders raw JSON on one line (for error frames in end reasons).
func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return buf.String()
}
