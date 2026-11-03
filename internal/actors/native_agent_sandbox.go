// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/agentenv"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/sandbox"
)

// native_agent_sandbox.go — design 066 S3. `##claude --sandbox <profile>` runs
// the pane's agent inside a sandbox (internal/sandbox) and keeps it there
// across every relaunch rysh makes.
//
// STICKY BY DESIGN. Once a pane is stamped agent.sandbox, a later `##claude`
// in it without --sandbox keeps the sandbox, and the restore sweep relaunches
// it sandboxed. Dropping it is a deliberate act (`##pane meta delete
// agent.sandbox`), never the side effect of a retyped line.
//
// FAIL CLOSED. Every line built here either runs the agent through
// `exec-clean --sandbox` or prints why it did not start. None of them has the
// static `env -u … claude` branch the unsandboxed line keeps for hosts without
// a rysh binary.

const (
	nativeAgentSandboxFlag = "--sandbox"
	// metaAgentSandbox is the profile name the pane's agent runs under.
	metaAgentSandbox = "agent.sandbox"
	// metaAgentSandboxDir is the rysh directory the profile and the sandbox
	// home were resolved in. Stamped with the profile so a resume needs no
	// daemon state to rebuild the exact same sandbox.
	metaAgentSandboxDir = "agent.sandbox_rysh_dir"
)

// paneMetaReserved is why `##pane meta` may not set or delete key, or "".
// The agent.sandbox* stamp is written only by `##claude --sandbox`: it decides
// whether host hooks run for the pane (lead ruling R3), so deleting it by hand
// would put them back on for a sandboxed agent.
func paneMetaReserved(key string) string {
	if strings.HasPrefix(key, metaAgentSandbox) {
		return fmt.Sprintf("meta key %q is reserved: it is set only by ##claude --sandbox", key)
	}
	if domain.IsPaneAgentProfileKey(key) {
		return fmt.Sprintf("meta key %q is reserved: it is set only by ##pane profile", key)
	}
	return ""
}

// nativeAgentStripSandbox pulls `--sandbox <profile>` / `--sandbox=<profile>`
// out of args, from any position, so it never reaches the CLI.
func nativeAgentStripSandbox(args []string) (rest []string, profile string, err error) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := strings.TrimSpace(args[i])
		if a != nativeAgentSandboxFlag && !strings.HasPrefix(a, nativeAgentSandboxFlag+"=") {
			rest = append(rest, args[i])
			continue
		}
		val := ""
		if v, ok := strings.CutPrefix(a, nativeAgentSandboxFlag+"="); ok {
			val = v
		} else if i+1 < len(args) {
			i++
			val = strings.TrimSpace(args[i])
		}
		if val == "" || strings.HasPrefix(val, "-") {
			return nil, "", fmt.Errorf("%s needs a profile name", nativeAgentSandboxFlag)
		}
		if profile != "" {
			return nil, "", fmt.Errorf("%s given twice", nativeAgentSandboxFlag)
		}
		if !sandbox.ValidName(val) {
			return nil, "", fmt.Errorf("%s %q: not a valid profile name", nativeAgentSandboxFlag, val)
		}
		profile = val
	}
	return rest, profile, nil
}

// nativeAgentSandboxMeta is the stamp for a sandboxed launch. An empty profile
// stamps empty values, which DELETES both keys.
func nativeAgentSandboxMeta(profile, ryshDir string) [][2]string {
	if profile == "" {
		ryshDir = ""
	}
	return [][2]string{{metaAgentSandbox, profile}, {metaAgentSandboxDir, ryshDir}}
}

// nativeAgentSandboxed wraps cmd for a sandboxed spec, or returns a line that
// starts nothing and says why.
func nativeAgentSandboxed(s nativeAgentSpec, cmd string) string {
	refuse := func(why string) string {
		return "printf '%s\\n' " + shellQuoteAnyShell("rysh: sandbox "+s.Sandbox+": "+why+" (the agent was not started)") + " >&2"
	}
	kind := agentenv.Claude
	if nativeAgentKind(s.Kind) == nativeAgentCodex {
		kind = agentenv.Codex
	}
	ws := strings.TrimSpace(s.Workspace)
	dir := strings.TrimSpace(s.SandboxRyshDir)
	if ws == "" || !filepath.IsAbs(ws) {
		return refuse("the pane's directory is unknown")
	}
	if dir == "" || !filepath.IsAbs(dir) {
		return refuse("the pane's rysh directory is unknown; re-run ##claude --sandbox " + s.Sandbox)
	}
	return agentenv.SandboxedTypedLine(kind, s.Sandbox, dir, ws, cmd)
}

// claudeSandboxSessionExists is claudeSessionExists for a sandboxed pane: its
// conversations live in the sandbox home, not in the host's ~/.claude.
func claudeSandboxSessionExists(ryshDir, profile, sid string) bool {
	if ryshDir == "" || !sandbox.ValidName(profile) {
		return false
	}
	return claudeSessionExistsIn(filepath.Join(sandbox.HomeDir(ryshDir, profile), ".claude", "projects"), sid)
}

// checkNativeAgentSandbox refuses, before anything changes, a sandboxed launch
// that cannot be set up: a profile that does not resolve, an unknown pane
// directory, or a combination v1 does not support.
func (w *WorkspaceActor) checkNativeAgentSandbox(kind, profile, cwd string, freshSession, prompt bool) error {
	if freshSession {
		return fmt.Errorf("%s cannot be combined with --fresh-session / %s yet", nativeAgentSandboxFlag, nativeAgentResumeDirFlag)
	}
	_ = prompt // a one-shot prompt is read by the pane shell before exec, so it works sandboxed
	if strings.TrimSpace(cwd) == "" {
		return fmt.Errorf("sandbox %s: this pane's directory is unknown", profile)
	}
	if _, err := sandbox.Resolve(w.cfg.RyshDir, profile); err != nil {
		return err
	}
	return nil
}

// nativeAgentNoSandboxFlag opts ONE launch out of the workspace's default
// sandbox (gap01 E4). Refused where sandbox.yaml says required: true.
const nativeAgentNoSandboxFlag = "--no-sandbox"

// nativeAgentStripNoSandbox pulls --no-sandbox out of args, from any position.
func nativeAgentStripNoSandbox(args []string) (rest []string, noSandbox bool) {
	rest = make([]string, 0, len(args))
	for _, a := range args {
		if strings.TrimSpace(a) == nativeAgentNoSandboxFlag {
			noSandbox = true
			continue
		}
		rest = append(rest, a)
	}
	return rest, noSandbox
}

// resolveNativeAgentSandbox decides the profile a launch runs under, or ""
// for none. Precedence: --no-sandbox, --sandbox, the pane's sticky stamp, the
// workspace default. Required refuses every path that would end at "". A
// sandbox.yaml that cannot be read refuses every launch: whether a sandbox is
// required is then unknown, and unknown must not mean "no".
func resolveNativeAgentSandbox(flag string, noSandbox bool, sticky string, set sandbox.Settings, setErr error) (string, error) {
	if setErr != nil {
		return "", fmt.Errorf("cannot read this workspace's sandbox settings, so no agent starts: %v", setErr)
	}
	if flag != "" && noSandbox {
		return "", fmt.Errorf("%s and %s contradict each other", nativeAgentSandboxFlag, nativeAgentNoSandboxFlag)
	}
	required := fmt.Errorf("this workspace requires a sandbox (sandbox.yaml: required: true) — start the agent with %s <profile>", nativeAgentSandboxFlag)
	switch {
	case noSandbox:
		if set.Required {
			return "", required
		}
		return "", nil
	case flag != "":
		return flag, nil
	case sticky != "":
		return sticky, nil
	case set.Default != "":
		return set.Default, nil
	case set.Required:
		return "", required
	}
	return "", nil
}

// nativeSandboxPolicy is what the restore sweep needs to honour a workspace's
// sandbox posture for panes that were started unsandboxed.
type nativeSandboxPolicy struct {
	RyshDir  string
	Settings sandbox.Settings
	Err      error
}

// nativeAgentNotStarted is a pane line that starts nothing and says why.
func nativeAgentNotStarted(why string) string {
	return "printf '%s\\n' " + shellQuoteAnyShell("rysh: "+why+" (the agent was not started)") + " >&2"
}
