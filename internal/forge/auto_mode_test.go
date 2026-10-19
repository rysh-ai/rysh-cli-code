// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"
)

// bigSpec builds an OpenAPI document with n GET operations.
func bigSpec(n int) []byte {
	paths := make([]string, n)
	for i := range paths {
		paths[i] = fmt.Sprintf(`"/r%d":{"get":{"operationId":"op%d","responses":{"200":{"description":"ok"}}}}`, i, i)
	}
	return []byte(`{"openapi":"3.0.0","info":{"title":"Big","version":"1"},"servers":[{"url":"https://api.example.com"}],"paths":{` +
		strings.Join(paths, ",") + `}}`)
}

// TestStoredAutoModeEnablesDynamic is the regression for `forge add`'s default
// --mode "auto": it is stored verbatim in integrations.json and used to register
// every spec static (120 tool(s), exposure: static). It must round-trip and
// mean auto — dynamic above ~50 ops, static below — as must an older file
// with no mode at all.
func TestStoredAutoModeEnablesDynamic(t *testing.T) {
	cases := []struct {
		name      string
		policy    string // raw "policy" JSON as written to integrations.json
		ops       int
		wantMode  string
		wantTools int
	}{
		{"auto, 120 ops", `{"mode":"auto"}`, 120, "dynamic", 3},
		{"auto, 5 ops", `{"mode":"auto"}`, 5, "static", 5},
		{"no mode (old file), 120 ops", `{}`, 120, "dynamic", 3},
		{"static, 120 ops", `{"mode":"static"}`, 120, "static", 120},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			rel, err := StoreSpec(dir, "big", bigSpec(c.ops), "json")
			if err != nil {
				t.Fatalf("StoreSpec: %v", err)
			}
			raw := fmt.Sprintf(`[{"name":"big","source":"openapi","spec_file":%q,"policy":%s,"enabled":true}]`, rel, c.policy)
			if err := os.WriteFile(storePath(dir), []byte(raw), 0o644); err != nil {
				t.Fatal(err)
			}

			defs, err := LoadStore(dir)
			if err != nil || len(defs) != 1 {
				t.Fatalf("LoadStore: %v %v", defs, err)
			}
			if err := SaveStore(dir, defs); err != nil {
				t.Fatalf("SaveStore: %v", err)
			}
			if c.policy == `{"mode":"auto"}` {
				saved, _ := os.ReadFile(storePath(dir))
				if !strings.Contains(string(saved), `"mode": "auto"`) {
					t.Fatalf("stored mode did not round-trip:\n%s", saved)
				}
			}

			mgr := NewManager(sharedtools.NewToolRegistry(), dir, nil)
			n, mode, err := mgr.EnableByName(context.Background(), "big", mgr.GlobalTarget())
			if err != nil {
				t.Fatalf("EnableByName: %v", err)
			}
			if string(mode) != c.wantMode || n != c.wantTools {
				t.Fatalf("%d tool(s) registered (exposure: %s), want %d (exposure: %s)", n, mode, c.wantTools, c.wantMode)
			}
		})
	}
}
