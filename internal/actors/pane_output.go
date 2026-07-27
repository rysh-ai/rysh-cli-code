package actors

// ---------------------------------------------------------------------------
// Buffer management
// ---------------------------------------------------------------------------

// Scrollback transport bounds — keep the reply well under the NATS max payload.
const (
	maxScrollbackRows  = 2000
	maxScrollbackBytes = 700 * 1024
)

// cappedBuffer is an append-mostly text buffer that retains at most a given
// number of trailing bytes. It is backed by a []byte so appends are amortized
// O(1) (Go slice growth), instead of the O(n) full reallocation that string
// concatenation (s += x) performed on every call — the previous hot-path cost
// for high-volume PTY/AI output (each append copied up to maxPaneBuffer bytes).
type cappedBuffer struct {
	buf []byte
}

// Append adds s, then trims from the front so at most max bytes are retained.
// max <= 0 means unbounded.
func (b *cappedBuffer) Append(s string, max int) {
	b.buf = append(b.buf, s...)
	if max > 0 && len(b.buf) > max {
		n := len(b.buf) - max
		copy(b.buf, b.buf[n:]) // forward copy of overlapping slice is safe
		b.buf = b.buf[:max]
	}
}

// Set replaces the buffer contents (used on KV restore).
func (b *cappedBuffer) Set(s string) { b.buf = append(b.buf[:0], s...) }

// Reset empties the buffer, retaining capacity.
func (b *cappedBuffer) Reset() { b.buf = b.buf[:0] }

// String returns the current contents as a string.
func (b *cappedBuffer) String() string { return string(b.buf) }

// Len returns the current byte length.
func (b *cappedBuffer) Len() int { return len(b.buf) }

// buildScrollbackRows returns the VT scrollback history followed by the current
// screen, rendered as ANSI rows (oldest first), for raw-pane scroll mode.
// Bounded by row count and total bytes (trimming from the oldest end).
// Cross-actor-safe: the vterm wrapper serializes access against the read loop.
func (p *PaneActor) buildScrollbackRows() []string {
	var rows []string
	if p.remoteInteractive {
		// Subscriber side: history reconstructed from the remote share's VTerm,
		// forwarded line-by-line, plus the current remote screen.
		rows = append(rows, p.remoteScrollback...)
		rows = append(rows, p.remoteVTScreen...)
	} else if p.vtermEmu != nil {
		rows = p.vtermEmu.ScrollbackANSI()
		rows = append(rows, p.vtermEmu.RenderANSI()...)
	} else {
		return nil
	}
	if len(rows) > maxScrollbackRows {
		rows = rows[len(rows)-maxScrollbackRows:]
	}
	total := 0
	for _, r := range rows {
		total += len(r) + 1
	}
	for total > maxScrollbackBytes && len(rows) > 1 {
		total -= len(rows[0]) + 1
		rows = rows[1:]
	}
	return rows
}

// scrollbackDelta returns the pane's current monotonic evicted-line total and
// the scrollback rows (rendered ANSI, oldest first) evicted since `since`.
// Used by a tab share's layout loop to forward incremental interactive history.
func (p *PaneActor) scrollbackDelta(since int64) (int64, []string) {
	if p.vtermEmu == nil {
		return 0, nil
	}
	evicted := p.vtermEmu.ScrollbackEvictedTotal()
	if evicted <= since {
		return evicted, nil
	}
	return evicted, p.vtermEmu.ScrollbackTailANSI(int(evicted - since))
}

// bufMax returns the byte cap for this pane's text output buffers: the
// configured `[ui] shell_buffer_bytes` when set, else the built-in default.
func (p *PaneActor) bufMax() int {
	if p.cfg.ShellBufferBytes > 0 {
		return p.cfg.ShellBufferBytes
	}
	return maxPaneBuffer
}

func (p *PaneActor) appendOutput(text string) {
	p.output.Append(text, p.bufMax())
	p.kvDirty = true
}

func (p *PaneActor) clearOutput() {
	p.output.Reset()
	p.privateBuffer.Reset()
	p.kvDirty = true
}

// PrivateOutput returns the dedicated private (raw) output buffer.
// Cross-actor read — informational only (same pattern as PaneHistory).
func (p *PaneActor) PrivateOutput() string {
	return p.privateBuffer.String()
}

// ChatOutput returns the chat-mode output buffer.
// Cross-actor read — informational only (same pattern as PrivateOutput).
func (p *PaneActor) ChatOutput() string {
	return p.chatOutput.String()
}

// ExternalOutput returns the external-mode output buffer.
// Cross-actor read — informational only.
func (p *PaneActor) ExternalOutput() string {
	return p.externalOutput.String()
}

// appendModeOutput appends to a dynamic per-mode buffer (a humanoid's own mode,
// keyed by the humanoid name). The buffer is created on first use.
func (p *PaneActor) appendModeOutput(mode, text string) {
	if p.modeOutputs == nil {
		p.modeOutputs = make(map[string]*cappedBuffer)
	}
	b := p.modeOutputs[mode]
	if b == nil {
		b = &cappedBuffer{}
		p.modeOutputs[mode] = b
	}
	b.Append(text, p.bufMax())
	p.kvDirty = true
}

// ModeOutput returns a dynamic mode's buffer contents (empty when absent).
// Cross-actor read — informational only.
func (p *PaneActor) ModeOutput(mode string) string {
	if b := p.modeOutputs[mode]; b != nil {
		return b.String()
	}
	return ""
}

// HoppedInfo holds hopped content metadata for cross-actor reads.
type HoppedInfo struct {
	Content     string
	ChatContent string
	Alias       string
	ID          string
	MemoryTurns int // LLM conversation turns forked into this pane (0 = text-only hop)
}

// HoppedInfo returns the hopped content and source info.
// Cross-actor read — informational only.
func (p *PaneActor) HoppedInfo() *HoppedInfo {
	if p.hoppedContent == "" && p.hoppedChatContent == "" && p.hoppedMemoryTurns == 0 {
		return nil
	}
	return &HoppedInfo{
		Content:     p.hoppedContent,
		ChatContent: p.hoppedChatContent,
		Alias:       p.hoppedFromAlias,
		ID:          p.hoppedFromID,
		MemoryTurns: p.hoppedMemoryTurns,
	}
}

// AppendSystemOutput appends text to the display output buffer only.
// It does NOT write to privateBuffer, does NOT publish to NATS, and does NOT
// mark state as KV-dirty.  Used for ## system command output that the user
// should see but that must never be recorded.
// Cross-actor write — display only (same pattern as PaneHistory reads).
func (p *PaneActor) AppendSystemOutput(text string) {
	p.output.Append(text, p.bufMax())
}

// appendShellOutput appends text to the shell-specific output buffer.
func (p *PaneActor) appendShellOutput(text string) {
	p.shellOutput.Append(text, p.bufMax())
	p.kvDirty = true
}

// appendAIOutput appends text to the AI-specific output buffer.
func (p *PaneActor) appendAIOutput(text string) {
	p.aiOutput.Append(text, p.bufMax())
	p.kvDirty = true
}

// AppendRyshOutput appends text to the rysh-mode output buffer.
// Cross-actor write — used by the AppendPaneRyshOutput chain and NATS handler.
func (p *PaneActor) AppendRyshOutput(text string) {
	p.ryshOutput.Append(text, p.bufMax())
	p.kvDirty = true
}

// appendChatOutput appends text to the chat-mode output buffer.
func (p *PaneActor) appendChatOutput(text string) {
	p.chatOutput.Append(text, p.bufMax())
	p.kvDirty = true
}

// appendExternalOutput appends text to the external-mode output buffer.
// External output is fed by HumanoidActor — inbound channel messages and the
// LLM's responses to them, rendered separately from the user's local chat.
func (p *PaneActor) appendExternalOutput(text string) {
	p.externalOutput.Append(text, p.bufMax())
	p.kvDirty = true
}

// appendPrivateBuffer accumulates raw output in the dedicated private buffer,
// capping at the configured PrivateBufferSize (0 = unbounded). The private
// buffer feeds AI context and sharing — never the local display — so ANSI
// escapes (SGR color kept for display when ShellColorOutput is on) are
// stripped here rather than wasting model tokens / polluting shared text.
func (p *PaneActor) appendPrivateBuffer(text string) {
	p.privateBuffer.Append(stripAnsiEscapes(text), p.privateBufferSize)
	p.kvBuffersDirty = true
}
