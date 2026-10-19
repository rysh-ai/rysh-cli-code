// SPDX-License-Identifier: Apache-2.0

package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// The web UI is compiled into the binary (//go:embed static/*), and the
// renderer build empties static/ before writing it. So a daemon can serve an
// old bundle while the checkout holds a new one, and nothing tells you which
// one is running (design 018 §4). /health now carries a short stamp of the
// embedded index.html — which names Vite's hashed assets, so it changes with
// any bundle change — and `make bundle-stamp-check` compares it with the
// checkout using the same shell hash these tests pin.

func getHealth(t *testing.T, port int) map[string]any {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /health: status %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /health: %v", err)
	}
	return body
}

// onDiskStamp is what `shasum -a 256 internal/web/static/index.html | cut -c1-12`
// prints: the first 12 hex chars of sha256 over the file.
func onDiskStamp(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read static/index.html: %v", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

func TestHealthCarriesTheEmbeddedBundleStamp(t *testing.T) {
	nc := startInProcessNATS(t)
	pub := msg.NewNATSPublisher(nc, msg.DefaultCodecRegistry())
	port := freePort(t)
	s := NewServer(port, "bundle-stamp-session", pub, nc, pub.Codecs())
	s.SetHost("127.0.0.1")
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/health", port))

	want := onDiskStamp(t)
	for i := 0; i < 2; i++ {
		body := getHealth(t, port)
		if body["status"] != "ok" {
			t.Fatalf("call %d: status = %v, want \"ok\" (existing callers check it)", i+1, body["status"])
		}
		if body["web_bundle"] != want {
			t.Fatalf("call %d: web_bundle = %v, want %q", i+1, body["web_bundle"], want)
		}
	}
}

// The stamp is exactly the shell's format — 12 lowercase hex chars of
// sha256(static/index.html) — and nothing else: /health is public, so it
// carries no paths, versions or host names.
func TestWebBundleStampMatchesTheShellHash(t *testing.T) {
	got := webBundleStamp()
	if got != onDiskStamp(t) {
		t.Fatalf("webBundleStamp() = %q, want sha256(static/index.html)[:12] = %q", got, onDiskStamp(t))
	}
	if len(got) != 12 {
		t.Fatalf("stamp %q is %d chars, want 12", got, len(got))
	}
}

// A binary built without static/index.html reports no stamp at all, so
// /health omits web_bundle instead of claiming the hash of an empty file.
func TestBundleStampIsEmptyWhenIndexIsMissing(t *testing.T) {
	if got := bundleStampOf(os.DirFS(t.TempDir()), "static/index.html"); got != "" {
		t.Fatalf("bundleStampOf(missing) = %q, want \"\"", got)
	}
}
