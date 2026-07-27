package ingest

import "testing"

const sampleOpenAPI = `{
  "openapi": "3.0.0",
  "info": {"title": "Petstore", "version": "1.2.3"},
  "servers": [{"url": "https://api.example.com"}],
  "components": {
    "securitySchemes": {"apiKey": {"type": "apiKey", "in": "header", "name": "X-Api-Key"}},
    "schemas": {"Pet": {"type": "object", "properties": {"name": {"type": "string"}}, "required": ["name"]}}
  },
  "security": [{"apiKey": []}],
  "paths": {
    "/pets": {
      "get": {"operationId": "listPets", "summary": "List pets",
        "parameters": [{"name": "limit", "in": "query", "schema": {"type": "integer"}}, {"name": "cursor", "in": "query", "schema": {"type": "string"}}],
        "responses": {"200": {"description": "ok"}}},
      "post": {"operationId": "createPet", "summary": "Create a pet",
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Pet"}}}},
        "responses": {"201": {"description": "created"}}}
    },
    "/pets/{id}": {
      "get": {"operationId": "getPet",
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {"200": {"description": "ok"}}}
    }
  }
}`

func TestOpenAPIIngest(t *testing.T) {
	api, err := OpenAPI([]byte(sampleOpenAPI))
	if err != nil {
		t.Fatalf("OpenAPI: %v", err)
	}
	if api.Name != "Petstore" || api.Version != "1.2.3" {
		t.Fatalf("info wrong: %s %s", api.Name, api.Version)
	}
	if api.BaseURL() != "https://api.example.com" {
		t.Fatalf("base url: %s", api.BaseURL())
	}
	if len(api.Operations) != 3 {
		t.Fatalf("want 3 operations, got %d", len(api.Operations))
	}
	if len(api.Auth) != 1 || api.Auth[0].KeyName != "X-Api-Key" {
		t.Fatalf("auth wrong: %+v", api.Auth)
	}

	create := api.OperationByID("createPet")
	if create == nil || !create.Mutating {
		t.Fatalf("createPet missing or not mutating: %+v", create)
	}
	body := api.Resolve(create.RequestBody)
	if body == nil || body.Properties["name"] == nil {
		t.Fatalf("createPet body did not resolve $ref: %+v", body)
	}

	list := api.OperationByID("listPets")
	if list == nil || list.Pagination == nil || list.Pagination.Style != "cursor" {
		t.Fatalf("listPets pagination not detected: %+v", list)
	}

	get := api.OperationByID("getPet")
	if get == nil {
		t.Fatalf("getPet missing")
	}
	var foundPath bool
	for _, p := range get.Params {
		if p.Name == "id" && p.In == "path" && p.Required {
			foundPath = true
		}
	}
	if !foundPath {
		t.Fatalf("getPet path param not required: %+v", get.Params)
	}

	// ToolInputSchema merges params + body into a flat object.
	schema := create.ToolInputSchema(api)
	if !contains(string(schema), `"name"`) {
		t.Fatalf("createPet tool schema missing body field: %s", schema)
	}
}

func TestOpenAPIRejectsNonV3(t *testing.T) {
	if _, err := OpenAPI([]byte(`{"swagger":"2.0","paths":{}}`)); err == nil {
		t.Fatalf("expected error for swagger 2.0")
	}
}

const sampleGraphQL = `{"data":{"__schema":{
  "queryType":{"name":"Query"},
  "mutationType":{"name":"Mutation"},
  "types":[
    {"kind":"OBJECT","name":"Query","fields":[
      {"name":"user","description":"get a user","args":[{"name":"id","type":{"kind":"NON_NULL","ofType":{"kind":"SCALAR","name":"ID"}}}],"type":{"kind":"OBJECT","name":"User"}}
    ]},
    {"kind":"OBJECT","name":"Mutation","fields":[
      {"name":"createUser","args":[{"name":"name","type":{"kind":"SCALAR","name":"String"}}],"type":{"kind":"OBJECT","name":"User"}}
    ]}
  ]
}}}`

func TestGraphQLIngest(t *testing.T) {
	api, err := GraphQL([]byte(sampleGraphQL))
	if err != nil {
		t.Fatalf("GraphQL: %v", err)
	}
	if api.SourceType != "graphql" || len(api.Operations) != 2 {
		t.Fatalf("graphql ingest wrong: %s %d ops", api.SourceType, len(api.Operations))
	}
	user := api.OperationByID("user")
	if user == nil || user.Mutating {
		t.Fatalf("user op wrong: %+v", user)
	}
	if user.RequestBody == nil || user.RequestBody.Properties["id"] == nil {
		t.Fatalf("user args not modeled: %+v", user.RequestBody)
	}
	create := api.OperationByID("createUser")
	if create == nil || !create.Mutating {
		t.Fatalf("createUser should be mutating: %+v", create)
	}
	if len(api.Skipped) != 0 {
		t.Fatalf("schema without subscriptions reported skipped ops: %+v", api.Skipped)
	}
}

const sampleGraphQLWithSubs = `{"data":{"__schema":{
  "queryType":{"name":"Query"},
  "mutationType":{"name":"Mutation"},
  "subscriptionType":{"name":"Subscription"},
  "types":[
    {"kind":"OBJECT","name":"Query","fields":[
      {"name":"user","args":[{"name":"id","type":{"kind":"NON_NULL","ofType":{"kind":"SCALAR","name":"ID"}}}],"type":{"kind":"OBJECT","name":"User"}}
    ]},
    {"kind":"OBJECT","name":"Mutation","fields":[
      {"name":"createUser","args":[{"name":"name","type":{"kind":"SCALAR","name":"String"}}],"type":{"kind":"OBJECT","name":"User"}}
    ]},
    {"kind":"OBJECT","name":"Subscription","fields":[
      {"name":"onUserCreated","args":[],"type":{"kind":"OBJECT","name":"User"}},
      {"name":"onEvent","args":[{"name":"topic","type":{"kind":"SCALAR","name":"String"}}],"type":{"kind":"OBJECT","name":"User"}}
    ]}
  ]
}}}`

// TestGraphQLSubscriptionsAreStreams guards the subscriptionType handling
// (design 015 §2.1): subscription fields are never exposed as POST operations
// (they are graphql-ws push streams) — they land in api.Streams, fully modeled
// (args included), so the runtime can expose them as streaming sessions and
// the CLI can report them specifically.
func TestGraphQLSubscriptionsAreStreams(t *testing.T) {
	api, err := GraphQL([]byte(sampleGraphQLWithSubs))
	if err != nil {
		t.Fatalf("GraphQL: %v", err)
	}
	if len(api.Operations) != 2 {
		t.Fatalf("want 2 operations (query+mutation only), got %d", len(api.Operations))
	}
	for _, id := range []string{"onUserCreated", "onEvent"} {
		if api.OperationByID(id) != nil {
			t.Errorf("subscription field %s was exposed as an operation", id)
		}
	}
	if len(api.Skipped) != 0 {
		t.Fatalf("Skipped = %+v, want none (subscriptions are streams now)", api.Skipped)
	}
	if len(api.Streams) != 2 {
		t.Fatalf("Streams = %+v, want the 2 subscription fields", api.Streams)
	}
	// Sorted by field name for deterministic output.
	if api.Streams[0].ID != "onEvent" || api.Streams[1].ID != "onUserCreated" {
		t.Errorf("Stream IDs = [%s %s], want [onEvent onUserCreated]", api.Streams[0].ID, api.Streams[1].ID)
	}
	onEvent := api.StreamByID("onEvent")
	if onEvent.RequestBody == nil || onEvent.RequestBody.Properties["topic"] == nil {
		t.Errorf("subscription args not modeled: %+v", onEvent.RequestBody)
	}
	if onEvent.Mutating {
		t.Errorf("subscriptions are reads; onEvent must be non-mutating")
	}
}

// TestGraphQLOnlySubscriptionsAccepted: a schema with ONLY subscription fields
// is addable now — the fields are exposed as streaming sessions.
func TestGraphQLOnlySubscriptionsAccepted(t *testing.T) {
	const onlySubs = `{"data":{"__schema":{
	  "subscriptionType":{"name":"Subscription"},
	  "types":[
	    {"kind":"OBJECT","name":"Subscription","fields":[
	      {"name":"onEvent","args":[],"type":{"kind":"OBJECT","name":"Event"}}
	    ]}
	  ]
	}}}`
	api, err := GraphQL([]byte(onlySubs))
	if err != nil {
		t.Fatalf("GraphQL() with only subscriptions should be accepted, got: %v", err)
	}
	if len(api.Operations) != 0 || len(api.Streams) != 1 {
		t.Fatalf("ops=%d streams=%d, want 0 ops and 1 stream", len(api.Operations), len(api.Streams))
	}
}

// TestGraphQLNoFieldsRejected pins the error when a schema has no callable or
// streamable fields at all.
func TestGraphQLNoFieldsRejected(t *testing.T) {
	const empty = `{"data":{"__schema":{"types":[]}}}`
	_, err := GraphQL([]byte(empty))
	if err == nil {
		t.Fatal("GraphQL() with no fields returned nil error, want an error")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
