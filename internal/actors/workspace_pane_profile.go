// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// `##pane profile` — a pane's agent profile (design 093 §12.3 H1): its own
// system prompt and a strict tool allow-list, for native rysh agent panes such
// as hosted family avatars.
//
//	##pane profile set <pane> [--tools a,b,c] [--prompt-file <path>]
//	##pane profile show <pane>
//	##pane profile clear <pane>
//
// `set` REPLACES the profile: a part it does not name is cleared. The prompt
// file is copied to <workspace>/.rysh/pane-profiles/<pane-id>.md (0600) and
// the copy's path is kept in pane meta, so the profile no longer depends on
// the file it was set from and survives a daemon restart with the pane's meta.
// The pane forwards every change to its LLM actor (pane_agent_profile.go).

const paneProfileUsage = "##pane profile set <pane> [--tools a,b,c] [--prompt-file <path>] | show <pane> | clear <pane>"

// toolNamePattern bounds an allow-list entry.
var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func (w *WorkspaceActor) handlePaneProfileCommand(out *strings.Builder, args []string) {
	if len(args) < 2 {
		ryshWriter(out).UsageLine(paneProfileUsage)
		w.failRyshUsage("usage: %s", paneProfileUsage)
		return
	}
	sub := strings.ToLower(args[0])
	ref := strings.TrimSpace(args[1])
	target := w.resolvePaneID(ref)
	if target == "" {
		fmt.Fprintf(out, "\n[rysh] pane not found: %s\n", ref)
		w.failRysh("pane not found: %s", ref)
		return
	}
	rest := args[2:]

	switch sub {
	case "set":
		rest, toolsArg := extractStringArg(rest, "--tools")
		rest, promptArg := extractStringArg(rest, "--prompt-file")
		if len(rest) > 0 {
			fmt.Fprintf(out, "\n[rysh] unexpected argument %q\n", rest[0])
			ryshWriter(out).UsageLine(paneProfileUsage)
			w.failRyshUsage("unexpected argument %q", rest[0])
			return
		}
		allow := domain.ParseAgentTools(toolsArg)
		if len(allow) == 0 && strings.TrimSpace(promptArg) == "" {
			fmt.Fprintf(out, "\n[rysh] ##pane profile set needs --tools and/or --prompt-file (use clear to remove a profile)\n")
			w.failRyshUsage("##pane profile set needs --tools and/or --prompt-file")
			return
		}
		for _, n := range allow {
			if !toolNamePattern.MatchString(n) {
				fmt.Fprintf(out, "\n[rysh] invalid tool name %q\n", n)
				w.failRysh("invalid tool name %q", n)
				return
			}
		}
		promptPath := ""
		if strings.TrimSpace(promptArg) != "" {
			p, n, err := w.storePaneProfilePrompt(target, promptArg)
			if err != nil {
				fmt.Fprintf(out, "\n[rysh] %v\n", err)
				w.failRysh("%v", err)
				return
			}
			promptPath = p
			defer fmt.Fprintf(out, "  prompt : %d bytes (%s)\n", n, filepath.Join(".rysh", domain.PaneProfileDirName, filepath.Base(p)))
		} else {
			w.removePaneProfilePrompt(target)
		}
		// Prompt first, tools second: the allow-list never transiently falls
		// back to the full tool set while a narrower one is being installed.
		w.sendPaneMeta(target, domain.PaneMetaAgentPromptFile, promptPath)
		w.sendPaneMeta(target, domain.PaneMetaAgentTools, strings.Join(allow, ","))
		fmt.Fprintf(out, "\n[rysh] pane %.8s agent profile set (applies from its next turn)\n", target)
		if len(allow) > 0 {
			fmt.Fprintf(out, "  tools  : %s\n", strings.Join(allow, ", "))
		} else {
			fmt.Fprintf(out, "  tools  : (session defaults)\n")
		}

	case "show", "get", "info":
		meta := w.paneMeta(target)
		allow := domain.ParseAgentTools(meta[domain.PaneMetaAgentTools])
		pf := strings.TrimSpace(meta[domain.PaneMetaAgentPromptFile])
		fmt.Fprintf(out, "\n[rysh] agent profile for pane %.8s\n", target)
		if len(allow) == 0 && pf == "" {
			fmt.Fprintf(out, "  (none — the pane uses the session defaults)\n")
			return
		}
		if len(allow) > 0 {
			fmt.Fprintf(out, "  tools  : %s\n", strings.Join(allow, ", "))
		} else {
			fmt.Fprintf(out, "  tools  : (session defaults)\n")
		}
		if pf == "" {
			fmt.Fprintf(out, "  prompt : (session default)\n")
			return
		}
		dir, _ := w.paneProfileDir()
		prompt, err := readProfilePrompt(pf, paneProfilePromptPath(dir, target))
		if err != nil {
			fmt.Fprintf(out, "  prompt : unreadable: %v\n", err)
			return
		}
		first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(prompt), "\n", 2)[0])
		if len(first) > 100 {
			first = first[:100] + "…"
		}
		fmt.Fprintf(out, "  prompt : %d bytes (%s)\n  first  : %s\n",
			len(prompt), filepath.Join(".rysh", domain.PaneProfileDirName, filepath.Base(pf)), first)

	case "clear", "rm", "delete":
		w.sendPaneMeta(target, domain.PaneMetaAgentTools, "")
		w.sendPaneMeta(target, domain.PaneMetaAgentPromptFile, "")
		w.removePaneProfilePrompt(target)
		fmt.Fprintf(out, "\n[rysh] pane %.8s agent profile cleared (session defaults from its next turn)\n", target)

	default:
		ryshWriter(out).UsageLine(paneProfileUsage)
		w.failRyshUsage("unknown ##pane profile action %q", sub)
	}
}

func (w *WorkspaceActor) sendPaneMeta(paneID, key, value string) {
	_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneSetMeta{Key: key, Value: value})
}

// paneProfileDir is the pane-profiles directory, resolved exactly as the
// pane resolves it when reading (paneProfileDir in pane_agent_profile.go).
func (w *WorkspaceActor) paneProfileDir() (string, error) {
	return paneProfileDir(w.agSetup, w.baseDir()), nil
}

// storePaneProfilePrompt copies the prompt file (relative paths resolve from
// the workspace root) into the pane-profiles directory and returns the copy's
// absolute path and size.
func (w *WorkspaceActor) storePaneProfilePrompt(paneID, src string) (string, int, error) {
	src = strings.TrimSpace(src)
	if !filepath.IsAbs(src) {
		src = filepath.Join(w.baseDir(), src)
	}
	fi, err := os.Stat(src)
	if err != nil {
		return "", 0, fmt.Errorf("--prompt-file %s: %v", filepath.Base(src), err)
	}
	if !fi.Mode().IsRegular() {
		return "", 0, fmt.Errorf("--prompt-file %s: not a regular file", filepath.Base(src))
	}
	if fi.Size() > maxProfilePromptBytes {
		return "", 0, fmt.Errorf("--prompt-file %s: larger than %d bytes", filepath.Base(src), maxProfilePromptBytes)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return "", 0, fmt.Errorf("--prompt-file %s: %v", filepath.Base(src), err)
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", 0, fmt.Errorf("--prompt-file %s: the file is empty", filepath.Base(src))
	}
	dst, err := w.writePaneProfilePrompt(paneID, b)
	if err != nil {
		return "", 0, err
	}
	return dst, len(b), nil
}

// writePaneProfilePrompt writes prompt text to the pane's one profile file
// (<dir>/<pane-id>.md, 0600, via a temp file and a rename) and returns its
// absolute path. Shared by `##pane profile set` (which copies a file) and
// MsgCLIPaneProfile (which carries the text).
func (w *WorkspaceActor) writePaneProfilePrompt(paneID string, b []byte) (string, error) {
	dir, err := w.paneProfileDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %v", filepath.Join(".rysh", domain.PaneProfileDirName), err)
	}
	dst := paneProfilePromptPath(dir, paneID)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return "", fmt.Errorf("write profile prompt: %v", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("write profile prompt: %v", err)
	}
	return dst, nil
}

// handleCLIPaneProfile serves MsgCLIPaneProfile (`rysh family apply`, design
// 093 H2): the same writes as `##pane profile set|clear` and the same read
// as the pane's own, with the prompt as text. The pane must be named by its
// full id: apply addresses panes by id only, so an alias or a given-name that
// happens to match is refused rather than resolved to some other pane.
func (w *WorkspaceActor) handleCLIPaneProfile(m *msg.MsgCLIPaneProfile) *msg.MsgCLIPaneProfileReply {
	pane := strings.TrimSpace(m.PaneID)
	if pane == "" || w.findPaneTab(pane) == nil {
		return &msg.MsgCLIPaneProfileReply{Error: fmt.Sprintf("pane not found: %s", m.PaneID)}
	}
	switch m.Op {
	case "get":
		meta := w.paneMeta(pane)
		r := &msg.MsgCLIPaneProfileReply{OK: true, Tools: domain.ParseAgentTools(meta[domain.PaneMetaAgentTools])}
		pf := strings.TrimSpace(meta[domain.PaneMetaAgentPromptFile])
		if pf == "" {
			return r
		}
		r.HasPrompt = true
		dir, _ := w.paneProfileDir()
		prompt, err := readProfilePrompt(pf, paneProfilePromptPath(dir, pane))
		if err != nil {
			r.PromptErr = err.Error()
			return r
		}
		r.Prompt = prompt
		return r

	case "set":
		allow := domain.ParseAgentTools(strings.Join(m.Tools, ","))
		if len(allow) == 0 {
			return &msg.MsgCLIPaneProfileReply{Error: "set needs a tool allow-list"}
		}
		for _, n := range allow {
			if !toolNamePattern.MatchString(n) {
				return &msg.MsgCLIPaneProfileReply{Error: fmt.Sprintf("invalid tool name %q", n)}
			}
		}
		if strings.TrimSpace(m.Prompt) == "" {
			return &msg.MsgCLIPaneProfileReply{Error: "set needs a prompt"}
		}
		if len(m.Prompt) > maxProfilePromptBytes {
			return &msg.MsgCLIPaneProfileReply{Error: fmt.Sprintf("prompt larger than %d bytes", maxProfilePromptBytes)}
		}
		path, err := w.writePaneProfilePrompt(pane, []byte(m.Prompt))
		if err != nil {
			return &msg.MsgCLIPaneProfileReply{Error: err.Error()}
		}
		// Prompt first, tools second, as `##pane profile set` does.
		w.sendPaneMeta(pane, domain.PaneMetaAgentPromptFile, path)
		w.sendPaneMeta(pane, domain.PaneMetaAgentTools, strings.Join(allow, ","))
		return &msg.MsgCLIPaneProfileReply{OK: true, Tools: allow, HasPrompt: true}

	case "clear":
		w.sendPaneMeta(pane, domain.PaneMetaAgentTools, "")
		w.sendPaneMeta(pane, domain.PaneMetaAgentPromptFile, "")
		w.removePaneProfilePrompt(pane)
		return &msg.MsgCLIPaneProfileReply{OK: true}
	}
	return &msg.MsgCLIPaneProfileReply{Error: fmt.Sprintf("unknown op %q (want get, set or clear)", m.Op)}
}

func (w *WorkspaceActor) removePaneProfilePrompt(paneID string) {
	dir, err := w.paneProfileDir()
	if err != nil {
		return
	}
	_ = os.Remove(paneProfilePromptPath(dir, paneID))
}

// extractStringArg removes `flag value` or `flag=value` from args and returns
// the remaining args and the value ("" when absent).
func extractStringArg(args []string, flag string) (rest []string, value string) {
	rest = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if v, ok := strings.CutPrefix(a, flag+"="); ok {
			value = v
			continue
		}
		if a == flag {
			if i+1 < len(args) {
				value = args[i+1]
				i++
			}
			continue
		}
		rest = append(rest, a)
	}
	return rest, value
}
