// SPDX-License-Identifier: Apache-2.0

package forgecmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleOpenAPI = `{
  "openapi": "3.0.0",
  "info": {"title": "Petstore", "version": "1.2.3"},
  "servers": [{"url": "https://api.example.com"}],
  "paths": {
    "/pets": {
      "get": {"operationId": "listPets", "responses": {"200": {"description": "ok"}}},
      "post": {"operationId": "createPet", "responses": {"201": {"description": "created"}}}
    }
  }
}`

// TestRunAddListTargets exercises the shared command path the in-session
// `##forge` handler uses: output goes to an io.Writer, and a spec path is
// resolved relative to workDir (not the process CWD).
func TestRunAddListTargets(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "spec.json"), []byte(sampleOpenAPI), 0o644); err != nil {
		t.Fatal(err)
	}

	// targets
	var buf bytes.Buffer
	if err := Run(work, []string{"targets"}, &buf); err != nil {
		t.Fatalf("targets: %v", err)
	}
	for _, want := range []string{"rysh-toolpack", "docs", "go-sdk", "ts-sdk", "py-sdk", "java-sdk", "mcp-server"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("targets output missing %q:\n%s", want, buf.String())
		}
	}

	// add (relative spec path, resolved against workDir)
	buf.Reset()
	if err := Run(work, []string{"add", "petstore", "spec.json", "--targets", "rysh-toolpack,docs"}, &buf); err != nil {
		t.Fatalf("add: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "ingested") || !strings.Contains(out, "2 operations") {
		t.Fatalf("add output unexpected:\n%s", out)
	}
	for _, f := range []string{
		filepath.Join(work, ".rysh", "forge", "integrations.json"),
		filepath.Join(work, ".rysh", "forge", "petstore", "spec.json"),
		filepath.Join(work, ".rysh", "forge", "petstore", "gen", "rysh-toolpack", "toolpack.json"),
	} {
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("expected generated file %s: %v", f, err)
		}
	}

	// list reflects the added integration
	buf.Reset()
	if err := Run(work, []string{"list"}, &buf); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(buf.String(), "petstore") || !strings.Contains(buf.String(), "disabled") {
		t.Fatalf("list output unexpected:\n%s", buf.String())
	}
}

func TestRunUsageAndUnknown(t *testing.T) {
	work := t.TempDir()

	// no args → usage, no error
	var buf bytes.Buffer
	if err := Run(work, nil, &buf); err != nil {
		t.Fatalf("usage: %v", err)
	}
	if !strings.Contains(buf.String(), "forge add") {
		t.Fatalf("usage output unexpected:\n%s", buf.String())
	}

	// unknown subcommand → error
	buf.Reset()
	if err := Run(work, []string{"bogus"}, &buf); err == nil {
		t.Fatal("expected error for unknown subcommand")
	}
}
