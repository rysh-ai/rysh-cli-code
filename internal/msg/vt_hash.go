// SPDX-License-Identifier: Apache-2.0

package msg

import (
	"hash/fnv"
	"sort"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

// VTRowHash is the FNV-1a hash of one rendered VT screen row. The TUI sends
// these in MsgGetPaneVT.RowHashes and the PaneActor compares them against its
// own rows, so both sides must use this one function.
func VTRowHash(row string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(row))
	return h.Sum64()
}

// VTRowHashes returns VTRowHash for every row of screen.
func VTRowHashes(screen []string) []uint64 {
	out := make([]uint64, len(screen))
	for i, row := range screen {
		out[i] = VTRowHash(row)
	}
	return out
}

// VTRowsHash folds a screen's row hashes into one value identifying the whole
// screen (row count included), echoed as MsgPaneVTReply.BaseHash.
func VTRowsHash(rowHashes []uint64) uint64 {
	h := fnv.New64a()
	var b [8]byte
	for _, v := range rowHashes {
		for i := 0; i < 8; i++ {
			b[i] = byte(v >> (8 * i))
		}
		_, _ = h.Write(b[:])
	}
	_, _ = h.Write([]byte{byte(len(rowHashes)), byte(len(rowHashes) >> 8)})
	return h.Sum64()
}

// VTRowDelta returns the rows of screen that differ from rowHashes: every row
// whose hash differs, plus every row beyond len(rowHashes).
func VTRowDelta(screen []string, rowHashes []uint64) []VTLineDelta {
	var changed []VTLineDelta
	for y, row := range screen {
		if y < len(rowHashes) && rowHashes[y] == VTRowHash(row) {
			continue
		}
		changed = append(changed, VTLineDelta{Y: y, S: row})
	}
	return changed
}

// PaneContentHash is an FNV-1a hash of the display-relevant fields of a pane
// snapshot. Two snapshots with the same hash render identically. The TUI keeps
// it per pane and sends it as MsgGetPaneSnapshot.IfContentHash; the PaneActor
// answers NotModified when its snapshot hashes the same.
func PaneContentHash(s domain.PaneSnapshot) uint64 {
	h := fnv.New64a()
	writeStr := func(str string) {
		_, _ = h.Write([]byte(str))
		_, _ = h.Write([]byte{0}) // field separator so a||b != ab
	}
	writeStr(s.Output)
	writeStr(s.AIOutput)
	writeStr(s.ChatOutput)
	writeStr(s.RyshOutput)
	writeStr(s.ExternalOutput)
	// Dynamic per-humanoid modes (snapshot-only render) — hash in sorted-key
	// order so a new channel message in a humanoid's mode triggers a re-render.
	if len(s.ModeOutputs) > 0 {
		modes := make([]string, 0, len(s.ModeOutputs))
		for k := range s.ModeOutputs {
			modes = append(modes, k)
		}
		sort.Strings(modes)
		for _, k := range modes {
			writeStr(k)
			writeStr(s.ModeOutputs[k])
		}
	}
	for _, line := range s.VTScreen {
		writeStr(line)
	}
	for _, line := range s.RemoteVTScreen {
		writeStr(line)
	}
	var cur [6]byte
	mix := func(off, v int) {
		cur[off] = byte(v)
		cur[off+1] = byte(v >> 8)
	}
	mix(0, s.VTCursorRow)
	mix(2, s.VTCursorCol)
	// Fold remote cursor in via a second write so it contributes too.
	_, _ = h.Write(cur[:])
	mix(0, s.RemoteVTCursorRow)
	mix(2, s.RemoteVTCursorCol)
	_, _ = h.Write(cur[:4])
	return h.Sum64()
}
