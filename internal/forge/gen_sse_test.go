// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/forge/ingest"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/gen"
)

// The TS and Python SDKs give an OpenAPI event stream a streaming method
// (an async generator / an iterator of ServerSentEvent), not a JSON call.
func TestSDKGeneratorsEmitEventStreams(t *testing.T) {
	api, err := ingest.OpenAPI([]byte(sseSpecJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := RouteEventStreams(api, []byte(sseSpecJSON)); err != nil {
		t.Fatal(err)
	}
	gen1 := func(target, file string) string {
		g, ok := gen.Get(target)
		if !ok {
			t.Fatalf("no %s target", target)
		}
		fs, err := g.Generate(api, gen.Options{PackageName: "things"})
		if err != nil {
			t.Fatal(err)
		}
		return string(fs.Files[file])
	}
	ts := gen1("ts-sdk", "client.ts")
	for _, want := range []string{
		"export interface ServerSentEvent",
		"async *streamThingEvents(",
		"AsyncGenerator<ServerSentEvent>",
		`"text/event-stream"`,
		"signal",
	} {
		if !strings.Contains(ts, want) {
			t.Errorf("ts-sdk lacks %q", want)
		}
	}
	if strings.Contains(ts, "async streamThingEvents(") {
		t.Error("ts-sdk also emits the stream as a unary call")
	}
	py := gen1("py-sdk", "client.py")
	for _, want := range []string{
		"class ServerSentEvent:",
		"def stream_thing_events(",
		"Iterator[ServerSentEvent]",
		`"text/event-stream"`,
	} {
		if !strings.Contains(py, want) {
			t.Errorf("py-sdk lacks %q", want)
		}
	}

	// A gRPC/GraphQL stream is not an HTTP event stream: no SSE method.
	api.SourceType = "grpc"
	if strings.Contains(gen1("ts-sdk", "client.ts"), "streamThingEvents") || strings.Contains(gen1("py-sdk", "client.py"), "stream_thing_events") {
		t.Error("a non-OpenAPI stream got an SSE method")
	}

	// A spec with no stream emits no SSE helper at all.
	plain, err := ingest.OpenAPI([]byte(smokeSpec))
	if err != nil {
		t.Fatal(err)
	}
	g, _ := gen.Get("ts-sdk")
	fs, _ := g.Generate(plain, gen.Options{})
	if strings.Contains(string(fs.Files["client.ts"]), "ServerSentEvent") {
		t.Error("ts-sdk emits the SSE helper for a spec without streams")
	}
}
