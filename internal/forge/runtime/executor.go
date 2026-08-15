// SPDX-License-Identifier: Apache-2.0

// Package runtime is the shared, cross-cutting machinery a generated tool-pack
// uses at call time: HTTP request assembly, auth injection, retries/backoff,
// pagination hints, jq-style response trimming, and a secret-redaction hook.
// Generators emit declarative metadata (the IR); this package executes it, so
// the runtime is shared rather than regenerated per API.
package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
)

// Credential carries resolved auth material for an integration. Which fields are
// used depends on the matched AuthScheme.
type Credential struct {
	APIKey   string // apiKey value, or bearer token
	Username string // http basic
	Password string // http basic
}

// bearerCtxKey carries a per-call bearer override (forged-API auth plan, Model B:
// the subscriber's own access token, injected by the owner for that one request
// only and never cached). It takes precedence over Options.TokenProvider.
type bearerCtxKey struct{}

// WithBearer returns a context that overrides the bearer token for a single
// executor call. Used by the owner to run a delegated invocation under the
// subscriber's identity without persisting their token.
func WithBearer(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, bearerCtxKey{}, token)
}

// bearerFromCtx returns the per-call bearer override, or "" if none.
func bearerFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(bearerCtxKey{}).(string); ok {
		return v
	}
	return ""
}

// Options configure an HTTPExecutor.
type Options struct {
	MaxRetries   int                 // retry count for 5xx/429 (default 2)
	Timeout      time.Duration       // per-request timeout (default 30s)
	Redact       func(string) string // applied to response text before return
	MaxBody      int64               // response read cap (default 4 MiB)
	ExtraHeaders map[string]string   // always-applied headers

	// Token auth (forged-API auth plan, Phase A). When TokenProvider is set, the
	// executor fetches a fresh bearer before each request and injects it
	// (superseding the static Credential bearer). On a 401, it calls
	// TokenRefresher once and retries the request a single extra time. Both come
	// from a runtime.TokenManager (TokenProvider=tm.Token, TokenRefresher=tm.Refresh).
	TokenProvider  func(context.Context) (string, error)
	TokenRefresher func(context.Context) (string, error)
	AuthHeader     string // injection header (default "Authorization")
	AuthScheme     string // injection scheme (default "Bearer"); "" ⇒ raw token
}

// HTTPExecutor performs REST calls described by IR operations.
type HTTPExecutor struct {
	client  *http.Client
	baseURL string
	auth    []ir.AuthScheme
	cred    Credential
	opts    Options
}

// NewHTTPExecutor builds an executor for one API.
func NewHTTPExecutor(baseURL string, auth []ir.AuthScheme, cred Credential, opts Options) *HTTPExecutor {
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 2
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.MaxBody <= 0 {
		opts.MaxBody = 4 << 20
	}
	return &HTTPExecutor{
		client:  &http.Client{Timeout: opts.Timeout},
		baseURL: strings.TrimRight(baseURL, "/"),
		auth:    auth,
		cred:    cred,
		opts:    opts,
	}
}

// Call executes op with a flat argument object (path/query/header params plus
// body properties, as produced by Operation.ToolInputSchema). jqFilter, when
// non-empty, trims the JSON response before it is returned (and before it would
// enter a model's context). The returned string is the (optionally filtered,
// optionally redacted) response body.
func (e *HTTPExecutor) Call(ctx context.Context, op *ir.Operation, args map[string]any, jqFilter string) (string, error) {
	reqURL, err := e.buildURL(op, args)
	if err != nil {
		return "", err
	}

	bodyArgs := e.bodyArgs(op, args)
	var rawBody []byte
	mutating := op.Mutating && bodyArgs != nil
	if mutating {
		rawBody, _ = json.Marshal(bodyArgs)
	}

	// Resolve the bearer: a per-call override (Model B) wins over the configured
	// token provider (Model A); both supersede the static Credential bearer.
	bearer, err := e.resolveBearer(ctx)
	if err != nil {
		return "", err
	}

	authRetried := false
	var lastErr error
	for attempt := 0; attempt <= e.opts.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				return "", err
			}
		}

		var bodyReader io.Reader
		if mutating {
			bodyReader = bytes.NewReader(rawBody)
		}
		req, err := http.NewRequestWithContext(ctx, op.Method, reqURL, bodyReader)
		if err != nil {
			return "", err
		}
		if mutating {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		e.applyHeaderParams(req, op, args)
		e.applyAuth(req)
		if bearer != "" {
			setBearer(req, e.opts.AuthHeader, e.opts.AuthScheme, bearer)
		}
		for k, v := range e.opts.ExtraHeaders {
			req.Header.Set(k, v)
		}

		resp, err := e.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, e.opts.MaxBody))
		resp.Body.Close()

		// Reactive auth refresh: on a 401, refresh the token ONCE and retry the
		// request a single extra time (this does not consume a 5xx/429 attempt).
		if resp.StatusCode == 401 && !authRetried && e.canRefresh(ctx) {
			authRetried = true
			if nt, rerr := e.opts.TokenRefresher(ctx); rerr == nil && nt != "" {
				bearer = nt
				attempt-- // free retry, independent of MaxRetries
				continue
			}
		}

		if (resp.StatusCode == 429 || resp.StatusCode >= 500) && attempt < e.opts.MaxRetries {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}

		var text string
		if jqFilter != "" {
			if filtered, ferr := ApplyJQ(data, jqFilter); ferr == nil {
				text = filtered
			} else {
				text = fmt.Sprintf("%s\n[jq_filter %q failed: %v]", prettyJSON(data), jqFilter, ferr)
			}
		} else {
			text = prettyJSON(data)
		}
		if e.opts.Redact != nil {
			text = e.opts.Redact(text)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("%s %s -> HTTP %d: %s", op.Method, op.Path, resp.StatusCode, truncate(text, 1024))
		}
		return text, nil
	}
	return "", fmt.Errorf("%s %s failed after %d attempts: %w", op.Method, op.Path, e.opts.MaxRetries+1, lastErr)
}

func (e *HTTPExecutor) buildURL(op *ir.Operation, args map[string]any) (string, error) {
	path := op.Path
	q := url.Values{}
	for _, p := range op.Params {
		v, ok := args[p.Name]
		if !ok {
			if p.Required && p.In == "path" {
				return "", fmt.Errorf("missing required path parameter %q", p.Name)
			}
			continue
		}
		switch p.In {
		case "path":
			path = strings.ReplaceAll(path, "{"+p.Name+"}", url.PathEscape(toStr(v)))
		case "query":
			q.Set(p.Name, toStr(v))
		}
	}
	full := e.baseURL + path
	if enc := q.Encode(); enc != "" {
		full += "?" + enc
	}
	return full, nil
}

func (e *HTTPExecutor) applyHeaderParams(req *http.Request, op *ir.Operation, args map[string]any) {
	for _, p := range op.Params {
		if p.In != "header" {
			continue
		}
		if v, ok := args[p.Name]; ok {
			req.Header.Set(p.Name, toStr(v))
		}
	}
}

// bodyArgs returns the subset of args that belong in the request body: anything
// not consumed as a path/query/header parameter. A wrapped "body" arg is unwrapped.
func (e *HTTPExecutor) bodyArgs(op *ir.Operation, args map[string]any) map[string]any {
	if !op.Mutating {
		return nil
	}
	if b, ok := args["body"]; ok {
		if m, ok := b.(map[string]any); ok {
			return m
		}
		return map[string]any{"body": b}
	}
	paramNames := map[string]bool{}
	for _, p := range op.Params {
		paramNames[p.Name] = true
	}
	body := map[string]any{}
	for k, v := range args {
		if !paramNames[k] {
			body[k] = v
		}
	}
	if len(body) == 0 {
		return nil
	}
	return body
}

// applyAuth injects credentials per the API's auth schemes. apiKey schemes set a
// header or query param; http bearer sets Authorization: Bearer; http basic sets
// Authorization: Basic.
func (e *HTTPExecutor) applyAuth(req *http.Request) { InjectAuth(req, e.auth, e.cred) }

// resolveBearer returns the bearer to inject for a call: a per-call context
// override (Model B) takes precedence, else the configured token provider
// (Model A), else "" (fall back to the static Credential via InjectAuth).
func (e *HTTPExecutor) resolveBearer(ctx context.Context) (string, error) {
	if v := bearerFromCtx(ctx); v != "" {
		return v, nil
	}
	if e.opts.TokenProvider != nil {
		t, err := e.opts.TokenProvider(ctx)
		if err != nil {
			return "", fmt.Errorf("acquire access token: %w", err)
		}
		return t, nil
	}
	return "", nil
}

// canRefresh reports whether a reactive (401) refresh is possible. A per-call
// override (Model B) is NOT refreshed by the owner — the subscriber owns that
// token, so the owner returns the 401 and the subscriber refreshes + retries.
func (e *HTTPExecutor) canRefresh(ctx context.Context) bool {
	return e.opts.TokenRefresher != nil && bearerFromCtx(ctx) == ""
}

// setBearer injects a bearer token into the configured header/scheme. Defaults to
// "Authorization: Bearer <token>".
func setBearer(req *http.Request, header, scheme, token string) {
	if header == "" {
		header = "Authorization"
	}
	if scheme == "" {
		scheme = "Bearer"
	}
	req.Header.Set(header, scheme+" "+token)
}

// InjectAuth applies the API's auth schemes to an HTTP request with the given
// credentials. Shared by the REST and GraphQL executors. apiKey schemes set a
// header or query param; http bearer/basic and oauth2 set Authorization.
func InjectAuth(req *http.Request, schemes []ir.AuthScheme, cred Credential) {
	for _, scheme := range schemes {
		switch scheme.Type {
		case "apiKey":
			if cred.APIKey == "" {
				continue
			}
			switch scheme.In {
			case "header":
				req.Header.Set(scheme.KeyName, cred.APIKey)
			case "query":
				q := req.URL.Query()
				q.Set(scheme.KeyName, cred.APIKey)
				req.URL.RawQuery = q.Encode()
			}
		case "http":
			switch strings.ToLower(scheme.Scheme) {
			case "bearer":
				if cred.APIKey != "" {
					req.Header.Set("Authorization", "Bearer "+cred.APIKey)
				}
			case "basic":
				if cred.Username != "" {
					token := base64.StdEncoding.EncodeToString([]byte(cred.Username + ":" + cred.Password))
					req.Header.Set("Authorization", "Basic "+token)
				}
			}
		case "oauth2":
			if cred.APIKey != "" {
				req.Header.Set("Authorization", "Bearer "+cred.APIKey)
			}
		}
	}
}

func backoff(attempt int) time.Duration {
	d := time.Duration(attempt*attempt) * 200 * time.Millisecond
	if d > 5*time.Second {
		d = 5 * time.Second
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func toStr(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

func prettyJSON(data []byte) string {
	var buf bytes.Buffer
	if json.Indent(&buf, data, "", "  ") == nil {
		return buf.String()
	}
	return string(data)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
