// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFindDownloadedCompanyConfigPicksTheNewest(t *testing.T) {
	downloads := t.TempDir()
	session := t.TempDir()
	old := filepath.Join(downloads, "old-cafe.rysh.config.yaml")
	newer := filepath.Join(downloads, "pera-bistro.rysh.config.yaml")
	for _, p := range []string{old, newer} {
		if err := os.WriteFile(p, []byte("upstream: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	// A file with the wrong suffix and a directory with the right one are ignored.
	_ = os.WriteFile(filepath.Join(downloads, "rysh.config.yaml"), []byte("x"), 0o600)
	_ = os.Mkdir(filepath.Join(downloads, "dir.rysh.config.yaml"), 0o755)
	// What browsers do to the name: a duplicate download, a Safari ".txt", upper case.
	for _, name := range []string{"pera.rysh.config (1).yaml", "Pera.RYSH.CONFIG.yaml.txt", "x.rysh.config.yml"} {
		if !isCompanyConfigName(name) {
			t.Errorf("%q must be recognised as a company config", name)
		}
	}
	for _, name := range []string{"rysh.config.yaml", "notes.yaml", "pera.rysh.config.yaml.zip"} {
		if isCompanyConfigName(name) {
			t.Errorf("%q must not be recognised", name)
		}
	}

	got, err := findDownloadedCompanyConfig([]string{session, downloads})
	if err != nil {
		t.Fatal(err)
	}
	if got != newer {
		t.Fatalf("got %s, want the newest %s", got, newer)
	}
	_, err = findDownloadedCompanyConfig([]string{session, filepath.Join(session, "missing")})
	if err == nil {
		t.Fatal("no file anywhere must be an error the user can read")
	}
	if !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), "0 company config(s)") {
		t.Fatalf("the error must say what was searched: %v", err)
	}
}

func TestWriteClaudeMCPConfigMergesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	existing := `{"mcpServers": {"github": {"command": "gh-mcp"}, "company": {"command": "/old/ry", "args": ["x"]}}, "other": 1}`
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := writeClaudeMCPConfig(dir, "/usr/local/bin/ry", "/w/rysh.config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("written .mcp.json is not JSON: %v", err)
	}
	servers := doc["mcpServers"].(map[string]any)
	if _, kept := servers["github"]; !kept {
		t.Fatal("an unrelated server must survive")
	}
	if doc["other"] != float64(1) {
		t.Fatal("unrelated top-level keys must survive")
	}
	company := servers["company"].(map[string]any)
	if company["command"] != "/usr/local/bin/ry" {
		t.Fatalf("company server must be replaced: %v", company)
	}
	args := company["args"].([]any)
	if len(args) != 2 || args[0] != "company" || args[1] != "mcp" {
		t.Fatalf("args: %v", args)
	}
	if env := company["env"].(map[string]any); env["RYSH_COMPANY_CONFIG"] != "/w/rysh.config.yaml" {
		t.Fatalf("env: %v", env)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o, want 0600", info.Mode().Perm())
	}

	// A file that is not JSON is left alone rather than overwritten.
	_ = os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte("{not json"), 0o600)
	if _, err := writeClaudeMCPConfig(dir, "/usr/local/bin/ry", "/w/rysh.config.yaml"); err == nil {
		t.Fatal("a corrupt .mcp.json must not be clobbered")
	}
	raw2, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if string(raw2) != "{not json" {
		t.Fatal("corrupt file was modified")
	}
}
