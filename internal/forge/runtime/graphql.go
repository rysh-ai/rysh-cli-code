// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
)

// GraphQLExecutor executes GraphQL operations against a single endpoint. Unlike
// the REST HTTPExecutor (which maps one operation per tool), GraphQL is exposed
// to agents through a small set of generic tools (query + schema): the agent
// writes the GraphQL query and this executor POSTs {query, variables} to the
// endpoint, applying the same auth, retries, jq-filtering, and redaction.
type GraphQLExecutor struct {
	client   *http.Client
	endpoint string
	// wsEndpoint overrides the websocket URL used for subscriptions; when empty
	// it is derived from endpoint by scheme convention (see WSEndpoint).
	wsEndpoint string
	auth       []ir.AuthScheme
	cred       Credential
	opts       Options
}

// NewGraphQLExecutor builds a GraphQL executor for one endpoint.
func NewGraphQLExecutor(endpoint string, auth []ir.AuthScheme, cred Credential, opts Options) *GraphQLExecutor {
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 2
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.MaxBody <= 0 {
		opts.MaxBody = 4 << 20
	}
	return &GraphQLExecutor{
		client:   &http.Client{Timeout: opts.Timeout},
		endpoint: endpoint,
		auth:     auth,
		cred:     cred,
		opts:     opts,
	}
}

// Endpoint returns the configured GraphQL endpoint URL.
func (e *GraphQLExecutor) Endpoint() string { return e.endpoint }

// Query executes a GraphQL document with optional variables, returning the
// (optionally jq-filtered, optionally redacted) JSON response. A GraphQL
// "errors" array in a 200 response is surfaced as an error.
func (e *GraphQLExecutor) Query(ctx context.Context, query string, variables map[string]any, jqFilter string) (string, error) {
	if strings.TrimSpace(query) == "" {
		return "", fmt.Errorf("graphql query is required")
	}
	payload := map[string]any{"query": query}
	if len(variables) > 0 {
		payload["variables"] = variables
	}
	body, _ := json.Marshal(payload)

	// Resolve the bearer (per-call override > token provider) once up front.
	var bearer string
	if v := bearerFromCtx(ctx); v != "" {
		bearer = v
	} else if e.opts.TokenProvider != nil {
		t, terr := e.opts.TokenProvider(ctx)
		if terr != nil {
			return "", fmt.Errorf("acquire access token: %w", terr)
		}
		bearer = t
	}
	canRefresh := e.opts.TokenRefresher != nil && bearerFromCtx(ctx) == ""
	authRetried := false

	var lastErr error
	for attempt := 0; attempt <= e.opts.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				return "", err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		InjectAuth(req, e.auth, e.cred)
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

		if resp.StatusCode == 401 && !authRetried && canRefresh {
			authRetried = true
			if nt, rerr := e.opts.TokenRefresher(ctx); rerr == nil && nt != "" {
				bearer = nt
				attempt--
				continue
			}
		}

		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			if attempt < e.opts.MaxRetries {
				lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
				continue
			}
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("graphql endpoint %s -> HTTP %d: %s", e.endpoint, resp.StatusCode, truncate(string(data), 1024))
		}

		// Surface GraphQL-level errors (200 with a non-empty "errors" array).
		if gqlErr := graphQLErrors(data); gqlErr != "" {
			return "", fmt.Errorf("graphql errors: %s", gqlErr)
		}

		text := prettyJSON(data)
		if jqFilter != "" {
			if filtered, ferr := ApplyJQ(data, jqFilter); ferr == nil {
				text = filtered
			} else {
				text = fmt.Sprintf("%s\n[jq_filter %q failed: %v]", text, jqFilter, ferr)
			}
		}
		if e.opts.Redact != nil {
			text = e.opts.Redact(text)
		}
		return text, nil
	}
	return "", fmt.Errorf("graphql request to %s failed after %d attempts: %w", e.endpoint, e.opts.MaxRetries+1, lastErr)
}

// graphQLErrors returns a compact rendering of the response's "errors" array, or
// "" if there are none.
func graphQLErrors(data []byte) string {
	var env struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &env); err != nil || len(env.Errors) == 0 {
		return ""
	}
	msgs := make([]string, 0, len(env.Errors))
	for _, e := range env.Errors {
		msgs = append(msgs, e.Message)
	}
	return strings.Join(msgs, "; ")
}
