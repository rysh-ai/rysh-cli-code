// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"crypto/tls"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	reflectv1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	reflectv1alpha "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// GRPCReflectOptions configures a live reflection fetch.
type GRPCReflectOptions struct {
	// TLS dials with TLS using the system roots; the default is plaintext
	// (h2c), which is what local/dev gRPC servers speak.
	TLS bool
	// Metadata is sent as call metadata on the reflection stream (e.g.
	// authorization tokens for reflection-gated servers).
	Metadata map[string]string
}

// GRPCReflect connects to a RUNNING gRPC server at target ("host:port"), uses
// the server reflection protocol to discover its services, and returns the
// wire bytes of a FileDescriptorSet containing every file needed to describe
// them — the exact input the file-based GRPC ingester consumes, so callers
// store it and flow it through the existing pipeline unchanged.
//
// The v1 reflection service is tried first, falling back to v1alpha when the
// server predates v1 (still common outside grpc-go).
func GRPCReflect(ctx context.Context, target string, opts GRPCReflectOptions) ([]byte, error) {
	creds := insecure.NewCredentials()
	if opts.TLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", target, err)
	}
	defer conn.Close()

	for k, v := range opts.Metadata {
		ctx = metadata.AppendToOutgoingContext(ctx, k, v)
	}

	files, err := reflectFiles(ctx, &reflectV1Client{c: reflectv1.NewServerReflectionClient(conn)})
	if status.Code(err) == codes.Unimplemented {
		files, err = reflectFiles(ctx, &reflectV1AlphaClient{c: reflectv1alpha.NewServerReflectionClient(conn)})
	}
	if status.Code(err) == codes.Unimplemented {
		return nil, fmt.Errorf("%s does not expose gRPC server reflection (v1 or v1alpha) — enable it, or pass a compiled FileDescriptorSet file instead", target)
	}
	if err != nil {
		return nil, fmt.Errorf("reflect %s: %w", target, err)
	}
	return proto.Marshal(&descriptorpb.FileDescriptorSet{File: files})
}

// reflectFiles drives one reflection stream: list services, then fetch the
// descriptor closure of each. Files are deduped by name (servers return each
// file's transitive dependencies with every response) and sorted for
// deterministic stored specs.
func reflectFiles(ctx context.Context, rc reflectClient) ([]*descriptorpb.FileDescriptorProto, error) {
	services, err := rc.listServices(ctx)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var files []*descriptorpb.FileDescriptorProto
	var reflectable int
	for _, svc := range services {
		// grpc.* services (reflection itself, health, channelz) are server
		// plumbing, not the API surface being forged.
		if strings.HasPrefix(svc, "grpc.") {
			continue
		}
		reflectable++
		raw, err := rc.fileContainingSymbol(svc)
		if err != nil {
			return nil, fmt.Errorf("descriptors for service %s: %w", svc, err)
		}
		for _, b := range raw {
			fd := &descriptorpb.FileDescriptorProto{}
			if err := proto.Unmarshal(b, fd); err != nil {
				return nil, fmt.Errorf("parse FileDescriptorProto for service %s: %w", svc, err)
			}
			if !seen[fd.GetName()] {
				seen[fd.GetName()] = true
				files = append(files, fd)
			}
		}
	}
	if reflectable == 0 {
		return nil, fmt.Errorf("server lists no services besides gRPC plumbing (grpc.*) — nothing to forge")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].GetName() < files[j].GetName() })
	return files, nil
}

// reflectClient abstracts the v1/v1alpha reflection streams, whose message
// types are distinct but field-for-field identical.
type reflectClient interface {
	listServices(ctx context.Context) ([]string, error)
	fileContainingSymbol(symbol string) ([][]byte, error)
}

type reflectV1Client struct {
	c      reflectv1.ServerReflectionClient
	stream reflectv1.ServerReflection_ServerReflectionInfoClient
}

func (r *reflectV1Client) listServices(ctx context.Context) ([]string, error) {
	var err error
	if r.stream, err = r.c.ServerReflectionInfo(ctx); err != nil {
		return nil, err
	}
	resp, err := r.roundTrip(&reflectv1.ServerReflectionRequest{
		MessageRequest: &reflectv1.ServerReflectionRequest_ListServices{ListServices: ""},
	})
	if err != nil {
		return nil, err
	}
	list := resp.GetListServicesResponse()
	if list == nil {
		return nil, fmt.Errorf("reflection server returned no service list")
	}
	names := make([]string, 0, len(list.GetService()))
	for _, s := range list.GetService() {
		names = append(names, s.GetName())
	}
	return names, nil
}

func (r *reflectV1Client) fileContainingSymbol(symbol string) ([][]byte, error) {
	resp, err := r.roundTrip(&reflectv1.ServerReflectionRequest{
		MessageRequest: &reflectv1.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: symbol},
	})
	if err != nil {
		return nil, err
	}
	fdr := resp.GetFileDescriptorResponse()
	if fdr == nil {
		return nil, fmt.Errorf("no file descriptor response for %q", symbol)
	}
	return fdr.GetFileDescriptorProto(), nil
}

// roundTrip sends one request and surfaces the in-band ErrorResponse (the
// reflection protocol reports per-request failures inside the stream, not as
// stream errors) as a *status.Status so codes propagate.
func (r *reflectV1Client) roundTrip(req *reflectv1.ServerReflectionRequest) (*reflectv1.ServerReflectionResponse, error) {
	if err := r.stream.Send(req); err != nil {
		// Send reports io.EOF on a dead stream; the real status (e.g.
		// Unimplemented, which drives the v1alpha fallback) comes from Recv.
		if _, rerr := r.stream.Recv(); rerr != nil {
			return nil, rerr
		}
		return nil, err
	}
	resp, err := r.stream.Recv()
	if err != nil {
		return nil, err
	}
	if e := resp.GetErrorResponse(); e != nil {
		return nil, status.Error(codes.Code(e.GetErrorCode()), e.GetErrorMessage())
	}
	return resp, nil
}

type reflectV1AlphaClient struct {
	c      reflectv1alpha.ServerReflectionClient
	stream reflectv1alpha.ServerReflection_ServerReflectionInfoClient
}

func (r *reflectV1AlphaClient) listServices(ctx context.Context) ([]string, error) {
	var err error
	if r.stream, err = r.c.ServerReflectionInfo(ctx); err != nil {
		return nil, err
	}
	resp, err := r.roundTrip(&reflectv1alpha.ServerReflectionRequest{
		MessageRequest: &reflectv1alpha.ServerReflectionRequest_ListServices{ListServices: ""},
	})
	if err != nil {
		return nil, err
	}
	list := resp.GetListServicesResponse()
	if list == nil {
		return nil, fmt.Errorf("reflection server returned no service list")
	}
	names := make([]string, 0, len(list.GetService()))
	for _, s := range list.GetService() {
		names = append(names, s.GetName())
	}
	return names, nil
}

func (r *reflectV1AlphaClient) fileContainingSymbol(symbol string) ([][]byte, error) {
	resp, err := r.roundTrip(&reflectv1alpha.ServerReflectionRequest{
		MessageRequest: &reflectv1alpha.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: symbol},
	})
	if err != nil {
		return nil, err
	}
	fdr := resp.GetFileDescriptorResponse()
	if fdr == nil {
		return nil, fmt.Errorf("no file descriptor response for %q", symbol)
	}
	return fdr.GetFileDescriptorProto(), nil
}

func (r *reflectV1AlphaClient) roundTrip(req *reflectv1alpha.ServerReflectionRequest) (*reflectv1alpha.ServerReflectionResponse, error) {
	if err := r.stream.Send(req); err != nil {
		// See reflectV1Client.roundTrip: the real status comes from Recv.
		if _, rerr := r.stream.Recv(); rerr != nil {
			return nil, rerr
		}
		return nil, err
	}
	resp, err := r.stream.Recv()
	if err != nil {
		return nil, err
	}
	if e := resp.GetErrorResponse(); e != nil {
		return nil, status.Error(codes.Code(e.GetErrorCode()), e.GetErrorMessage())
	}
	return resp, nil
}
