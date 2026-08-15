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
)

// streamingDescriptorSet builds a FileDescriptorSet equivalent to:
//
//	package feed;
//	message Item { string text = 1; }
//	service Feed {
//	  rpc GetItem(Item) returns (Item);           // unary → exposed
//	  rpc Watch(Item) returns (stream Item);      // server-streaming → streaming session
//	  rpc Tail(Item) returns (stream Item);       // server-streaming → streaming session
//	  rpc Chat(stream Item) returns (stream Item);// bidi → skipped
//	}
func streamingDescriptorSet(t *testing.T) []byte {
	t.Helper()
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL

	item := &descriptorpb.DescriptorProto{
		Name: proto.String("Item"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{Name: proto.String("text"), Number: proto.Int32(1), Label: &opt, Type: &str},
		},
	}
	method := func(name string, cs, ss bool) *descriptorpb.MethodDescriptorProto {
		m := &descriptorpb.MethodDescriptorProto{
			Name:       proto.String(name),
			InputType:  proto.String(".feed.Item"),
			OutputType: proto.String(".feed.Item"),
		}
		if cs {
			m.ClientStreaming = proto.Bool(true)
		}
		if ss {
			m.ServerStreaming = proto.Bool(true)
		}
		return m
	}
	fds := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{{
			Name:        proto.String("feed.proto"),
			Package:     proto.String("feed"),
			Syntax:      proto.String("proto3"),
			MessageType: []*descriptorpb.DescriptorProto{item},
			Service: []*descriptorpb.ServiceDescriptorProto{{
				Name: proto.String("Feed"),
				Method: []*descriptorpb.MethodDescriptorProto{
					method("GetItem", false, false),
					method("Watch", false, true),
					method("Tail", false, true),
					method("Chat", true, true),
				},
			}},
		}},
	}
	b, err := proto.Marshal(fds)
	if err != nil {
		t.Fatalf("marshal descriptor set: %v", err)
	}
	return b
}

// TestForgeAdd_ReportsStreamingRouting pins the add-time summary for a gRPC
// source with streaming methods (design 015 §2.2): server-streaming methods
// are reported as EXPOSED via streaming sessions (specific names), while
// client-streaming/bidi methods are reported as skipped with the reason — so
// every claim matches what the ingester actually did.
func TestForgeAdd_ReportsStreamingRouting(t *testing.T) {
	workDir := t.TempDir()
	specPath := filepath.Join(workDir, "feed.pb")
	if err := os.WriteFile(specPath, streamingDescriptorSet(t), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	var out bytes.Buffer
	err := Run(workDir, []string{
		"add", "feed", "feed.pb",
		"--base-url", "http://127.0.0.1:8080",
		"--targets", "docs",
	}, &out)
	if err != nil {
		t.Fatalf("forge add failed: %v\noutput:\n%s", err, out.String())
	}
	got := out.String()

	wantExposed := "2 server-streaming methods exposed as streaming sessions (stream_start / stream_session): Feed_Tail, Feed_Watch"
	if !strings.Contains(got, wantExposed) {
		t.Errorf("add output missing streaming-session summary %q, got:\n%s", wantExposed, got)
	}
	wantSkipped := "1 streaming method skipped (bidi: Feed_Chat) — client/bidi streams cannot ride a JSON-over-HTTP bridge"
	if !strings.Contains(got, wantSkipped) {
		t.Errorf("add output missing client/bidi skip summary %q, got:\n%s", wantSkipped, got)
	}
	// Only the unary method may be counted as an operation.
	if !strings.Contains(got, "1 operations") {
		t.Errorf("add output should report 1 ingested operation (unary only), got:\n%s", got)
	}
}

// TestForgeAdd_NoStreamingNoClaim: a unary-only descriptor must not print any
// skipped/streaming exclusion line — no claims about things that did not happen.
func TestForgeAdd_NoStreamingNoClaim(t *testing.T) {
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
		t.Fatalf("forge add failed: %v\noutput:\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "skipped") {
		t.Errorf("unary-only add printed a skip claim, got:\n%s", out.String())
	}
}

// TestForgeAdd_ReportsSubscriptionsExposed pins the add-time summary for a
// GraphQL schema with a subscription root: subscription fields are reported as
// exposed via streaming sessions over graphql-ws (they used to be silently
// dropped, then skipped; now they are real streams).
func TestForgeAdd_ReportsSubscriptionsExposed(t *testing.T) {
	const schema = `{"data":{"__schema":{
	  "queryType":{"name":"Query"},
	  "subscriptionType":{"name":"Subscription"},
	  "types":[
	    {"kind":"OBJECT","name":"Query","fields":[
	      {"name":"user","args":[],"type":{"kind":"OBJECT","name":"User"}}
	    ]},
	    {"kind":"OBJECT","name":"Subscription","fields":[
	      {"name":"onUserCreated","args":[],"type":{"kind":"OBJECT","name":"User"}},
	      {"name":"onEvent","args":[],"type":{"kind":"OBJECT","name":"User"}}
	    ]}
	  ]
	}}}`
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "schema.graphql.json"), []byte(schema), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	var out bytes.Buffer
	err := Run(workDir, []string{
		"add", "gql", "schema.graphql.json",
		"--base-url", "http://127.0.0.1:8080/graphql",
		"--targets", "docs",
	}, &out)
	if err != nil {
		t.Fatalf("forge add failed: %v\noutput:\n%s", err, out.String())
	}
	got := out.String()

	want := "2 subscription fields exposed as streaming sessions over graphql-ws (graphql_subscribe / stream_session): onEvent, onUserCreated"
	if !strings.Contains(got, want) {
		t.Errorf("add output missing subscription summary %q, got:\n%s", want, got)
	}
	if strings.Contains(got, "skipped") {
		t.Errorf("subscriptions must not be reported as skipped anymore, got:\n%s", got)
	}
}
