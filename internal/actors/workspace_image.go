package actors

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/rysh-ai/rysh-cli-shared/provider"
)

// Follow-up 1b — ##image <path> command.
//
// MVP shape:
//
//   1. The user types `##image /path/to/screenshot.png` in a pane.
//   2. The workspace reads the file, base64-encodes it, infers the media
//      type, and stashes a provider.ContentBlock keyed by the active
//      pane ID on the WorkspaceActor.
//   3. The next non-`##` submission for that pane is intercepted by
//      handleSubmitInput, which attaches the pending block to
//      MsgPaneSubmitInput.ContentBlocks and clears the stash.
//   4. The PaneActor forwards the blocks to the LLM actor via
//      MsgAgenticPrompt.ContentBlocks (the LLM actor builds a structured
//      user turn that the provider serialises as the Anthropic array form).
//
// Deferred to per-client follow-ups: chrome captureVisibleTab,
// mobile picker, TUI paste detection, JetStream Object Store
// persistence (current MVP keeps base64 inline in the conversation).

// ImageMaxBytes caps the pre-encode image file size accepted by the
// command. ~5 MB matches Anthropic's per-image limit. Exposed as a var
// so a host can tune it per deployment.
var ImageMaxBytes int64 = 5 * 1024 * 1024

// handleImageCommand processes `##image <path>` and stashes a pending
// content block on the workspace for the active pane.
func (w *WorkspaceActor) handleImageCommand(out *strings.Builder, paneID string, args []string) {
	if len(args) == 0 {
		fmt.Fprintf(out, "\n[image] usage: ##image <path>\n")
		fmt.Fprintf(out, "  attaches the image at <path> to the next prompt in this pane\n")
		return
	}
	if paneID == "" {
		fmt.Fprintf(out, "\n[image] no active pane\n")
		return
	}
	if args[0] == "clear" {
		w.clearPendingImage(paneID)
		fmt.Fprintf(out, "\n[image] pending image cleared\n")
		return
	}

	path := args[0]
	if !filepath.IsAbs(path) {
		if wd, err := os.Getwd(); err == nil {
			path = filepath.Join(wd, path)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		fmt.Fprintf(out, "\n[image] cannot read %s: %v\n", path, err)
		return
	}
	if info.IsDir() {
		fmt.Fprintf(out, "\n[image] %s is a directory, not an image\n", path)
		return
	}
	if info.Size() > ImageMaxBytes {
		fmt.Fprintf(out, "\n[image] %s is %.1f MB (max %d MB)\n", path, float64(info.Size())/(1024*1024), ImageMaxBytes/(1024*1024))
		return
	}

	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(out, "\n[image] cannot read %s: %v\n", path, err)
		return
	}

	mediaType := http.DetectContentType(data)
	if !strings.HasPrefix(mediaType, "image/") {
		fmt.Fprintf(out, "\n[image] %s does not look like an image (detected %s)\n", path, mediaType)
		return
	}

	block := provider.ContentBlockImageBase64(mediaType, base64.StdEncoding.EncodeToString(data))
	w.setPendingImage(paneID, block)
	fmt.Fprintf(out, "\n[image] attached %s (%s, %.1f KB) to next prompt in this pane\n", path, mediaType, float64(info.Size())/1024)
}

// setPendingImage stashes a content block on the workspace, keyed by
// pane ID. Lazy map init.
func (w *WorkspaceActor) setPendingImage(paneID string, block provider.ContentBlock) {
	if w.pendingImages == nil {
		w.pendingImages = make(map[string]provider.ContentBlock)
	}
	w.pendingImages[paneID] = block
}

// takePendingImage atomically consumes and returns the pending image
// for a pane (or zero-value when none is set).
func (w *WorkspaceActor) takePendingImage(paneID string) (provider.ContentBlock, bool) {
	if w.pendingImages == nil {
		return provider.ContentBlock{}, false
	}
	block, ok := w.pendingImages[paneID]
	if !ok {
		return provider.ContentBlock{}, false
	}
	delete(w.pendingImages, paneID)
	return block, true
}

func (w *WorkspaceActor) clearPendingImage(paneID string) {
	if w.pendingImages == nil {
		return
	}
	delete(w.pendingImages, paneID)
}
