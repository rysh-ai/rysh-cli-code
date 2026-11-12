// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/forge/ingest"
)

// An OpenAPI operation whose success responses are only text/event-stream is
// a push stream (boat gap #5): it belongs in api.Streams, where the unary
// generators do not wrap it as one request/response call that would hang.
const sseSpecJSON = `{
  "openapi": "3.1.0",
  "info": {"title": "Things", "version": "1.0.0"},
  "servers": [{"url": "https://api.example.com"}],
  "paths": {
    "/things/{id}": {"get": {"operationId": "getThing",
      "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
      "responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"type": "object"}}}}}}},
    "/things/{id}/events": {"get": {"operationId": "streamThingEvents",
      "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
      "responses": {"200": {"description": "events", "content": {"text/event-stream": {"schema": {"type": "string"}}}},
                    "404": {"description": "no", "content": {"application/json": {"schema": {"type": "object"}}}}}}},
    "/mixed": {"get": {"operationId": "getMixed",
      "responses": {"200": {"description": "either", "content": {"application/json": {}, "text/event-stream": {}}}}}}
  }
}`

const sseSpecYAML = `openapi: 3.0.3
info: {title: Things, version: 1.0.0}
servers: [{url: "https://api.example.com"}]
paths:
  /things/{id}/events:
    get:
      operationId: streamThingEvents
      parameters: [{name: id, in: path, required: true, schema: {type: string}}]
      responses:
        "200":
          description: events
          content:
            text/event-stream; charset=utf-8:
              schema: {type: string}
`

func TestRouteEventStreamsMovesSSEOperations(t *testing.T) {
	api, err := ingest.OpenAPI([]byte(sseSpecJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := RouteEventStreams(api, []byte(sseSpecJSON)); err != nil {
		t.Fatal(err)
	}
	ops := map[string]bool{}
	for _, op := range api.Operations {
		ops[op.ID] = true
	}
	if ops["streamThingEvents"] {
		t.Fatal("the event stream is still a unary operation")
	}
	if !ops["getThing"] || !ops["getMixed"] {
		t.Fatalf("JSON operations moved: %v", ops)
	}
	if len(api.Streams) != 1 || api.Streams[0].ID != "streamThingEvents" || api.Streams[0].Method != "GET" {
		t.Fatalf("streams = %+v", api.Streams)
	}
	// Idempotent: a second pass changes nothing.
	if err := RouteEventStreams(api, []byte(sseSpecJSON)); err != nil || len(api.Streams) != 1 || len(api.Operations) != 2 {
		t.Fatalf("second pass: err=%v streams=%d ops=%d", err, len(api.Streams), len(api.Operations))
	}
}

func TestRouteEventStreamsReadsYAMLAndMediaTypeParameters(t *testing.T) {
	api, err := ingest.OpenAPI([]byte(sseSpecYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := RouteEventStreams(api, []byte(sseSpecYAML)); err != nil {
		t.Fatal(err)
	}
	if len(api.Streams) != 1 || len(api.Operations) != 0 {
		t.Fatalf("streams=%+v ops=%+v", api.Streams, api.Operations)
	}
}

func TestRouteEventStreamsLeavesOtherSourcesAlone(t *testing.T) {
	api, err := ingest.OpenAPI([]byte(sseSpecJSON))
	if err != nil {
		t.Fatal(err)
	}
	api.SourceType = "grpc"
	if err := RouteEventStreams(api, []byte(sseSpecJSON)); err != nil {
		t.Fatal(err)
	}
	if len(api.Streams) != 0 {
		t.Fatalf("a non-OpenAPI source was rewritten: %+v", api.Streams)
	}
}

// Path-level parameters (a list next to the methods) are not operations.
func TestRouteEventStreamsSkipsPathLevelParameters(t *testing.T) {
	spec := `{"openapi":"3.1.0","info":{"title":"T","version":"1"},"servers":[{"url":"https://a.example"}],
"paths":{"/r/{id}/events":{"parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],
"get":{"operationId":"streamR","responses":{"200":{"description":"s","content":{"text/event-stream":{}}}}},
"summary":"text","x-extra":{"a":1}}}}`
	api, err := ingest.OpenAPI([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	if err := RouteEventStreams(api, []byte(spec)); err != nil {
		t.Fatalf("path-level parameters: %v", err)
	}
	if len(api.Streams) != 1 || len(api.Operations) != 0 {
		t.Fatalf("streams=%+v ops=%+v", api.Streams, api.Operations)
	}
}
