// SPDX-License-Identifier: Apache-2.0

package forge

// E23 T2 end to end: an integration stored with "grpc_protocol":"connect" and
// enabled through the Manager streams from a REAL connect-go server via the
// registered stream_start/stream_session tools — the same path a model drives.
// The store is written as raw JSON on purpose: it pins the persisted key name,
// which is the contract between `forge add --grpc-protocol` and enable.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
	"google.golang.org/protobuf/types/known/structpb"
)

func connectFeedServer(t *testing.T) *httptest.Server {
	t.Helper()
	const proc = "/feed.Feed/Watch"
	mux := http.NewServeMux()
	mux.Handle(proc, connect.NewServerStreamHandler(proc,
		func(_ context.Context, req *connect.Request[structpb.Struct], st *connect.ServerStream[structpb.Struct]) error {
			for i := 1; i <= 2; i++ {
				msg, _ := structpb.NewStruct(map[string]any{
					"text": fmt.Sprintf("hello-%d-%s", i, req.Msg.GetFields()["text"].GetStringValue()),
				})
				if err := st.Send(msg); err != nil {
					return err
				}
			}
			return nil
		}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// enableFeedRaw stores the feed integration with the given raw grpc_protocol
// JSON fragment ("" for none) and enables it globally.
func enableFeedRaw(t *testing.T, baseURL, protocolJSON string) *sharedtools.ToolRegistry {
	t.Helper()
	dir := t.TempDir()
	rel, err := StoreSpec(dir, "feed", feedDescriptorSet(t), "pb")
	if err != nil {
		t.Fatalf("StoreSpec: %v", err)
	}
	store := fmt.Sprintf(`[{"name":"feed","source":"grpc","spec_file":%q,"base_url":%q%s,"policy":{}}]`, rel, baseURL, protocolJSON)
	if err := os.WriteFile(filepath.Join(dir, ".rysh", "forge", "integrations.json"), []byte(store), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := sharedtools.NewToolRegistry()
	mgr := NewManager(reg, dir, nil)
	t.Cleanup(mgr.Close)
	if _, _, err := mgr.EnableByName(context.Background(), "feed", ScopeTarget{}); err != nil {
		t.Fatalf("EnableByName: %v", err)
	}
	return reg
}

// readUntilDone drives stream_start then polls stream_session until the
// session reports it ended, returning everything read.
func readUntilDone(t *testing.T, reg *sharedtools.ToolRegistry) string {
	t.Helper()
	start, ok := reg.Get("feed_stream_start")
	if !ok {
		t.Fatalf("feed_stream_start not registered; names=%v", reg.Names())
	}
	out, err := start.Execute(context.Background(), json.RawMessage(`{"id":"Feed_Watch","args":{"text":"x"}}`))
	if err != nil || out.Error != "" {
		t.Fatalf("stream start: err=%v toolerr=%q", err, out.Error)
	}
	id := out.Metadata["session_id"]
	sess, _ := reg.Get("feed_stream_session")
	var all strings.Builder
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		read, _ := sess.Execute(context.Background(), json.RawMessage(`{"action":"read","session_id":"`+id+`"}`))
		all.WriteString(read.Content)
		all.WriteString(read.Error)
		if strings.Contains(read.Content, "ended") || strings.Contains(read.Content, "HTTP ") {
			return all.String()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session never ended; read so far:\n%s", all.String())
	return ""
}

// TestConnectProtocolStreamsEndToEnd: the opted-in integration streams both
// messages from the real Connect server and ends cleanly.
func TestConnectProtocolStreamsEndToEnd(t *testing.T) {
	srv := connectFeedServer(t)
	got := readUntilDone(t, enableFeedRaw(t, srv.URL, `,"grpc_protocol":"connect"`))
	if !strings.Contains(got, "hello-1-x") || !strings.Contains(got, "hello-2-x") {
		t.Fatalf("session output missing the Connect messages:\n%s", got)
	}
	if strings.Contains(got, "HTTP 415") {
		t.Fatalf("server refused the request as non-Connect:\n%s", got)
	}
}

// TestGatewayProtocolCannotReachConnectServer documents why the setting is
// explicit: the default (grpc-gateway JSON POST) request is refused by a
// Connect server for a streaming procedure, so it must be opted into per API.
func TestGatewayProtocolCannotReachConnectServer(t *testing.T) {
	srv := connectFeedServer(t)
	got := readUntilDone(t, enableFeedRaw(t, srv.URL, ""))
	if !strings.Contains(got, "HTTP 415") {
		t.Fatalf("want the Connect server's 415 for a non-Connect streaming request, got:\n%s", got)
	}
}
