package forge

// Manager-level lifecycle tests for streaming sessions (design 015 §2.1/§2.2):
// enabling a gRPC integration with server-streaming methods registers the
// stream_start/stream_session pair; enabling a GraphQL integration with
// subscriptions registers graphql_subscribe; and every teardown path (Disable,
// UnregisterScope, Close) cancels the live sessions so no stream outlives its
// integration.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

// feedDescriptorSet builds: package feed; service Feed { rpc GetItem(Item)
// returns (Item); rpc Watch(Item) returns (stream Item); }
func feedDescriptorSet(t *testing.T) []byte {
	t.Helper()
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	fds := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{{
			Name:    proto.String("feed.proto"),
			Package: proto.String("feed"),
			Syntax:  proto.String("proto3"),
			MessageType: []*descriptorpb.DescriptorProto{{
				Name: proto.String("Item"),
				Field: []*descriptorpb.FieldDescriptorProto{
					{Name: proto.String("text"), Number: proto.Int32(1), Label: &opt, Type: &str},
				},
			}},
			Service: []*descriptorpb.ServiceDescriptorProto{{
				Name: proto.String("Feed"),
				Method: []*descriptorpb.MethodDescriptorProto{
					{Name: proto.String("GetItem"), InputType: proto.String(".feed.Item"), OutputType: proto.String(".feed.Item")},
					{Name: proto.String("Watch"), InputType: proto.String(".feed.Item"), OutputType: proto.String(".feed.Item"), ServerStreaming: proto.Bool(true)},
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

// enableFeed stores + enables a gRPC integration against baseURL and returns
// the manager and registry.
func enableFeed(t *testing.T, baseURL string) (*Manager, *sharedtools.ToolRegistry, string) {
	t.Helper()
	dir := t.TempDir()
	rel, err := StoreSpec(dir, "feed", feedDescriptorSet(t), "pb")
	if err != nil {
		t.Fatalf("StoreSpec: %v", err)
	}
	def := Integration{Name: "feed", Source: SourceGRPC, SpecFile: rel, BaseURL: baseURL}
	if err := SaveStore(dir, []Integration{def}); err != nil {
		t.Fatalf("SaveStore: %v", err)
	}
	reg := sharedtools.NewToolRegistry()
	mgr := NewManager(reg, dir, nil)
	return mgr, reg, dir
}

// startWatch enables the integration, starts a Watch session through the
// registered tool, and waits for the first frame (so the stream is provably
// open) before returning the session id.
func startWatch(t *testing.T, mgr *Manager, reg *sharedtools.ToolRegistry, target ScopeTarget) string {
	t.Helper()
	if _, _, err := mgr.EnableByName(context.Background(), "feed", target); err != nil {
		t.Fatalf("EnableByName: %v", err)
	}
	tgt := target.Registry
	if tgt == nil {
		tgt = reg
	}
	start, ok := tgt.Get("feed_stream_start")
	if !ok {
		t.Fatalf("feed_stream_start not registered; names=%v", tgt.Names())
	}
	out, err := start.Execute(context.Background(), json.RawMessage(`{"id":"Feed_Watch","args":{"text":"x"}}`))
	if err != nil || out.Error != "" {
		t.Fatalf("stream start: err=%v toolerr=%q", err, out.Error)
	}
	id := out.Metadata["session_id"]

	// Wait for the first frame so the HTTP request is provably in flight
	// before the caller exercises a teardown seam.
	sess, ok := tgt.Get("feed_stream_session")
	if !ok {
		t.Fatalf("feed_stream_session not registered")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		read, _ := sess.Execute(context.Background(), json.RawMessage(`{"action":"read","session_id":"`+id+`"}`))
		if strings.Contains(read.Content, "hello") {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("first frame never arrived: %+v", read)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// blockingNDJSONServer emits one frame then holds the stream open until the
// client cancels; the returned channel closes when it observes cancellation.
func blockingNDJSONServer(t *testing.T) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	cancelled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		fmt.Fprintln(w, `{"result":{"text":"hello"}}`)
		fl.Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	return srv, cancelled
}

func awaitCancel(t *testing.T, c <-chan struct{}, seam string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(5 * time.Second):
		t.Fatalf("stream not cancelled by %s", seam)
	}
}

// TestGRPCEnableRegistersStreamTools: enabling a gRPC integration registers
// unary tools AND the streaming pair, and the streaming tool works end to end.
func TestGRPCEnableRegistersStreamTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/feed.Feed/Watch" {
			fmt.Fprintln(w, `{"result":{"text":"tick"}}`)
			return
		}
		w.Write([]byte(`{"text":"pong"}`))
	}))
	defer srv.Close()

	mgr, reg, _ := enableFeed(t, srv.URL)
	defer mgr.Close()
	n, _, err := mgr.EnableByName(context.Background(), "feed", mgr.GlobalTarget())
	if err != nil {
		t.Fatalf("EnableByName: %v", err)
	}
	// 1 unary tool (static mode) + stream_start + stream_session.
	if n != 3 {
		t.Fatalf("registered %d tools, want 3 (unary + stream pair)", n)
	}
	for _, name := range []string{"feed_Feed_GetItem", "feed_stream_start", "feed_stream_session"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("%s not registered; names=%v", name, reg.Names())
		}
	}

	start, _ := reg.Get("feed_stream_start")
	out, err := start.Execute(context.Background(), json.RawMessage(`{"id":"Feed_Watch","args":{"text":"x"}}`))
	if err != nil || out.Error != "" {
		t.Fatalf("stream start: err=%v toolerr=%q", err, out.Error)
	}
	id := out.Metadata["session_id"]

	sess, _ := reg.Get("feed_stream_session")
	deadline := time.Now().Add(5 * time.Second)
	for {
		read, _ := sess.Execute(context.Background(), json.RawMessage(`{"action":"read","session_id":"`+id+`"}`))
		if strings.Contains(read.Content, "tick") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never read the frame: %+v", read)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStreamsCancelledOnDisable: ##integration disable cancels live sessions.
func TestStreamsCancelledOnDisable(t *testing.T) {
	srv, cancelled := blockingNDJSONServer(t)
	defer srv.Close()
	mgr, reg, _ := enableFeed(t, srv.URL)
	defer mgr.Close()
	startWatch(t, mgr, reg, mgr.GlobalTarget())
	if err := mgr.Disable("feed"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	awaitCancel(t, cancelled, "Disable")
}

// TestStreamsCancelledOnScopeTeardown: tearing down the scope an integration
// was enabled at (pane/lane/tab close) cancels its sessions.
func TestStreamsCancelledOnScopeTeardown(t *testing.T) {
	srv, cancelled := blockingNDJSONServer(t)
	defer srv.Close()
	mgr, reg, _ := enableFeed(t, srv.URL)
	defer mgr.Close()
	lane := sharedtools.NewChildRegistry(reg)
	startWatch(t, mgr, reg, ScopeTarget{Key: "lane:L1", Registry: lane})
	mgr.UnregisterScope("lane:L1")
	awaitCancel(t, cancelled, "UnregisterScope")
}

// TestStreamsCancelledOnClose: manager Close (workspace/daemon shutdown seam)
// cancels every live session.
func TestStreamsCancelledOnClose(t *testing.T) {
	srv, cancelled := blockingNDJSONServer(t)
	defer srv.Close()
	mgr, reg, _ := enableFeed(t, srv.URL)
	startWatch(t, mgr, reg, mgr.GlobalTarget())
	mgr.Close()
	awaitCancel(t, cancelled, "Close")
}

// TestGraphQLEnableRegistersSubscribeTools: a GraphQL integration whose schema
// has subscription fields registers graphql_subscribe + stream_session next to
// the classic schema/query pair, and honors the --ws-url override.
func TestGraphQLEnableRegistersSubscribeTools(t *testing.T) {
	const schema = `{"data":{"__schema":{
	  "queryType":{"name":"Query"},
	  "subscriptionType":{"name":"Subscription"},
	  "types":[
	    {"kind":"OBJECT","name":"Query","fields":[
	      {"name":"user","args":[],"type":{"kind":"OBJECT","name":"User"}}
	    ]},
	    {"kind":"OBJECT","name":"Subscription","fields":[
	      {"name":"onUserCreated","args":[],"type":{"kind":"OBJECT","name":"User"}}
	    ]}
	  ]
	}}}`
	dir := t.TempDir()
	rel, _ := StoreSpec(dir, "gql", []byte(schema), "json")
	def := Integration{Name: "gql", Source: SourceGraphQL, SpecFile: rel, BaseURL: "http://api.test/graphql", WSURL: "ws://api.test/subs"}
	_ = SaveStore(dir, []Integration{def})

	reg := sharedtools.NewToolRegistry()
	mgr := NewManager(reg, dir, nil)
	defer mgr.Close()
	n, mode, err := mgr.EnableByName(context.Background(), "gql", mgr.GlobalTarget())
	if err != nil {
		t.Fatalf("EnableByName: %v", err)
	}
	if n != 4 || mode != "graphql" {
		t.Fatalf("enable result n=%d mode=%q, want 4 tools (schema, query, subscribe, session)", n, mode)
	}
	for _, name := range []string{"gql_graphql_schema", "gql_graphql_query", "gql_graphql_subscribe", "gql_stream_session"} {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("%s not registered; names=%v", name, reg.Names())
		}
	}
}

// TestGraphQLEnableWithoutSubscriptions keeps the legacy 2-tool surface when
// the schema has no subscription root.
func TestGraphQLEnableWithoutSubscriptions(t *testing.T) {
	const schema = `{"data":{"__schema":{
	  "queryType":{"name":"Query"},
	  "types":[
	    {"kind":"OBJECT","name":"Query","fields":[
	      {"name":"user","args":[],"type":{"kind":"OBJECT","name":"User"}}
	    ]}
	  ]
	}}}`
	dir := t.TempDir()
	rel, _ := StoreSpec(dir, "gql", []byte(schema), "json")
	_ = SaveStore(dir, []Integration{{Name: "gql", Source: SourceGraphQL, SpecFile: rel, BaseURL: "http://api.test/graphql"}})

	reg := sharedtools.NewToolRegistry()
	mgr := NewManager(reg, dir, nil)
	defer mgr.Close()
	n, _, err := mgr.EnableByName(context.Background(), "gql", mgr.GlobalTarget())
	if err != nil {
		t.Fatalf("EnableByName: %v", err)
	}
	if n != 2 {
		t.Fatalf("n=%d, want the classic 2 graphql tools", n)
	}
}
