// SPDX-License-Identifier: Apache-2.0

package forgecmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/rysh-ai/rysh-cli-code/internal/forge"
)

// echoDescriptorSet builds a minimal but valid FileDescriptorSet:
//
//	package echo;
//	message EchoRequest { string text = 1; }
//	message EchoReply   { string text = 1; }
//	service Echo { rpc GetEcho(EchoRequest) returns (EchoReply); }
func echoDescriptorSet(t *testing.T) []byte {
	t.Helper()
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL

	msg := func(name string) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{
			Name: proto.String(name),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("text"), Number: proto.Int32(1), Label: &opt, Type: &str},
			},
		}
	}

	fds := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{{
			Name:        proto.String("echo.proto"),
			Package:     proto.String("echo"),
			Syntax:      proto.String("proto3"),
			MessageType: []*descriptorpb.DescriptorProto{msg("EchoRequest"), msg("EchoReply")},
			Service: []*descriptorpb.ServiceDescriptorProto{{
				Name: proto.String("Echo"),
				Method: []*descriptorpb.MethodDescriptorProto{{
					Name:       proto.String("GetEcho"),
					InputType:  proto.String(".echo.EchoRequest"),
					OutputType: proto.String(".echo.EchoReply"),
				}},
			}},
		}},
	}

	b, err := proto.Marshal(fds)
	if err != nil {
		t.Fatalf("marshal descriptor set: %v", err)
	}
	return b
}

// TestForgeAdd_GRPCIsReachable is the regression guard for the gRPC ingest
// path, which was ~80% written, fully unit-tested, and completely unreachable:
// there was no SourceGRPC constant, Integration.Validate rejected the value,
// and neither ingestSpec nor LoadSpecAPI had a case — so ingest.GRPC had zero
// non-test callers and a .pb descriptor fell through to the OpenAPI parser.
func TestForgeAdd_GRPCIsReachable(t *testing.T) {
	workDir := t.TempDir()
	specPath := filepath.Join(workDir, "echo.pb")
	if err := os.WriteFile(specPath, echoDescriptorSet(t), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	var out bytes.Buffer
	err := Run(workDir, []string{
		"add", "echo", "echo.pb",
		"--base-url", "http://127.0.0.1:8080",
		"--targets", "docs",
	}, &out)
	if err != nil {
		t.Fatalf("forge add with a descriptor set failed: %v\noutput:\n%s", err, out.String())
	}

	// The source must have been inferred as grpc, not silently parsed as OpenAPI.
	defs, err := forge.LoadStore(workDir)
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	if len(defs) != 1 {
		t.Fatalf("expected 1 integration, got %d", len(defs))
	}
	if defs[0].Source != forge.SourceGRPC {
		t.Fatalf("source = %q, want %q — inference or Validate rejected gRPC",
			defs[0].Source, forge.SourceGRPC)
	}

	// The stored spec must round-trip back through the gRPC ingester.
	api, err := forge.LoadSpecAPI(workDir, defs[0])
	if err != nil {
		t.Fatalf("LoadSpecAPI on a stored descriptor set: %v", err)
	}
	ops := api.Operations
	if len(ops) != 1 {
		t.Fatalf("expected 1 operation from the descriptor set, got %d", len(ops))
	}
	if !strings.Contains(ops[0].Path, "echo.Echo/GetEcho") {
		t.Fatalf("operation path = %q, want the gRPC wire path", ops[0].Path)
	}

	// The honest-limits note must be shown — a user who does not know the
	// endpoint needs JSON transcoding will otherwise see every call 404.
	if !strings.Contains(out.String(), "grpc-gateway") {
		t.Errorf("add output must disclose the JSON-over-HTTP requirement, got:\n%s", out.String())
	}
}

// TestInferSource_DoesNotMisclassify guards the detection ordering: the binary
// sniffer must not steal OpenAPI or GraphQL specs, and must catch a descriptor
// set whose filename gives no hint.
func TestInferSource_DoesNotMisclassify(t *testing.T) {
	descriptor := echoDescriptorSet(t)

	tests := []struct {
		name string
		path string
		spec []byte
		want string
	}{
		{"descriptor by extension", "api.pb", descriptor, forge.SourceGRPC},
		{"descriptor by .protoset", "api.protoset", descriptor, forge.SourceGRPC},
		{"descriptor with no hint in the name", "blob", descriptor, forge.SourceGRPC},
		{
			name: "openapi json is not grpc",
			path: "api.json",
			spec: []byte(`{"openapi":"3.0.0","paths":{"/x":{"get":{"operationId":"x"}}}}`),
			want: forge.SourceOpenAPI,
		},
		{
			name: "graphql introspection is not grpc",
			path: "schema.json",
			spec: []byte(`{"__schema":{"queryType":{"name":"Query"}}}`),
			want: forge.SourceGraphQL,
		},
		{
			name: "graphql by extension is not grpc",
			path: "schema.gql",
			spec: []byte("type Query { hello: String }"),
			want: forge.SourceGraphQL,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := inferSource(tc.path, tc.spec); got != tc.want {
				t.Fatalf("inferSource(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// TestIntegrationValidate_AcceptsGRPC pins the store-level gate that used to
// reject the value outright.
func TestIntegrationValidate_AcceptsGRPC(t *testing.T) {
	def := forge.Integration{Name: "echo", Source: forge.SourceGRPC, SpecFile: "echo.pb"}
	if err := def.Validate(); err != nil {
		t.Fatalf("Validate rejected a gRPC integration: %v", err)
	}

	bad := forge.Integration{Name: "x", Source: "soap", SpecFile: "x.wsdl"}
	if err := bad.Validate(); err == nil {
		t.Fatal("Validate must still reject an unknown source")
	} else if !strings.Contains(err.Error(), "grpc") {
		t.Errorf("the error should list grpc as a valid source, got: %v", err)
	}
}
