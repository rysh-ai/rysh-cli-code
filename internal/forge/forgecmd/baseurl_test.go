package forgecmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/forge"
)

// relServerSpec has a RELATIVE server URL ("/"), like the bundled weather
// sample — so without an explicit base URL the generated tools would issue
// scheme-less requests.
const relServerSpec = `{
  "openapi": "3.0.0",
  "info": {"title": "weather", "version": "1.0"},
  "servers": [{"url": "/"}],
  "paths": {"/api/weather": {"get": {"operationId": "getWeather", "responses": {"200": {"description": "ok"}}}}}
}`

// TestAddPositionalBaseURL verifies the natural `forge add <name> <spec> <url>`
// form stores the base URL (not just --base-url), and that a missing/relative
// base URL produces a loud warning.
func TestAddPositionalBaseURL(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "spec.json"), []byte(relServerSpec), 0o644); err != nil {
		t.Fatal(err)
	}

	// Positional base URL is stored.
	var buf bytes.Buffer
	if err := Run(work, []string{"add", "weather", "spec.json", "http://localhost:8080", "--targets", "rysh-toolpack"}, &buf); err != nil {
		t.Fatalf("add: %v", err)
	}
	defs, err := forge.LoadStore(work)
	if err != nil || len(defs) != 1 {
		t.Fatalf("load store: err=%v defs=%v", err, defs)
	}
	if defs[0].BaseURL != "http://localhost:8080" {
		t.Fatalf("positional base URL not stored: BaseURL=%q", defs[0].BaseURL)
	}
	if strings.Contains(buf.String(), "no absolute base URL") {
		t.Fatalf("should NOT warn when a positional base URL was given:\n%s", buf.String())
	}

	// No base URL + relative spec server → warns.
	_ = os.RemoveAll(filepath.Join(work, ".rysh"))
	buf.Reset()
	if err := Run(work, []string{"add", "weather2", "spec.json", "--targets", "rysh-toolpack"}, &buf); err != nil {
		t.Fatalf("add2: %v", err)
	}
	if !strings.Contains(buf.String(), "no absolute base URL") {
		t.Fatalf("expected a base-URL warning, got:\n%s", buf.String())
	}
}
