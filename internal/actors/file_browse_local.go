// SPDX-License-Identifier: Apache-2.0

package actors

// Local (embedded web UI) entry point into the file-browse implementation.
//
// file_browse.go serves REMOTE viewers through the share relay's NATS .fs
// subject. The embedded web UI (##rysh web — desktop and mobile browsers on
// this machine's daemon) has no share in the picture, so it reaches the same
// sandbox/classification/caps through WebFSBrowse instead, which the
// WorkspaceActor injects into web.Server via SetFSBrowser (the web package
// cannot import actors — actors imports web).
//
// The browse root is the target pane's live working directory (same
// resolution as a share without a pinned root), absolute paths are never
// allowed, and list/read behavior is byte-identical to the share responder:
// both call the fsListCore/fsReadCore extracted from the original methods.

import (
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// WebFSBrowse handles one file-browse request from the embedded web UI.
// op is "list" or "read"; paneID selects the pane whose cwd is the sandbox
// root. Returns the reply value, or an error code + message ("" code = ok).
func WebFSBrowse(pub *msg.NATSPublisher, op, paneID, path string, offset, length int64) (any, string, string) {
	pane, ok := findWorkspacePane(pub, paneID)
	if !ok {
		return nil, "pane_not_found", "pane not found in workspace"
	}
	root := resolvePaneRoot(pane.ShellPID, pane.StartupDir)
	if root == "" {
		return nil, "io_error", "could not resolve browse root"
	}
	abs, rel, errCode, errMsg := resolveSandboxedPath(root, path, false)
	if errCode != "" {
		return nil, errCode, errMsg
	}
	switch op {
	case "list":
		reply, code, m := fsListCore(root, abs, rel, "")
		if code != "" {
			return nil, code, m
		}
		return reply, "", ""
	case "read":
		reply, code, m := fsReadCore(abs, rel, "", offset, length)
		if code != "" {
			return nil, code, m
		}
		return reply, "", ""
	default:
		return nil, "io_error", "unknown op: " + op
	}
}

// findWorkspacePane looks a pane up by id anywhere in the workspace snapshot
// (any tab/lane/group). Unlike the share responder's resolveTargetPane it is
// not entity-scoped: the embedded UI may browse from every pane it can show.
func findWorkspacePane(pub *msg.NATSPublisher, paneID string) (domain.PaneSnapshot, bool) {
	if paneID == "" {
		return domain.PaneSnapshot{}, false
	}
	reply, err := pub.Request(msg.T("ws", "snapshot"), &msg.MsgGetWorkspaceSnapshot{}, 2*time.Second)
	if err != nil {
		return domain.PaneSnapshot{}, false
	}
	snapReply, ok := reply.(*msg.MsgWorkspaceSnapshotReply)
	if !ok {
		return domain.PaneSnapshot{}, false
	}
	if p := domain.FindPaneInWorkspace(&snapReply.Snapshot, paneID); p != nil {
		return *p, true
	}
	return domain.PaneSnapshot{}, false
}

// webFSBrowser returns the file-browse closure injected into the embedded
// web server (web.Server.SetFSBrowser). A method on WorkspaceActor so both
// web-server start paths (auto-start and ##rysh web start) wire it the same.
func (w *WorkspaceActor) webFSBrowser() func(op, paneID, path string, offset, length int64) (any, string, string) {
	pub := w.pub
	return func(op, paneID, path string, offset, length int64) (any, string, string) {
		return WebFSBrowse(pub, op, paneID, path, offset, length)
	}
}
