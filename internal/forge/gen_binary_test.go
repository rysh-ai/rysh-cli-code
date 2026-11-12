// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/forge/ingest"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/gen"
)

// Binary bodies (application/octet-stream, format: binary) go as raw bytes and
// come back as bytes; a query parameter named "path" (or "query", "headers")
// never overwrites the generated method's own locals (rr-order-154 C10 file
// routes found both).
const binarySpec = `{"openapi":"3.1.0","info":{"title":"F","version":"1"},"servers":[{"url":"https://a.example"}],
"paths":{"/files":{
 "put":{"operationId":"putFile","parameters":[{"name":"path","in":"query","required":true,"schema":{"type":"string"}}],
  "requestBody":{"required":true,"content":{"application/octet-stream":{"schema":{"type":"string","format":"binary"}}}},
  "responses":{"204":{"description":"ok"}}},
 "get":{"operationId":"getFile","parameters":[{"name":"path","in":"query","required":true,"schema":{"type":"string"}}],
  "responses":{"200":{"description":"f","content":{"application/octet-stream":{"schema":{"type":"string","format":"binary"}}}}}}}}}`

func genFile(t *testing.T, target, file string) string {
	t.Helper()
	api, err := ingest.OpenAPI([]byte(binarySpec))
	if err != nil {
		t.Fatal(err)
	}
	g, _ := gen.Get(target)
	fs, err := g.Generate(api, gen.Options{PackageName: "f"})
	if err != nil {
		t.Fatal(err)
	}
	return string(fs.Files[file])
}

func TestTSSDKSendsAndReturnsBinaryBodies(t *testing.T) {
	ts := genFile(t, "ts-sdk", "client.ts")
	for _, want := range []string{
		"body: Uint8Array | string;",
		`return this.requestBinary("PUT", path, query, headers, params["body"], false);`,
		`return this.requestBinary("GET", path, query, headers, undefined, true);`,
		`"application/octet-stream"`,
		"new Uint8Array(await resp.arrayBuffer())",
	} {
		if !strings.Contains(ts, want) {
			t.Errorf("ts-sdk lacks %q", want)
		}
	}
	if strings.Contains(ts, `body["body"] = params["body"]`) {
		t.Error("ts-sdk still wraps the binary body as JSON")
	}
}

func TestPySDKSendsAndReturnsBinaryBodiesAndKeepsItsLocals(t *testing.T) {
	py := genFile(t, "py-sdk", "client.py")
	for _, want := range []string{
		"body: Union[bytes, str],",
		`return self._request_binary("PUT", _path, _query, _headers, body, False)`,
		`return self._request_binary("GET", _path, _query, _headers, None, True)`,
		`_path = "/files"`,
		`_query["path"] = path`,
		`"application/octet-stream"`,
	} {
		if !strings.Contains(py, want) {
			t.Errorf("py-sdk lacks %q", want)
		}
	}
	if strings.Contains(py, "\n        path = \"/files\"") {
		t.Error("py-sdk's local path variable shadows the path parameter")
	}
}
