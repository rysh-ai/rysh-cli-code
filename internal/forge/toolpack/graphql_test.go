package toolpack

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/runtime"
)

func graphqlAPI() *ir.API {
	return &ir.API{
		Name: "gql", Version: "1", SourceType: "graphql",
		Servers: []ir.Server{{URL: "/graphql"}},
		Operations: []ir.Operation{
			{ID: "user", Method: "POST", Path: "/graphql", Summary: "get a user", Tags: []string{"Query"},
				RequestBody: &ir.Schema{Type: "object", Properties: map[string]*ir.Schema{"id": {Type: "string"}}}},
			{ID: "createUser", Method: "POST", Path: "/graphql", Mutating: true, Tags: []string{"Mutation"},
				RequestBody: &ir.Schema{Type: "object", Properties: map[string]*ir.Schema{"name": {Type: "string"}}}},
		},
	}
}

func TestRegisterGraphQL(t *testing.T) {
	api := graphqlAPI()
	reg := sharedtools.NewToolRegistry()
	exec := runtime.NewGraphQLExecutor("https://x/graphql", nil, runtime.Credential{}, runtime.Options{})
	names := RegisterGraphQL(reg, api, exec, Policy{Prefix: "gql_"})

	if len(names) != 2 {
		t.Fatalf("want 2 graphql tools, got %d: %v", len(names), names)
	}
	if _, ok := reg.Get("gql_graphql_query"); !ok {
		t.Fatalf("gql_graphql_query not registered; names=%v", reg.Names())
	}
	schema, ok := reg.Get("gql_graphql_schema")
	if !ok {
		t.Fatalf("gql_graphql_schema not registered")
	}
	out, _ := schema.Execute(context.Background(), json.RawMessage(`{}`))
	if !strings.Contains(out.Content, "user") || !strings.Contains(out.Content, "createUser") {
		t.Fatalf("schema tool did not list fields: %s", out.Content)
	}
}

func TestGraphQLQueryApprovalHeuristic(t *testing.T) {
	api := graphqlAPI()
	reg := sharedtools.NewToolRegistry()
	exec := runtime.NewGraphQLExecutor("https://x/graphql", nil, runtime.Credential{}, runtime.Options{})
	RegisterGraphQL(reg, api, exec, Policy{Prefix: "gql_"})

	q, _ := reg.Get("gql_graphql_query")
	if !q.RequiresApproval(json.RawMessage(`{"query":"mutation { createUser(name:\"x\"){ id } }"}`)) {
		t.Errorf("mutation should require approval")
	}
	if q.RequiresApproval(json.RawMessage(`{"query":"query { user(id:\"1\"){ id } }"}`)) {
		t.Errorf("query should not require approval")
	}
}

func TestGraphQLQueryExecutes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"user":{"id":"u1"}}}`))
	}))
	defer srv.Close()

	api := graphqlAPI()
	reg := sharedtools.NewToolRegistry()
	exec := runtime.NewGraphQLExecutor(srv.URL, nil, runtime.Credential{}, runtime.Options{})
	RegisterGraphQL(reg, api, exec, Policy{Prefix: "gql_"})

	q, _ := reg.Get("gql_graphql_query")
	out, err := q.Execute(context.Background(), json.RawMessage(`{"query":"query { user(id:\"u1\"){ id } }","jq_filter":".data.user.id"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.TrimSpace(out.Content) != `"u1"` {
		t.Fatalf("unexpected output: %q", out.Content)
	}
}
