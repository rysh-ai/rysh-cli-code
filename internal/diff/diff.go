package diff

import (
	"fmt"
	"strings"
)

// Generate produces a unified diff string between oldContent and newContent.
// It uses a simple LCS-based diff algorithm with 3 lines of context.
func Generate(oldContent, newContent, filePath string) string {
	oldLines := splitLines(oldContent)
	newLines := splitLines(newContent)

	edits := computeEdits(oldLines, newLines)
	hunks := groupIntoHunks(edits, len(oldLines), len(newLines), 3)

	if len(hunks) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("--- a/%s\n", filePath))
	sb.WriteString(fmt.Sprintf("+++ b/%s\n", filePath))

	for _, h := range hunks {
		sb.WriteString(formatHunk(h, oldLines, newLines))
	}

	return sb.String()
}

// GenerateForNewFile generates a diff showing the entire file as added.
func GenerateForNewFile(content, filePath string) string {
	lines := splitLines(content)
	if len(lines) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("--- /dev/null\n"))
	sb.WriteString(fmt.Sprintf("+++ b/%s\n", filePath))
	sb.WriteString(fmt.Sprintf("@@ -0,0 +1,%d @@\n", len(lines)))

	for _, line := range lines {
		sb.WriteString("+" + line + "\n")
	}

	return sb.String()
}

// GenerateForDelete generates a diff showing the entire file as removed.
func GenerateForDelete(content, filePath string) string {
	lines := splitLines(content)
	if len(lines) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("--- a/%s\n", filePath))
	sb.WriteString(fmt.Sprintf("+++ /dev/null\n"))
	sb.WriteString(fmt.Sprintf("@@ -1,%d +0,0 @@\n", len(lines)))

	for _, line := range lines {
		sb.WriteString("-" + line + "\n")
	}

	return sb.String()
}

// editKind represents the type of edit operation.
type editKind int

const (
	editEqual  editKind = iota
	editDelete editKind = iota
	editInsert editKind = iota
)

// edit represents a single line edit operation.
type edit struct {
	kind   editKind
	oldIdx int // 0-based index in old content
	newIdx int // 0-based index in new content
}

// hunk represents a contiguous region of changes with surrounding context.
type hunk struct {
	oldStart int // 1-based start line in old file
	oldCount int
	newStart int // 1-based start line in new file
	newCount int
	edits    []edit
}

// splitLines splits content into lines, handling the empty string case.
func splitLines(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	// Remove trailing empty string from split if content ends with newline
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// computeEdits computes the edit sequence using LCS-based diff.
func computeEdits(oldLines, newLines []string) []edit {
	m := len(oldLines)
	n := len(newLines)

	// Compute LCS table
	lcs := make([][]int, m+1)
	for i := range lcs {
		lcs[i] = make([]int, n+1)
	}

	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if oldLines[i-1] == newLines[j-1] {
				lcs[i][j] = lcs[i-1][j-1] + 1
			} else if lcs[i-1][j] >= lcs[i][j-1] {
				lcs[i][j] = lcs[i-1][j]
			} else {
				lcs[i][j] = lcs[i][j-1]
			}
		}
	}

	// Backtrack to produce edit sequence
	var edits []edit
	i, j := m, n
	for i > 0 || j > 0 {
		if i > 0 && j > 0 && oldLines[i-1] == newLines[j-1] {
			edits = append(edits, edit{kind: editEqual, oldIdx: i - 1, newIdx: j - 1})
			i--
			j--
		} else if j > 0 && (i == 0 || lcs[i][j-1] >= lcs[i-1][j]) {
			edits = append(edits, edit{kind: editInsert, newIdx: j - 1})
			j--
		} else {
			edits = append(edits, edit{kind: editDelete, oldIdx: i - 1})
			i--
		}
	}

	// Reverse to get forward order
	for left, right := 0, len(edits)-1; left < right; left, right = left+1, right-1 {
		edits[left], edits[right] = edits[right], edits[left]
	}

	return edits
}

// groupIntoHunks groups edits into hunks with the specified context lines.
func groupIntoHunks(edits []edit, oldLen, newLen, contextLines int) []hunk {
	// Find change regions (indices into edits slice where changes occur)
	type changeRegion struct {
		start, end int // indices into edits
	}

	var regions []changeRegion
	inChange := false
	var currentStart int

	for i, e := range edits {
		if e.kind != editEqual {
			if !inChange {
				inChange = true
				currentStart = i
			}
		} else {
			if inChange {
				regions = append(regions, changeRegion{currentStart, i})
				inChange = false
			}
		}
	}
	if inChange {
		regions = append(regions, changeRegion{currentStart, len(edits)})
	}

	if len(regions) == 0 {
		return nil
	}

	// Merge regions that are close enough (within 2*contextLines)
	type mergedRegion struct {
		start, end int
	}
	var merged []mergedRegion
	current := mergedRegion{regions[0].start, regions[0].end}

	for i := 1; i < len(regions); i++ {
		gap := regions[i].start - current.end
		if gap <= 2*contextLines {
			current.end = regions[i].end
		} else {
			merged = append(merged, current)
			current = mergedRegion{regions[i].start, regions[i].end}
		}
	}
	merged = append(merged, current)

	// Build hunks with context
	var hunks []hunk
	for _, mr := range merged {
		// Expand to include context
		hunkStart := mr.start - contextLines
		if hunkStart < 0 {
			hunkStart = 0
		}
		hunkEnd := mr.end + contextLines
		if hunkEnd > len(edits) {
			hunkEnd = len(edits)
		}

		hunkEdits := edits[hunkStart:hunkEnd]

		// Calculate old/new start and count
		var oldStart, oldCount, newStart, newCount int
		firstSet := false
		for _, e := range hunkEdits {
			if !firstSet {
				switch e.kind {
				case editEqual:
					oldStart = e.oldIdx + 1
					newStart = e.newIdx + 1
				case editDelete:
					oldStart = e.oldIdx + 1
					newStart = e.newIdx + 1
					// For delete, newIdx isn't set meaningfully; compute from context
				case editInsert:
					oldStart = e.oldIdx + 1
					newStart = e.newIdx + 1
				}
				firstSet = true
			}
			switch e.kind {
			case editEqual:
				oldCount++
				newCount++
			case editDelete:
				oldCount++
			case editInsert:
				newCount++
			}
		}

		// Fix start positions: find first valid old/new index
		oldStart = 0
		newStart = 0
		for _, e := range hunkEdits {
			if e.kind == editEqual || e.kind == editDelete {
				oldStart = e.oldIdx + 1
				break
			}
			if e.kind == editInsert {
				// newIdx tells us position, derive oldStart from surrounding context
				continue
			}
		}
		for _, e := range hunkEdits {
			if e.kind == editEqual || e.kind == editInsert {
				newStart = e.newIdx + 1
				break
			}
		}

		// If we still haven't found starts, use 1
		if oldStart == 0 {
			oldStart = 1
		}
		if newStart == 0 {
			newStart = 1
		}

		hunks = append(hunks, hunk{
			oldStart: oldStart,
			oldCount: oldCount,
			newStart: newStart,
			newCount: newCount,
			edits:    hunkEdits,
		})
	}

	return hunks
}

// formatHunk formats a single hunk as unified diff text.
func formatHunk(h hunk, oldLines, newLines []string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", h.oldStart, h.oldCount, h.newStart, h.newCount))

	for _, e := range h.edits {
		switch e.kind {
		case editEqual:
			sb.WriteString(" " + oldLines[e.oldIdx] + "\n")
		case editDelete:
			sb.WriteString("-" + oldLines[e.oldIdx] + "\n")
		case editInsert:
			sb.WriteString("+" + newLines[e.newIdx] + "\n")
		}
	}

	return sb.String()
}
