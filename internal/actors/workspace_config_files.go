// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The two conveniences that make `##config load` a no-argument command for a
// customer: finding the file they just downloaded, and letting a Claude Code
// pane use the company tools without any key or MCP set-up of their own.

// configLoadSearchDirs is where a bare `##config load` looks, most likely
// first: the session directory itself, then the places browsers save to.
func configLoadSearchDirs(sessionDir string) []string {
	dirs := []string{}
	if sessionDir != "" {
		dirs = append(dirs, sessionDir)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		dirs = append(dirs, filepath.Join(home, "Downloads"), filepath.Join(home, "Desktop"), home)
	}
	return dirs
}

// isCompanyConfigName accepts what a browser may have done to the name the
// server sent: a second download becomes "pera.rysh.config (1).yaml", Safari
// sometimes appends ".txt", and case is not reliable on every filesystem.
func isCompanyConfigName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.TrimSuffix(n, ".txt")
	if !strings.HasSuffix(n, ".yaml") && !strings.HasSuffix(n, ".yml") {
		return false
	}
	return strings.Contains(n, ".rysh.config")
}

// findDownloadedCompanyConfig returns the newest company config across dirs.
// The newest wins because a customer who downloaded twice wants the second
// file, whose key is the one still listed on the dashboard. When nothing is
// found the error says, per directory, whether it was searched or could not
// be read — on macOS a terminal that was never allowed into Downloads gets a
// permission error there, and the message names the fix.
func findDownloadedCompanyConfig(dirs []string) (string, error) {
	type hit struct {
		path string
		mod  time.Time
	}
	var hits []hit
	var report []string
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				report = append(report, d+": does not exist")
			} else if errors.Is(err, fs.ErrPermission) {
				report = append(report, d+": permission denied (macOS: System Settings → Privacy & Security → Files and Folders → allow your terminal)")
			} else {
				report = append(report, d+": "+err.Error())
			}
			continue
		}
		n := 0
		for _, e := range entries {
			if e.IsDir() || !isCompanyConfigName(e.Name()) {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			n++
			hits = append(hits, hit{filepath.Join(d, e.Name()), info.ModTime()})
		}
		report = append(report, fmt.Sprintf("%s: %d file(s), %d company config(s)", d, len(entries), n))
	}
	if len(hits) == 0 {
		return "", errors.New("no *.rysh.config.yaml found — looked in " + strings.Join(report, "; "))
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].mod.After(hits[j].mod) })
	return hits[0].path, nil
}

// writeClaudeMCPConfig adds (or replaces) the "company" server in the
// session directory's .mcp.json — the project-scoped MCP configuration Claude
// Code reads from its working directory — so `##claude` and `##pane new
// --claude` get the company_* tools with Claude Code's own login and no key
// in rysh. Every other server in the file is kept as it was.
func writeClaudeMCPConfig(sessionDir, exe, cfgFile string) (string, error) {
	path := filepath.Join(sessionDir, ".mcp.json")
	doc := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return path, errors.New(".mcp.json is not valid JSON; not touching it")
		}
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	servers[companyMCPServerName] = map[string]any{
		"command": exe,
		"args":    []string{"company", "mcp"},
		"env":     map[string]string{"RYSH_COMPANY_CONFIG": cfgFile},
	}
	doc["mcpServers"] = servers
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return path, err
	}
	return path, os.WriteFile(path, append(out, '\n'), 0o600)
}
