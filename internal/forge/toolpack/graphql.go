// SPDX-License-Identifier: Apache-2.0

package toolpack

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/runtime"
)

// RegisterGraphQL exposes a GraphQL API to agents through constant-footprint
// tools — <prefix>graphql_schema (discover query/mutation/subscription fields
// from the IR) and <prefix>graphql_query (execute a GraphQL document); when the
// schema has subscription fields AND sm is non-nil, it also registers
// <prefix>graphql_subscribe + <prefix>stream_session (streaming sessions over
// graphql-ws, design 015 §2.1). This mirrors the dynamic REST exposure: the
// agent discovers, then invokes, without flooding the context. Returns the
// registered tool names.
func RegisterGraphQL(reg *sharedtools.ToolRegistry, api *ir.API, exec *runtime.GraphQLExecutor, sm *runtime.StreamManager, pol Policy) []string {
	prefix := pol.Prefix
	schemaName := uniqueName(reg, SanitizeName(prefix+"graphql_schema"))
	queryName := uniqueName(reg, SanitizeName(prefix+"graphql_query"))

	reg.Register(schemaName, &gqlSchemaExecutor{
		api: api,
		spec: sharedtools.ToolSpec{
			Name:        schemaName,
			Description: fmt.Sprintf("[forge:%s] List the GraphQL API's query and mutation fields (with arguments) so you can compose a query for graphql_query.", api.Name),
			Parameters:  json.RawMessage(`{"type":"object","properties":{"filter":{"type":"string","description":"optional keyword filter over field name / args / kind"}}}`),
		},
	})
	reg.Register(queryName, &gqlQueryExecutor{
		exec:          exec,
		api:           api,
		forceApproval: pol.ForceApproval,
		spec: sharedtools.ToolSpec{
			Name:        queryName,
			Description: fmt.Sprintf("[forge:%s] Execute a GraphQL document against the endpoint. Pass the full query/mutation string and optional variables. Mutations require approval. Optionally pass jq_filter to trim the response.", api.Name),
			Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"the GraphQL document, e.g. 'query($id:ID!){ user(id:$id){ id name } }'"},"variables":{"type":"object","description":"GraphQL variables object"},"jq_filter":{"type":"string","description":"optional jq-style filter to trim the JSON response, e.g. .data.user.id"}},"required":["query"]}`),
		},
	})
	names := []string{schemaName, queryName}
	if sm != nil {
		names = append(names, registerGraphQLStreams(reg, api, exec, sm, pol)...)
	}
	return names
}

// gqlSchemaExecutor returns the GraphQL field catalog from the IR.
type gqlSchemaExecutor struct {
	api  *ir.API
	spec sharedtools.ToolSpec
}

func (e *gqlSchemaExecutor) Spec() sharedtools.ToolSpec            { return e.spec }
func (e *gqlSchemaExecutor) RequiresApproval(json.RawMessage) bool { return false }

func (e *gqlSchemaExecutor) Execute(_ context.Context, params json.RawMessage) (*sharedtools.ToolOutput, error) {
	var p struct {
		Filter string `json:"filter"`
	}
	_ = json.Unmarshal(params, &p)
	q := strings.ToLower(strings.TrimSpace(p.Filter))

	ops := e.api.SortedOperations()
	var sb strings.Builder
	fmt.Fprintf(&sb, "GraphQL fields for %q:\n", e.api.Name)
	for i := range ops {
		op := &ops[i]
		kind := "query"
		if op.Mutating {
			kind = "mutation"
		}
		args := gqlArgs(e.api, op)
		hay := strings.ToLower(op.ID + " " + kind + " " + args)
		if q != "" && !strings.Contains(hay, q) {
			continue
		}
		fmt.Fprintf(&sb, "  %-9s %s(%s)\n", kind, op.ID, args)
		if op.Summary != "" {
			fmt.Fprintf(&sb, "            %s\n", op.Summary)
		}
	}
	// Subscription fields are streams: point at graphql_subscribe, not graphql_query.
	for i := range e.api.Streams {
		op := &e.api.Streams[i]
		args := gqlArgs(e.api, op)
		hay := strings.ToLower(op.ID + " subscription " + args)
		if q != "" && !strings.Contains(hay, q) {
			continue
		}
		fmt.Fprintf(&sb, "  %-9s %s(%s) — streaming; start with graphql_subscribe\n", "subscription", op.ID, args)
		if op.Summary != "" {
			fmt.Fprintf(&sb, "            %s\n", op.Summary)
		}
	}
	sb.WriteString("\nCompose a document and run it with graphql_query, e.g.\n  query { <field>(<args>) { <fields you want> } }")
	return &sharedtools.ToolOutput{Content: sb.String()}, nil
}

// gqlArgs renders an operation's argument list "name: type, …" from the IR body.
func gqlArgs(api *ir.API, op *ir.Operation) string {
	body := api.Resolve(op.RequestBody)
	if body == nil || len(body.Properties) == 0 {
		return ""
	}
	names := make([]string, 0, len(body.Properties))
	for n := range body.Properties {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		t := "String"
		if body.Properties[n] != nil && body.Properties[n].Type != "" {
			t = body.Properties[n].Type
		}
		parts = append(parts, n+": "+t)
	}
	return strings.Join(parts, ", ")
}

// gqlQueryExecutor executes an arbitrary GraphQL document.
type gqlQueryExecutor struct {
	exec          *runtime.GraphQLExecutor
	api           *ir.API
	forceApproval bool
	spec          sharedtools.ToolSpec
}

func (e *gqlQueryExecutor) Spec() sharedtools.ToolSpec { return e.spec }

// RequiresApproval gates mutations (the document's leading operation type is
// "mutation"), or everything when forceApproval is set.
func (e *gqlQueryExecutor) RequiresApproval(params json.RawMessage) bool {
	if e.forceApproval {
		return true
	}
	var p struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal(params, &p)
	return isGraphQLMutation(p.Query)
}

func (e *gqlQueryExecutor) Execute(ctx context.Context, params json.RawMessage) (*sharedtools.ToolOutput, error) {
	var p struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
		JQFilter  string         `json:"jq_filter"`
	}
	if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.Query) == "" {
		return &sharedtools.ToolOutput{Error: "parameter 'query' (a GraphQL document) is required"}, nil
	}
	out, err := e.exec.Query(ctx, p.Query, p.Variables, p.JQFilter)
	if err != nil {
		return &sharedtools.ToolOutput{Error: err.Error()}, nil
	}
	return &sharedtools.ToolOutput{Content: out, Metadata: map[string]string{"integration": e.api.Name, "kind": "graphql"}}, nil
}

// isGraphQLMutation reports whether a document's first operation is a mutation.
func isGraphQLMutation(query string) bool {
	s := strings.TrimSpace(query)
	// Skip a leading BOM/comment lines.
	for strings.HasPrefix(s, "#") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = strings.TrimSpace(s[i+1:])
		} else {
			break
		}
	}
	return strings.HasPrefix(strings.ToLower(s), "mutation")
}
