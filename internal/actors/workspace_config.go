// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/asynkron/protoactor-go/actor"

	"github.com/rysh-ai/rysh-cli-code/internal/config"
)

// ##config (design 037, second cut).
//
// `##config load <file>` binds THIS session to the AI-native company described
// by a rysh.config.yaml the owner downloaded from the company page. It writes
// the upstream connection (URL, read-only key, workspace id) and the company
// block into the session's own config, applies both to the running daemon,
// (re)starts the company follower, opens the company view in the pane the
// command was typed in, and registers `rysh company mcp` as an MCP server so
// the agents in this session get company_* tools. Nothing needs a restart.
//
// The session's NAME is deliberately not changed: a running session keeps the
// name its daemon was started with. The file's `rysh.session_name` applies
// when rysh is started fresh in a directory that holds the file.

// companyMCPServerName is the MCP server registered for the bound company; a
// second load replaces it rather than adding a twin.
const companyMCPServerName = "company"

func (w *WorkspaceActor) handleConfigCommand(out *strings.Builder, paneID string, args []string) {
	o := ryshWriter(out)
	sub := ""
	if len(args) > 0 {
		sub = strings.ToLower(strings.TrimSpace(args[0]))
	}
	switch sub {
	case "load":
		arg := ""
		if len(args) > 1 {
			arg = strings.Join(args[1:], " ")
		}
		w.handleConfigLoad(o, paneID, arg)
	case "", "show", "status":
		w.printConfigBinding(o)
	default:
		o.UsageLineIn("config", "##config load <file> | ##config show")
		w.failRyshUsage("unknown ##config subcommand %q", sub)
	}
}

func (w *WorkspaceActor) handleConfigLoad(o ryshOut, paneID, arg string) {
	// No argument: the file the customer just downloaded. Nobody types a path.
	arg = strings.Trim(strings.TrimSpace(arg), "\"'")
	path := ""
	if arg == "" {
		found, ferr := findDownloadedCompanyConfig(configLoadSearchDirs(w.absBaseDir()))
		if ferr != nil {
			o.Header("config load: nothing to load")
			o.Row("  %v", ferr)
			o.Row("  download the file from the company page (\"Download config\"), then run ##config load again,")
			o.Row("  or give its path:  ##config load ~/Downloads/<company>.rysh.config.yaml")
			w.failRysh("config load: %v", ferr)
			return
		}
		path = found
	} else {
		path = configLoadExpandPath(arg)
	}
	binding, err := config.ReadCompanyBinding(path)
	if err != nil {
		o.Header("config load failed")
		o.Row("  %v", err)
		o.Row("  nothing changed")
		w.failRysh("config load: %v", err)
		return
	}

	// The session's own config file: write the binding there so the next
	// start of this session comes up bound without the command. Loading the
	// session's own file (rysh started in ~/Downloads, say) skips the write.
	cfgFile := config.ConfigPath(strings.TrimSpace(w.cfg.ConfigFile))
	if abs, aerr := filepath.Abs(cfgFile); aerr == nil {
		cfgFile = abs
	}
	sameFile := false
	if abs, aerr := filepath.Abs(path); aerr == nil && abs == cfgFile {
		sameFile = true
	}
	if !sameFile {
		if _, serr := os.Stat(cfgFile); errors.Is(serr, fs.ErrNotExist) {
			header := "# rysh.config.yaml — written by ##config load on " + time.Now().UTC().Format(time.RFC3339) + "\n"
			if werr := os.WriteFile(cfgFile, []byte(header), 0o600); werr != nil {
				o.Header("config load failed")
				o.Row("  could not create %s: %v", cfgFile, werr)
				w.failRysh("config load: %v", werr)
				return
			}
		}
		if werr := config.SetUpstreamConnection(cfgFile, binding.URL, binding.APIKey, binding.WorkspaceID); werr != nil {
			o.Header("config load failed")
			o.Row("  could not write %s: %v", cfgFile, werr)
			w.failRysh("config load: %v", werr)
			return
		}
		if werr := config.SetCompanyBlock(cfgFile, binding.Company); werr != nil {
			o.Header("config load failed")
			o.Row("  could not write %s: %v", cfgFile, werr)
			w.failRysh("config load: %v", werr)
			return
		}
		_ = os.Chmod(cfgFile, 0o600)
	}

	// Apply to the running session: ##upstream status and ##company answer
	// from w.cfg, and the follower is built from it.
	w.cfg.Upstream.Enabled = true
	w.cfg.Upstream.URL = binding.URL
	w.cfg.Upstream.APIKey = binding.APIKey
	w.cfg.Upstream.Workspace = binding.WorkspaceID
	w.cfg.Company = binding.Company
	w.cfg.ConfigFile = cfgFile

	followerErr := w.restartCompanyFollower()
	w.openCompanyPane(paneID)
	tools := w.registerCompanyTools(paneID, cfgFile)
	claudeNote := "not written"
	if exe, eerr := os.Executable(); eerr == nil {
		if p, werr := writeClaudeMCPConfig(w.absBaseDir(), exe, cfgFile); werr == nil {
			claudeNote = p
		} else {
			claudeNote = "not written: " + werr.Error()
		}
	}

	o.Header("bound to " + binding.Company.Name)
	o.Rule()
	o.Field("loaded", "%s", path)
	o.Field("company", "%s (%s)", binding.Company.Name, binding.Company.FQDN)
	o.Field("server", "%s", binding.URL)
	o.Field("workspace_id", "%s", binding.WorkspaceID)
	o.Field("access", "read-only key; revoke it from the company page")
	if followerErr == nil {
		o.Field("follower", "started — fetching now")
	} else {
		o.Field("follower", "not started: %v", followerErr)
	}
	o.Field("agents", "%s", tools)
	o.Field("claude", "%s (##claude in a pane gets the same tools)", claudeNote)
	o.Rule()
	if !sameFile {
		o.Row("  written to %s", cfgFile)
	}
	o.Row("  this pane opens the company view; ##company lists orders, enquiries and activity")
	o.Row("  from a shell: rysh company status|orders|enquiries|activity|services [--json]")
	o.Row("")
}

// restartCompanyFollower stops the follower a previous binding (or the daemon
// start) spawned and starts one for the current w.cfg. The name is fixed, so
// the stop must complete before the spawn; a missing actor answers the stop
// immediately from the dead-letter process.
func (w *WorkspaceActor) restartCompanyFollower() error {
	if w.actorSystem == nil || w.nc == nil || w.pub == nil {
		return errors.New("actor system unavailable")
	}
	root := w.actorSystem.Root
	_ = root.StopFuture(actor.NewPID(w.actorSystem.Address(), "company")).Wait()
	props := actor.PropsFromProducer(func() actor.Actor {
		return NewCompanyActor(w.nc, w.pub, w.cfg.Company, w.cfg.Upstream)
	})
	_, err := root.SpawnNamed(props, "company")
	return err
}

// registerCompanyTools hands `rysh company mcp` to the MCP manager as a stdio
// server named "company", the same path `##mcp add` takes, so its tools reach
// the agents in this session through the existing bridge, approval gates and
// audit. The server reads the binding from the session's config file, which
// the load just wrote. The connect itself runs off-mailbox; its outcome lands
// on the pane as an [mcp] line.
func (w *WorkspaceActor) registerCompanyTools(paneID, cfgFile string) string {
	if w.agSetup == nil || w.agSetup.MCP == nil {
		return "no tools — agentic mode is off in this session"
	}
	exe, err := os.Executable()
	if err != nil {
		return "no tools — " + err.Error()
	}
	_ = w.agSetup.MCP.RemoveServer(companyMCPServerName)
	var sink strings.Builder
	w.handleMCPSubcommand(&sink, paneID, []string{
		"add", companyMCPServerName, "stdio", exe, "company", "mcp",
		"--env", "RYSH_COMPANY_CONFIG=" + cfgFile,
		"--prefix", "",
	})
	return "company_status, company_orders, company_enquiries, company_activity, company_services (MCP server \"company\", connecting…)"
}

func (w *WorkspaceActor) printConfigBinding(o ryshOut) {
	o.Header("config")
	o.Rule()
	o.Field("file", "%s", config.ConfigPath(strings.TrimSpace(w.cfg.ConfigFile)))
	if w.cfg.Company.Enabled() {
		o.Field("company", "%s (%s)", w.cfg.Company.Name, w.cfg.Company.FQDN)
		server := w.cfg.Company.Server
		if server == "" {
			server = w.cfg.Upstream.URL
		}
		o.Field("server", "%s", server)
		o.Field("workspace_id", "%s", w.cfg.Upstream.Workspace)
	} else {
		o.Field("company", "none — ##config load <file> binds one")
	}
	if w.cfg.Upstream.Enabled {
		o.Field("upstream", "%s", w.cfg.Upstream.URL)
	} else {
		o.Field("upstream", "off")
	}
	o.Rule()
	o.Row("")
}

// absBaseDir is the session directory as an absolute path — where the
// session's config, .mcp.json and the panes' shells live.
func (w *WorkspaceActor) absBaseDir() string {
	if abs, err := filepath.Abs(w.baseDir()); err == nil {
		return abs
	}
	return w.baseDir()
}

// configLoadExpandPath turns ~/x into an absolute path under the home
// directory; anything else is returned as typed.
func configLoadExpandPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
