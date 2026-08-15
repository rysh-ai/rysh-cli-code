// SPDX-License-Identifier: Apache-2.0

package forgecmd

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	reflectv1grpc "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/rysh-ai/rysh-cli-code/internal/forge"
)

const liveIntrospectionJSON = `{"data":{"__schema":{
  "queryType":{"name":"Query"},
  "mutationType":{"name":"Mutation"},
  "types":[
    {"kind":"OBJECT","name":"Query","fields":[
      {"name":"user","args":[{"name":"id","type":{"kind":"NON_NULL","ofType":{"kind":"SCALAR","name":"ID"}}}],"type":{"kind":"OBJECT","name":"User"}}
    ]},
    {"kind":"OBJECT","name":"Mutation","fields":[
      {"name":"createUser","args":[{"name":"name","type":{"kind":"SCALAR","name":"String"}}],"type":{"kind":"OBJECT","name":"User"}}
    ]}
  ]
}}}`

// TestForgeAdd_GraphQLLive drives the full CLI path: `forge add x
// --graphql-url URL` must fetch the schema over the wire, store it, and land
// in the exact state a file-based `forge add x schema.json` produces.
func TestForgeAdd_GraphQLLive(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(liveIntrospectionJSON))
	}))
	defer srv.Close()

	liveDir := t.TempDir()
	var out bytes.Buffer
	err := Run(liveDir, []string{
		"add", "gql", "--graphql-url", srv.URL,
		"--header", "Authorization: Bearer tok",
		"--targets", "docs",
	}, &out)
	if err != nil {
		t.Fatalf("forge add --graphql-url: %v\noutput:\n%s", err, out.String())
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("--header not forwarded, Authorization = %q", gotAuth)
	}

	defs, err := forge.LoadStore(liveDir)
	if err != nil || len(defs) != 1 {
		t.Fatalf("store after live add: defs=%v err=%v", defs, err)
	}
	if defs[0].Source != forge.SourceGraphQL {
		t.Fatalf("source = %q, want graphql", defs[0].Source)
	}
	// The live URL becomes the default base URL, so the "no absolute base
	// URL" trap does not fire for live adds.
	if defs[0].BaseURL != srv.URL {
		t.Fatalf("base URL = %q, want %q", defs[0].BaseURL, srv.URL)
	}
	liveAPI, err := forge.LoadSpecAPI(liveDir, defs[0])
	if err != nil {
		t.Fatalf("LoadSpecAPI(live): %v", err)
	}

	// File-based reference run over the identical introspection JSON.
	fileDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(fileDir, "schema.json"), []byte(liveIntrospectionJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	var fout bytes.Buffer
	if err := Run(fileDir, []string{"add", "gql", "schema.json", "--targets", "docs"}, &fout); err != nil {
		t.Fatalf("file-based add: %v", err)
	}
	fdefs, _ := forge.LoadStore(fileDir)
	fileAPI, err := forge.LoadSpecAPI(fileDir, fdefs[0])
	if err != nil {
		t.Fatalf("LoadSpecAPI(file): %v", err)
	}

	if len(liveAPI.Operations) != len(fileAPI.Operations) {
		t.Fatalf("live ops %d != file ops %d", len(liveAPI.Operations), len(fileAPI.Operations))
	}
	for i := range fileAPI.Operations {
		if liveAPI.Operations[i].ID != fileAPI.Operations[i].ID {
			t.Fatalf("op %d: live %q != file %q", i, liveAPI.Operations[i].ID, fileAPI.Operations[i].ID)
		}
	}
	// Same generated artifacts as the file path (docs target).
	if _, err := os.Stat(filepath.Join(forge.SpecDir(liveDir, "gql"), "gen", "docs")); err != nil {
		t.Fatalf("docs not generated for live add: %v", err)
	}
}

// TestForgeAdd_GRPCLive drives `forge add x --grpc-target host:port` against
// an in-process reflection server and checks it matches the file-based
// descriptor-set add.
func TestForgeAdd_GRPCLive(t *testing.T) {
	// Same echo fixture as grpc_reach_test.go, served over reflection.
	var fds descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(echoDescriptorSet(t), &fds); err != nil {
		t.Fatal(err)
	}
	fd, err := protodesc.NewFile(fds.GetFile()[0], nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	files := &protoregistry.Files{}
	if err := files.RegisterFile(fd); err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	s.RegisterService(&grpc.ServiceDesc{
		ServiceName: "echo.Echo",
		Methods:     []grpc.MethodDesc{{MethodName: "GetEcho"}},
		Metadata:    "echo.proto",
	}, nil)
	reflectv1grpc.RegisterServerReflectionServer(s,
		reflection.NewServerV1(reflection.ServerOptions{Services: s, DescriptorResolver: files}))
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(lis)
	t.Cleanup(s.Stop)

	workDir := t.TempDir()
	var out bytes.Buffer
	err = Run(workDir, []string{
		"add", "echo", "--grpc-target", lis.Addr().String(),
		"--targets", "docs",
	}, &out)
	if err != nil {
		t.Fatalf("forge add --grpc-target: %v\noutput:\n%s", err, out.String())
	}

	defs, err := forge.LoadStore(workDir)
	if err != nil || len(defs) != 1 {
		t.Fatalf("store after live add: defs=%v err=%v", defs, err)
	}
	if defs[0].Source != forge.SourceGRPC {
		t.Fatalf("source = %q, want grpc", defs[0].Source)
	}
	api, err := forge.LoadSpecAPI(workDir, defs[0])
	if err != nil {
		t.Fatalf("LoadSpecAPI: %v", err)
	}
	if len(api.Operations) != 1 || !strings.Contains(api.Operations[0].Path, "echo.Echo/GetEcho") {
		t.Fatalf("reflection add did not produce the file-ingest surface: %+v", api.Operations)
	}
	// The honest JSON-transcoding note must appear for live gRPC too.
	if !strings.Contains(out.String(), "grpc-gateway") {
		t.Errorf("add output must disclose the JSON-over-HTTP requirement, got:\n%s", out.String())
	}
}

// TestForgeAdd_LiveFlagValidation pins the argument contract.
func TestForgeAdd_LiveFlagValidation(t *testing.T) {
	workDir := t.TempDir()
	tests := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{"no spec and no live flag", []string{"add", "x"}, "usage"},
		{"spec file plus live flag", []string{"add", "x", "spec.json", "--graphql-url", "http://h/graphql"}, "not both"},
		{"both live flags", []string{"add", "x", "--graphql-url", "http://h/graphql", "--grpc-target", "h:1"}, "mutually exclusive"},
		{"bad header", []string{"add", "x", "--graphql-url", "http://h/graphql", "--header", "novalue"}, "invalid --header"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := Run(workDir, tc.args, &out)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}
