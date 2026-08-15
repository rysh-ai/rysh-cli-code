// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
)

// buildDemoFileDescriptorSet constructs a small FileDescriptorSet equivalent to:
//
//	package demo;
//	message HelloRequest { string name = 1; int32 times = 2; }
//	message HelloReply   { string message = 1; }
//	service Greeter {
//	  rpc SayHello(HelloRequest) returns (HelloReply);     // mutating
//	  rpc GetGreeting(HelloRequest) returns (HelloReply);  // read-style
//	}
func buildDemoFileDescriptorSet() *descriptorpb.FileDescriptorSet {
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING
	i32 := descriptorpb.FieldDescriptorProto_TYPE_INT32
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL

	helloReq := &descriptorpb.DescriptorProto{
		Name: proto.String("HelloRequest"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("name"), Number: proto.Int32(1), Label: &opt, Type: &str},
			{Name: proto.String("times"), Number: proto.Int32(2), Label: &opt, Type: &i32},
		},
	}
	helloReply := &descriptorpb.DescriptorProto{
		Name: proto.String("HelloReply"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("message"), Number: proto.Int32(1), Label: &opt, Type: &str},
		},
	}

	greeter := &descriptorpb.ServiceDescriptorProto{
		Name: proto.String("Greeter"),
		Method: []*descriptorpb.MethodDescriptorProto{
			{
				Name:       proto.String("SayHello"),
				InputType:  proto.String(".demo.HelloRequest"),
				OutputType: proto.String(".demo.HelloReply"),
			},
			{
				Name:       proto.String("GetGreeting"),
				InputType:  proto.String(".demo.HelloRequest"),
				OutputType: proto.String(".demo.HelloReply"),
			},
		},
	}

	file := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("demo.proto"),
		Package:     proto.String("demo"),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{helloReq, helloReply},
		Service:     []*descriptorpb.ServiceDescriptorProto{greeter},
	}

	return &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{file}}
}

func TestGRPCIngest(t *testing.T) {
	fds := buildDemoFileDescriptorSet()
	data, err := proto.Marshal(fds)
	if err != nil {
		t.Fatalf("marshal FileDescriptorSet: %v", err)
	}

	api, err := GRPC(data)
	if err != nil {
		t.Fatalf("GRPC() error: %v", err)
	}

	if api.SourceType != "grpc" {
		t.Errorf("SourceType = %q, want %q", api.SourceType, "grpc")
	}
	if api.Name != "demo" {
		t.Errorf("Name = %q, want %q", api.Name, "demo")
	}
	if api.Version != "proto" {
		t.Errorf("Version = %q, want %q", api.Version, "proto")
	}
	if len(api.Servers) != 1 || api.Servers[0].URL != "" {
		t.Errorf("Servers = %+v, want one empty-URL server", api.Servers)
	}
	if len(api.Operations) != 2 {
		t.Fatalf("got %d operations, want 2", len(api.Operations))
	}
	if len(api.Skipped) != 0 {
		t.Fatalf("unary-only service reported skipped methods: %+v", api.Skipped)
	}

	sayHello := api.OperationByID("Greeter_SayHello")
	if sayHello == nil {
		t.Fatalf("missing operation Greeter_SayHello; got %v", opIDs(api.Operations))
	}
	if sayHello.Method != "POST" {
		t.Errorf("SayHello.Method = %q, want POST", sayHello.Method)
	}
	if sayHello.Path != "/demo.Greeter/SayHello" {
		t.Errorf("SayHello.Path = %q, want /demo.Greeter/SayHello", sayHello.Path)
	}
	if !sayHello.Mutating {
		t.Errorf("SayHello.Mutating = false, want true")
	}
	if len(sayHello.Tags) != 1 || sayHello.Tags[0] != "Greeter" {
		t.Errorf("SayHello.Tags = %v, want [Greeter]", sayHello.Tags)
	}

	getGreeting := api.OperationByID("Greeter_GetGreeting")
	if getGreeting == nil {
		t.Fatalf("missing operation Greeter_GetGreeting")
	}
	if getGreeting.Path != "/demo.Greeter/GetGreeting" {
		t.Errorf("GetGreeting.Path = %q, want /demo.Greeter/GetGreeting", getGreeting.Path)
	}
	if getGreeting.Mutating {
		t.Errorf("GetGreeting.Mutating = true, want false (read-style method)")
	}

	// RequestBody is a $ref to the named HelloRequest schema; fields live there.
	if sayHello.RequestBody == nil || sayHello.RequestBody.Ref != "HelloRequest" {
		t.Fatalf("SayHello.RequestBody = %+v, want ref to HelloRequest", sayHello.RequestBody)
	}
	reqSchema := api.Schemas["HelloRequest"]
	if reqSchema == nil {
		t.Fatalf("HelloRequest schema not registered; have %v", schemaNames(api.Schemas))
	}
	nameField, ok := reqSchema.Properties["name"]
	if !ok {
		t.Fatalf("HelloRequest missing 'name' field; props %v", propNames(reqSchema))
	}
	if nameField.Type != "string" {
		t.Errorf("HelloRequest.name type = %q, want string", nameField.Type)
	}
	timesField, ok := reqSchema.Properties["times"]
	if !ok {
		t.Fatalf("HelloRequest missing 'times' field")
	}
	if timesField.Type != "integer" {
		t.Errorf("HelloRequest.times type = %q, want integer", timesField.Type)
	}

	// Response wired to the output message.
	if got := getGreeting.Responses["200"]; got == nil || got.Ref != "HelloReply" {
		t.Errorf("GetGreeting 200 response = %+v, want ref to HelloReply", got)
	}
	if api.Schemas["HelloReply"] == nil {
		t.Errorf("HelloReply schema not registered")
	}
}

// buildStreamingFileDescriptorSet constructs a FileDescriptorSet equivalent to:
//
//	package pub;
//	message Msg { string text = 1; }
//	service Pub {
//	  rpc GetMsg(Msg) returns (Msg);            // unary → exposed
//	  rpc Watch(Msg) returns (stream Msg);      // server-streaming → api.Streams
//	  rpc Tail(Msg) returns (stream Msg);       // server-streaming → api.Streams
//	  rpc Upload(stream Msg) returns (Msg);     // client-streaming → skipped
//	  rpc Chat(stream Msg) returns (stream Msg);// bidi → skipped
//	}
func buildStreamingFileDescriptorSet(t *testing.T) []byte {
	t.Helper()
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL

	msg := &descriptorpb.DescriptorProto{
		Name: proto.String("Msg"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("text"), Number: proto.Int32(1), Label: &opt, Type: &str},
		},
	}
	method := func(name string, clientStream, serverStream bool) *descriptorpb.MethodDescriptorProto {
		m := &descriptorpb.MethodDescriptorProto{
			Name:       proto.String(name),
			InputType:  proto.String(".pub.Msg"),
			OutputType: proto.String(".pub.Msg"),
		}
		if clientStream {
			m.ClientStreaming = proto.Bool(true)
		}
		if serverStream {
			m.ServerStreaming = proto.Bool(true)
		}
		return m
	}
	fds := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{{
			Name:        proto.String("pub.proto"),
			Package:     proto.String("pub"),
			Syntax:      proto.String("proto3"),
			MessageType: []*descriptorpb.DescriptorProto{msg},
			Service: []*descriptorpb.ServiceDescriptorProto{{
				Name: proto.String("Pub"),
				Method: []*descriptorpb.MethodDescriptorProto{
					method("GetMsg", false, false),
					method("Watch", false, true),
					method("Tail", false, true),
					method("Upload", true, false),
					method("Chat", true, true),
				},
			}},
		}},
	}
	data, err := proto.Marshal(fds)
	if err != nil {
		t.Fatalf("marshal FileDescriptorSet: %v", err)
	}
	return data
}

// TestGRPCRoutesStreamingMethods guards the streaming classification (design
// 015 §2.2): unary methods become operations; server-streaming methods become
// api.Streams entries (exposed as streaming sessions) with a full request
// model; client-streaming and bidi methods land in api.Skipped by kind (they
// cannot ride a JSON-over-HTTP bridge). Nothing may be emitted as a unary tool
// that would hang or truncate at call time.
func TestGRPCRoutesStreamingMethods(t *testing.T) {
	api, err := GRPC(buildStreamingFileDescriptorSet(t))
	if err != nil {
		t.Fatalf("GRPC() error: %v", err)
	}

	if len(api.Operations) != 1 {
		t.Fatalf("got %d operations (%v), want only the unary method Pub_GetMsg",
			len(api.Operations), opIDs(api.Operations))
	}
	if api.OperationByID("Pub_GetMsg") == nil {
		t.Fatalf("unary method Pub_GetMsg not exposed; got %v", opIDs(api.Operations))
	}
	for _, id := range []string{"Pub_Watch", "Pub_Tail", "Pub_Upload", "Pub_Chat"} {
		if api.OperationByID(id) != nil {
			t.Errorf("streaming method %s was emitted as a unary tool; it must not be", id)
		}
	}

	// Server-streaming → Streams, fully modeled (request body, wire path).
	if len(api.Streams) != 2 {
		t.Fatalf("Streams = %+v, want the 2 server-streaming methods", api.Streams)
	}
	for _, id := range []string{"Pub_Watch", "Pub_Tail"} {
		st := api.StreamByID(id)
		if st == nil {
			t.Errorf("server-streaming method %s missing from Streams", id)
			continue
		}
		if st.Path != "/pub.Pub/"+strings.TrimPrefix(id, "Pub_") {
			t.Errorf("stream %s path = %q, want the gRPC wire path", id, st.Path)
		}
		if st.RequestBody == nil {
			t.Errorf("stream %s request message not modeled", id)
		}
	}
	// The AIP-130 read-prefix heuristic applies to streams too: Watch* is a
	// read; Tail (unknown verb) stays mutating-by-default (approval-gated).
	if api.StreamByID("Pub_Watch").Mutating {
		t.Errorf("Pub_Watch should be non-mutating (Watch prefix)")
	}
	if !api.StreamByID("Pub_Tail").Mutating {
		t.Errorf("Pub_Tail should default to mutating (unknown verb ⇒ approval-gated)")
	}

	// Client-streaming / bidi → Skipped by kind.
	wantSkipped := map[string]string{
		"Pub_Upload": ir.SkipClientStreaming,
		"Pub_Chat":   ir.SkipBidiStreaming,
	}
	if len(api.Skipped) != len(wantSkipped) {
		t.Fatalf("Skipped = %+v, want %d entries (client/bidi only)", api.Skipped, len(wantSkipped))
	}
	for _, s := range api.Skipped {
		want, ok := wantSkipped[s.ID]
		if !ok {
			t.Errorf("unexpected skipped entry %+v", s)
			continue
		}
		if s.Kind != want {
			t.Errorf("skipped %s kind = %q, want %q", s.ID, s.Kind, want)
		}
		delete(wantSkipped, s.ID)
	}
	for id := range wantSkipped {
		t.Errorf("streaming method %s missing from Skipped", id)
	}
}

// TestGRPCServerStreamingOnlyIsAccepted: a service with ONLY server-streaming
// methods is now addable — the methods are exposed as streaming sessions.
func TestGRPCServerStreamingOnlyIsAccepted(t *testing.T) {
	api, err := GRPC(onlyStreamDescriptorSet(t, false))
	if err != nil {
		t.Fatalf("GRPC() with only server-streaming methods should be accepted, got: %v", err)
	}
	if len(api.Operations) != 0 || len(api.Streams) != 1 {
		t.Fatalf("ops=%d streams=%d, want 0 ops and 1 stream", len(api.Operations), len(api.Streams))
	}
}

// TestGRPCAllClientBidiIsRejected pins the error when a service has ONLY
// client-streaming/bidi methods: nothing is callable, so add must fail with a
// message that says why instead of "contains no services".
func TestGRPCAllClientBidiIsRejected(t *testing.T) {
	_, err := GRPC(onlyStreamDescriptorSet(t, true))
	if err == nil {
		t.Fatal("GRPC() with only client/bidi methods returned nil error, want an error")
	}
	if !contains(err.Error(), "streaming") && !contains(err.Error(), "bidi") {
		t.Errorf("error %q should say the methods are client-streaming/bidi", err)
	}
}

// onlyStreamDescriptorSet builds a one-method service: server-streaming Watch
// when clientToo is false, else a bidi Chat.
func onlyStreamDescriptorSet(t *testing.T, clientToo bool) []byte {
	t.Helper()
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	method := &descriptorpb.MethodDescriptorProto{
		Name:            proto.String("Watch"),
		InputType:       proto.String(".only.Msg"),
		OutputType:      proto.String(".only.Msg"),
		ServerStreaming: proto.Bool(true),
	}
	if clientToo {
		method.Name = proto.String("Chat")
		method.ClientStreaming = proto.Bool(true)
	}
	fds := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{{
			Name:    proto.String("only.proto"),
			Package: proto.String("only"),
			Syntax:  proto.String("proto3"),
			MessageType: []*descriptorpb.DescriptorProto{{
				Name: proto.String("Msg"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("text"), Number: proto.Int32(1), Label: &opt, Type: &str},
				},
			}},
			Service: []*descriptorpb.ServiceDescriptorProto{{
				Name:   proto.String("OnlyStream"),
				Method: []*descriptorpb.MethodDescriptorProto{method},
			}},
		}},
	}
	data, err := proto.Marshal(fds)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func TestGRPCRejectsGarbage(t *testing.T) {
	if _, err := GRPC([]byte("garbage")); err == nil {
		t.Fatal("GRPC(garbage) returned nil error, want a parse error")
	}
}

func TestGRPCRejectsNoServices(t *testing.T) {
	fds := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{{
			Name:    proto.String("empty.proto"),
			Package: proto.String("empty"),
		}},
	}
	data, err := proto.Marshal(fds)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := GRPC(data); err == nil {
		t.Fatal("GRPC() with no services returned nil error, want an error")
	}
}

func opIDs(ops []ir.Operation) []string {
	out := make([]string, 0, len(ops))
	for _, o := range ops {
		out = append(out, o.ID)
	}
	return out
}

func schemaNames(m map[string]*ir.Schema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func propNames(s *ir.Schema) []string {
	out := make([]string, 0, len(s.Properties))
	for k := range s.Properties {
		out = append(out, k)
	}
	return out
}
