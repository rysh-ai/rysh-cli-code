package actors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// File-browse responder.
//
// This is the SOURCE side of the mobile file-browser feature (see
// docs/mobile-file-browser-plan.md, "Component 1 — rysh-cli (source) changes").
// A remote subscriber, via the rysh-server, issues NATS request/reply on the
// per-share subject:
//
//	ws.{workspace}.share.{shareID}.fs
//
// and this responder performs the actual host file I/O and replies with
// nMsg.Respond(jsonBytes). It is deliberately NOT a protoactor: file reads can
// block, so the work runs in the NATS subscription callback goroutine (each
// callback is independent), never on an actor mailbox. UpstreamShareActor owns
// the lifecycle — the responder is subscribed whenever the share is connected.
// File browsing is always enabled (AllowFileBrowse is always true), confined to
// the pane's working-dir subtree unless AllowAbsolute widens it.
//
// How the browse root + pid are resolved: the responder does NOT hold a
// reference to the PaneActor (that would couple two actor lifecycles and race
// pane state). Instead, when a request arrives it fetches a fresh workspace
// snapshot through the thread-safe publisher (the same pattern
// UpstreamShareActor.buildBufferResponse / paneBelongsToEntity already use) and
// reads the target pane's ShellPID + StartupDir from the snapshot. This keeps
// all pane state access on the publisher's request/reply path and means the
// responder works for both single-pane and multi-pane (tab) shares.

const (
	// fsMaxEntries caps directory listings; beyond this the reply sets truncated.
	fsMaxEntries = 2000
	// fsMaxTextChunk is the largest text payload returned in a single read chunk.
	fsMaxTextChunk = 256 * 1024 // 256 KiB
	// fsMaxTextTotal is the hard cap on a text file's total size (too_large beyond).
	fsMaxTextTotal = 5 * 1024 * 1024 // 5 MiB
	// fsMaxImage is the hard cap on an image file's total size (too_large beyond).
	fsMaxImage = 6 * 1024 * 1024 // 6 MiB
	// fsSniffLen is how many leading bytes are sniffed for content classification.
	fsSniffLen = 512
)

// fsRequest is the inbound JSON request on the .fs subject.
type fsRequest struct {
	Op           string `json:"op"` // "list" | "read" | "stat"
	RequestID    string `json:"request_id"`
	ShareID      string `json:"share_id"`
	TargetPaneID string `json:"target_pane_id"`
	Path         string `json:"path"`
	Offset       int64  `json:"offset"`
	Length       int64  `json:"length"`
}

// fsEntry is one directory entry in a "list" reply.
type fsEntry struct {
	Name         string `json:"name"`
	Kind         string `json:"kind"` // "file" | "dir"
	Size         int64  `json:"size"`
	MTimeMs      int64  `json:"mtime_ms"`
	ContentClass string `json:"content_class"` // "text" | "image" | "unsupported" | "dir"
	MIME         string `json:"mime"`
}

// fsListReply is the reply for op="list".
type fsListReply struct {
	OK        bool      `json:"ok"`
	RequestID string    `json:"request_id"`
	Root      string    `json:"root"`
	Path      string    `json:"path"`
	Entries   []fsEntry `json:"entries"`
	Truncated bool      `json:"truncated"`
}

// fsReadReply is the reply for op="read".
type fsReadReply struct {
	OK           bool   `json:"ok"`
	RequestID    string `json:"request_id"`
	Path         string `json:"path"`
	ContentClass string `json:"content_class"` // "text" | "image"
	MIME         string `json:"mime"`
	Encoding     string `json:"encoding"` // "utf8" | "base64"
	Data         string `json:"data"`
	Offset       int64  `json:"offset"`
	Length       int64  `json:"length"`
	TotalSize    int64  `json:"total_size"`
	EOF          bool   `json:"eof"`
	Redacted     bool   `json:"redacted"`
}

// fsStatReply is the reply for op="stat" (single-entry metadata).
type fsStatReply struct {
	OK        bool    `json:"ok"`
	RequestID string  `json:"request_id"`
	Root      string  `json:"root"`
	Path      string  `json:"path"`
	Entry     fsEntry `json:"entry"`
}

// fsErrorReply is the error reply for any op.
type fsErrorReply struct {
	OK        bool   `json:"ok"`
	RequestID string `json:"request_id"`
	Error     string `json:"error"`
	Message   string `json:"message"`
}

// subscribeFileBrowse subscribes the .fs request/reply responder on the upstream
// connection. Idempotent: it no-ops when a subscription already exists. Called
// from connectRemote and reconnect. The responder is ALWAYS subscribed while
// connected — file browsing is always enabled (AllowFileBrowse is always true),
// so there is always a responder and the server never sees a no-responder
// timeout (which it would surface as an opaque 503). The handler runs file I/O
// in the callback goroutine, not the actor mailbox.
func (u *UpstreamShareActor) subscribeFileBrowse() {
	if u.remoteNC == nil || u.fsSub != nil {
		return
	}
	subject := fsSubject(u.workspace, u.shareID)
	sub, err := u.remoteNC.Subscribe(subject, func(nMsg *nats.Msg) {
		if nMsg.Reply == "" {
			return
		}
		u.handleFileBrowseRequest(nMsg)
	})
	if err != nil {
		slog.Error("upstream-share: subscribe file-browse failed",
			"shareID", u.shareID, "err", err)
		return
	}
	u.fsSub = sub
	slog.Info("upstream-share: file-browse responder subscribed",
		"shareID", u.shareID, "subject", subject,
		"allowFileBrowse", u.restrictions.AllowFileBrowse, "allowAbsolute", u.restrictions.AllowAbsolute)
}

// unsubscribeFileBrowse tears down the .fs responder (on disconnect/close).
// Safe to call when not subscribed.
func (u *UpstreamShareActor) unsubscribeFileBrowse() {
	if u.fsSub != nil {
		_ = u.fsSub.Unsubscribe()
		u.fsSub = nil
		slog.Info("upstream-share: file-browse responder disabled", "shareID", u.shareID)
	}
}

// fsSubject builds the per-share file-browse request/reply subject.
func fsSubject(workspace, shareID string) string {
	return "ws." + workspace + ".share." + shareID + ".fs"
}

// handleFileBrowseRequest decodes and dispatches a single .fs request. It runs in
// the NATS callback goroutine. It reads only immutable actor fields (shareID,
// entityType, entityID, paneIDs) plus u.restrictions; the latter is written on
// the actor thread and read here in a callback goroutine — this is the same
// benign-race pattern already used across UpstreamShareActor (connected/remoteNC).
// File I/O is fully local to this goroutine.
func (u *UpstreamShareActor) handleFileBrowseRequest(nMsg *nats.Msg) {
	var req fsRequest
	if err := json.Unmarshal(nMsg.Data, &req); err != nil {
		respondFSError(nMsg, "", "io_error", "invalid request json")
		return
	}

	if !u.restrictions.AllowFileBrowse {
		respondFSError(nMsg, req.RequestID, "disabled", "file browsing is not enabled for this share")
		return
	}

	root, errCode, errMsg := u.resolveBrowseRoot(req.TargetPaneID)
	if errCode != "" {
		respondFSError(nMsg, req.RequestID, errCode, errMsg)
		return
	}

	abs, relPath, errCode, errMsg := resolveSandboxedPath(root, req.Path, u.restrictions.AllowAbsolute)
	if errCode != "" {
		respondFSError(nMsg, req.RequestID, errCode, errMsg)
		return
	}

	switch req.Op {
	case "list":
		u.fsList(nMsg, req, root, abs, relPath)
	case "read":
		u.fsRead(nMsg, req, abs, relPath)
	case "stat":
		u.fsStat(nMsg, req, root, abs, relPath)
	default:
		respondFSError(nMsg, req.RequestID, "io_error", "unknown op: "+req.Op)
	}
}

// resolveBrowseRoot returns the file-browse root for a request.
//
// A SharedRootFolder pinned at share time (see WorkspaceActor.captureSharedRoot)
// always wins: every target pane of a tab share then browses from the same root,
// and a pane share browses from the directory captured when it was shared. This
// is independent of target_pane_id, matching "all panes use the tab's root".
//
// When no root was pinned (or the pinned directory no longer exists), it falls
// back to resolving the target pane's live working directory.
func (u *UpstreamShareActor) resolveBrowseRoot(targetPaneID string) (root, errCode, errMsg string) {
	if u.sharedRootFolder != "" {
		if info, err := os.Stat(u.sharedRootFolder); err == nil && info.IsDir() {
			return u.sharedRootFolder, "", ""
		}
	}

	pane, ok := u.resolveTargetPane(targetPaneID)
	if !ok {
		return "", "pane_not_found", "target pane is not part of this share"
	}
	if r := resolvePaneRoot(pane.ShellPID, pane.StartupDir); r != "" {
		return r, "", ""
	}
	return "", "io_error", "could not resolve browse root"
}

// resolvePaneRoot resolves a pane's working directory, best-effort. See
// resolvePaneRootWithSource for the resolution order and platform notes.
func resolvePaneRoot(shellPID int, startupDir string) string {
	root, _ := resolvePaneRootWithSource(shellPID, startupDir)
	return root
}

// resolvePaneRootWithSource resolves a pane's browse root and reports how it was
// resolved: "live" (the process's current working directory, reflecting any
// `cd`), "startup" (the pane's launch directory), or "daemon" (the daemon's own
// working directory — a last-resort fallback). "" means nothing resolved.
//
// IMPORTANT: the live-cwd lookup must NOT be gated on runtime.GOOS. procCwd is
// cross-platform (/proc on Linux, lsof on macOS/BSD); foregroundProcForPID is
// Linux-only and returns 0 elsewhere, so on macOS we read the shell's own cwd.
// An earlier version guarded this whole block with `runtime.GOOS == "linux"`,
// which made every macOS share fall through to the daemon dir (the home folder).
func resolvePaneRootWithSource(shellPID int, startupDir string) (root, source string) {
	if shellPID > 0 {
		// Live foreground cwd first (Linux): the foreground process group leader's
		// /proc/<pid>/cwd is the directory after `cd`. Then the shell's own cwd
		// (resolved via lsof inside procCwd on macOS/BSD).
		if fg := foregroundProcForPID(shellPID); fg > 0 {
			if d := procCwd(fg); d != "" {
				return d, "live"
			}
		}
		if d := procCwd(shellPID); d != "" {
			return d, "live"
		}
	}
	if dir := strings.TrimSpace(startupDir); dir != "" {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir, "startup"
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return wd, "daemon"
	}
	return "", ""
}

// resolveTargetPane returns the snapshot of the pane to browse. For a single-pane
// share target_pane_id is optional (the share's one pane is used); for multi-pane
// (tab/lane/group) shares target_pane_id selects the pane and is validated to be
// part of the shared entity.
func (u *UpstreamShareActor) resolveTargetPane(targetPaneID string) (domain.PaneSnapshot, bool) {
	reply, err := u.pub.Request(msg.T("ws", "snapshot"), &msg.MsgGetWorkspaceSnapshot{}, 2*time.Second)
	if err != nil {
		return domain.PaneSnapshot{}, false
	}
	snapReply, ok := reply.(*msg.MsgWorkspaceSnapshotReply)
	if !ok {
		return domain.PaneSnapshot{}, false
	}
	panes := u.collectEntityPanes(snapReply.Snapshot)
	if len(panes) == 0 {
		return domain.PaneSnapshot{}, false
	}
	if targetPaneID == "" {
		return panes[0], true
	}
	for _, p := range panes {
		if p.ID == targetPaneID {
			return p, true
		}
	}
	return domain.PaneSnapshot{}, false
}

// fsList lists one directory level at abs and replies.
func (u *UpstreamShareActor) fsList(nMsg *nats.Msg, req fsRequest, root, abs, relPath string) {
	reply, code, message := fsListCore(root, abs, relPath, req.RequestID)
	if code != "" {
		respondFSError(nMsg, req.RequestID, code, message)
		return
	}
	respondFS(nMsg, reply)
}

// fsListCore lists one directory level at abs. Shared by the share responder
// above and the embedded web UI (WebFSBrowse); "" code means success.
func fsListCore(root, abs, relPath, requestID string) (fsListReply, string, string) {
	info, err := os.Stat(abs)
	if err != nil {
		return fsListReply{}, statErrorCode(err), err.Error()
	}
	if !info.IsDir() {
		return fsListReply{}, "io_error", "not a directory"
	}
	dirEntries, err := os.ReadDir(abs)
	if err != nil {
		return fsListReply{}, statErrorCode(err), err.Error()
	}
	// dirs first, then files, alphabetical within each group.
	sort.Slice(dirEntries, func(i, j int) bool {
		di, dj := dirEntries[i].IsDir(), dirEntries[j].IsDir()
		if di != dj {
			return di
		}
		return dirEntries[i].Name() < dirEntries[j].Name()
	})

	entries := make([]fsEntry, 0, len(dirEntries))
	truncated := false
	for _, de := range dirEntries {
		if len(entries) >= fsMaxEntries {
			truncated = true
			break
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		e := fsEntry{
			Name:    de.Name(),
			Size:    fi.Size(),
			MTimeMs: fi.ModTime().UnixMilli(),
		}
		if de.IsDir() {
			e.Kind = "dir"
			e.ContentClass = "dir"
		} else {
			e.Kind = "file"
			cls, mime := classifyFile(filepath.Join(abs, de.Name()), de.Name())
			e.ContentClass = cls
			e.MIME = mime
		}
		entries = append(entries, e)
	}

	return fsListReply{
		OK:        true,
		RequestID: requestID,
		Root:      root,
		Path:      relPath,
		Entries:   entries,
		Truncated: truncated,
	}, "", ""
}

// fsStat returns metadata for a single file or directory.
func (u *UpstreamShareActor) fsStat(nMsg *nats.Msg, req fsRequest, root, abs, relPath string) {
	info, err := os.Stat(abs)
	if err != nil {
		respondFSError(nMsg, req.RequestID, statErrorCode(err), err.Error())
		return
	}
	e := fsEntry{
		Name:    filepath.Base(abs),
		Size:    info.Size(),
		MTimeMs: info.ModTime().UnixMilli(),
	}
	if info.IsDir() {
		e.Kind = "dir"
		e.ContentClass = "dir"
	} else {
		e.Kind = "file"
		cls, mime := classifyFile(abs, e.Name)
		e.ContentClass = cls
		e.MIME = mime
	}
	respondFS(nMsg, fsStatReply{
		OK:        true,
		RequestID: req.RequestID,
		Root:      root,
		Path:      relPath,
		Entry:     e,
	})
}

// fsRead reads a chunk of a text or image file and replies. Text is returned as
// utf8 in fsMaxTextChunk-sized chunks (eof false until done); images are returned
// whole as base64. Size caps apply (too_large); unsupported types are rejected.
func (u *UpstreamShareActor) fsRead(nMsg *nats.Msg, req fsRequest, abs, relPath string) {
	reply, code, message := fsReadCore(abs, relPath, req.RequestID, req.Offset, req.Length)
	if code != "" {
		respondFSError(nMsg, req.RequestID, code, message)
		return
	}
	respondFS(nMsg, reply)
}

// fsReadCore reads a chunk of a text file or a whole image. Shared by the
// share responder above and the embedded web UI (WebFSBrowse); "" code means
// success.
func fsReadCore(abs, relPath, requestID string, reqOffset, reqLength int64) (fsReadReply, string, string) {
	info, err := os.Stat(abs)
	if err != nil {
		return fsReadReply{}, statErrorCode(err), err.Error()
	}
	if info.IsDir() {
		return fsReadReply{}, "unsupported_type", "path is a directory; use op=list"
	}
	cls, mime := classifyFile(abs, filepath.Base(abs))
	total := info.Size()

	switch cls {
	case "text":
		if total > fsMaxTextTotal {
			return fsReadReply{}, "too_large",
				"text file exceeds the maximum supported size"
		}
		offset := reqOffset
		if offset < 0 {
			offset = 0
		}
		if offset > total {
			offset = total
		}
		length := reqLength
		if length <= 0 || length > fsMaxTextChunk {
			length = fsMaxTextChunk
		}
		if offset+length > total {
			length = total - offset
		}
		buf := make([]byte, length)
		f, err := os.Open(abs)
		if err != nil {
			return fsReadReply{}, statErrorCode(err), err.Error()
		}
		defer f.Close()
		n, err := f.ReadAt(buf, offset)
		if err != nil && int64(n) != length {
			// ReadAt returns io.EOF when it fills the buffer exactly at EOF; only
			// treat a short read as an error.
			if int64(n) < length {
				buf = buf[:n]
			}
		}
		buf = buf[:n]
		// Secret redaction intentionally DISABLED (owner's choice): shared file
		// content is forwarded verbatim, matching the live-output path (which has
		// never redacted). redactSecrets is kept (and unit-tested) so a future
		// opt-in config knob can re-enable the pass without rebuilding it.
		return fsReadReply{
			OK:           true,
			RequestID:    requestID,
			Path:         relPath,
			ContentClass: "text",
			MIME:         mime,
			Encoding:     "utf8",
			Data:         string(buf),
			Offset:       offset,
			Length:       int64(n),
			TotalSize:    total,
			EOF:          offset+int64(n) >= total,
			Redacted:     false,
		}, "", ""

	case "image":
		if total > fsMaxImage {
			return fsReadReply{}, "too_large",
				"image exceeds the maximum supported size"
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return fsReadReply{}, statErrorCode(err), err.Error()
		}
		return fsReadReply{
			OK:           true,
			RequestID:    requestID,
			Path:         relPath,
			ContentClass: "image",
			MIME:         mime,
			Encoding:     "base64",
			Data:         base64.StdEncoding.EncodeToString(data),
			Offset:       0,
			Length:       total,
			TotalSize:    total,
			EOF:          true,
			Redacted:     false,
		}, "", ""

	default:
		return fsReadReply{}, "unsupported_type",
			"file type is not viewable (only text and images are supported)"
	}
}

// ---------------------------------------------------------------------------
// Path safety / sandbox
// ---------------------------------------------------------------------------

// resolveSandboxedPath resolves the request path against root and verifies the
// result stays within root. It returns the absolute on-disk path and the
// normalized path relative to root. When allowAbsolute is false, absolute
// request paths are rejected and the result is confined to the root subtree;
// "denied" is returned on any `..` escape or out-of-root symlink target.
func resolveSandboxedPath(root, reqPath string, allowAbsolute bool) (abs, rel, errCode, errMsg string) {
	realRoot, err := canonicalize(root)
	if err != nil {
		return "", "", "io_error", "browse root is unavailable"
	}

	reqPath = strings.TrimSpace(reqPath)

	var target string
	if filepath.IsAbs(reqPath) {
		if !allowAbsolute {
			return "", "", "denied", "absolute paths are not permitted for this share"
		}
		target = filepath.Clean(reqPath)
	} else {
		// Join under root and clean; this resolves `.`/`..` lexically.
		target = filepath.Clean(filepath.Join(realRoot, reqPath))
	}

	// Early lexical sandbox check on the cleaned (pre-symlink) target. This
	// catches `..` escapes regardless of whether the escaped leaf exists, so a
	// traversal to a non-existent path is "denied" rather than "not_found".
	if !allowAbsolute && !withinRoot(realRoot, target) {
		return "", "", "denied", "path escapes the shared root"
	}

	// Canonicalize (follow symlinks). For a not-yet-existing leaf, EvalSymlinks
	// fails, so fall back to canonicalizing the parent and re-appending the base.
	realTarget, err := canonicalize(target)
	if err != nil {
		parent := filepath.Dir(target)
		base := filepath.Base(target)
		realParent, perr := canonicalize(parent)
		if perr != nil {
			return "", "", "not_found", "path does not exist"
		}
		realTarget = filepath.Join(realParent, base)
	}

	// Re-check after symlink resolution: catches symlinks whose target leaves root.
	if !allowAbsolute && !withinRoot(realRoot, realTarget) {
		return "", "", "denied", "path escapes the shared root"
	}

	rel, err = filepath.Rel(realRoot, realTarget)
	if err != nil || rel == "." {
		rel = ""
	}
	return realTarget, rel, "", ""
}

// canonicalize returns the absolute, symlink-evaluated, cleaned form of p.
func canonicalize(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

// withinRoot reports whether target is root itself or a descendant of root.
// Both must already be cleaned/absolute.
func withinRoot(root, target string) bool {
	if target == root {
		return true
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ---------------------------------------------------------------------------
// Content classification
// ---------------------------------------------------------------------------

// imageExtensions are extensions classified as viewable images.
var imageExtensions = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
	".svg":  "image/svg+xml",
	".ico":  "image/x-icon",
}

// textExtensions are extensions treated as text regardless of byte sniffing
// (covers source/config files that DetectContentType may report as octet-stream).
var textExtensions = map[string]bool{
	".go": true, ".py": true, ".js": true, ".ts": true, ".tsx": true, ".jsx": true,
	".c": true, ".h": true, ".cpp": true, ".cc": true, ".hpp": true, ".rs": true,
	".java": true, ".kt": true, ".rb": true, ".php": true, ".swift": true, ".scala": true,
	".sh": true, ".bash": true, ".zsh": true, ".fish": true, ".ps1": true,
	".md": true, ".markdown": true, ".txt": true, ".text": true, ".log": true,
	".json": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true, ".cfg": true,
	".conf": true, ".env": true, ".xml": true, ".html": true, ".htm": true, ".css": true,
	".scss": true, ".sql": true, ".csv": true, ".tsv": true, ".proto": true,
	".gitignore": true, ".dockerignore": true, ".mod": true, ".sum": true,
	".lua": true, ".vim": true, ".el": true, ".clj": true, ".ex": true, ".exs": true,
	".dart": true, ".r": true, ".m": true, ".pl": true, ".tf": true, ".gradle": true,
	".properties": true, ".makefile": true, ".cmake": true, ".bat": true,
}

// classifyFile maps a file to a content class ("text" | "image" | "unsupported")
// and a MIME type. It uses an extension allowlist first, then sniffs the first
// fsSniffLen bytes with http.DetectContentType.
func classifyFile(absPath, name string) (class, mime string) {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == "" {
		// Extensionless files like "Makefile", "Dockerfile".
		lower := strings.ToLower(name)
		if textExtensions["."+lower] || lower == "makefile" || lower == "dockerfile" ||
			lower == "readme" || lower == "license" || lower == "changelog" {
			return "text", "text/plain"
		}
	}
	if m, ok := imageExtensions[ext]; ok {
		return "image", m
	}
	if textExtensions[ext] {
		return "text", extMIME(ext)
	}

	// Fall back to byte sniffing.
	f, err := os.Open(absPath)
	if err != nil {
		return "unsupported", "application/octet-stream"
	}
	defer f.Close()
	buf := make([]byte, fsSniffLen)
	n, _ := f.Read(buf)
	buf = buf[:n]
	detected := http.DetectContentType(buf)

	switch {
	case strings.HasPrefix(detected, "text/"):
		return "text", detected
	case strings.HasPrefix(detected, "image/"):
		return "image", detected
	case detected == "application/json" || detected == "application/xml":
		return "text", detected
	case isProbablyText(buf):
		return "text", "text/plain"
	default:
		return "unsupported", detected
	}
}

// extMIME returns a friendly MIME for a known text extension.
func extMIME(ext string) string {
	switch ext {
	case ".go":
		return "text/x-go"
	case ".json":
		return "application/json"
	case ".md", ".markdown":
		return "text/markdown"
	case ".html", ".htm":
		return "text/html"
	case ".css":
		return "text/css"
	case ".js":
		return "text/javascript"
	case ".xml":
		return "text/xml"
	case ".yaml", ".yml":
		return "text/yaml"
	case ".csv":
		return "text/csv"
	default:
		return "text/plain"
	}
}

// isProbablyText reports whether a byte sample looks like text: valid-ish UTF-8
// with no NUL bytes and a low proportion of control characters.
func isProbablyText(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	ctrl := 0
	for _, c := range b {
		if c == 0 {
			return false // NUL → binary
		}
		if c < 0x09 || (c > 0x0d && c < 0x20) {
			ctrl++
		}
	}
	return ctrl*100/len(b) < 10
}

// ---------------------------------------------------------------------------
// Secret redaction
// ---------------------------------------------------------------------------

// redactSecrets runs text content through a best-effort secret redactor and
// reports whether the bytes changed.
//
// CURRENTLY UNUSED BY THE LIVE PATH: secret redaction is intentionally disabled
// for shared file content (owner's choice) — the .fs read responder forwards
// bytes verbatim, matching the live shared-output path, which has never
// redacted. The pass is kept (and unit-tested) so a future opt-in config knob
// can re-enable it without rebuilding it. It is a minimal line-oriented pass
// masking obvious credential-bearing lines (KEY=secret style assignments for
// sensitive-looking names, and common token prefixes).
func redactSecrets(content []byte) ([]byte, bool) {
	if len(content) == 0 {
		return content, false
	}
	lines := strings.Split(string(content), "\n")
	changed := false
	for i, line := range lines {
		if red, ok := redactLine(line); ok {
			lines[i] = red
			changed = true
		}
	}
	if !changed {
		return content, false
	}
	return []byte(strings.Join(lines, "\n")), true
}

// sensitiveKeyHints are substrings that, when present in an assignment's key,
// mark the value as a secret to mask.
var sensitiveKeyHints = []string{
	"password", "passwd", "secret", "token", "apikey", "api_key",
	"access_key", "secret_key", "private_key", "credential", "auth",
}

// redactLine masks the value of a "KEY=value" / "KEY: value" assignment whose key
// looks sensitive, and masks lines containing well-known token prefixes. Returns
// (redactedLine, true) when it changed the line.
func redactLine(line string) (string, bool) {
	lower := strings.ToLower(line)

	// KEY=value or KEY: value with a sensitive key name.
	for _, sep := range []string{"=", ":"} {
		idx := strings.Index(line, sep)
		if idx <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:idx]))
		val := strings.TrimSpace(line[idx+1:])
		if val == "" {
			continue
		}
		for _, hint := range sensitiveKeyHints {
			if strings.Contains(key, hint) {
				return line[:idx] + sep + " <redacted>", true
			}
		}
		break
	}

	// Common token prefixes anywhere in the line.
	for _, prefix := range []string{"sk-", "ghp_", "github_pat_", "xoxb-", "xoxp-", "aws_secret_access_key", "-----begin"} {
		if strings.Contains(lower, prefix) {
			return "<redacted>", true
		}
	}
	return line, false
}

// ---------------------------------------------------------------------------
// /proc helpers (live cwd resolution on Linux)
// ---------------------------------------------------------------------------

// procCwd returns the live working directory of a process, or "" if it cannot
// be resolved (gone process, permission denied, unsupported platform). On Linux
// it reads /proc/<pid>/cwd; on macOS/BSD (no /proc) it shells out to lsof. This
// is what lets a shared pane's CURRENT directory (after `cd`) become the mobile
// browse root, not just the shell's startup directory.
func procCwd(pid int) string {
	if pid <= 0 {
		return ""
	}
	var dir string
	if runtime.GOOS == "linux" {
		d, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
		if err != nil {
			return ""
		}
		dir = d
	} else {
		dir = cwdViaLsof(pid)
	}
	if dir == "" {
		return ""
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return ""
	}
	return dir
}

// cwdViaLsof returns process pid's current working directory using lsof, for
// platforms without /proc (macOS/BSD). `lsof -Fn` prints one field per line; the
// cwd path is the line beginning with 'n'. Bounded by a short timeout so a
// stalled lsof never blocks the caller (it runs on an actor mailbox at share
// time). Returns "" if lsof is unavailable or the path cannot be parsed.
func cwdViaLsof(pid int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return strings.TrimPrefix(line, "n")
		}
	}
	return ""
}

// foregroundProcForPID returns a pid to read the live cwd from: the foreground
// process group's leader if one differs from the shell, else 0 (caller falls
// back to the shell pid). On Linux the shell's controlling terminal exposes the
// foreground pgid via /proc/<shellPID>/stat field 8 (tpgid); the pgid doubles as
// the group leader's pid.
func foregroundProcForPID(shellPID int) int {
	if shellPID <= 0 || runtime.GOOS != "linux" {
		return 0
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(shellPID) + "/stat")
	if err != nil {
		return 0
	}
	// /proc/<pid>/stat: "pid (comm) state ppid pgrp session tty_nr tpgid ...".
	// comm may contain spaces/parens, so split after the last ')'.
	s := string(data)
	rp := strings.LastIndexByte(s, ')')
	if rp < 0 || rp+1 >= len(s) {
		return 0
	}
	fields := strings.Fields(s[rp+1:])
	// fields[0]=state, ... tpgid is the 8th of the original layout, i.e. index 5
	// here (state, ppid, pgrp, session, tty_nr, tpgid).
	if len(fields) < 6 {
		return 0
	}
	tpgid, _ := strconv.Atoi(fields[5])
	if tpgid > 0 && tpgid != shellPID {
		return tpgid
	}
	return 0
}

// ---------------------------------------------------------------------------
// Reply helpers
// ---------------------------------------------------------------------------

// statErrorCode maps an os file error to a wire error code.
func statErrorCode(err error) string {
	switch {
	case os.IsNotExist(err):
		return "not_found"
	case os.IsPermission(err):
		return "denied"
	default:
		return "io_error"
	}
}

// respondFS marshals v and sends it as the reply.
func respondFS(nMsg *nats.Msg, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = nMsg.Respond(data)
}

// respondFSError sends an error reply.
func respondFSError(nMsg *nats.Msg, requestID, code, message string) {
	respondFS(nMsg, fsErrorReply{
		OK:        false,
		RequestID: requestID,
		Error:     code,
		Message:   message,
	})
}
