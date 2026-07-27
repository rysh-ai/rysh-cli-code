package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// graphqlIntrospectionQuery is the standard full introspection document (the
// one graphql-js ships as getIntrospectionQuery). We request the complete
// schema — not just the subset the ingester reads today — so the stored spec
// is a faithful snapshot: future ingester improvements (enums, input objects,
// deprecations) re-generate from the same stored bytes without re-fetching.
const graphqlIntrospectionQuery = `query IntrospectionQuery {
  __schema {
    queryType { name }
    mutationType { name }
    subscriptionType { name }
    types { ...FullType }
    directives { name description locations args { ...InputValue } }
  }
}
fragment FullType on __Type {
  kind name description
  fields(includeDeprecated: true) {
    name description
    args { ...InputValue }
    type { ...TypeRef }
    isDeprecated deprecationReason
  }
  inputFields { ...InputValue }
  interfaces { ...TypeRef }
  enumValues(includeDeprecated: true) { name description isDeprecated deprecationReason }
  possibleTypes { ...TypeRef }
}
fragment InputValue on __InputValue { name description type { ...TypeRef } defaultValue }
fragment TypeRef on __Type {
  kind name
  ofType { kind name
    ofType { kind name
      ofType { kind name
        ofType { kind name
          ofType { kind name
            ofType { kind name
              ofType { kind name } } } } } } }
}`

// GraphQLIntrospect fetches the schema of a RUNNING GraphQL endpoint by
// POSTing the standard introspection query and returns the raw response body
// — the same introspection JSON the file-based GraphQL ingester consumes — so
// callers store it and flow it through the existing pipeline unchanged.
//
// headers are set after the defaults, so a caller-supplied Content-Type or
// Accept wins. timeout <= 0 falls back to 30s: an unbounded hang inside
// `forge add` would look like a wedged CLI.
func GraphQLIntrospect(ctx context.Context, endpoint string, headers http.Header, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	payload, err := json.Marshal(map[string]string{"query": graphqlIntrospectionQuery})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build introspection request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, vs := range headers {
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("introspect %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	// 32 MiB is far above any sane schema; the cap keeps a misbehaving endpoint
	// from exhausting memory.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("read introspection response from %s: %w", endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("introspect %s: HTTP %d: %s", endpoint, resp.StatusCode, snippet(body))
	}

	// Distinguish the three server-side failure shapes so the user gets an
	// actionable message instead of a generic parse error downstream:
	// invalid JSON, a GraphQL errors array (typically "introspection is
	// disabled"), and a well-formed response with no __schema.
	var env struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Data struct {
			Schema json.RawMessage `json:"__schema"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("introspect %s: response is not JSON (%v): %s", endpoint, err, snippet(body))
	}
	if len(env.Errors) > 0 {
		msgs := make([]string, 0, len(env.Errors))
		for _, e := range env.Errors {
			msgs = append(msgs, e.Message)
		}
		return nil, fmt.Errorf("introspect %s: GraphQL errors: %s (introspection may be disabled on this endpoint)",
			endpoint, strings.Join(msgs, "; "))
	}
	if len(env.Data.Schema) == 0 || string(env.Data.Schema) == "null" {
		return nil, fmt.Errorf("introspect %s: response has no __schema — introspection appears to be disabled", endpoint)
	}
	return body, nil
}

// snippet trims a response body for inclusion in an error message.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}
