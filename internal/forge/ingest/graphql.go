package ingest

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
)

// GraphQL converts a GraphQL introspection result (the JSON returned by the
// standard __schema introspection query, with or without the {"data":…}
// envelope) into the IR. Query fields become non-mutating operations and
// Mutation fields become mutating operations, each POSTed to the GraphQL
// endpoint with its arguments modeled as the request body.
//
// Live execution is wired through the generic graphql_schema / graphql_query
// tools (toolpack.RegisterGraphQL + runtime.GraphQLExecutor): the agent
// composes a document and the executor POSTs {query, variables} to the
// endpoint.
//
// Subscription fields are NOT exposed as operations: subscriptions are push
// streams delivered over graphql-ws (or SSE), which a single request/response
// POST tool cannot honestly serve — a "subscription tool" would return nothing
// or hang. They are recorded in API.Skipped so the CLI reports the exclusion
// specifically instead of dropping subscriptionType on the floor.
func GraphQL(data []byte) (*ir.API, error) {
	var env struct {
		Data struct {
			Schema *gqlSchema `json:"__schema"`
		} `json:"data"`
		Schema *gqlSchema `json:"__schema"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("parse GraphQL introspection: %w", err)
	}
	schema := env.Data.Schema
	if schema == nil {
		schema = env.Schema
	}
	if schema == nil {
		return nil, fmt.Errorf("no __schema found in introspection result")
	}

	byName := map[string]*gqlType{}
	for i := range schema.Types {
		byName[schema.Types[i].Name] = &schema.Types[i]
	}

	api := &ir.API{
		Name:       "graphql-api",
		Version:    "introspection",
		SourceType: "graphql",
		Servers:    []ir.Server{{URL: "/graphql"}},
		Schemas:    map[string]*ir.Schema{},
	}

	addRoot := func(ref *gqlTypeRef, mutating bool) {
		if ref == nil {
			return
		}
		t := byName[ref.Name]
		if t == nil {
			return
		}
		fields := append([]gqlField(nil), t.Fields...)
		sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
		for _, f := range fields {
			op := ir.Operation{
				ID:       sanitizeID(f.Name),
				Method:   "POST",
				Path:     "/graphql",
				Summary:  f.Description,
				Mutating: mutating,
				Tags:     []string{ref.Name},
			}
			if len(f.Args) > 0 {
				body := &ir.Schema{Type: "object", Properties: map[string]*ir.Schema{}}
				for _, a := range f.Args {
					body.Properties[a.Name] = &ir.Schema{Type: gqlBaseType(&a.Type), Description: a.Description}
					if isNonNull(&a.Type) {
						body.Required = append(body.Required, a.Name)
					}
				}
				op.RequestBody = body
			}
			api.Operations = append(api.Operations, op)
		}
	}
	addRoot(schema.QueryType, false)
	addRoot(schema.MutationType, true)

	// Subscription fields: counted and reported, never exposed (see the doc
	// comment above).
	if schema.SubscriptionType != nil {
		if t := byName[schema.SubscriptionType.Name]; t != nil {
			fields := append([]gqlField(nil), t.Fields...)
			sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
			for _, f := range fields {
				api.Skipped = append(api.Skipped, ir.SkippedOperation{
					ID:   sanitizeID(f.Name),
					Kind: ir.SkipSubscription,
				})
			}
		}
	}

	if len(api.Operations) == 0 {
		if n := len(api.Skipped); n > 0 {
			return nil, fmt.Errorf("introspection contained no query/mutation fields — only %d subscription field(s), which forge does not expose as tools yet", n)
		}
		return nil, fmt.Errorf("introspection contained no query/mutation fields")
	}
	return api, nil
}

type gqlSchema struct {
	QueryType        *gqlTypeRef `json:"queryType"`
	MutationType     *gqlTypeRef `json:"mutationType"`
	SubscriptionType *gqlTypeRef `json:"subscriptionType"`
	Types            []gqlType   `json:"types"`
}

type gqlTypeRef struct {
	Kind   string      `json:"kind"`
	Name   string      `json:"name"`
	OfType *gqlTypeRef `json:"ofType"`
}

type gqlType struct {
	Kind        string     `json:"kind"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Fields      []gqlField `json:"fields"`
}

type gqlField struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Args        []gqlInputValue `json:"args"`
	Type        gqlTypeRef      `json:"type"`
}

type gqlInputValue struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Type        gqlTypeRef `json:"type"`
}

func isNonNull(t *gqlTypeRef) bool { return t != nil && t.Kind == "NON_NULL" }

// gqlBaseType unwraps NON_NULL/LIST wrappers and maps the named GraphQL scalar
// to a JSON-schema type.
func gqlBaseType(t *gqlTypeRef) string {
	for t != nil && (t.Kind == "NON_NULL" || t.Kind == "LIST") {
		if t.Kind == "LIST" {
			return "array"
		}
		t = t.OfType
	}
	if t == nil {
		return "string"
	}
	switch t.Name {
	case "Int":
		return "integer"
	case "Float":
		return "number"
	case "Boolean":
		return "boolean"
	case "String", "ID":
		return "string"
	default:
		return "object"
	}
}
