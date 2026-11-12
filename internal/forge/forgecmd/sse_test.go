// SPDX-License-Identifier: Apache-2.0

package forgecmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `forge add` routes an OpenAPI event stream to a streaming SDK method and
// says so.
func TestForgeAddRoutesOpenAPIEventStreams(t *testing.T) {
	dir := t.TempDir()
	spec := `{"openapi":"3.1.0","info":{"title":"T","version":"1"},"servers":[{"url":"https://api.example.com"}],
"paths":{"/e":{"get":{"operationId":"streamE","responses":{"200":{"description":"s","content":{"text/event-stream":{}}}}}},
"/x":{"get":{"operationId":"getX","responses":{"200":{"description":"x","content":{"application/json":{}}}}}}}}`
	if err := os.WriteFile(filepath.Join(dir, "t.json"), []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := Run(dir, []string{"add", "t", "t.json", "--targets", "ts-sdk", "--out", "gen"}, &out); err != nil {
		t.Fatalf("forge add: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "1 server-sent event stream generated as a streaming SDK method: streamE") {
		t.Fatalf("no stream summary:\n%s", out.String())
	}
	ts, err := os.ReadFile(filepath.Join(dir, "gen", "ts-sdk", "client.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ts), "async *streamE(") || !strings.Contains(string(ts), "async getX(") {
		t.Fatalf("client.ts:\n%s", ts)
	}
}
