package actors

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// ---------------------------------------------------------------------------
// Snapshot
// ---------------------------------------------------------------------------

// buildSnapshot builds the pane snapshot.
//
// includeContent controls whether the heavy display buffers (merged/per-mode
// output and command history) are populated. The TUI's layout-only fetch passes
// false: it streams per-pane content directly over NATS and only needs the
// structural/metadata fields here for layout. Internal callers (KV persistence,
// sharing, CLI inspection) and the TUI's direct per-pane backfill pass true.
//
// includeConversations additionally populates the structured-conversation
// buffers, consumed only by restoreConversations (KV restore); only the
// 2s-gated persistence path passes true.
func (p *PaneActor) buildSnapshot(includeContent, includeConversations bool) domain.PaneSnapshot {
	snap := domain.PaneSnapshot{
		ID:                    p.id,
		Title:                 p.title,
		PaneType:              p.paneType,
		Mode:                  p.mode,
		EnabledModes:          p.enabledModes,
		WebURL:                p.webURL,
		WebProfile:            p.webProfile,
		WebTitle:              p.webTitle,
		WebActivateSeq:        p.webActivateSeq,
		Status:                p.status,
		LastCommand:           p.lastCommand,
		ProviderName:          p.providerName,
		ProviderOverride:      p.providerOverride,
		ProviderOverrideModel: p.providerOverrideModel,
		GivenName:             p.givenName,
		ListeningToID:         p.listeningToID,
		RegisteredHumanoid:    p.registeredHumanoid,
		Sharing:               p.sharing,
		UpstreamURL:           p.upstreamURL,
		UpstreamConnected:     p.upstreamConnected,
		ControllingShareID:    p.controllingShareID,
		ControllingPaneAlias:  p.controllingPaneAlias,
		ConnectedToPaneID:     p.connectedToPaneID,
		HoppedFromAlias:       p.hoppedFromAlias,
		HoppedFromID:          p.hoppedFromID,
		HasHoppedContent:      p.hoppedContent != "" || p.hoppedChatContent != "",
		ApprovalPaneGroups:    p.approvalPaneGroups,
		AttentionEnabled:      p.approvalAttentionEnabled,
	}

	// SecretNAT / ReSet state for the TUI badge. A boolean flag only — the
	// mapping table itself lives in memory inside the manager and is never
	// part of any snapshot.
	if p.agSetup != nil && p.agSetup.SecretNAT != nil {
		snap.SnatEnabled = p.agSetup.SecretNAT.Session(p.id).Enabled()
	}

	// Command history is small (capped command lists) and the TUI needs it for
	// arrow-key recall — there is no separate content stream for it — so keep it
	// in layout-only snapshots. Only the heavy display buffers are gated.
	snap.MergedHistory = p.mergedHistory
	snap.ShellHistory = p.shellHistory
	snap.PromptHistory = p.promptHistory
	snap.RyshHistory = p.ryshHistory
	snap.ChatHistory = p.chatHistory
	snap.ExternalHistory = p.externalHistory

	// Heavy display buffers — omitted for layout-only snapshots; the TUI streams
	// this content directly per-pane and accumulates it locally.
	if includeContent {
		snap.Output = strings.TrimRight(p.output.String(), "\n")
		snap.ShellOutput = strings.TrimRight(p.shellOutput.String(), "\n")
		snap.AIOutput = strings.TrimRight(p.aiOutput.String(), "\n")
		snap.RyshOutput = strings.TrimRight(p.ryshOutput.String(), "\n")
		snap.ChatOutput = strings.TrimRight(p.chatOutput.String(), "\n")
		snap.ExternalOutput = strings.TrimRight(p.externalOutput.String(), "\n")
		snap.ModeOutputs = p.snapshotModeOutputs()
	}

	// Expose the shell pid so the TUI can resolve the pane's live working
	// directory for shell tab-completion (paths complete relative to wherever
	// the user has cd'd, not the session start dir).
	if p.cmd != nil && p.cmd.Process != nil {
		snap.ShellPID = p.cmd.Process.Pid
	}

	// Live shell cwd as reported via OSC 7 (push-based; "" until the shell's
	// first prompt). Preferred over ShellPID+lsof by the TUI when present.
	if cwd, ok := p.shellCwdAtomic.Load().(string); ok && cwd != "" {
		snap.ShellCwd = cwd
	}

	// Native pass-through (##native): the TUI needs it for the double-Esc
	// exit gesture and footer hints.
	snap.NativeMode = p.nativeMode

	// Expose the pane's startup directory so the file-browse responder can use it
	// as a browse-root fallback when the live cwd (via /proc) is unavailable.
	snap.StartupDir = strings.TrimSpace(p.cfg.WorkingDirectory)

	// Structured conversation data is consumed only by restoreConversations (KV
	// restore at startup); the TUI render, sharing and mirror paths all read the
	// legacy buffers above. It is dead weight on the high-frequency snapshot/reply
	// path, so it is populated only when building a snapshot for persistence.
	if includeConversations {
		snap.Conversations = p.snapshotConversations()
		snap.MergedConv = convertConvMsgs(p.mergedConversation)
		snap.ConvHistories = p.snapshotConvHistories()
		snap.MergedConvHistory = convertConvMsgs(p.mergedConvHistory)
	}

	// Decide whether the pane is showing an interactive program.
	//
	// A program running in the PTY foreground process group (i.e. anything but
	// the shell) makes the pane interactive: keystrokes are forwarded to it and
	// its VT screen is shown, until it exits and the shell regains the prompt.
	// This terminal-accurate model is necessary because inline TUIs such as
	// codex give NO terminal signal at all (no alt screen, no hidden cursor, no
	// mouse tracking) — there is nothing for the VTerm heuristic to latch onto,
	// yet they own the terminal input and are very much interactive.
	//
	// We also remember the foreground program's process group so the pane stays
	// interactive across brief moments when the program is not the foreground
	// (e.g. an inline TUI re-initialising, or spawning a helper) until its whole
	// process group exits — detected by signalling it (processGroupAlive), not
	// by a transient foreground change. The shellForeground gate keeps a
	// SUSPENDED program (Ctrl+Z, shell back at its prompt) non-interactive and
	// lets it resume. The VTerm heuristic remains as an immediate signal and a
	// fallback where foreground detection is unavailable.
	//
	// Note: command output is still routed to the rysh shell buffer by
	// rawReadLoop (which keys off the heuristic, not the foreground group), so
	// ordinary commands' output is preserved in scrollback even though the live
	// view is the VT screen while they run.
	interactive := p.computeInteractive()

	// Include VT screen data when in interactive mode.
	if interactive && p.vtermEmu != nil {
		snap.RawMode = true
		// FullScreen is the NARROW signal (alt screen or hidden cursor) — the
		// same definition rawReadLoop keys its interactive transitions on.
		// RawMode alone is broader (any foreground child: cat, ls, make) and
		// exists for in-pane VT rendering; those panes must NEVER escalate to
		// the full-terminal PTY relay. If they did, the relay would dump raw
		// PTY bytes over the real terminal, and rawReadLoop — whose interactive
		// flag never rose for a plain command — would never publish relay.exit,
		// leaving the terminal obscured and in an inconsistent state.
		snap.FullScreen = p.rawMode || p.cursorHidden
		snap.MouseEnabled = p.vtermEmu.IsMouseEnabled()
		snap.AppCursorKeys = p.vtermEmu.IsAppCursorKeys()
		// The VT screen is heavy and replaces wholesale; include it only when
		// content is requested. The RawMode flag above stays in layout-only
		// snapshots so the TUI knows to fast-fetch this pane's frames directly.
		if includeContent {
			snap.VTScreen = p.vtermEmu.RenderANSIWithCursor()
			snap.VTCursorRow, snap.VTCursorCol = p.vtermEmu.CursorPos()
		}
	}

	// Include remote interactive sharing state.
	if p.remoteInteractive {
		snap.RemoteInteractive = true
		if includeContent {
			snap.RemoteVTScreen = p.remoteVTScreen
			snap.RemoteVTCursorRow = p.remoteVTCursorRow
			snap.RemoteVTCursorCol = p.remoteVTCursorCol
		}
	}

	// Include share restrictions if any are configured.
	if len(p.shareRestrictions.DisabledModes) > 0 ||
		len(p.shareRestrictions.ShellAllowList) > 0 ||
		len(p.shareRestrictions.ShellForbidList) > 0 {
		snap.ShareRestrictions = &domain.ShareRestrictions{
			DisabledModes:   p.shareRestrictions.DisabledModes,
			ShellAllowList:  p.shareRestrictions.ShellAllowList,
			ShellForbidList: p.shareRestrictions.ShellForbidList,
		}
	}

	// Include remote restrictions when in controller mode.
	if p.remoteRestrictions != nil {
		snap.RemoteShareRestrictions = &domain.ShareRestrictions{
			DisabledModes:   p.remoteRestrictions.DisabledModes,
			ShellAllowList:  p.remoteRestrictions.ShellAllowList,
			ShellForbidList: p.remoteRestrictions.ShellForbidList,
		}
	}

	return snap
}

// computeInteractive decides whether the pane is currently showing an interactive
// program, updating p.interactivePgid as a side effect so the determination stays
// fresh across transient foreground changes. Extracted from buildSnapshot so the
// lightweight VT-frame path (buildVTFrame) and the full snapshot agree on exactly
// the same rule. Runs on the actor (mailbox) goroutine.
//
// A program running in the PTY foreground process group (i.e. anything but the
// shell) makes the pane interactive: keystrokes are forwarded to it and its VT
// screen is shown, until it exits and the shell regains the prompt. This
// terminal-accurate model is necessary because inline TUIs such as codex give NO
// terminal signal at all (no alt screen, no hidden cursor, no mouse tracking) —
// nothing for the VTerm heuristic to latch onto — yet they own the terminal input.
//
// The foreground program's process group is remembered so the pane stays
// interactive across brief moments when the program is not the foreground (an
// inline TUI re-initialising, or spawning a helper) until its whole process group
// exits — detected by signalling it (processGroupAlive), not by a transient
// foreground change. The shellForeground gate keeps a SUSPENDED program (Ctrl+Z,
// shell back at its prompt) non-interactive and lets it resume. The VTerm
// heuristic remains as an immediate signal and a fallback where foreground
// detection is unavailable.
func (p *PaneActor) computeInteractive() bool {
	fg := foregroundPgrp(p.ptyFile)
	childForeground := fg > 0 && fg != p.shellPgid
	shellForeground := fg > 0 && fg == p.shellPgid
	if childForeground {
		p.interactivePgid = fg
	}
	if p.interactivePgid != 0 && !processGroupAlive(p.interactivePgid) {
		// The foreground program (and its whole process group) has exited.
		p.interactivePgid = 0
	}
	// Native pass-through (##native): the pane is interactive by definition —
	// the terminal itself is the UI, even with the shell at its prompt.
	if p.nativeMode {
		return true
	}
	// When the shell is definitively the foreground process group it is sitting
	// at its prompt, so the pane is NOT interactive — even if a just-exited
	// program left a lingering heuristic state. (codex exits with the cursor
	// still hidden; without this the 3s cursor-hidden debounce keeps asserting
	// raw mode and the bash PS1 shows for a few seconds before snapping back to
	// the rysh prompt.) Otherwise the pane is interactive while a program runs
	// (foreground child, or its process group still alive across a transient),
	// or per the VTerm heuristic when foreground detection is unavailable.
	return !shellForeground &&
		(p.rawMode || p.cursorHidden || childForeground || p.interactivePgid != 0)
}

// buildVTFrame builds the lightweight interactive VT frame (screen + cursor) for a
// raw/interactive pane WITHOUT touching the heavy output/history buffers or the
// full snapshot marshal. It is served per rawDirty signal so a redraw-heavy inline
// app (e.g. claude) refreshes one pane's VT screen cheaply — the common multi-pane
// cause of sluggish keystrokes — instead of pulling the whole pane snapshot on
// every frame. Interactive is false when the pane is not (or no longer) showing an
// interactive program; the TUI then leaves its last frame and lets the slower
// full-content reconcile catch the transition. Runs on the actor goroutine; the
// VTerm reads are mutex-guarded inside the VTerm wrapper.
func (p *PaneActor) buildVTFrame() *msg.MsgPaneVTReply {
	if !p.computeInteractive() || p.vtermEmu == nil {
		return &msg.MsgPaneVTReply{PaneID: p.id, Interactive: false}
	}
	row, col := p.vtermEmu.CursorPos()
	return &msg.MsgPaneVTReply{
		PaneID:      p.id,
		Interactive: true,
		Screen:      p.vtermEmu.RenderANSIWithCursor(),
		CursorRow:   row,
		CursorCol:   col,
	}
}

// History returns a copy of the command history for the given mode.
func (p *PaneActor) History(mode string) []string {
	switch mode {
	case "shell":
		out := make([]string, len(p.shellHistory))
		copy(out, p.shellHistory)
		return out
	case "prompt":
		out := make([]string, len(p.promptHistory))
		copy(out, p.promptHistory)
		return out
	case "rysh":
		out := make([]string, len(p.ryshHistory))
		copy(out, p.ryshHistory)
		return out
	case "chat":
		out := make([]string, len(p.chatHistory))
		copy(out, p.chatHistory)
		return out
	}
	return nil
}

// ---------------------------------------------------------------------------
// KV persistence
// ---------------------------------------------------------------------------

// maybePersist writes pane state to KV at most once per 2s. It builds the full
// (conversation-bearing) snapshot itself, and only when it actually writes, so
// the heavy conversation snapshotting happens ~once per 2s rather than on every
// snapshot request.
func (p *PaneActor) maybePersist() {
	if time.Since(p.lastKVWrite) < 2*time.Second {
		return
	}
	if p.kvDirty {
		p.persistNow(p.buildSnapshot(true, true))
	}
	if p.kvBuffersDirty {
		p.persistBuffers()
	}
}

func (p *PaneActor) persistNow(snap domain.PaneSnapshot) {
	if p.kvStore == nil {
		return
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return
	}
	if _, err := p.kvStore.Put(p.id, data); err != nil {
		return
	}
	p.kvDirty = false
	p.lastKVWrite = time.Now()
}

// persistBuffers writes the private buffer to its own KV key,
// separate from the pane snapshot.
func (p *PaneActor) persistBuffers() {
	if p.kvStore == nil {
		return
	}
	_, _ = p.kvStore.Put(p.id+".private", []byte(p.privateBuffer.String()))
	p.kvBuffersDirty = false
}

func (p *PaneActor) flushKV() {
	p.persistNow(p.buildSnapshot(true, true))
	p.persistBuffers()
}

// FlushKV forces an immediate KV write of the current state. Called externally
// (e.g. by WorkspaceActor during shutdown).
func (p *PaneActor) FlushKV() {
	p.flushKV()
}

// DeleteKV removes this pane's entry and its buffer entries from the KV store.
func (p *PaneActor) DeleteKV() {
	if p.kvStore == nil {
		return
	}
	_ = p.kvStore.Delete(p.id)
	_ = p.kvStore.Delete(p.id + ".private")
	_ = p.kvStore.Delete(p.id + ".public")
	_ = p.kvStore.Delete(p.id + ".llm_conversation")
}

// RestoreState loads previously persisted state including output, mode, status,
// lastCommand, command history, and private/public buffers.  The buffers are
// read from their own KV keys ({paneID}.private, {paneID}.public) which are
// separate from the pane snapshot.  Called before the actor is started when
// restoring a session.
func (p *PaneActor) RestoreState(snap domain.PaneSnapshot) {
	p.output.Set(snap.Output)
	p.mode = snap.Mode
	// Pane variant survives a restart: a restored replay pane must stay
	// shell-less. Only set when the snapshot carries one — normal panes keep
	// whatever the group assigned.
	if snap.PaneType != "" {
		p.paneType = snap.PaneType
	}
	// Backward-compat: pre-field snapshots have no EnabledModes — keep the
	// constructor default rather than disabling every mode.
	if len(snap.EnabledModes) > 0 {
		p.enabledModes = snap.EnabledModes
	}
	p.webURL = snap.WebURL
	p.webProfile = snap.WebProfile
	p.webTitle = snap.WebTitle
	p.webActivateSeq = snap.WebActivateSeq
	p.webStateAtomic.Store(webPaneState{Profile: snap.WebProfile, URL: snap.WebURL})
	p.status = snap.Status
	p.lastCommand = snap.LastCommand
	p.givenName = snap.GivenName
	// `##pane provider` override: only the name/model pair is recorded here.
	// The live provider is rebuilt in *actor.Started (installProviderOverride),
	// after the holder exists but before the executor spawns.
	p.providerOverride = snap.ProviderOverride
	p.providerOverrideModel = snap.ProviderOverrideModel
	p.mergedHistory = snap.MergedHistory
	p.shellHistory = snap.ShellHistory
	p.promptHistory = snap.PromptHistory
	p.ryshHistory = snap.RyshHistory
	p.chatHistory = snap.ChatHistory
	p.externalHistory = snap.ExternalHistory
	p.shellOutput.Set(snap.ShellOutput)
	p.aiOutput.Set(snap.AIOutput)
	p.ryshOutput.Set(snap.RyshOutput)
	p.chatOutput.Set(snap.ChatOutput)
	p.externalOutput.Set(snap.ExternalOutput)
	p.restoreModeOutputs(snap.ModeOutputs)
	// Reconstruct the humanoid-mode set from restored EnabledModes: any enabled
	// mode that is not a fixed mode is a dynamic per-humanoid mode.
	for _, mode := range p.enabledModes {
		if !validPaneModes[mode] {
			if p.humanoidModes == nil {
				p.humanoidModes = make(map[string]bool)
			}
			p.humanoidModes[mode] = true
		}
	}
	p.approvalPaneGroups = snap.ApprovalPaneGroups
	p.listeningToID = snap.ListeningToID
	p.registeredHumanoid = snap.RegisteredHumanoid
	p.approvalAttentionEnabled = snap.AttentionEnabled
	p.nativeMode = snap.NativeMode

	// Restore structured conversation buffers.
	p.restoreConversations(snap)

	// Restore share restrictions.
	if snap.ShareRestrictions != nil {
		p.shareRestrictions = msg.ShareRestrictions{
			DisabledModes:   snap.ShareRestrictions.DisabledModes,
			ShellAllowList:  snap.ShareRestrictions.ShellAllowList,
			ShellForbidList: snap.ShareRestrictions.ShellForbidList,
		}
	}

	// Restore private buffer from its own KV key.
	if p.kvStore != nil {
		if entry, err := p.kvStore.Get(p.id + ".private"); err == nil {
			p.privateBuffer.Set(string(entry.Value()))
		}
	}
}

// SetTitle updates the pane's display title (used externally from workspace).
func (p *PaneActor) SetTitle(title string) {
	p.title = title
	p.kvDirty = true
}

// GivenName returns the user-assigned given-name for this pane.
func (p *PaneActor) GivenName() string {
	return p.givenName
}
