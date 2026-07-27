package proxy

import (
	"strconv"
	"strings"
	"testing"
)

// TestServerPort proves Port() reports the bound loopback port (0 before start),
// so the daemon can record the governed endpoint in the session registry.
func TestServerPort(t *testing.T) {
	srv := New(nil, nil, nil, nil, false)
	if p := srv.Port(); p != 0 {
		t.Fatalf("Port() before Start = %d, want 0", p)
	}
	if _, err := srv.Start(""); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Stop()

	port := srv.Port()
	if port <= 0 {
		t.Fatalf("Port() after Start = %d, want > 0", port)
	}
	// It must match the port in BaseURL (http://127.0.0.1:PORT).
	base := srv.BaseURL()
	i := strings.LastIndex(base, ":")
	if i < 0 || strconv.Itoa(port) != base[i+1:] {
		t.Fatalf("Port() = %d does not match BaseURL %q", port, base)
	}
}
