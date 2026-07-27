// Package ir defines the Forge Intermediate Representation: a language- and
// target-agnostic model of an API. Every Forge generator (tool-pack, MCP
// server, docs, SDKs) reads only the IR, so adding a new source format (OpenAPI,
// GraphQL, gRPC) means writing one ingester, and adding a new output means
// writing one generator — never an N×M matrix.
package ir

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// API is the normalized description of an API.
type API struct {
	Name        string             `json:"name"`
	Version     string             `json:"version"`
	Description string             `json:"description,omitempty"`
	SourceType  string             `json:"source_type"` // "openapi" | "graphql" | "grpc"
	Servers     []Server           `json:"servers,omitempty"`
	Auth        []AuthScheme       `json:"auth,omitempty"`
	Operations  []Operation        `json:"operations"`
	Schemas     map[string]*Schema `json:"schemas,omitempty"` // named component schemas
	// Skipped lists operations the ingester found in the source spec but
	// deliberately did NOT expose as callable operations (gRPC streaming
	// methods, GraphQL subscription fields). Generators ignore it; the CLI
	// reports it at add time so exclusions are specific instead of silent.
	Skipped []SkippedOperation `json:"skipped,omitempty"`
}

// SkippedOperation records one deliberately-unexposed source operation.
type SkippedOperation struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // one of the Skip* constants below
}

// SkippedOperation.Kind values.
const (
	SkipClientStreaming = "client-streaming" // gRPC client-streaming method
	SkipServerStreaming = "server-streaming" // gRPC server-streaming method
	SkipBidiStreaming   = "bidi-streaming"   // gRPC bidirectional-streaming method
	SkipSubscription    = "subscription"     // GraphQL subscription field
)

// Server is a base URL the API is served from.
type Server struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
}

// AuthScheme describes one authentication mechanism.
type AuthScheme struct {
	Name    string `json:"name"`               // security scheme name (key)
	Type    string `json:"type"`               // "apiKey" | "http" | "oauth2"
	In      string `json:"in,omitempty"`       // apiKey: "header" | "query" | "cookie"
	KeyName string `json:"key_name,omitempty"` // apiKey: the header/query parameter name
	Scheme  string `json:"scheme,omitempty"`   // http: "bearer" | "basic"
}

// Operation is a single callable endpoint.
type Operation struct {
	ID          string             `json:"id"`     // operationId → tool/method name (sanitized)
	Method      string             `json:"method"` // GET/POST/PUT/PATCH/DELETE
	Path        string             `json:"path"`   // /v1/customers/{id}
	Summary     string             `json:"summary,omitempty"`
	Description string             `json:"description,omitempty"`
	Params      []Param            `json:"params,omitempty"`
	RequestBody *Schema            `json:"request_body,omitempty"`
	Responses   map[string]*Schema `json:"responses,omitempty"`
	Auth        []string           `json:"auth,omitempty"`
	Pagination  *PaginationHint    `json:"pagination,omitempty"`
	Mutating    bool               `json:"mutating"` // non-GET ⇒ approval candidate
	Tags        []string           `json:"tags,omitempty"`
}

// Param is a path / query / header / cookie parameter.
type Param struct {
	Name        string  `json:"name"`
	In          string  `json:"in"` // path | query | header | cookie
	Required    bool    `json:"required"`
	Description string  `json:"description,omitempty"`
	Schema      *Schema `json:"schema,omitempty"`
}

// PaginationHint records detected pagination so generators can emit iterators.
type PaginationHint struct {
	Style     string `json:"style"`                // "cursor" | "page" | "offset"
	Param     string `json:"param"`                // request param that drives paging
	NextField string `json:"next_field,omitempty"` // response field holding the next token
}

// Schema is a normalized JSON-Schema node. A reference is represented by Ref
// (the component name); generators resolve it via API.Schemas when needed.
type Schema struct {
	Name                 string             `json:"name,omitempty"` // component name, if a named schema
	Ref                  string             `json:"ref,omitempty"`  // referenced component name
	Type                 string             `json:"type,omitempty"` // object|array|string|number|integer|boolean
	Format               string             `json:"format,omitempty"`
	Description          string             `json:"description,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	Enum                 []string           `json:"enum,omitempty"`
	Nullable             bool               `json:"nullable,omitempty"`
	AdditionalProperties *Schema            `json:"additional_properties,omitempty"`
}

// OperationByID returns the operation with the given ID, or nil.
func (a *API) OperationByID(id string) *Operation {
	for i := range a.Operations {
		if a.Operations[i].ID == id {
			return &a.Operations[i]
		}
	}
	return nil
}

// BaseURL returns the first server URL, or "" if none.
func (a *API) BaseURL() string {
	if len(a.Servers) > 0 {
		return a.Servers[0].URL
	}
	return ""
}

// Resolve dereferences a schema one level against the API's named schemas. It
// returns the input unchanged when it is not a reference.
func (a *API) Resolve(s *Schema) *Schema {
	if s == nil || s.Ref == "" {
		return s
	}
	if named, ok := a.Schemas[s.Ref]; ok {
		return named
	}
	return s
}

// ToolInputSchema builds a single flat JSON-Schema object for an operation by
// merging its path/query/header parameters and request body into one object —
// the convention agent tool callers expect (one flat input object per tool).
// A non-object body is wrapped under a "body" property.
func (op *Operation) ToolInputSchema(api *API) json.RawMessage {
	props := map[string]*Schema{}
	var required []string

	for i := range op.Params {
		p := op.Params[i]
		if p.In == "cookie" {
			continue // cookies aren't model-supplied tool args
		}
		sc := p.Schema
		if sc == nil {
			sc = &Schema{Type: "string"}
		}
		props[p.Name] = sc
		if p.Required {
			required = append(required, p.Name)
		}
	}

	if op.RequestBody != nil {
		body := api.Resolve(op.RequestBody)
		if body != nil && body.Type == "object" && len(body.Properties) > 0 {
			for name, ps := range body.Properties {
				props[name] = ps
			}
			required = append(required, body.Required...)
		} else if body != nil {
			props["body"] = op.RequestBody
			required = append(required, "body")
		}
	}

	root := &Schema{Type: "object", Properties: props, Required: dedupe(required)}
	return root.JSONSchema(api)
}

// JSONSchema renders a Schema as a draft-07-ish JSON Schema document (the shape
// tools.ToolSpec.Parameters and MCP inputSchema expect). References are inlined
// one level to keep the schema self-contained for the model.
func (s *Schema) JSONSchema(api *API) json.RawMessage {
	var buf bytes.Buffer
	writeSchemaJSON(&buf, s, api, 0)
	return json.RawMessage(buf.Bytes())
}

func writeSchemaJSON(buf *bytes.Buffer, s *Schema, api *API, depth int) {
	if s == nil {
		buf.WriteString(`{"type":"object"}`)
		return
	}
	if s.Ref != "" && api != nil && depth < 6 {
		if resolved := api.Schemas[s.Ref]; resolved != nil {
			writeSchemaJSON(buf, resolved, api, depth+1)
			return
		}
	}

	obj := map[string]any{}
	t := s.Type
	if t == "" && len(s.Properties) > 0 {
		t = "object"
	}
	if t != "" {
		obj["type"] = t
	}
	if s.Format != "" {
		obj["format"] = s.Format
	}
	if s.Description != "" {
		obj["description"] = s.Description
	}
	if len(s.Enum) > 0 {
		obj["enum"] = s.Enum
	}
	if t == "object" || len(s.Properties) > 0 {
		propsRaw := map[string]json.RawMessage{}
		for name, ps := range s.Properties {
			var b bytes.Buffer
			writeSchemaJSON(&b, ps, api, depth+1)
			propsRaw[name] = json.RawMessage(b.Bytes())
		}
		obj["properties"] = propsRaw
		if len(s.Required) > 0 {
			obj["required"] = s.Required
		}
	}
	if t == "array" {
		var b bytes.Buffer
		writeSchemaJSON(&b, s.Items, api, depth+1)
		obj["items"] = json.RawMessage(b.Bytes())
	}
	data, err := json.Marshal(obj)
	if err != nil {
		buf.WriteString(`{"type":"object"}`)
		return
	}
	buf.Write(data)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// SortedOperations returns operations sorted by ID for deterministic output.
func (a *API) SortedOperations() []Operation {
	ops := make([]Operation, len(a.Operations))
	copy(ops, a.Operations)
	sort.Slice(ops, func(i, j int) bool { return ops[i].ID < ops[j].ID })
	return ops
}

// Tags returns the sorted unique set of tags across all operations.
func (a *API) Tags() []string {
	seen := map[string]bool{}
	for _, op := range a.Operations {
		for _, t := range op.Tags {
			seen[t] = true
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Slug returns a filesystem/URL-safe lower-case identifier for the API name.
func (a *API) Slug() string {
	return Slugify(a.Name)
}

// Slugify lower-cases and replaces non-alphanumeric runs with single hyphens.
func Slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
