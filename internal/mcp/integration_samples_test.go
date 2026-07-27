//go:build mcpsamples

// Integration tests that validate the MCP client against the real reference
// servers in rysh-mcp-samples. They are gated behind the "mcpsamples" build tag
// so the default test suite stays fast and hermetic. Run with:
//
//	GOWORK=off go test -tags mcpsamples ./internal/mcp/... -run Sample -v
//
// They build (stdio) or run (http) the sample binaries on demand and skip if the
// samples tree or the Go toolchain is unavailable.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// findSamplesDir walks up from the working directory to locate the in-repo
// rysh-mcp-samples tree.
func findSamplesDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		cand := filepath.Join(dir, "rysh-mcp-samples")
		if _, err := os.Stat(filepath.Join(cand, "mcp-server", "main.go")); err == nil {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("rysh-mcp-samples not found; skipping sample integration test")
		}
		dir = parent
	}
	return ""
}

func buildSample(t *testing.T, sampleDir string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sample-bin")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = sampleDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not build sample %s: %v\n%s", sampleDir, err, out)
	}
	return bin
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestSampleStdioServer exercises the REAL subprocess stdio path (process spawn,
// pipes, stderr drain) against the canonical stdio MCP server.
func TestSampleStdioServer(t *testing.T) {
	samples := findSamplesDir(t)
	serverDir := filepath.Join(samples, "mcp-server")
	bin := buildSample(t, serverDir)

	reg := sharedtools.NewToolRegistry()
	mgr := NewManager(reg, t.TempDir())
	defer mgr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	n, err := mgr.AddServer(ctx, ServerDef{Name: "weather", Transport: TransportStdio, Command: bin})
	if err != nil {
		t.Fatalf("AddServer(stdio): %v", err)
	}
	if n < 2 {
		t.Fatalf("expected ≥2 tools from sample, got %d", n)
	}

	te, ok := reg.Get("weather_get_weather")
	if !ok {
		t.Fatalf("weather_get_weather not registered; names=%v", reg.Names())
	}
	out, err := te.Execute(ctx, json.RawMessage(`{"city":"london"}`))
	if err != nil {
		t.Fatalf("Execute get_weather: %v", err)
	}
	if !strings.Contains(strings.ToLower(out.Content), "london") {
		t.Fatalf("unexpected weather result: %q", out.Content)
	}
	t.Logf("stdio sample get_weather(london) => %s", strings.ReplaceAll(out.Content, "\n", " "))
}

// TestSampleHTTPWrapper validates the REAL Streamable-HTTP path against the
// sample REST server + MCP wrapper.
func TestSampleHTTPWrapper(t *testing.T) {
	samples := findSamplesDir(t)
	restBin := buildSample(t, filepath.Join(samples, "sample-rest-server"))
	wrapBin := buildSample(t, filepath.Join(samples, "mcp-rest-api-wrapper"))

	restPort := freePort(t)
	mcpPort := freePort(t)

	rest := exec.Command(restBin)
	rest.Env = append(os.Environ(), fmt.Sprintf("MCP_LISTEN_ADDR=:%d", restPort))
	if err := rest.Start(); err != nil {
		t.Fatalf("start rest server: %v", err)
	}
	defer func() { _ = rest.Process.Kill(); _ = rest.Wait() }()

	wrap := exec.Command(wrapBin)
	wrap.Env = append(os.Environ(),
		fmt.Sprintf("MCP_LISTEN_ADDR=:%d", mcpPort),
		fmt.Sprintf("REST_API_URL=http://127.0.0.1:%d", restPort),
	)
	if err := wrap.Start(); err != nil {
		t.Fatalf("start wrapper: %v", err)
	}
	defer func() { _ = wrap.Process.Kill(); _ = wrap.Wait() }()

	mcpURL := fmt.Sprintf("http://127.0.0.1:%d/mcp", mcpPort)
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/healthz", mcpPort)
	waitHTTP(t, healthURL, 10*time.Second)

	reg := sharedtools.NewToolRegistry()
	mgr := NewManager(reg, t.TempDir())
	defer mgr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	n, err := mgr.AddServer(ctx, ServerDef{Name: "weather", Transport: TransportHTTP, URL: mcpURL})
	if err != nil {
		t.Fatalf("AddServer(http): %v", err)
	}
	if n < 2 {
		t.Fatalf("expected ≥2 tools from wrapper, got %d", n)
	}

	te, ok := reg.Get("weather_get_weather")
	if !ok {
		t.Fatalf("weather_get_weather not registered; names=%v", reg.Names())
	}
	out, err := te.Execute(ctx, json.RawMessage(`{"city":"tokyo"}`))
	if err != nil {
		t.Fatalf("Execute get_weather: %v", err)
	}
	if out.Error != "" {
		t.Fatalf("tool error: %s", out.Error)
	}
	if !strings.Contains(strings.ToLower(out.Content), "tokyo") {
		t.Fatalf("unexpected weather result: %q", out.Content)
	}
	t.Logf("http wrapper get_weather(tokyo) => %s", strings.ReplaceAll(out.Content, "\n", " "))
}

func waitHTTP(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("service at %s did not become healthy within %s", url, timeout)
}
