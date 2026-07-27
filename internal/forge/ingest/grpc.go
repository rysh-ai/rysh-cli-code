package ingest

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
)

// GRPC ingests a compiled protobuf FileDescriptorSet into the Forge IR.
//
// The input bytes are the binary FileDescriptorSet produced by either
//
//	protoc --descriptor_set_out=set.pb --include_imports *.proto
//	buf build -o set.pb
//
// Each UNARY service method becomes one ir.Operation modeled as a unary POST
// to the gRPC HTTP/2 path "/<fully-qualified-service>/<Method>" (the wire path
// gRPC uses), so generators can describe and document the surface uniformly
// with REST sources. gRPC has no REST base URL, so the single server URL is
// empty.
//
// Streaming methods (client-streaming, server-streaming, bidi) are NOT emitted
// as operations: the forge live-call runtime is a single JSON request → single
// JSON response executor (one-shot body read, MaxBody cap, per-call timeout).
// grpc-gateway transcodes server-streaming as an unbounded newline-delimited
// JSON response and Connect uses a different enveloped framing, so a unary tool
// for a streaming method would hang, truncate, or fail at call time. Design 015
// maps streaming to background ring-buffer sessions; until that session
// lifecycle exists in the forge tool layer, streaming methods are recorded in
// API.Skipped (by kind) so the exclusion is reported, never silent.
//
// Field-type mapping (proto → JSON-schema): see protoFieldSchema. The
// read-vs-write (mutating) heuristic and recursion handling are documented on
// readMethodPrefixes and buildMessageSchema respectively.
func GRPC(data []byte) (*ir.API, error) {
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(data, &fds); err != nil {
		return nil, fmt.Errorf("parse FileDescriptorSet: %w", err)
	}
	if len(fds.GetFile()) == 0 {
		return nil, fmt.Errorf("FileDescriptorSet contains no files")
	}

	// Index every message type by its fully-qualified name (".pkg.Outer.Inner")
	// and also by simple name, so field references — which carry the FQ name —
	// and cross-file/imported references resolve regardless of nesting.
	idx := newProtoIndex()
	for _, f := range fds.GetFile() {
		pkg := f.GetPackage()
		for _, m := range f.GetMessageType() {
			idx.addMessage(pkg, "", m)
		}
		for _, e := range f.GetEnumType() {
			idx.addEnum(pkg, "", e.GetName())
		}
	}

	api := &ir.API{
		Name:       firstNonEmpty(fds.GetFile()[0].GetPackage(), "grpc-api"),
		Version:    "proto",
		SourceType: "grpc",
		Servers:    []ir.Server{{URL: ""}}, // gRPC has no REST base URL.
		Schemas:    map[string]*ir.Schema{},
	}

	for _, f := range fds.GetFile() {
		pkg := f.GetPackage()
		for _, svc := range f.GetService() {
			svcName := svc.GetName()
			fqService := svcName
			if pkg != "" {
				fqService = pkg + "." + svcName
			}
			for _, method := range svc.GetMethod() {
				methodName := method.GetName()
				// Non-unary methods are skipped, not exposed — see the doc
				// comment above for why a unary tool for a streaming method
				// would be broken at call time.
				if method.GetClientStreaming() || method.GetServerStreaming() {
					api.Skipped = append(api.Skipped, ir.SkippedOperation{
						ID:   sanitizeID(svcName + "_" + methodName),
						Kind: streamKind(method),
					})
					continue
				}
				op := ir.Operation{
					ID:        sanitizeID(svcName + "_" + methodName),
					Method:    "POST",
					Path:      "/" + fqService + "/" + methodName,
					Tags:      []string{svcName},
					Mutating:  isMutatingMethod(methodName),
					Responses: map[string]*ir.Schema{},
				}
				if msg := idx.message(method.GetInputType()); msg != nil {
					op.RequestBody = idx.buildMessageSchema(msg, api.Schemas, map[string]bool{})
				}
				if msg := idx.message(method.GetOutputType()); msg != nil {
					op.Responses["200"] = idx.buildMessageSchema(msg, api.Schemas, map[string]bool{})
				}
				api.Operations = append(api.Operations, op)
			}
		}
	}

	if len(api.Operations) == 0 {
		if n := len(api.Skipped); n > 0 {
			return nil, fmt.Errorf("FileDescriptorSet contains no unary methods: all %d method(s) are streaming, which forge does not expose as tools yet", n)
		}
		return nil, fmt.Errorf("FileDescriptorSet contains no services")
	}
	return api, nil
}

// streamKind classifies a non-unary method by its streaming direction.
func streamKind(m *descriptorpb.MethodDescriptorProto) string {
	switch {
	case m.GetClientStreaming() && m.GetServerStreaming():
		return ir.SkipBidiStreaming
	case m.GetClientStreaming():
		return ir.SkipClientStreaming
	default:
		return ir.SkipServerStreaming
	}
}

// readMethodPrefixes lists the verb prefixes that mark a method as read-only.
// proto/gRPC has no machine-readable side-effect annotation in the descriptor,
// so we use the widely-followed naming convention (AIP-130 style): methods that
// start with one of these verbs are treated as non-mutating; everything else
// (Create/Update/Delete/Send/… and any custom verb) is mutating by default.
var readMethodPrefixes = []string{"Get", "List", "Watch", "Search", "Query", "Lookup"}

// isMutatingMethod reports whether a method should be flagged as mutating
// (an approval candidate). See readMethodPrefixes for the heuristic.
func isMutatingMethod(name string) bool {
	for _, p := range readMethodPrefixes {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// protoIndex resolves field type references and tracks message descriptors.
type protoIndex struct {
	// byFQ maps a fully-qualified name (without leading dot, e.g.
	// "demo.HelloRequest") to its descriptor.
	byFQ map[string]*descriptorpb.DescriptorProto
	// bySimple maps a simple name ("HelloRequest") to its descriptor, used as a
	// fallback when an FQ lookup misses (best-effort imported-type resolution).
	bySimple map[string]*descriptorpb.DescriptorProto
	// enums tracks fully-qualified enum names so message fields can be mapped to
	// "string" without trying to resolve them as messages.
	enums map[string]bool
}

func newProtoIndex() *protoIndex {
	return &protoIndex{
		byFQ:     map[string]*descriptorpb.DescriptorProto{},
		bySimple: map[string]*descriptorpb.DescriptorProto{},
		enums:    map[string]bool{},
	}
}

// addMessage registers a message and recurses into nested types. parent is the
// dot-joined prefix of enclosing message names (empty at file scope).
func (idx *protoIndex) addMessage(pkg, parent string, m *descriptorpb.DescriptorProto) {
	scope := joinScope(pkg, parent)
	fq := joinScope(scope, m.GetName())
	idx.byFQ[fq] = m
	// First registration wins for the simple-name fallback (deterministic for
	// the common single-definition case).
	if _, ok := idx.bySimple[m.GetName()]; !ok {
		idx.bySimple[m.GetName()] = m
	}
	for _, nested := range m.GetNestedType() {
		idx.addMessage(pkg, joinScope(parent, m.GetName()), nested)
	}
	for _, e := range m.GetEnumType() {
		idx.addEnum(pkg, joinScope(parent, m.GetName()), e.GetName())
	}
}

func (idx *protoIndex) addEnum(pkg, parent, name string) {
	idx.enums[joinScope(joinScope(pkg, parent), name)] = true
}

func joinScope(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "." + b
	}
}

// message resolves a field/method type reference (a fully-qualified name with a
// leading dot, e.g. ".demo.HelloRequest") to a message descriptor, falling back
// to a simple-name lookup for robustness.
func (idx *protoIndex) message(typeName string) *descriptorpb.DescriptorProto {
	fq := strings.TrimPrefix(typeName, ".")
	if m, ok := idx.byFQ[fq]; ok {
		return m
	}
	return idx.bySimple[simpleName(typeName)]
}

func (idx *protoIndex) isEnum(typeName string) bool {
	return idx.enums[strings.TrimPrefix(typeName, ".")]
}

// buildMessageSchema converts a message descriptor into an ir.Schema and records
// it (and any referenced messages) in schemas under its simple name. The
// returned schema is a $ref to that named schema so deeply nested or repeated
// occurrences stay compact.
//
// Recursion is bounded two ways: (1) once a message name is present in schemas
// it is never rebuilt, and (2) a per-build "visiting" set short-circuits cycles
// reached before the schema finishes registering — in both cases nested fields
// emit a Ref by name rather than re-expanding, so self-referential and mutually
// recursive messages terminate.
func (idx *protoIndex) buildMessageSchema(m *descriptorpb.DescriptorProto, schemas map[string]*ir.Schema, visiting map[string]bool) *ir.Schema {
	name := m.GetName()
	if _, done := schemas[name]; done {
		return &ir.Schema{Ref: name}
	}
	if visiting[name] {
		return &ir.Schema{Ref: name}
	}
	visiting[name] = true

	out := &ir.Schema{Name: name, Type: "object", Properties: map[string]*ir.Schema{}}
	// Register before populating so a self-reference within a field resolves to
	// the in-progress schema rather than recursing forever.
	schemas[name] = out

	for _, field := range m.GetField() {
		out.Properties[field.GetName()] = idx.protoFieldSchema(field, schemas, visiting)
	}

	delete(visiting, name)
	return &ir.Schema{Ref: name}
}

// protoFieldSchema maps a single proto field to a JSON-schema node.
//
// Scalar mapping:
//
//	string                                   → string
//	bytes                                    → string (format "byte")
//	bool                                     → boolean
//	float, double                            → number
//	int32, sint32, sfixed32, uint32, fixed32 → integer (format "int32")
//	int64, sint64, sfixed64, uint64, fixed64 → integer (format "int64": these
//	                                           are JSON-encoded as strings)
//	enum                                     → string
//	message                                  → object ($ref by simple name)
//
// A repeated field becomes an array whose items use the element mapping above.
// A proto map<K,V> (a repeated field of an auto-generated map-entry message) is
// modeled as an object with additionalProperties set to the value schema.
func (idx *protoIndex) protoFieldSchema(field *descriptorpb.FieldDescriptorProto, schemas map[string]*ir.Schema, visiting map[string]bool) *ir.Schema {
	// Map fields: repeated message whose entry type has options.map_entry=true.
	if field.GetLabel() == descriptorpb.FieldDescriptorProto_LABEL_REPEATED &&
		field.GetType() == descriptorpb.FieldDescriptorProto_TYPE_MESSAGE {
		if entry := idx.message(field.GetTypeName()); entry != nil && entry.GetOptions().GetMapEntry() {
			obj := &ir.Schema{Type: "object"}
			if valField := mapValueField(entry); valField != nil {
				obj.AdditionalProperties = idx.elementSchema(valField, schemas, visiting)
			}
			return obj
		}
	}

	elem := idx.elementSchema(field, schemas, visiting)
	if field.GetLabel() == descriptorpb.FieldDescriptorProto_LABEL_REPEATED {
		return &ir.Schema{Type: "array", Items: elem}
	}
	return elem
}

// elementSchema maps a field's element type (ignoring its repeated label) to a
// JSON-schema node.
func (idx *protoIndex) elementSchema(field *descriptorpb.FieldDescriptorProto, schemas map[string]*ir.Schema, visiting map[string]bool) *ir.Schema {
	switch field.GetType() {
	case descriptorpb.FieldDescriptorProto_TYPE_STRING:
		return &ir.Schema{Type: "string"}
	case descriptorpb.FieldDescriptorProto_TYPE_BYTES:
		return &ir.Schema{Type: "string", Format: "byte"}
	case descriptorpb.FieldDescriptorProto_TYPE_BOOL:
		return &ir.Schema{Type: "boolean"}
	case descriptorpb.FieldDescriptorProto_TYPE_FLOAT,
		descriptorpb.FieldDescriptorProto_TYPE_DOUBLE:
		return &ir.Schema{Type: "number"}
	case descriptorpb.FieldDescriptorProto_TYPE_INT32,
		descriptorpb.FieldDescriptorProto_TYPE_SINT32,
		descriptorpb.FieldDescriptorProto_TYPE_SFIXED32,
		descriptorpb.FieldDescriptorProto_TYPE_UINT32,
		descriptorpb.FieldDescriptorProto_TYPE_FIXED32:
		return &ir.Schema{Type: "integer", Format: "int32"}
	case descriptorpb.FieldDescriptorProto_TYPE_INT64,
		descriptorpb.FieldDescriptorProto_TYPE_SINT64,
		descriptorpb.FieldDescriptorProto_TYPE_SFIXED64,
		descriptorpb.FieldDescriptorProto_TYPE_UINT64,
		descriptorpb.FieldDescriptorProto_TYPE_FIXED64:
		// 64-bit integers are encoded as JSON strings by the proto JSON mapping.
		return &ir.Schema{Type: "integer", Format: "int64"}
	case descriptorpb.FieldDescriptorProto_TYPE_ENUM:
		return &ir.Schema{Type: "string"}
	case descriptorpb.FieldDescriptorProto_TYPE_MESSAGE,
		descriptorpb.FieldDescriptorProto_TYPE_GROUP:
		if msg := idx.message(field.GetTypeName()); msg != nil {
			return idx.buildMessageSchema(msg, schemas, visiting)
		}
		// Unresolved (e.g. a well-known type not in the set) → opaque object.
		return &ir.Schema{Type: "object"}
	default:
		if idx.isEnum(field.GetTypeName()) {
			return &ir.Schema{Type: "string"}
		}
		return &ir.Schema{Type: "string"}
	}
}

// mapValueField returns the "value" field (number 2) of a map-entry message.
func mapValueField(entry *descriptorpb.DescriptorProto) *descriptorpb.FieldDescriptorProto {
	for _, f := range entry.GetField() {
		if f.GetNumber() == 2 || f.GetName() == "value" {
			return f
		}
	}
	return nil
}

// simpleName returns the last dotted component of a (possibly leading-dot)
// fully-qualified type name.
func simpleName(typeName string) string {
	tn := strings.TrimPrefix(typeName, ".")
	if i := strings.LastIndex(tn, "."); i >= 0 {
		return tn[i+1:]
	}
	return tn
}
