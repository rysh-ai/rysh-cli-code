package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// These benchmarks quantify the lightweight VT-only pull (MsgGetPaneVT) against
// the full pane-snapshot fetch (MsgGetPaneSnapshot) that the raw-VT render path
// used to issue on EVERY frame. For a redraw-heavy inline TUI (claude) in a
// multi-pane layout the TUI pulls one of these per rawDirty signal — up to
// ~30-60×/sec — so the per-frame cost directly governs keystroke responsiveness.
//
// Each benchmark mirrors the real wire path content_stream.go uses:
//   reply struct → JSON payload → NATSEnvelope → bytes   (PaneActor reply side)
//   bytes → NATSEnvelope → CodecRegistry.Decode → reply  (TUI fetch side)
//
// Run:  go test ./internal/tui -run x -bench 'PerFrame|FrameHash|Wire' -benchmem

// ---- representative data ---------------------------------------------------

// vtScreenRows builds a claude-like interactive screen: `rows` lines of ~width
// columns, each carrying a few SGR colour sequences (as the VTerm renderer emits).
func vtScreenRows(rows, width int) []string {
	out := make([]string, rows)
	for y := 0; y < rows; y++ {
		var b strings.Builder
		b.WriteString("\x1b[38;5;240m│\x1b[0m ")
		b.WriteString(fmt.Sprintf("\x1b[1;36m%3d\x1b[0m ", y))
		filler := fmt.Sprintf("the quick brown fox jumps over the lazy dog row %d ", y)
		for b.Len() < width {
			b.WriteString(filler)
		}
		out[y] = b.String()[:width] + "\x1b[0m"
	}
	return out
}

// fatBuffer returns ~n bytes of multi-line text, like a pane's accumulated output.
func fatBuffer(n int) string {
	line := "[2026-06-24T12:00:00Z] tool_result: ok — processed 42 files, 0 errors, wrote output.\n"
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(line)
	}
	return b.String()[:n]
}

func histList(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("git commit -m \"iteration %d: refactor the snapshot path\"", i)
	}
	return out
}

// fullPaneSnapshot is what the per-frame raw fetch (LayoutOnly=false) pulled: the
// VT screen PLUS every heavy output buffer and command history.
func fullPaneSnapshot() domain.PaneSnapshot {
	screen := vtScreenRows(50, 120)
	return domain.PaneSnapshot{
		ID:            "pane-abc123",
		Title:         "claude",
		Mode:          "prompt",
		Status:        "interactive",
		RawMode:       true,
		VTScreen:      screen,
		VTCursorRow:   49,
		VTCursorCol:   12,
		Output:        fatBuffer(15000),
		ShellOutput:   fatBuffer(8000),
		AIOutput:      fatBuffer(12000),
		ChatOutput:    fatBuffer(2000),
		RyshOutput:    fatBuffer(1000),
		MergedHistory: histList(30),
		ShellHistory:  histList(30),
		PromptHistory: histList(30),
		RyshHistory:   histList(10),
		ChatHistory:   histList(10),
	}
}

// vtOnlyReply is what the lightweight pull returns: just the live frame.
func vtOnlyReply() *msgpkg.MsgPaneVTReply {
	return &msgpkg.MsgPaneVTReply{
		PaneID:      "pane-abc123",
		Interactive: true,
		Screen:      vtScreenRows(50, 120),
		CursorRow:   49,
		CursorCol:   12,
	}
}

// encodeReply marshals a reply through the same NATSEnvelope wrapping the
// PaneActor's Reply path uses, returning the bytes that hit the wire.
func encodeReply(tag string, reply interface{}) []byte {
	payload, _ := json.Marshal(reply)
	wire, _ := json.Marshal(msgpkg.NATSEnvelope{TypeTag: tag, Payload: payload})
	return wire
}

// ---- per-frame round-trip --------------------------------------------------

func benchPerFrame(b *testing.B, tag string, reply interface{}) {
	codecs := msgpkg.DefaultCodecRegistry()
	// Report the steady-state wire size (encode is deterministic here).
	b.ReportMetric(float64(len(encodeReply(tag, reply))), "wire-bytes")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// PaneActor reply side: build the wire bytes.
		wire := encodeReply(tag, reply)
		// TUI fetch side: decode the envelope and the inner reply.
		var env msgpkg.NATSEnvelope
		if err := json.Unmarshal(wire, &env); err != nil {
			b.Fatal(err)
		}
		if _, err := codecs.Decode(env.TypeTag, env.Payload); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPerFrameFullSnapshot is the OLD per-frame cost: a full pane snapshot.
func BenchmarkPerFrameFullSnapshot(b *testing.B) {
	benchPerFrame(b, msgpkg.TagPaneSnapshotReply, &msgpkg.MsgPaneSnapshotReply{Snapshot: fullPaneSnapshot()})
}

// BenchmarkPerFrameVTOnly is the NEW per-frame cost: the lightweight VT pull.
func BenchmarkPerFrameVTOnly(b *testing.B) {
	benchPerFrame(b, msgpkg.TagPaneVTReply, vtOnlyReply())
}

// ---- TUI apply / dedupe hash ----------------------------------------------

// BenchmarkFrameHashFull is the dedupe hash the full path runs each frame —
// snapContentHash folds in EVERY buffer (output, ai, chat, rysh, external, VT…).
func BenchmarkFrameHashFull(b *testing.B) {
	snap := fullPaneSnapshot()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = snapContentHash(snap)
	}
}

// BenchmarkFrameHashVT is the dedupe hash the lightweight path runs — only the
// VT screen + cursor.
func BenchmarkFrameHashVT(b *testing.B) {
	screen := vtScreenRows(50, 120)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = vtFrameHash(screen, 49, 12, true)
	}
}

// TestVTPullWireSizes is not a benchmark but prints the steady-state wire size
// reduction so `go test -v` documents the saving alongside the timings.
func TestVTPullWireSizes(t *testing.T) {
	full := len(encodeReply(msgpkg.TagPaneSnapshotReply, &msgpkg.MsgPaneSnapshotReply{Snapshot: fullPaneSnapshot()}))
	vt := len(encodeReply(msgpkg.TagPaneVTReply, vtOnlyReply()))
	t.Logf("per-frame wire bytes: full-snapshot=%d  vt-only=%d  reduction=%.1f%% (%.1fx smaller)",
		full, vt, 100*float64(full-vt)/float64(full), float64(full)/float64(vt))
	if vt >= full {
		t.Fatalf("VT-only reply (%d) should be smaller than full snapshot (%d)", vt, full)
	}
}
