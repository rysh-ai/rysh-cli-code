// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"io"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
	"github.com/rysh-ai/rysh-cli-code/internal/upstream"
)

// handleUpstreamConnect implements `##upstream connect <url> <api-key>` — the
// on-ramp from a fresh install to a live shared session.
//
// Before it, connecting meant: find the dashboard, sign up, find the workspace
// page, copy a snippet, hand-edit rysh.config.yaml, restart — and get the
// `workspace` key right, which had to be the workspace UUID rather than the
// name the user actually knows. That last step is a trap with no feedback: a
// name there produces a session that connects and silently sees no shares.
//
// So the command takes the two things a user can be given (a server URL and an
// API key), asks the server which workspace that key belongs to, and writes the
// resolved id itself. Nobody types a UUID.
//
// What it does NOT do is turn on `upstream.governance`. That opts the daemon
// into reporting local spend to the server — a data-egress decision that is
// deliberately separate from connecting, and the output says so rather than
// leaving the user to infer it.
func (w *WorkspaceActor) handleUpstreamConnect(out io.Writer, args []string) {
	o := ryshWriter(out)

	url := ""
	apiKey := ""
	if len(args) > 0 {
		url = strings.TrimSpace(args[0])
	}
	if len(args) > 1 {
		apiKey = strings.TrimSpace(args[1])
	}
	if url == "" || apiKey == "" {
		o.UsageLine("##upstream connect <url> <api-key>")
		o.Row("  the api key is returned when you register, and on the workspace page")
		o.Row("  example: ##upstream connect https://api.rysh.ai rysh_sk_...")
		w.failRyshUsage("usage: %s", "##upstream connect <url> <api-key>")
		return
	}

	// Resolve the workspace from the key BEFORE touching the config. A
	// half-written [upstream] block is worse than no command at all: the next
	// start would come up "enabled" against a server that rejected us.
	probe := config.UpstreamConfig{URL: url, APIKey: apiKey}
	info, err := upstream.FetchWorkspaceInfo(probe)
	if err != nil {
		o.Header("upstream connect failed")
		o.Row("  %v", err)
		o.Row("  nothing was written to rysh.config.yaml")
		w.failRysh("upstream connect: %v", err)
		return
	}

	cfgFile := config.ConfigPath(strings.TrimSpace(w.cfg.ConfigFile))
	if err := config.SetUpstreamConnection(cfgFile, url, apiKey, info.ID); err != nil {
		o.Header("upstream connect failed")
		o.Row("  resolved workspace %q (%s) but could not write %s: %v", info.Name, info.ID, cfgFile, err)
		w.failRysh("upstream connect: write config: %v", err)
		return
	}

	// Apply to the running session too, so ##upstream status stops reporting
	// "not enabled" the moment this returns. The sharing actors themselves are
	// spawned at workspace start from this config, which is why a restart is
	// still required for shares to flow — stated below rather than implied.
	w.cfg.Upstream.Enabled = true
	w.cfg.Upstream.URL = url
	w.cfg.Upstream.APIKey = apiKey
	w.cfg.Upstream.Workspace = info.ID

	o.Header("upstream connected")
	o.Rule()
	o.Field("url", "%s", url)
	o.Field("workspace", "%s", info.Name)
	o.Field("workspace_id", "%s", info.ID)
	o.Field("governance", "off (unchanged)")
	o.Rule()
	o.Row("  written to %s", cfgFile)
	o.Row("  governance stays off: reporting local spend to the server is a separate")
	o.Row("  opt-in — set upstream.governance: true only if you want it")
	o.Row("  restart rysh for sharing to activate, then: ##share pane")
	o.Row("")
}
