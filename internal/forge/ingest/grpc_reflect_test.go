package ingest

import (
	"context"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
	reflectv1grpc "google.golang.org/grpc/reflection/grpc_reflection_v1"
	reflectv1alphagrpc "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// startReflectionServer runs an in-process gRPC server that registers the
// demo.Greeter service (from the shared buildDemoFileDescriptorSet fixture)
// and serves reflection for it. Handlers are nil — reflection only reads
// descriptors, never invokes methods. version selects which reflection
// service generation the server exposes.
func startReflectionServer(t *testing.T, version string) string {
	t.Helper()

	fdp := buildDemoFileDescriptorSet().GetFile()[0]
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	files := &protoregistry.Files{}
	if err := files.RegisterFile(fd); err != nil {
		t.Fatalf("register file: %v", err)
	}

	s := grpc.NewServer()
	s.RegisterService(&grpc.ServiceDesc{
		ServiceName: "demo.Greeter",
		Methods:     []grpc.MethodDesc{{MethodName: "SayHello"}, {MethodName: "GetGreeting"}},
		Metadata:    "demo.proto",
	}, nil)

	// The demo descriptor lives only in the local registry, not in
	// protoregistry.GlobalFiles, so the reflection server needs an explicit
	// DescriptorResolver.
	opts := reflection.ServerOptions{Services: s, DescriptorResolver: files}
	switch version {
	case "v1":
		reflectv1grpc.RegisterServerReflectionServer(s, reflection.NewServerV1(opts))
	case "v1alpha":
		reflectv1alphagrpc.RegisterServerReflectionServer(s, reflection.NewServer(opts))
	case "none":
		// No reflection service at all — the error path.
	default:
		t.Fatalf("unknown reflection version %q", version)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go s.Serve(lis)
	t.Cleanup(s.Stop)
	return lis.Addr().String()
}

// reflectAndIngest pulls descriptors from target and runs them through the
// standard GRPC ingester.
func reflectAndIngest(t *testing.T, target string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return GRPCReflect(ctx, target, GRPCReflectOptions{})
}

// TestGRPCReflect_MatchesFileIngest is the core B4 guarantee for gRPC: the
// descriptor set pulled over server reflection must ingest to the exact IR
// the file-based path produces for the same proto.
func TestGRPCReflect_MatchesFileIngest(t *testing.T) {
	target := startReflectionServer(t, "v1")

	live, err := reflectAndIngest(t, target)
	if err != nil {
		t.Fatalf("GRPCReflect: %v", err)
	}
	liveAPI, err := GRPC(live)
	if err != nil {
		t.Fatalf("GRPC(live bytes): %v", err)
	}

	fileBytes, err := proto.Marshal(buildDemoFileDescriptorSet())
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	fileAPI, err := GRPC(fileBytes)
	if err != nil {
		t.Fatalf("GRPC(file bytes): %v", err)
	}

	if !reflect.DeepEqual(liveAPI, fileAPI) {
		t.Fatalf("reflection-derived IR differs from file-based IR:\nlive: %+v\nfile: %+v", liveAPI, fileAPI)
	}
	if len(liveAPI.Operations) != 2 {
		t.Fatalf("want 2 operations, got %d", len(liveAPI.Operations))
	}
	for _, id := range []string{"Greeter_SayHello", "Greeter_GetGreeting"} {
		if liveAPI.OperationByID(id) == nil {
			t.Errorf("operation %s not discovered over reflection", id)
		}
	}
}

// TestGRPCReflect_V1AlphaFallback covers servers that predate the v1
// reflection service (still common outside grpc-go).
func TestGRPCReflect_V1AlphaFallback(t *testing.T) {
	target := startReflectionServer(t, "v1alpha")

	live, err := reflectAndIngest(t, target)
	if err != nil {
		t.Fatalf("GRPCReflect against a v1alpha-only server: %v", err)
	}
	api, err := GRPC(live)
	if err != nil {
		t.Fatalf("GRPC(live bytes): %v", err)
	}
	if api.OperationByID("Greeter_SayHello") == nil {
		t.Fatalf("v1alpha fallback did not discover demo.Greeter, ops: %+v", api.Operations)
	}
}

func TestGRPCReflect_NoReflection(t *testing.T) {
	target := startReflectionServer(t, "none")

	_, err := reflectAndIngest(t, target)
	if err == nil {
		t.Fatal("expected an error against a server without reflection")
	}
	if !strings.Contains(err.Error(), "reflection") {
		t.Fatalf("error should say reflection is missing, got: %v", err)
	}
}
