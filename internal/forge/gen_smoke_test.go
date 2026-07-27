package forge

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/gen"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/ingest"

	// Register all generator targets.
	_ "github.com/rysh-ai/rysh-cli-code/internal/forge/gen/docs"
	_ "github.com/rysh-ai/rysh-cli-code/internal/forge/gen/gosdk"
	_ "github.com/rysh-ai/rysh-cli-code/internal/forge/gen/javasdk"
	_ "github.com/rysh-ai/rysh-cli-code/internal/forge/gen/mcpserver"
	_ "github.com/rysh-ai/rysh-cli-code/internal/forge/gen/pysdk"
	_ "github.com/rysh-ai/rysh-cli-code/internal/forge/gen/tssdk"
	_ "github.com/rysh-ai/rysh-cli-code/internal/forge/toolpack"
)

const smokeSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "Smoke", "version": "1.0.0"},
  "servers": [{"url": "https://api.example.com"}],
  "components": {"securitySchemes": {"k": {"type": "apiKey", "in": "header", "name": "X-Api-Key"}}},
  "security": [{"k": []}],
  "paths": {
    "/things/{id}": {"get": {"operationId": "getThing",
      "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}, {"name": "verbose", "in": "query", "schema": {"type": "boolean"}}],
      "responses": {"200": {"description": "ok"}}}},
    "/things": {"post": {"operationId": "createThing",
      "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {"name": {"type": "string"}}}}}},
      "responses": {"201": {"description": "ok"}}}}
  }
}`

func TestAllGeneratorsProduceOutput(t *testing.T) {
	api, err := ingest.OpenAPI([]byte(smokeSpec))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	want := []string{"rysh-toolpack", "docs", "mcp-server", "go-sdk", "ts-sdk", "py-sdk", "java-sdk"}
	for _, name := range want {
		g, ok := gen.Get(name)
		if !ok {
			t.Errorf("generator %q not registered (have %v)", name, gen.Names())
			continue
		}
		fs, err := g.Generate(api, gen.Options{Prefix: "smoke_", PackageName: "smoke", ModulePath: "example.com/smoke"})
		if err != nil {
			t.Errorf("%s.Generate: %v", name, err)
			continue
		}
		if len(fs.Files) == 0 {
			t.Errorf("%s produced no files", name)
		}
	}
}

// TestEmittedGoCompiles writes the generated Go targets (mcp-server, go-sdk) and
// compiles them — verifying the emitted source (especially the mcp-server
// template) is valid Go. Skipped when the go toolchain is unavailable.
func TestEmittedGoCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping emitted-code compile in -short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not found")
	}
	api, err := ingest.OpenAPI([]byte(smokeSpec))
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	for _, target := range []string{"mcp-server", "go-sdk"} {
		g, _ := gen.Get(target)
		fs, err := g.Generate(api, gen.Options{Prefix: "smoke_", ModulePath: "example.com/smoke-" + target})
		if err != nil {
			t.Fatalf("%s generate: %v", target, err)
		}
		dir := filepath.Join(t.TempDir(), target)
		if err := fs.WriteDir(dir); err != nil {
			t.Fatalf("write %s: %v", target, err)
		}
		cmd := exec.Command(goBin, "build", "./...")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("emitted %s does not compile: %v\n%s", target, err, out)
		}
	}
}
