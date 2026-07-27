package toolpack

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

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/runtime"
)

func grpcStreamAPI() *ir.API {
	return &ir.API{
		Name: "feed", Version: "proto", SourceType: "grpc",
		Streams: []ir.Operation{
			{ID: "Feed_Watch", Method: "POST", Path: "/feed.Feed/Watch",
				RequestBody: &ir.Schema{Type: "object", Properties: map[string]*ir.Schema{"topic": {Type: "string"}}}},
			{ID: "Feed_Publish", Method: "POST", Path: "/feed.Feed/Publish", Mutating: true},
		},
	}
}

// TestRegisterGRPCStreams pins the 2-tool surface (stream_start +
// stream_session, mirroring bash_background/bash_session) and the end-to-end
// start → read → stop flow through the registered tools.
func TestRegisterGRPCStreams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for i := 1; i <= 2; i++ {
			fmt.Fprintf(w, `{"result":{"n":%d}}`+"\n", i)
			fl.Flush()
		}
	}))
	defer srv.Close()

	api := grpcStreamAPI()
	reg := sharedtools.NewToolRegistry()
	exec := runtime.NewHTTPExecutor(srv.URL, nil, runtime.Credential{}, runtime.Options{})
	sm := runtime.NewStreamManager(runtime.StreamOptions{NoSweeper: true})
	defer sm.CloseAll()

	names := RegisterGRPCStreams(reg, api, exec, sm, Policy{Prefix: "feed_"})
	if len(names) != 2 || names[0] != "feed_stream_start" || names[1] != "feed_stream_session" {
		t.Fatalf("names = %v, want [feed_stream_start feed_stream_session]", names)
	}

	start, _ := reg.Get("feed_stream_start")
	out, err := start.Execute(context.Background(), json.RawMessage(`{"id":"Feed_Watch","args":{"topic":"x"}}`))
	if err != nil || out.Error != "" {
		t.Fatalf("start: err=%v toolerr=%q", err, out.Error)
	}
	id := out.Metadata["session_id"]
	if id == "" || !strings.Contains(out.Content, "Streaming session started") {
		t.Fatalf("start output missing session id: %+v", out)
	}

	sess, _ := reg.Get("feed_stream_session")
	var read *sharedtools.ToolOutput
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		read, _ = sess.Execute(context.Background(), json.RawMessage(`{"action":"read","session_id":"`+id+`"}`))
		if strings.Contains(read.Content, `"n": 2`) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if read == nil || !strings.Contains(read.Content, `"n": 2`) {
		t.Fatalf("read never returned the frames: %+v", read)
	}

	list, _ := sess.Execute(context.Background(), json.RawMessage(`{"action":"list"}`))
	if !strings.Contains(list.Content, id) || !strings.Contains(list.Content, "grpc-stream") {
		t.Fatalf("list output missing the session: %s", list.Content)
	}

	stop, _ := sess.Execute(context.Background(), json.RawMessage(`{"action":"stop","session_id":"`+id+`"}`))
	if stop.Error != "" || !strings.Contains(stop.Content, "stopped") {
		t.Fatalf("stop: %+v", stop)
	}
	// The session is gone afterwards.
	gone, _ := sess.Execute(context.Background(), json.RawMessage(`{"action":"read","session_id":"`+id+`"}`))
	if gone.Error == "" {
		t.Fatalf("read after stop should error, got %+v", gone)
	}
}

// TestGRPCStreamStartApproval mirrors the invoke_endpoint convention: gating
// follows the target stream method's Mutating flag, and ForceApproval gates
// everything. Unknown ids are gated (safe default).
func TestGRPCStreamStartApproval(t *testing.T) {
	api := grpcStreamAPI()
	reg := sharedtools.NewToolRegistry()
	sm := runtime.NewStreamManager(runtime.StreamOptions{NoSweeper: true})
	defer sm.CloseAll()
	exec := runtime.NewHTTPExecutor("http://127.0.0.1:1", nil, runtime.Credential{}, runtime.Options{})
	RegisterGRPCStreams(reg, api, exec, sm, Policy{Prefix: "feed_"})

	start, _ := reg.Get("feed_stream_start")
	if start.RequiresApproval(json.RawMessage(`{"id":"Feed_Watch"}`)) {
		t.Errorf("Watch (read) must not require approval")
	}
	if !start.RequiresApproval(json.RawMessage(`{"id":"Feed_Publish"}`)) {
		t.Errorf("Publish (mutating) must require approval")
	}
	if !start.RequiresApproval(json.RawMessage(`{"id":"Nope"}`)) {
		t.Errorf("unknown stream id must require approval")
	}

	reg2 := sharedtools.NewToolRegistry()
	RegisterGRPCStreams(reg2, api, exec, sm, Policy{Prefix: "feed_", ForceApproval: true})
	start2, _ := reg2.Get("feed_stream_start")
	if !start2.RequiresApproval(json.RawMessage(`{"id":"Feed_Watch"}`)) {
		t.Errorf("ForceApproval must gate stream starts too")
	}
}

// TestRegisterGRPCStreamsNoStreams: an API without streams registers nothing.
func TestRegisterGRPCStreamsNoStreams(t *testing.T) {
	api := &ir.API{Name: "plain", SourceType: "grpc"}
	reg := sharedtools.NewToolRegistry()
	sm := runtime.NewStreamManager(runtime.StreamOptions{NoSweeper: true})
	defer sm.CloseAll()
	exec := runtime.NewHTTPExecutor("http://127.0.0.1:1", nil, runtime.Credential{}, runtime.Options{})
	if names := RegisterGRPCStreams(reg, api, exec, sm, Policy{Prefix: "p_"}); names != nil {
		t.Fatalf("no streams ⇒ no tools, got %v", names)
	}
}

// TestRegisterGraphQLWithSubscriptions: RegisterGraphQL with a stream manager
// adds graphql_subscribe + stream_session, the schema tool lists subscription
// fields, and the subscribe tool rejects non-subscription documents.
func TestRegisterGraphQLWithSubscriptions(t *testing.T) {
	api := graphqlAPI()
	api.Streams = []ir.Operation{{ID: "onUserCreated", Method: "POST", Path: "/graphql", Tags: []string{"Subscription"}}}
	reg := sharedtools.NewToolRegistry()
	exec := runtime.NewGraphQLExecutor("https://x/graphql", nil, runtime.Credential{}, runtime.Options{})
	sm := runtime.NewStreamManager(runtime.StreamOptions{NoSweeper: true})
	defer sm.CloseAll()

	names := RegisterGraphQL(reg, api, exec, sm, Policy{Prefix: "gql_"})
	want := []string{"gql_graphql_schema", "gql_graphql_query", "gql_graphql_subscribe", "gql_stream_session"}
	if len(names) != len(want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	for _, n := range want {
		if _, ok := reg.Get(n); !ok {
			t.Errorf("tool %s not registered; names=%v", n, names)
		}
	}

	schema, _ := reg.Get("gql_graphql_schema")
	out, _ := schema.Execute(context.Background(), json.RawMessage(`{}`))
	if !strings.Contains(out.Content, "onUserCreated") || !strings.Contains(out.Content, "graphql_subscribe") {
		t.Fatalf("schema tool must list subscriptions with the subscribe hint: %s", out.Content)
	}

	sub, _ := reg.Get("gql_graphql_subscribe")
	bad, _ := sub.Execute(context.Background(), json.RawMessage(`{"query":"query { user { id } }"}`))
	if bad.Error == "" || !strings.Contains(bad.Error, "subscription") {
		t.Fatalf("subscribe must reject non-subscription documents, got %+v", bad)
	}
	if sub.RequiresApproval(json.RawMessage(`{"query":"subscription { x }"}`)) {
		t.Errorf("subscriptions are reads; no approval without ForceApproval")
	}
}

// TestRegisterGraphQLNilStreamManager: without a stream manager the surface
// stays exactly the legacy 2 tools even when the schema has subscriptions.
func TestRegisterGraphQLNilStreamManager(t *testing.T) {
	api := graphqlAPI()
	api.Streams = []ir.Operation{{ID: "onUserCreated", Method: "POST", Path: "/graphql"}}
	reg := sharedtools.NewToolRegistry()
	exec := runtime.NewGraphQLExecutor("https://x/graphql", nil, runtime.Credential{}, runtime.Options{})
	names := RegisterGraphQL(reg, api, exec, nil, Policy{Prefix: "gql_"})
	if len(names) != 2 {
		t.Fatalf("nil sm ⇒ 2 tools, got %v", names)
	}
}
