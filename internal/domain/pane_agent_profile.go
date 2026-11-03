// SPDX-License-Identifier: Apache-2.0

package domain

import "strings"

// Pane agent profile meta (design 093 §12.3 H1). `##pane profile set` writes
// these on the pane; the pane re-sends the profile to its LLM actor whenever
// they change and every time that actor is created, so a profile survives a
// daemon restart (pane meta rides its own durable KV record).
//
// Both keys are RESERVED: only `##pane profile` may write them (`##pane meta
// set` refuses), so a profile's prompt file always lives under the workspace's
// .rysh/pane-profiles/ directory.
const (
	// PaneMetaAgentTools is the tool allow-list, comma-separated.
	PaneMetaAgentTools = "agent.tools"
	// PaneMetaAgentPromptFile is the absolute path of the profile's system
	// prompt file (kept out of meta itself: a persona prompt can be long, and
	// meta rides every pane snapshot).
	PaneMetaAgentPromptFile = "agent.prompt_file"
	// PaneProfileDirName is the directory under <workspace>/.rysh that holds
	// profile prompt files.
	PaneProfileDirName = "pane-profiles"
)

// IsPaneAgentProfileKey reports whether key is one of the profile meta keys.
func IsPaneAgentProfileKey(key string) bool {
	return key == PaneMetaAgentTools || key == PaneMetaAgentPromptFile
}

// ParseAgentTools splits a comma-separated allow-list, trimming blanks and
// dropping empties and duplicates (order kept).
func ParseAgentTools(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

// AgentToolsAllow reports whether the allow-list in pane meta names tool.
func AgentToolsAllow(meta map[string]string, tool string) bool {
	for _, t := range ParseAgentTools(meta[PaneMetaAgentTools]) {
		if t == tool {
			return true
		}
	}
	return false
}
