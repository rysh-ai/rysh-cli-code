// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
)

func TestNATSAPIKeyHeaderSetsTheUpgradeHeader(t *testing.T) {
	var o nats.Options
	if err := NATSAPIKeyHeader("rysh_key")(&o); err != nil {
		t.Fatal(err)
	}
	if got := o.WebSocketConnectionHeaders.Get("X-API-Key"); got != "rysh_key" {
		t.Fatalf("X-API-Key = %q", got)
	}
	var empty nats.Options
	if err := NATSAPIKeyHeader("")(&empty); err != nil || empty.WebSocketConnectionHeaders != nil {
		t.Fatalf("an empty key added headers: %v", empty.WebSocketConnectionHeaders)
	}
}

// Guard (F-86): no upstream NATS dial in this repository builds a URL that
// carries the API key. The eight that did now use NATSAPIKeyHeader.
func TestNoNATSDialPutsTheAPIKeyInTheURL(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, `api_key=" +`) || strings.Contains(line, `&api_key=`) {
				t.Errorf("%s:%d builds a URL with the API key: %s", path, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
