// SPDX-License-Identifier: Apache-2.0

package actors

// The rysh.yaml forge path (workspace[].forge.integrations) must be able to
// select the gRPC streaming protocol exactly like `forge add --grpc-protocol`
// (E23 W2 task 3): same key semantics, same validation. The config is parsed
// from YAML text and the result read from the raw store JSON, so the test pins
// both the YAML key and the persisted key.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"gopkg.in/yaml.v3"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

func feedPB(t *testing.T) []byte {
	t.Helper()
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	b, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{{
		Name: proto.String("feed.proto"), Package: proto.String("feed"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{{Name: proto.String("Item"),
			Field: []*descriptorpb.FieldDescriptorProto{{Name: proto.String("text"), Number: proto.Int32(1), Label: &opt, Type: &str}}}},
		Service: []*descriptorpb.ServiceDescriptorProto{{Name: proto.String("Feed"), Method: []*descriptorpb.MethodDescriptorProto{
			{Name: proto.String("Watch"), InputType: proto.String(".feed.Item"), OutputType: proto.String(".feed.Item"), ServerStreaming: proto.Bool(true)},
		}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// runForgeConfigYAML writes the given spec, parses the YAML forge block and
// runs the config job, returning the raw integrations.json ("" when absent).
func runForgeConfigYAML(t *testing.T, specName string, spec []byte, yml string) string {
	t.Helper()
	work := t.TempDir()
	specPath := filepath.Join(work, specName)
	if err := os.WriteFile(specPath, spec, 0o644); err != nil {
		t.Fatal(err)
	}
	var fc config.ForgeConfig
	if err := yaml.Unmarshal([]byte(strings.ReplaceAll(yml, "SPEC", specPath)), &fc); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	(&forgeConfigJob{fc: fc, workDir: work}).run()
	data, _ := os.ReadFile(filepath.Join(work, ".rysh", "forge", "integrations.json"))
	return string(data)
}

func TestForgeConfigGRPCProtocolConnectPersisted(t *testing.T) {
	store := runForgeConfigYAML(t, "feed.pb", feedPB(t), `
integrations:
  - name: feed
    spec: SPEC
    base_url: http://127.0.0.1:1
    source: grpc
    grpc_protocol: connect
`)
	if !strings.Contains(store, `"grpc_protocol": "connect"`) {
		t.Fatalf("rysh.yaml grpc_protocol did not reach the store:\n%s", store)
	}
}

func TestForgeConfigGRPCProtocolValidated(t *testing.T) {
	// A bogus value is refused like the CLI flag refuses it: nothing is added.
	if store := runForgeConfigYAML(t, "feed.pb", feedPB(t), `
integrations:
  - name: feed
    spec: SPEC
    source: grpc
    grpc_protocol: grpc-web
`); strings.Contains(store, `"feed"`) {
		t.Fatalf("bogus grpc_protocol was accepted:\n%s", store)
	}
	// And it only applies to gRPC sources.
	if store := runForgeConfigYAML(t, "spec.json", []byte(`{"openapi":"3.0.0","info":{"title":"x","version":"1"},"servers":[{"url":"http://127.0.0.1:1"}],"paths":{}}`), `
integrations:
  - name: rest
    spec: SPEC
    grpc_protocol: connect
`); strings.Contains(store, `"rest"`) {
		t.Fatalf("grpc_protocol on an OpenAPI source was accepted:\n%s", store)
	}
}
