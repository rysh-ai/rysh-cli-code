package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
)

func (m Model) View() string {
	header := m.renderHeader()
	body := m.renderBody()
	footer := m.renderFooter()
	return lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
}

// Powerline end-cap glyphs (require a Powerline/Nerd Font in the terminal).
// Each header element is "circumfixed" with a left + right filled cap coloured
// to match the element's background, yielding an isolated angled bubble:
//
//	 = left  filled triangle ()
//	 = right filled triangle ()
const (
	plLeftCap  = ""
	plRightCap = ""
)

func (m Model) renderHeader() string {
	// Two-row header rendered as Powerline bubbles: every workspace on the top
	// row, the active workspace's tabs on the second row. Workspaces use a
	// symmetric bubble (left  + right  caps, i.e. <ws>); tabs use a
	// right-pointing arrow (both caps , i.e. >tab>):
	//
	//   <ws-1> <ws-2> <ws-3>
	//   >drivingtest> >myagents> >klippers> >rysh-ai>
	//
	// The header stays exactly two rows tall, keeping the body height budget
	// unchanged.
	wsRow := m.renderWorkspaceStrip()
	tabRow := m.renderTabStrip()
	return lipgloss.NewStyle().
		Width(max(20, m.width)).
		Padding(0, 1, 0, 1).
		Render(wsRow + "\n" + tabRow)
}

// powerlineSeg circumfixes already-background-painted content with Powerline
// end-caps. A "normal" cap draws the glyph ink in the segment background colour
// against the default terminal background — perfect for a cap whose ink side
// touches the body (the right cap of every bubble, and the left  cap of the
// symmetric <…> workspace bubble).
//
// A reversed left cap is used for the >…> tab arrow: there the ink side of the
// glyph points away from the body, so drawing it normally leaves a terminal-
// coloured gap between the cap and the body. Reversing swaps fg/bg using the
// terminal's real default colours, so the cell background becomes the segment
// colour (fusing with the body) while the ink (the notch) blends into the gap.
func powerlineSeg(bg lipgloss.Color, leftCap, rightCap string, reverseLeft bool, content string) string {
	capStyle := lipgloss.NewStyle().Foreground(bg)
	leftStyle := capStyle
	if reverseLeft {
		leftStyle = capStyle.Reverse(true)
	}
	return leftStyle.Render(leftCap) + content + capStyle.Render(rightCap)
}

// renderTabStrip renders the active workspace's tabs as Powerline bubbles. The
// active tab is highlighted; tabs awaiting attention carry a red ●count marker.
func (m Model) renderTabStrip() string {
	if len(m.snapshot.Tabs) == 0 {
		body := lipgloss.NewStyle().Background(plTabInactiveBg).Foreground(plTabInactiveFg).
			Italic(true).Render(" no tabs — ctrl+t n ")
		return powerlineSeg(plTabInactiveBg, plRightCap, plRightCap, true, body)
	}
	segs := make([]string, 0, len(m.snapshot.Tabs))
	for _, tab := range m.snapshot.Tabs {
		title := strings.TrimSpace(tab.Title)
		if title == "" {
			title = "tab"
		}
		active := tab.ID == m.snapshot.ActiveTabID
		bg, fg := plTabInactiveBg, plTabInactiveFg
		if active {
			bg, fg = plTabActiveBg, plTabActiveFg
		}
		nameStyle := lipgloss.NewStyle().Background(bg).Foreground(fg).Bold(active)
		body := nameStyle.Render(" " + title + " ")
		if attn := m.tabAttentionCount(tab); attn > 0 && (active || m.attentionBlink) {
			attnSpan := lipgloss.NewStyle().Background(bg).Foreground(plAttnFg).Bold(true).
				Render(fmt.Sprintf("●%d ", attn))
			body = nameStyle.Render(" "+title+" ") + attnSpan
		}
		// Tabs are right-pointing arrows: both caps use plRightCap (>tab>). The
		// left cap is reversed so its background fuses with the tab body and the
		// notch blends into the terminal-coloured gap.
		segs = append(segs, powerlineSeg(bg, plRightCap, plRightCap, true, body))
	}
	return strings.Join(segs, " ")
}

// renderWorkspaceStrip renders every workspace as a Powerline bubble, with the
// active one highlighted. Driven entirely by the active workspace's snapshot
// (which carries the full workspace name list from the WorkspaceFarmActor).
func (m Model) renderWorkspaceStrip() string {
	names := m.snapshot.Workspaces
	if len(names) == 0 {
		name := m.cfg.SessionName
		if name == "" {
			name = "default"
		}
		names = []string{name}
	}
	segs := make([]string, 0, len(names))
	for i, name := range names {
		if strings.TrimSpace(name) == "" {
			name = fmt.Sprintf("ws-%d", i+1)
		}
		active := i == m.snapshot.ActiveWorkspace
		bg, fg := plWsInactiveBg, plWsInactiveFg
		if active {
			bg, fg = plWsActiveBg, plWsActiveFg
		}
		body := lipgloss.NewStyle().Background(bg).Foreground(fg).Bold(active).Render(" " + name + " ")
		// Workspaces are symmetric bubbles: left  + right  caps (<ws>); the
		// left  cap's ink already touches the body, so no reverse is needed.
		segs = append(segs, powerlineSeg(bg, plLeftCap, plRightCap, false, body))
	}
	return strings.Join(segs, " ")
}

func (m Model) renderBody() string {
	// The `##llm select` picker takes the whole body. See renderLLMPicker for
	// why it replaces the pane grid instead of floating over it.
	if m.mode == modeLLMPicker && m.llmPicker != nil {
		return m.renderLLMPicker(max(20, m.width), max(10, m.height-6))
	}
	tab := m.activeTab()
	if tab == nil {
		return panelStyle.Width(max(20, m.width)).Height(max(10, m.height-6)).Render("no active tab, create one with ctrl+t n")
	}

	// Fullscreen mode: render only the active (or pinned) pane at full size.
	if m.fullscreenPaneID != "" {
		for _, pane := range tab.FlatPanes() {
			if pane.ID == m.fullscreenPaneID {
				// Use the full terminal width minus outer padding (2) and borders (2).
				fsWidth := max(20, m.width-4)
				fsHeight := max(8, m.height-8)
				style := activePaneStyle.BorderForeground(m.activePaneBorderColor()).Width(fsWidth).Height(fsHeight)
				panel := m.buildPanePanel(pane, *tab, fsWidth, fsHeight)
				title := m.paneBorderTitle(pane)
				if pane.Sharing {
					title = title + " [SHARED]"
				}
				if pane.SnatEnabled {
					title = title + " [SNAT]"
				}
				if off := m.scrollOffset(pane.ID); off > 0 {
					title = fmt.Sprintf("%s ↑%d", title, off)
				}
				// Show the lane name if this is the first pane of its lane.
				rightLabel := ""
				fsLanes := tab.FlatLanes()
				for _, fl := range fsLanes {
					if len(fl.VisiblePanes) > 0 && fl.VisiblePanes[0].ID == pane.ID {
						rightLabel = fl.Name
						if rightLabel == "" {
							rightLabel = tab.PipelineName
						}
						if rightLabel == "" {
							rightLabel = "no-pipeline"
						}
						break
					}
				}
				rendered := injectBorderTitle(style.Render(panel), title, true, m.activePaneBorderColor(), fsWidth, rightLabel)
				return lipgloss.NewStyle().Padding(0, 1).Render(rendered)
			}
		}
		// Fullscreen pane no longer exists — fall through to normal layout.
	}

	lanes := tab.FlatLanes()
	if len(lanes) == 0 {
		return panelStyle.Width(max(20, m.width)).Height(max(10, m.height-6)).Render("no panes")
	}

	colWidths := laneWidths(lanes, m.paneAvailWidth(len(lanes)))
	totalHeight := max(8, m.height-8)

	renderedCols := make([]string, len(lanes))
	for c, lane := range lanes {
		colWidth := colWidths[c]
		col := lane.VisiblePanes
		if len(col) == 0 {
			renderedCols[c] = ""
			continue
		}

		// Separate expanded (visible) and collapsed (stacked background) panes.
		// Collapsed panes each consume exactly 1 row for their title line.
		var expandedPanes []domain.PaneSnapshot
		var collapsedPanes []domain.PaneSnapshot
		for _, pane := range col {
			if pane.StackCollapsed {
				collapsedPanes = append(collapsedPanes, pane)
			} else {
				expandedPanes = append(expandedPanes, pane)
			}
		}

		collapsedRows := len(collapsedPanes) // 1 row each
		// Available content height: subtract 2 rows per extra expanded pane
		// (top+bottom borders) and 1 row per collapsed pane title.
		availH := totalHeight - 2*max(0, len(expandedPanes)-1) - collapsedRows
		heights := flexPaneHeights(expandedPanes, availH)

		renderedPanes := make([]string, 0, len(col))
		expandedIdx := 0
		for _, pane := range col {
			isActive := pane.ID == m.snapshot.ActivePaneID

			if pane.StackCollapsed {
				// Render collapsed pane as a single styled title line.
				renderedPanes = append(renderedPanes, m.renderCollapsedStackPane(pane, colWidth))
				continue
			}

			h := heights[expandedIdx]
			expandedIdx++

			style := paneStyle.Width(colWidth).Height(h)
			if isActive {
				style = activePaneStyle.BorderForeground(m.activePaneBorderColor()).Width(colWidth).Height(h)
			} else if attnInfo, hasAttn := m.attentionState[pane.ID]; hasAttn && attnInfo.Count > 0 && m.attentionBlink {
				attnColor := m.attentionBorderColor(attnInfo.Priority)
				style = panelStyle.BorderForeground(attnColor).Padding(0, 1).Width(colWidth).Height(h)
			}
			panel := m.buildPanePanel(pane, *tab, colWidth, h)
			title := m.paneBorderTitle(pane)
			if attnInfo, hasAttn := m.attentionState[pane.ID]; hasAttn && attnInfo.Count > 0 && m.attentionBlink {
				title = fmt.Sprintf("%s %s[%d]", title, m.attentionIcon(attnInfo.Category), attnInfo.Count)
			}
			if pane.Sharing {
				title = title + " [SHARED]"
			}
			if pane.SnatEnabled {
				title = title + " [SNAT]"
			}
			if off := m.scrollOffset(pane.ID); off > 0 {
				title = fmt.Sprintf("%s ↑%d", title, off)
			}
			// Show the lane name on the right side of the first expanded pane of
			// every lane. The lane name defaults to the tab's pipeline name when
			// the lane has no explicit name set.
			if expandedIdx == 1 {
				laneName := lane.Name
				if laneName == "" {
					laneName = tab.PipelineName
				}
				if laneName == "" {
					laneName = "no-pipeline"
				}
				renderedPanes = append(renderedPanes, injectBorderTitle(style.Render(panel), title, isActive, m.activePaneBorderColor(), colWidth, laneName))
			} else {
				renderedPanes = append(renderedPanes, injectBorderTitle(style.Render(panel), title, isActive, m.activePaneBorderColor(), colWidth))
			}
		}
		renderedCols[c] = lipgloss.JoinVertical(lipgloss.Left, renderedPanes...)
	}

	return lipgloss.NewStyle().Padding(0, 1).Render(lipgloss.JoinHorizontal(lipgloss.Top, renderedCols...))
}

func (m Model) keyHelp() string {
	if activeTab := m.activeTab(); activeTab != nil && activeTab.PipelineActive {
		return "mode: pipeline | esc×2→exit  ctrl+p p→exit  enter submit  arrows navigate"
	}
	if m.mode == modeRaw {
		if m.activePaneNative() {
			return "mode: native shell | esc esc→ai mode  ctrl+o prefix  ##native off to leave"
		}
		return "mode: raw (interactive) | esc esc→switch mode  ctrl+o prefix (escape hatch)"
	}
	if m.mode == modePrefix {
		return "mode: prefix (ctrl+o) | d detach  any-key cancel"
	}
	if m.mode == modeAltPPrefix {
		return "mode: alt+p prefix | f fullscreen-toggle  any-key cancel"
	}
	if m.mode == modeRenamePane {
		return "rename pane: type new title  enter confirm  esc cancel"
	}
	if m.mode == modeRenameTab {
		return "rename tab: type new title  enter confirm  esc cancel"
	}
	if m.mode == modeTab {
		return "mode: tab | alt+left/right tabs  shift+left/right move  1-9 jump  n new-tab  r rename-tab  esc|. exit"
	}
	if m.mode == modeWorkspace {
		return "mode: workspace | ←/→ or h/l prev/next  1-9 jump  esc|. exit"
	}
	if m.mode == modePane {
		return "mode: pane | n split-right  v split-down  s stack-pane  x close-pane  r rename-pane  p pipeline  y rysh  c chat  d detach  esc|. exit"
	}
	if m.mode == modeNavigate {
		return "mode: navigate | arrows/h/j/k/l traverse panes  esc|. exit  ctrl+space exit"
	}
	if m.mode == modeResize {
		return "mode: resize | h/l width  j/k height  arrows resize  esc|. pane-mode  ctrl+p normal"
	}
	if m.mode == modeStack {
		return "mode: stack | j/k navigate stack  1-9 jump to [n/N]  esc|. exit"
	}
	if m.mode == modeMovePane {
		return "mode: move-pane (ctrl+y) | up/k move-up  down/j move-down (reorders within stack, else the group)  any-key exit"
	}
	if m.mode == modeLayout {
		return "mode: layout | e equalize-all  h/= equalize-width  v equalize-height  arrows resize  m maximize  s swap-lane  esc|. exit"
	}
	if m.mode == modeEmail {
		// The email client owns its keys (see updateEmailMode); the footer reflects
		// them instead of the generic input-mode hint, and switches to the compose
		// sub-view's keys while a manual reply is being typed.
		if ev := m.emailViews[m.snapshot.ActivePaneID]; ev != nil && ev.answer == answerCompose {
			return "mode: email compose | type reply  ctrl+d send  esc cancel"
		}
		return "mode: email | j/k move  tab switch column  enter open  r answer  c compose  g refresh  esc back  ctrl+o prefix"
	}
	if m.mode == modeLLMPicker {
		return m.llmPickerFooter()
	}
	if m.mode == modeApproval {
		desc := ""
		if m.pendingApproval != nil {
			desc = m.pendingApproval.Description
		}
		return fmt.Sprintf("APPROVAL: %s | y approve  Y approve-always  n reject  N reject+reason  esc reject", desc)
	}
	if m.mode == modeRejectReason {
		return "type rejection reason, press enter to submit, esc to go back"
	}
	var inputModeHint string
	switch m.activeInputMode() {
	case "shell":
		inputModeHint = "input:shell(>)  esc×2→prompt"
	case "prompt":
		inputModeHint = "input:prompt(<)  esc×2→rysh"
	case "rysh":
		inputModeHint = "input:rysh(##)  esc×2→chat"
	case "chat":
		inputModeHint = "input:chat(@)  esc×2→external"
	case "external":
		inputModeHint = "input:external  esc×2→shell"
	default:
		inputModeHint = "input:shell(>)  esc×2→prompt"
	}
	inputModeHint += "  ctrl+p p→pipeline"
	fsHint := ""
	if m.fullscreenPaneID != "" {
		fsHint = "  [FULLSCREEN alt+p f to restore]"
	}
	stackHint := ""
	if m.activeTab() != nil {
		for _, lane := range m.activeTab().Lanes {
			for _, g := range lane.PaneGroups {
				if len(g.Panes) > 1 {
					stackHint = "  ctrl+s stack  ctrl+y move-pane"
					break
				}
			}
			if stackHint != "" {
				break
			}
		}
	}
	scrollHint := ""
	if m.scrollOffset(m.snapshot.ActivePaneID) > 0 {
		scrollHint = "  [SCROLLED ↑ End=bottom]"
	}
	voiceHint := ""
	if m.voice != nil {
		voiceHint = "  " + m.voiceHotkey + " voice"
	}
	wsHint := ""
	if len(m.snapshot.Workspaces) > 1 {
		wsHint = "  ctrl+w workspaces"
	}
	searchHint := ""
	if m.shellReadlineActive() {
		// Ctrl+R is the one Ctrl key the shell line reserves: reverse-i-search.
		if m.search.active {
			return "mode: reverse-i-search | type to search  ctrl+r older  enter run  esc edit  ctrl+g cancel"
		}
		searchHint = "  ctrl+r search"
		if m.voiceHotkey == "ctrl+r" {
			voiceHint = "" // ctrl+r is reverse-i-search here; voice keeps it elsewhere
		}
	}
	return "mode: normal | " + inputModeHint + "  ctrl+space navigate  [/] tabs  alt+←/→ tabs  ctrl+p pane-mgmt  ctrl+t tab-mode  ctrl+l layout-mode  alt+p f fullscreen  ctrl+n new-pane" + searchHint + "  enter submit" + stackHint + wsHint + fsHint + scrollHint + voiceHint
}

func (m Model) renderFooter() string {
	if m.mode == modeRenamePane || m.mode == modeRenameTab {
		// Show the rename input instead of the key-help line.
		return lipgloss.NewStyle().Padding(0, 1, 1, 1).Render(m.renameInput.View())
	}
	if m.mode == modeRejectReason {
		// Show the rejection reason input.
		return lipgloss.NewStyle().Padding(0, 1, 1, 1).Render(m.rejectInput.View())
	}
	help := metaStyle.Render(m.keyHelp())
	if m.flashMsg != "" && time.Now().Before(m.flashExpires) {
		flashStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("46")).Bold(true)
		help = flashStyle.Render(m.flashMsg) + "  " + help
	}
	if vs := m.voiceStatus(); vs != "" {
		help = vs + "  " + help
	}
	if ms := m.mcpStatus(); ms != "" {
		help = ms + "  " + help
	}
	if ss := m.spendStatus(); ss != "" {
		help = ss + "  " + help
	}
	return lipgloss.NewStyle().Padding(0, 1, 1, 1).Render(help)
}

var (
	// Powerline header palette. Each element is a filled "bubble": tabs use an
	// azure accent when active, workspaces a teal accent, and both recede into a
	// muted grey when inactive. End-caps are coloured to the bubble background by
	// powerlineSeg, so only these fg/bg pairs are needed here.
	plTabActiveBg   = lipgloss.Color("33")  // azure — active tab bubble
	plTabActiveFg   = lipgloss.Color("231") // bright text on the active tab
	plTabInactiveBg = lipgloss.Color("238") // muted grey — inactive tab bubbles
	plTabInactiveFg = lipgloss.Color("250") // light grey text on inactive tabs
	plWsActiveBg    = lipgloss.Color("30")  // teal — active workspace bubble
	plWsActiveFg    = lipgloss.Color("231") // bright text on the active workspace
	plWsInactiveBg  = lipgloss.Color("238") // muted grey — inactive workspace bubbles
	plWsInactiveFg  = lipgloss.Color("250") // light grey text on inactive workspaces
	plAttnFg        = lipgloss.Color("196") // red ●count attention marker

	panelStyle      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240"))
	paneStyle       = panelStyle.Padding(0, 1)
	activePaneStyle = panelStyle.
			BorderForeground(lipgloss.Color("44")).
			Padding(0, 1)
	metaStyle           = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	completionHintStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	completionSelStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Background(lipgloss.Color("62")).Bold(true)
	selectionStyle      = lipgloss.NewStyle().Reverse(true)
	promptAnchorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("75")).Bold(true) // cyan < anchor

	// Border colours — mid-tone accents that stay legible on both light and
	// dark terminal backgrounds. The pane title text itself does NOT set a
	// foreground colour; it inherits the terminal's default foreground so it
	// respects the user's terminal theme (hard-coding white made titles
	// invisible on light backgrounds). Active vs inactive is conveyed with
	// bold / faint relative to the terminal's own foreground.
	inactiveBorderColor = lipgloss.Color("240")
	activeBorderColor   = lipgloss.Color("44")
	// insertBorderColor tints the focused pane's border green while the pane
	// is in "insert mode" (keystrokes land in the pane). This gives the user
	// an at-a-glance signal that input is being captured, versus the default
	// cyan accent shown while in a navigation/command mode (where keys must be
	// dismissed with Esc before typing resumes).
	insertBorderColor = lipgloss.Color("42")
)

// insertMode reports whether the active pane is currently capturing text input
// directly — i.e. a keystroke is typed into the pane rather than triggering a
// navigation or command action. True in normal mode (the dual shell/prompt
// input line) and raw mode (keys forwarded to an interactive PTY program).
func (m Model) insertMode() bool {
	return m.mode == modeNormal || m.mode == modeRaw
}

// activePaneBorderColor returns the accent colour for the focused pane's
// border: green while in insert mode (input goes to the pane), otherwise the
// default cyan accent used during navigation/command modes.
func (m Model) activePaneBorderColor() lipgloss.Color {
	if m.insertMode() {
		return insertBorderColor
	}
	return activeBorderColor
}

// injectBorderTitle post-processes a lipgloss-rendered pane box and embeds
// the pane title into the top-left of the border line. An optional rightTitle
// is placed on the right side of the border (e.g. pipeline name).
// The paneWidth parameter, when > 0, constrains the border to the intended
// visible width (lipgloss may render wider lines than the target width).
func injectBorderTitle(rendered, title string, isActive bool, activeColor lipgloss.Color, paneWidth int, rightTitle ...string) string {
	// Locate the first line boundary.
	nlIdx := strings.Index(rendered, "\n")
	var topLine, rest string
	if nlIdx == -1 {
		topLine = rendered
		rest = ""
	} else {
		topLine = rendered[:nlIdx]
		rest = rendered[nlIdx:] // includes the leading \n
	}

	// Work on the plain (ANSI-stripped) top border line.
	plainTop := ansi.Strip(topLine)
	runes := []rune(plainTop)
	if len(runes) < 4 || runes[0] != '╭' {
		// Not a rounded border or too narrow — return unchanged.
		return rendered
	}

	totalWidth := ansi.StringWidth(plainTop)
	// Use paneWidth if provided and smaller than the rendered width.
	// lipgloss may render borders wider than the target column width;
	// we need the right title placed within the visible area.
	if paneWidth > 0 && paneWidth+2 < totalWidth {
		totalWidth = paneWidth + 2 // +2 for ╭ and ╮ border corners
	}
	inner := totalWidth - 2 // cols between the two corner characters

	// Choose colours. The border uses an explicit accent colour, but the
	// title text inherits the terminal's default foreground so it respects
	// the terminal's font colour on both light and dark backgrounds. The
	// active pane is emphasised with bold; inactive titles are dimmed with
	// faint — both relative to the terminal's own foreground.
	borderColor := inactiveBorderColor
	tSt := lipgloss.NewStyle().Faint(true)
	if isActive {
		borderColor = activeColor
		tSt = lipgloss.NewStyle().Bold(true)
	}
	bSt := lipgloss.NewStyle().Foreground(borderColor)

	// Title text with surrounding spaces.
	titlePart := " " + title + " "
	titleLen := ansi.StringWidth(titlePart)
	leadingDashes := 1                            // "─" between ╭ and the title
	remaining := inner - leadingDashes - titleLen // trailing dashes before ╮

	if remaining < 1 {
		// Title too long: truncate.
		maxTitleRunes := inner - leadingDashes - 2 // keep at least 1 trailing dash + space
		if maxTitleRunes < 1 {
			// No room for a title at all — leave border unchanged.
			return rendered
		}
		r := []rune(title)
		if len(r) > maxTitleRunes-2 {
			r = r[:maxTitleRunes-2]
		}
		titlePart = " " + string(r) + " "
		titleLen = ansi.StringWidth(titlePart)
		remaining = inner - leadingDashes - titleLen
		if remaining < 0 {
			remaining = 0
		}
	}

	// Compute right-side title if provided.
	rightPart := ""
	rightLen := 0
	if len(rightTitle) > 0 && rightTitle[0] != "" {
		rightPart = " " + rightTitle[0] + " "
		rightLen = ansi.StringWidth(rightPart)
	}

	// Right title uses a distinct colour (dimmer than left title).
	rightTitleColor := lipgloss.Color("249") // light gray, distinct from border
	if isActive {
		rightTitleColor = lipgloss.Color("117") // light blue for active pane
	}
	rSt := lipgloss.NewStyle().Foreground(rightTitleColor)

	var sb strings.Builder
	sb.WriteString(bSt.Render("╭" + strings.Repeat("─", leadingDashes)))
	sb.WriteString(tSt.Render(titlePart))

	if rightLen > 0 && remaining > rightLen+1 {
		// Enough room: dashes between left title and right title.
		middleDashes := remaining - rightLen
		sb.WriteString(bSt.Render(strings.Repeat("─", middleDashes)))
		sb.WriteString(rSt.Render(rightPart))
	} else if remaining > 0 {
		// No room for right title (or none given): just dashes.
		sb.WriteString(bSt.Render(strings.Repeat("─", remaining)))
	}
	sb.WriteString(bSt.Render("╮"))

	return sb.String() + rest
}

// autoNameWidth is the width to which the auto-generated name segment is first
// normalized in a pane border title: truncated to the first autoNameWidth
// characters when longer, right-padded with dots when shorter.
const autoNameWidth = 20

// autoNameDisplayWidth is the final width of the auto-name segment actually
// shown in the title: the first autoNameDisplayWidth characters of the
// dot-trailed autoNameWidth-wide name.
const autoNameDisplayWidth = 12

// modeNameWidth is the fixed display width of the mode-name segment in a pane
// border title. The mode name is always rendered as exactly this many
// characters: truncated when longer, right-padded with dots when shorter.
const modeNameWidth = 7

// fixedWidthDots returns s truncated to the first width runes, or right-padded
// with dots to width runes when shorter.
func fixedWidthDots(s string, width int) string {
	r := []rune(s)
	if len(r) > width {
		return string(r[:width])
	}
	return s + strings.Repeat(".", width-len(r))
}

// fixedWidthAutoName normalizes name to autoNameWidth runes (truncated when
// longer, dot-padded when shorter) and then returns the first
// autoNameDisplayWidth runes of that dot-trailed name.
func fixedWidthAutoName(name string) string {
	padded := []rune(fixedWidthDots(name, autoNameWidth))
	if len(padded) > autoNameDisplayWidth {
		return string(padded[:autoNameDisplayWidth])
	}
	return string(padded)
}

// numDigits returns the number of decimal digits in n (minimum 1, n assumed >= 0).
func numDigits(n int) int {
	if n <= 0 {
		return 1
	}
	d := 0
	for n > 0 {
		d++
		n /= 10
	}
	return d
}

// paneBorderTitle builds the border title string for a pane.
// Format for stacked panes: "[N/M] auto-generated-name | mode-name | given-name"
// Format for non-stacked:   "auto-generated-name | mode-name | given-name"
// If no given name is set, the given-name segment is omitted.
// The auto-generated name is always rendered as exactly autoNameWidth (20)
// characters and the mode name as exactly modeNameWidth (7) characters:
// truncated when longer, dot-padded when shorter.
// For stacked panes the index is right-aligned to the width of the count so the
// auto-name starts at the same column on every row: a single-digit index gets
// two spaces after "[N/M]", a two-digit index (matching a two-digit count) gets
// one space, etc.
func (m Model) paneBorderTitle(pane domain.PaneSnapshot) string {
	modeLabel := fixedWidthDots(m.paneModeTitleLabel(pane.ID), modeNameWidth)
	autoName := fixedWidthAutoName(pane.Title)
	var base string
	if pane.GivenName != "" {
		base = fmt.Sprintf("%s | %s | %s", autoName, modeLabel, pane.GivenName)
	} else {
		base = autoName + " | " + modeLabel
	}
	if pane.StackTotal > 1 {
		index := pane.StackPosition + 1
		gap := 1 + numDigits(pane.StackTotal) - numDigits(index)
		if gap < 1 {
			gap = 1
		}
		return fmt.Sprintf("[%d/%d]%s%s", index, pane.StackTotal, strings.Repeat(" ", gap), base)
	}
	return base
}

// renderCollapsedStackPane renders a collapsed (background) stacked pane as a
// single styled title line. This produces the Zellij-style stacked pane
// appearance where inactive panes show only their title bar.
func (m Model) renderCollapsedStackPane(pane domain.PaneSnapshot, colWidth int) string {
	label := fmt.Sprintf(" %s ", m.paneBorderTitle(pane))

	inner := max(4, colWidth)
	labelLen := ansi.StringWidth(label)

	// Truncate label if wider than available space.
	if labelLen > inner-4 {
		r := []rune(label)
		if len(r) > inner-6 {
			r = r[:inner-6]
		}
		label = string(r) + "… "
		labelLen = ansi.StringWidth(label)
	}

	leadingDashes := 2
	trailingDashes := inner - leadingDashes - labelLen
	if trailingDashes < 1 {
		trailingDashes = 1
	}

	borderColor := lipgloss.Color("238")
	titleColor := lipgloss.Color("245")
	dashStyle := lipgloss.NewStyle().Foreground(borderColor)
	titleStyle := lipgloss.NewStyle().Foreground(titleColor)

	return dashStyle.Render("──") + titleStyle.Render(label) + dashStyle.Render(strings.Repeat("─", trailingDashes))
}

// paneMetaText builds the meta/header line shown at the top of a pane's text
// area. It MUST stay identical between the on-screen render (buildPanePanel) and
// the selection coordinate-mapping (buildDisplayLines); any divergence shifts
// the columns/rows that mouse selection maps to.
func paneMetaText(pane domain.PaneSnapshot) string {
	metaText := fmt.Sprintf("%s | flex:%d", pane.Status, pane.Flex)
	if pane.SnatEnabled {
		metaText += " | snat:on"
	}
	if pane.Sharing {
		shortID := pane.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		metaText += fmt.Sprintf(" | shared:pane_id:%s", shortID)
	}
	if pane.ConnectedToPaneID != "" {
		shortID := pane.ConnectedToPaneID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}
		metaText += fmt.Sprintf(" | connected_to:pane_id:%s", shortID)
	}
	return metaText
}

// wrapRows splits an output string into logical lines and wraps each one to the
// given content width, returning the resulting on-screen display rows. Wrapping
// is ANSI-aware (colour escapes are preserved across wraps). A leading
// right-align sentinel (\x02) is kept on the first row of its logical line so
// the renderer can still right-align prompt echoes. Blank logical lines are
// preserved as a single blank row.
//
// This is the single source of truth for how many rows a pane's output occupies
// on screen; both the renderer and the scroll clamps measure rows through it so
// they can never disagree (which is what made wrapped output scroll/overflow
// incorrectly before).
// wrapCacheEntry memoizes the result of wrapRows for one pane.
type wrapCacheEntry struct {
	output string
	width  int
	rows   []string
}

// wrapRowsCached returns wrapRows(output, width) for a pane, reusing the
// previous result when neither the output nor the width changed since the last
// frame. wrapRows is O(output size) ANSI parsing; for an idle pane this turns a
// per-frame re-wrap of the full buffer into a cheap string comparison. The
// returned slice is never mutated by callers (they copy into styledLines), so
// sharing the cached slice is safe.
func (m Model) wrapRowsCached(paneID, output string, width int) []string {
	if m.paneWrapCache != nil {
		if e, ok := m.paneWrapCache[paneID]; ok && e.width == width && e.output == output {
			return e.rows
		}
	}
	rows := wrapRows(output, width)
	if m.paneWrapCache != nil {
		m.paneWrapCache[paneID] = wrapCacheEntry{output: output, width: width, rows: rows}
	}
	return rows
}

func wrapRows(output string, width int) []string {
	lines := strings.Split(output, "\n")
	if width <= 0 {
		return lines
	}
	rows := make([]string, 0, len(lines))
	for _, line := range lines {
		sentinel := ""
		body := line
		if strings.HasPrefix(line, "\x02") {
			sentinel = "\x02"
			body = line[len("\x02"):]
		}
		wrapped := strings.Split(ansi.Wrap(body, width, ""), "\n")
		for i, wl := range wrapped {
			if i == 0 {
				rows = append(rows, sentinel+wl)
			} else {
				rows = append(rows, wl)
			}
		}
	}
	return rows
}

// visibleRowWindow returns the slice of rows visible given a row budget
// (maxLines) and a scroll offset measured from the bottom (0 = tail). It mirrors
// the clamping in the scroll helpers so the rendered window and the scroll math
// always line up.
func visibleRowWindow(rows []string, maxLines, scrollOffset int) []string {
	if maxLines <= 0 || len(rows) <= maxLines {
		return rows
	}
	endIdx := len(rows) - scrollOffset
	if endIdx > len(rows) {
		endIdx = len(rows)
	}
	startIdx := endIdx - maxLines
	if startIdx < 0 {
		startIdx = 0
		endIdx = min(maxLines, len(rows))
	}
	return rows[startIdx:endIdx]
}

// buildDisplayLines constructs the plain-text lines that correspond exactly to
// what lipgloss renders on screen for a pane's text area.
func buildDisplayLines(pane domain.PaneSnapshot, tab domain.TabSnapshot, textWidth, height int, inputValue string, scrollOffset int, inputMode string) []string {
	metaText := paneMetaText(pane)

	// One selector for every render path (see paneModeOutput) — these lines must
	// match what the live view draws, or selection and copy would return text
	// the user never saw.
	output := paneModeOutput(pane, inputMode)
	// Wrap to display rows first, then take the visible window, so scrollback is
	// measured in on-screen rows and wrapped lines cannot overflow the pane.
	maxLines := height - 4
	if maxLines < 0 {
		maxLines = 0
	}
	outputLines := visibleRowWindow(wrapRows(output, textWidth), maxLines, scrollOffset)

	// Build raw content lines with ANSI and right-align sentinel (\x02) stripped.
	rawLines := []string{metaText, ""}
	for _, line := range outputLines {
		line = strings.TrimPrefix(line, "\x02")
		rawLines = append(rawLines, ansi.Strip(line))
	}
	rawLines = append(rawLines, "", "> "+inputValue)

	if textWidth <= 0 {
		return rawLines
	}

	var displayLines []string
	for _, line := range rawLines {
		wrapped := ansi.Wrap(line, textWidth, "")
		for _, wl := range strings.Split(wrapped, "\n") {
			visW := ansi.StringWidth(wl)
			if visW < textWidth {
				wl += strings.Repeat(" ", textWidth-visW)
			} else if visW > textWidth {
				wl = ansi.Truncate(wl, textWidth, "")
			}
			displayLines = append(displayLines, wl)
		}
	}

	blankLine := strings.Repeat(" ", textWidth)
	for len(displayLines) < height {
		displayLines = append(displayLines, blankLine)
	}

	return displayLines
}

// buildPanePanel builds the displayable content string for a single pane.
// stampEdgeCursor renders a remote VT line truncated to contentCols but reserves
// the last column for a reverse-video cursor marker. Used when an interactive
// app's drawn cursor sits beyond the (narrower) subscriber width, where a plain
// ansi.Truncate would drop the cursor cell entirely. The marker is clamped to the
// right edge so the cursor stays visible even though its true column is off-screen.
func stampEdgeCursor(line string, contentCols int) string {
	if contentCols <= 1 {
		return "\x1b[7m \x1b[0m"
	}
	base := ansi.Truncate(line, contentCols-1, "")
	pad := contentCols - 1 - ansi.StringWidth(base)
	if pad < 0 {
		pad = 0
	}
	return base + strings.Repeat(" ", pad) + "\x1b[7m \x1b[0m"
}

func (m Model) buildPanePanel(pane domain.PaneSnapshot, tab domain.TabSnapshot, paneWidth, height int) string {
	metaText := paneMetaText(pane)

	renderWidth := max(8, paneWidth-4)

	// Scrollback (copy) mode: render the frozen history window for the active
	// interactive pane instead of its live VT screen. Rows are truncated to the
	// content width (ANSI-aware) in case the pane was resized since capture.
	if pane.ID == m.snapshot.ActivePaneID && m.mode == modeRawScroll && m.rawScrollPaneID == pane.ID && len(m.rawScrollRows) > 0 {
		maxLines := height - 1
		if maxLines < 0 {
			maxLines = 0
		}
		rows := m.rawScrollRows
		end := len(rows) - m.rawScrollOffset
		if end > len(rows) {
			end = len(rows)
		}
		if end < 0 {
			end = 0
		}
		start := end - maxLines
		if start < 0 {
			start = 0
		}
		win := make([]string, 0, maxLines)
		for _, r := range rows[start:end] {
			win = append(win, ansi.Truncate(r, renderWidth, ""))
		}
		for len(win) < maxLines {
			win = append(win, "")
		}
		title := fmt.Sprintf("%s | SCROLLBACK ↑%d/%d  j/k PgUp/PgDn Home/End  q=exit",
			paneMetaText(pane), m.rawScrollOffset, m.rawScrollMaxOffset())
		return fmt.Sprintf("%s\n%s", metaStyle.Render(title), strings.Join(win, "\n"))
	}

	// When pane is in raw (interactive) mode, render the VT screen directly.
	// Raw mode has no input box or blank separators — only the meta line is
	// overhead. Use height-1 to match the PTY sizing in sendPaneResizes().
	// VT screen lines already contain ANSI colour/attribute escape sequences
	// from VTerm.RenderANSI(), so no rune-based truncation is applied (it
	// would corrupt escape sequences). Width is correct because the VT
	// emulator is sized to match the pane's column count.
	if pane.RawMode && len(pane.VTScreen) > 0 && m.paneShowsLiveApp(&pane) {
		maxLines := height - 1 // only meta line overhead
		if maxLines < 0 {
			maxLines = 0
		}
		vtLines := pane.VTScreen
		if len(vtLines) > maxLines {
			vtLines = vtLines[:maxLines]
		}
		for len(vtLines) < maxLines {
			vtLines = append(vtLines, "")
		}

		content := strings.Join(vtLines, "\n")

		return fmt.Sprintf("%s\n%s",
			metaStyle.Render(metaText),
			content)
	}

	// When pane is displaying a remote interactive share, render the remote VT screen.
	// The remote VTerm is sized to the SOURCE pane's dimensions, which may differ
	// from the subscriber's pane width. Truncate each line to the subscriber's
	// content width (paneWidth-4, matching local PTY column sizing) using
	// ansi.Truncate to correctly handle ANSI escape sequences.
	if pane.RemoteInteractive && len(pane.RemoteVTScreen) > 0 {
		maxLines := height - 1
		if maxLines < 0 {
			maxLines = 0
		}
		contentCols := max(1, paneWidth-4)
		vtLines := pane.RemoteVTScreen
		if len(vtLines) > maxLines {
			vtLines = vtLines[:maxLines]
		}
		truncated := make([]string, 0, maxLines)
		for i, vl := range vtLines {
			line := ansi.Truncate(vl, contentCols, "")
			// Interactive apps (e.g. Claude Code) hide the real cursor and draw
			// their own as a reverse-video content cell. The source pane can be
			// wider than this (narrower) subscriber pane, so that cursor cell may
			// sit at/beyond contentCols, where ansi.Truncate drops it — leaving the
			// shared pane with no visible cursor. Re-stamp a reverse-video marker
			// clamped to the cursor row's right edge so the cursor stays visible.
			// Cursors within the visible width survive truncation untouched.
			if i == pane.RemoteVTCursorRow && pane.RemoteVTCursorCol >= contentCols {
				line = stampEdgeCursor(vl, contentCols)
			}
			truncated = append(truncated, line)
		}
		for len(truncated) < maxLines {
			truncated = append(truncated, "")
		}
		content := strings.Join(truncated, "\n")

		return fmt.Sprintf("%s\n%s",
			metaStyle.Render(metaText),
			content)
	}

	// Email client mode: a dedicated three-column inbox/reader/answer surface,
	// composed and sized to the pane (like the raw/remote branches above) rather
	// than the generic single-buffer render below.
	if m.paneInputModeFor(pane.ID) == "email" {
		return m.buildEmailPanel(pane, paneWidth, height)
	}

	// Switch output based on THIS pane's own input mode (not the active pane's),
	// so a chat/prompt/rysh pane keeps showing its own buffer when another pane
	// is focused instead of reverting to the merged shell/AI Output.
	//
	// This used to be an inline switch that handled neither "web" nor dynamic
	// per-humanoid modes, so those panes fell through to the merged shell buffer
	// — a web pane in a terminal rendered shell output while claiming to be a
	// browser. It now shares the one selector with the scroll math and the
	// copy-line builder, which is where the "this surface is not painted here"
	// placeholders live.
	output := paneModeOutput(pane, m.paneInputModeFor(pane.ID))

	// In pipeline mode, replace output only for the active pane.
	if tab.PipelineActive && pane.ID == m.snapshot.ActivePaneID {
		if liveOutput, ok := m.pipelineOutputs[tab.ID]; ok && liveOutput != "" {
			output = liveOutput
		} else if tab.PipelineOutput != "" {
			output = tab.PipelineOutput
		}
	}

	// Shell tab-completion candidates render inside the active pane, just above
	// the prompt (shell-style), and consume content rows from the budget.
	var completionLines []string
	if pane.ID == m.snapshot.ActivePaneID && m.completion.active && m.completion.paneID == pane.ID {
		completionLines = m.completionHintLines(renderWidth)
	}

	// Wrap output into on-screen rows (ANSI-aware) at the pane's content width so
	// the rendered slice never exceeds the row budget — long lines wrap instead
	// of overflowing the pane — and the scroll offset lands on the rows the user
	// actually sees. The content width matches paneRect.w (border + padding
	// excluded), which is what mouse hit-testing and selection use.
	contentWidth := max(1, paneWidth-2)
	outputLines := m.wrapRowsCached(pane.ID, output, contentWidth)
	maxLines := height - 4 - len(completionLines)
	if maxLines < 0 {
		maxLines = 0
	}
	outputLines = visibleRowWindow(outputLines, maxLines, m.scrollOffset(pane.ID))

	hasSelection := m.selection.active && m.selection.paneID == pane.ID

	if !hasSelection {
		// Render output lines, right-aligning any lines prefixed with \x02 (STX).
		styledLines := make([]string, len(outputLines))
		for i, line := range outputLines {
			if strings.HasPrefix(line, "\x02") {
				text := strings.TrimPrefix(line, "\x02")
				styledLines[i] = lipgloss.NewStyle().
					Width(renderWidth).
					Align(lipgloss.Right).
					Foreground(lipgloss.Color("75")).
					Bold(true).
					Render(text)
			} else {
				styledLines[i] = line
			}
		}
		content := strings.Join(styledLines, "\n")
		if len(completionLines) > 0 {
			content = content + "\n" + strings.Join(completionLines, "\n")
		}
		prompt := m.paneInputView(pane.ID, max(12, paneWidth-4))
		return fmt.Sprintf("%s\n\n%s\n\n%s",
			metaStyle.Render(metaText),
			content,
			prompt)
	}

	// --- Selection-aware rendering ---
	textWidth := max(1, paneWidth-2)
	paneMode := m.paneInputModeFor(pane.ID)
	displayLines := buildDisplayLines(pane, tab, textWidth, height, m.inputValueFor(pane.ID), m.scrollOffset(pane.ID), paneMode)

	startRow, startCol, endRow, endCol := m.selection.normalized()

	styledLines := make([]string, len(displayLines))
	for i, line := range displayLines {
		if i >= startRow && i <= endRow {
			styledLines[i] = applyLineSelection(line, i, startRow, startCol, endRow, endCol)
		} else {
			styledLines[i] = line
		}
	}

	return strings.Join(styledLines, "\n")
}

// applyLineSelection highlights the selected portion of a single line.
func applyLineSelection(line string, lineIdx, startRow, startCol, endRow, endCol int) string {
	runes := []rune(line)
	lineLen := len(runes)

	var selStart, selEnd int
	switch {
	case lineIdx == startRow && lineIdx == endRow:
		selStart = startCol
		selEnd = endCol + 1
	case lineIdx == startRow:
		selStart = startCol
		selEnd = lineLen
	case lineIdx == endRow:
		selStart = 0
		selEnd = endCol + 1
	default:
		selStart = 0
		selEnd = lineLen
	}

	selStart = max(0, min(selStart, lineLen))
	selEnd = max(selStart, min(selEnd, lineLen))

	if lineLen == 0 {
		return selectionStyle.Render(" ")
	}
	if selStart >= selEnd {
		return line
	}

	before := string(runes[:selStart])
	selected := string(runes[selStart:selEnd])
	after := string(runes[selEnd:])

	return before + selectionStyle.Render(selected) + after
}
