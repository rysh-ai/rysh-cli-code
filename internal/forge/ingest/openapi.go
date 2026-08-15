// SPDX-License-Identifier: Apache-2.0

// Package ingest converts source API descriptions into the Forge IR. OpenAPI
// 3.0/3.1 is the primary format; GraphQL (introspection) is also supported.
package ingest

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
)

// OpenAPI parses an OpenAPI 3.0/3.1 document (JSON or YAML) into the IR.
func OpenAPI(data []byte) (*ir.API, error) {
	jsonData, err := toJSON(data)
	if err != nil {
		return nil, fmt.Errorf("decode spec: %w", err)
	}

	var doc oasDoc
	if err := json.Unmarshal(jsonData, &doc); err != nil {
		return nil, fmt.Errorf("parse OpenAPI: %w", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		return nil, fmt.Errorf("unsupported OpenAPI version %q (want 3.x)", doc.OpenAPI)
	}

	api := &ir.API{
		Name:        firstNonEmpty(doc.Info.Title, "api"),
		Version:     firstNonEmpty(doc.Info.Version, "0.0.0"),
		Description: doc.Info.Description,
		SourceType:  "openapi",
		Schemas:     map[string]*ir.Schema{},
	}
	for _, s := range doc.Servers {
		api.Servers = append(api.Servers, ir.Server{URL: s.URL, Description: s.Description})
	}

	// Named component schemas first, so operations can reference them.
	for name, sc := range doc.Components.Schemas {
		conv := convSchema(sc)
		conv.Name = name
		api.Schemas[name] = conv
	}

	// Security schemes.
	secNames := make([]string, 0, len(doc.Components.SecuritySchemes))
	for name := range doc.Components.SecuritySchemes {
		secNames = append(secNames, name)
	}
	sort.Strings(secNames)
	for _, name := range secNames {
		ss := doc.Components.SecuritySchemes[name]
		api.Auth = append(api.Auth, ir.AuthScheme{
			Name:    name,
			Type:    ss.Type,
			In:      ss.In,
			KeyName: ss.Name,
			Scheme:  ss.Scheme,
		})
	}

	// Operations, in a deterministic path order.
	paths := make([]string, 0, len(doc.Paths))
	for p := range doc.Paths {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, path := range paths {
		item := doc.Paths[path]
		for _, m := range methodOrder {
			oasOp := item.op(m)
			if oasOp == nil {
				continue
			}
			op := convOperation(path, m, oasOp, item.Parameters, doc.Security)
			api.Operations = append(api.Operations, op)
		}
	}

	if len(api.Operations) == 0 {
		return nil, fmt.Errorf("spec contains no operations")
	}
	return api, nil
}

// toJSON returns the input as JSON, converting from YAML if it is not already
// JSON. YAML is decoded to a generic value then re-encoded as JSON so the typed
// structs only need json tags.
func toJSON(data []byte) ([]byte, error) {
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return data, nil
	}
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	v = normalizeYAML(v)
	return json.Marshal(v)
}

// normalizeYAML converts map[interface{}]interface{} (yaml.v2 style) and nested
// values into JSON-marshalable map[string]interface{}. yaml.v3 already yields
// map[string]interface{}, but we normalize defensively.
func normalizeYAML(v any) any {
	switch t := v.(type) {
	case map[any]any:
		m := map[string]any{}
		for k, val := range t {
			m[fmt.Sprintf("%v", k)] = normalizeYAML(val)
		}
		return m
	case map[string]any:
		for k, val := range t {
			t[k] = normalizeYAML(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = normalizeYAML(val)
		}
		return t
	default:
		return v
	}
}

// ---- OpenAPI document structs (subset we consume) ----

type oasDoc struct {
	OpenAPI    string                 `json:"openapi"`
	Info       oasInfo                `json:"info"`
	Servers    []oasServer            `json:"servers"`
	Paths      map[string]oasPathItem `json:"paths"`
	Components oasComponents          `json:"components"`
	Security   []map[string][]string  `json:"security"`
}

type oasInfo struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

type oasServer struct {
	URL         string `json:"url"`
	Description string `json:"description"`
}

type oasPathItem struct {
	Get        *oasOperation `json:"get"`
	Post       *oasOperation `json:"post"`
	Put        *oasOperation `json:"put"`
	Patch      *oasOperation `json:"patch"`
	Delete     *oasOperation `json:"delete"`
	Head       *oasOperation `json:"head"`
	Options    *oasOperation `json:"options"`
	Parameters []oasParam    `json:"parameters"`
}

func (p oasPathItem) op(method string) *oasOperation {
	switch method {
	case "GET":
		return p.Get
	case "POST":
		return p.Post
	case "PUT":
		return p.Put
	case "PATCH":
		return p.Patch
	case "DELETE":
		return p.Delete
	case "HEAD":
		return p.Head
	case "OPTIONS":
		return p.Options
	}
	return nil
}

var methodOrder = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}

type oasOperation struct {
	OperationID string                 `json:"operationId"`
	Summary     string                 `json:"summary"`
	Description string                 `json:"description"`
	Tags        []string               `json:"tags"`
	Parameters  []oasParam             `json:"parameters"`
	RequestBody *oasRequestBody        `json:"requestBody"`
	Responses   map[string]oasResponse `json:"responses"`
	Security    []map[string][]string  `json:"security"`
}

type oasParam struct {
	Name        string     `json:"name"`
	In          string     `json:"in"`
	Required    bool       `json:"required"`
	Description string     `json:"description"`
	Schema      *oasSchema `json:"schema"`
}

type oasRequestBody struct {
	Required bool                    `json:"required"`
	Content  map[string]oasMediaType `json:"content"`
}

type oasResponse struct {
	Description string                  `json:"description"`
	Content     map[string]oasMediaType `json:"content"`
}

type oasMediaType struct {
	Schema *oasSchema `json:"schema"`
}

type oasComponents struct {
	Schemas         map[string]*oasSchema   `json:"schemas"`
	SecuritySchemes map[string]oasSecScheme `json:"securitySchemes"`
}

type oasSecScheme struct {
	Type   string `json:"type"`
	In     string `json:"in"`
	Name   string `json:"name"`
	Scheme string `json:"scheme"`
}

type oasSchema struct {
	Ref                  string                `json:"$ref"`
	Type                 string                `json:"type"`
	Format               string                `json:"format"`
	Description          string                `json:"description"`
	Properties           map[string]*oasSchema `json:"properties"`
	Required             []string              `json:"required"`
	Items                *oasSchema            `json:"items"`
	Enum                 []any                 `json:"enum"`
	Nullable             bool                  `json:"nullable"`
	AllOf                []*oasSchema          `json:"allOf"`
	AdditionalProperties json.RawMessage       `json:"additionalProperties"`
}

// ---- conversion to IR ----

func convOperation(path, method string, o *oasOperation, pathParams []oasParam, docSec []map[string][]string) ir.Operation {
	op := ir.Operation{
		ID:          operationID(o.OperationID, method, path),
		Method:      method,
		Path:        path,
		Summary:     o.Summary,
		Description: o.Description,
		Tags:        o.Tags,
		Mutating:    method != "GET" && method != "HEAD" && method != "OPTIONS",
		Responses:   map[string]*ir.Schema{},
	}

	// Path-level params first, then operation-level (operation overrides).
	seen := map[string]bool{}
	add := func(ps []oasParam) {
		for _, p := range ps {
			key := p.In + ":" + p.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			op.Params = append(op.Params, ir.Param{
				Name:        p.Name,
				In:          p.In,
				Required:    p.Required || p.In == "path",
				Description: p.Description,
				Schema:      convSchema(p.Schema),
			})
		}
	}
	add(o.Parameters)
	add(pathParams)

	if o.RequestBody != nil {
		if mt, ok := pickJSONContent(o.RequestBody.Content); ok {
			op.RequestBody = convSchema(mt.Schema)
		}
	}

	for code, resp := range o.Responses {
		if mt, ok := pickJSONContent(resp.Content); ok {
			op.Responses[code] = convSchema(mt.Schema)
		}
	}

	// Security requirement names (operation overrides document-level).
	sec := o.Security
	if sec == nil {
		sec = docSec
	}
	for _, req := range sec {
		for name := range req {
			op.Auth = append(op.Auth, name)
		}
	}
	sort.Strings(op.Auth)

	op.Pagination = detectPagination(&op)
	return op
}

func convSchema(s *oasSchema) *ir.Schema {
	if s == nil {
		return nil
	}
	if s.Ref != "" {
		return &ir.Schema{Ref: refName(s.Ref)}
	}
	// Collapse allOf by merging member properties (a common spec idiom).
	if len(s.AllOf) > 0 {
		merged := &ir.Schema{Type: "object", Properties: map[string]*ir.Schema{}}
		for _, m := range s.AllOf {
			cm := convSchema(m)
			cm = resolveInline(cm)
			if cm == nil {
				continue
			}
			for k, v := range cm.Properties {
				merged.Properties[k] = v
			}
			merged.Required = append(merged.Required, cm.Required...)
		}
		merged.Description = s.Description
		return merged
	}

	out := &ir.Schema{
		Type:        s.Type,
		Format:      s.Format,
		Description: s.Description,
		Required:    s.Required,
		Nullable:    s.Nullable,
		Enum:        enumStrings(s.Enum),
	}
	if len(s.Properties) > 0 {
		out.Properties = map[string]*ir.Schema{}
		for name, ps := range s.Properties {
			out.Properties[name] = convSchema(ps)
		}
		if out.Type == "" {
			out.Type = "object"
		}
	}
	if s.Items != nil {
		out.Items = convSchema(s.Items)
		if out.Type == "" {
			out.Type = "array"
		}
	}
	return out
}

// resolveInline is a no-op placeholder for allOf members that are refs; for
// MVP we keep ref members as-is (their props merge only if already inline).
func resolveInline(s *ir.Schema) *ir.Schema { return s }

func detectPagination(op *ir.Operation) *ir.PaginationHint {
	for _, p := range op.Params {
		switch strings.ToLower(p.Name) {
		case "cursor", "page_token", "pagetoken", "next", "starting_after":
			return &ir.PaginationHint{Style: "cursor", Param: p.Name}
		case "page":
			return &ir.PaginationHint{Style: "page", Param: p.Name}
		case "offset", "skip":
			return &ir.PaginationHint{Style: "offset", Param: p.Name}
		}
	}
	return nil
}

func pickJSONContent(content map[string]oasMediaType) (oasMediaType, bool) {
	if mt, ok := content["application/json"]; ok {
		return mt, true
	}
	// Fall back to any */json content type.
	for ct, mt := range content {
		if strings.Contains(ct, "json") {
			return mt, true
		}
	}
	// Otherwise the first available.
	for _, mt := range content {
		return mt, true
	}
	return oasMediaType{}, false
}

func refName(ref string) string {
	i := strings.LastIndex(ref, "/")
	if i >= 0 {
		return ref[i+1:]
	}
	return ref
}

func enumStrings(in []any) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, fmt.Sprintf("%v", v))
	}
	return out
}

// operationID derives a stable operation id: the spec's operationId when
// present (sanitized), else "{method}_{path}" with path params/segments folded.
func operationID(opID, method, path string) string {
	if opID != "" {
		return sanitizeID(opID)
	}
	segs := strings.FieldsFunc(path, func(r rune) bool { return r == '/' })
	clean := make([]string, 0, len(segs)+1)
	clean = append(clean, strings.ToLower(method))
	for _, s := range segs {
		s = strings.Trim(s, "{}")
		clean = append(clean, s)
	}
	return sanitizeID(strings.Join(clean, "_"))
}

func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r == '-', r == '.', r == '/', r == ' ':
			b.WriteByte('_')
		}
	}
	out := b.String()
	for strings.Contains(out, "__") {
		out = strings.ReplaceAll(out, "__", "_")
	}
	return strings.Trim(out, "_")
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
