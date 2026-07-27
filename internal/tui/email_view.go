package tui

// The terminal email client — a three-column, Gmail-style inbox rendered inside a
// pane that is in "email" content mode (enabled + activated by a humanoid with an
// email channel when it registers its output pane; see actors.updateOutputRouting).
//
// Data flows over the SAME structured NATS request/reply the desktop app uses
// (internal/msg/email_messages.go): the TUI publishes MsgHumanoidEmailList /
// MsgHumanoidEmailRead / MsgHumanoidSetFocus / MsgHumanoidEmailCompose to the
// humanoid's inbox and consumes the typed replies pushed on
// humanoid.<name>.email.{list,detail,compose,changed}. Nothing is re-parsed from
// tool text. The AI-answer path reuses the humanoid prompt loop (its streamed
// draft is mirrored into the pane's external buffer, shown in the answer dock);
// manual compose sends straight over SMTP via MsgHumanoidEmailCompose.

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// emailAnswerMode is the active answer sub-view within the email client.
type emailAnswerMode int

const (
	answerNone    emailAnswerMode = iota // reading only
	answerAI                             // humanoid is drafting a reply (streamed into the dock)
	answerCompose                        // user is typing their own reply
)

// emailFocusCol identifies the focused column (Tab cycles them).
const (
	emailColFolders = 0
	emailColList    = 1
	emailColReader  = 2
)

// emailViewState is the per-pane state of the email client. Held in
// Model.emailViews keyed by pane ID.
type emailViewState struct {
	humanoid     string                // the email humanoid feeding this pane
	list         []msgpkg.EmailSummary // inbox listing (newest first)
	listErr      string                // last list error, if any
	requested    bool                  // a list request has been fired at least once
	selected     int                   // index into list
	focusCol     int                   // emailCol{Folders,List,Reader}
	detail       *msgpkg.EmailDetail   // the opened email (reader column)
	detailErr    string                // last read error, if any
	readerScroll int                   // body scroll offset (lines)
	answer       emailAnswerMode       // answer dock state
	compose      string                // manual-compose body being typed
	status       string                // transient status line (send outcome, hints)
}

// ---- Model wiring ---------------------------------------------------------

// emailEventKind tags a decoded humanoid→UI email push.
type emailEventKind int

const (
	emailEvtList emailEventKind = iota
	emailEvtDetail
	emailEvtChanged
	emailEvtCompose
)

// emailEvent is one decoded email push routed from NATS to the Model.
type emailEvent struct {
	kind     emailEventKind
	humanoid string
	emails   []msgpkg.EmailSummary
	detail   *msgpkg.EmailDetail
	ok       bool
	err      string
}

// emailEventMsg / activateModeMsg are the tea.Msg forms delivered to Update.
type emailEventMsg struct{ ev emailEvent }
type activateModeItem struct {
	paneID string
	mode   string
}
type activateModeMsg struct{ item activateModeItem }

// setupEmailSubscriptions wires the structured email push subjects and the
// activate-mode push into buffered channels, mirroring setupContentSubscriptions.
// Drop-on-full is safe: a missed list/detail is re-fetched on the next user
// action or MsgHumanoidEmailChanged; a missed activate is corrected by the next
// layout refresh.
func setupEmailSubscriptions(b *bus.Bus) (emailCh chan emailEvent, activateCh chan activateModeItem) {
	emailCh = make(chan emailEvent, 256)
	activateCh = make(chan activateModeItem, 64)
	codecs := b.Codecs()
	conn := b.Conn()

	decode := func(data []byte) (interface{}, bool) {
		var env msgpkg.NATSEnvelope
		if json.Unmarshal(data, &env) != nil {
			return nil, false
		}
		d, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return nil, false
		}
		return d, true
	}
	push := func(ev emailEvent) {
		select {
		case emailCh <- ev:
		default:
		}
	}

	_, _ = conn.Subscribe(msgpkg.T("humanoid", "*", "email", "list"), func(n *nats.Msg) {
		if d, ok := decode(n.Data); ok {
			if r, ok := d.(*msgpkg.MsgHumanoidEmailListReply); ok {
				push(emailEvent{kind: emailEvtList, humanoid: r.HumanoidName, emails: r.Emails, err: r.Err})
			}
		}
	})
	_, _ = conn.Subscribe(msgpkg.T("humanoid", "*", "email", "detail"), func(n *nats.Msg) {
		if d, ok := decode(n.Data); ok {
			if r, ok := d.(*msgpkg.MsgHumanoidEmailReadReply); ok {
				push(emailEvent{kind: emailEvtDetail, humanoid: r.HumanoidName, detail: r.Email, err: r.Err})
			}
		}
	})
	_, _ = conn.Subscribe(msgpkg.T("humanoid", "*", "email", "changed"), func(n *nats.Msg) {
		if d, ok := decode(n.Data); ok {
			if r, ok := d.(*msgpkg.MsgHumanoidEmailChanged); ok {
				push(emailEvent{kind: emailEvtChanged, humanoid: r.HumanoidName})
			}
		}
	})
	_, _ = conn.Subscribe(msgpkg.T("humanoid", "*", "email", "compose"), func(n *nats.Msg) {
		if d, ok := decode(n.Data); ok {
			if r, ok := d.(*msgpkg.MsgHumanoidEmailComposeReply); ok {
				push(emailEvent{kind: emailEvtCompose, humanoid: r.HumanoidName, ok: r.Ok, err: r.Err})
			}
		}
	})
	_, _ = conn.Subscribe(msgpkg.T("pane", "*", "activateMode"), func(n *nats.Msg) {
		var env msgpkg.NATSEnvelope
		if json.Unmarshal(n.Data, &env) != nil {
			return
		}
		d, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		if r, ok := d.(*msgpkg.MsgPaneActivateMode); ok {
			select {
			case activateCh <- activateModeItem{paneID: r.PaneID, mode: r.Mode}:
			default:
			}
		}
	})
	return emailCh, activateCh
}

// listenEmailEventCmd / listenActivateModeCmd block for the next event and yield
// it as a tea.Msg, re-armed by Update after each delivery.
func (m Model) listenEmailEventCmd() tea.Cmd {
	ch := m.emailEventCh
	return func() tea.Msg { return emailEventMsg{ev: <-ch} }
}
func (m Model) listenActivateModeCmd() tea.Cmd {
	ch := m.activateModeCh
	return func() tea.Msg { return activateModeMsg{item: <-ch} }
}

// emailViewFor returns (creating if needed) the email state for a pane, seeding
// the humanoid name from the snapshot's registered humanoid.
func (m *Model) emailViewFor(pane domain.PaneSnapshot) *emailViewState {
	ev := m.emailViews[pane.ID]
	if ev == nil {
		ev = &emailViewState{focusCol: emailColList, selected: 0}
		m.emailViews[pane.ID] = ev
	}
	if ev.humanoid == "" {
		ev.humanoid = pane.RegisteredHumanoid
	}
	return ev
}

// applyActivateMode handles a backend "switch this pane to <mode>" push: it flips
// the frontend's local active mode for the pane (only when the mode is enabled),
// so ##humanoid register-output surfaces the humanoid's view without the user
// cycling to it by hand. For "email", it kicks off the first inbox fetch.
func (m *Model) applyActivateMode(it activateModeItem) tea.Cmd {
	pane := m.findPaneInSnapshot(it.paneID)
	if pane == nil {
		return nil
	}
	if it.mode == "shell" || paneModeEnabled(*pane, it.mode) {
		m.paneInputModes[it.paneID] = it.mode
	} else {
		// The mode-enable push that updateOutputRouting sends alongside this
		// activate has not propagated into this snapshot yet, so the mode is not
		// selectable this frame. Remember it — syncPaneInputs flips the pane the
		// moment the mode reports enabled. Without this, register-output silently
		// no-ops (the original "nothing shows up" bug).
		if m.pendingActivate == nil {
			m.pendingActivate = make(map[string]string)
		}
		m.pendingActivate[it.paneID] = it.mode
	}
	// Kick the first inbox fetch whether the flip is immediate or deferred: the
	// humanoid stores the reply, so the list is populated by the time the pane
	// becomes visible.
	if it.mode == "email" {
		ev := m.emailViewFor(*pane)
		if !ev.requested {
			return m.emailListCmd(ev.humanoid)
		}
	}
	return nil
}

// applyEmailEvent folds a decoded humanoid→UI email push into the matching pane
// state(s). Events are matched by humanoid name (a pane owns exactly one email
// humanoid).
func (m *Model) applyEmailEvent(ev emailEvent) tea.Cmd {
	var cmd tea.Cmd
	for _, st := range m.emailViews {
		if st.humanoid != ev.humanoid {
			continue
		}
		switch ev.kind {
		case emailEvtList:
			st.list = ev.emails
			st.listErr = ev.err
			st.requested = true
			if st.selected >= len(st.list) {
				st.selected = max(0, len(st.list)-1)
			}
		case emailEvtDetail:
			st.detail = ev.detail
			st.detailErr = ev.err
			st.readerScroll = 0
			if ev.detail != nil {
				st.focusCol = emailColReader
			}
		case emailEvtChanged:
			// New mail — refresh the listing.
			cmd = m.emailListCmd(st.humanoid)
		case emailEvtCompose:
			if ev.ok {
				st.status = "sent ✓"
				st.answer = answerNone
				st.compose = ""
			} else {
				st.status = "send failed: " + ev.err
			}
		}
	}
	return cmd
}

// ---- Request commands (TUI → humanoid inbox) ------------------------------

func (m Model) emailListCmd(humanoid string) tea.Cmd {
	if humanoid == "" {
		return nil
	}
	pub := m.pub
	subject := msgpkg.T("humanoid", humanoid, "inbox")
	return func() tea.Msg {
		_ = pub.Send(subject, &msgpkg.MsgHumanoidEmailList{Count: 20})
		return nil
	}
}

func (m Model) emailReadCmd(humanoid string, uid int) tea.Cmd {
	if humanoid == "" {
		return nil
	}
	pub := m.pub
	subject := msgpkg.T("humanoid", humanoid, "inbox")
	return func() tea.Msg {
		_ = pub.Send(subject, &msgpkg.MsgHumanoidEmailRead{UID: uid})
		return nil
	}
}

// emailFocusCmd tells the humanoid which email is open, so its "reply to this"
// draft targets this message (mirrors the desktop email_focus wiring).
func (m Model) emailFocusCmd(humanoid string, d *msgpkg.EmailDetail) tea.Cmd {
	if humanoid == "" || d == nil {
		return nil
	}
	pub := m.pub
	subject := msgpkg.T("humanoid", humanoid, "inbox")
	focus := &msgpkg.MsgHumanoidSetFocus{
		Listing: false, UID: d.UID, MessageID: d.MessageID,
		ThreadID: d.MessageID, From: d.From, Subject: d.Subject, Body: d.Body,
	}
	return func() tea.Msg {
		_ = pub.Send(subject, focus)
		return nil
	}
}

// emailComposeCmd sends a human-authored reply straight over SMTP.
func (m Model) emailComposeCmd(humanoid, to, subject, body, inReplyTo string) tea.Cmd {
	if humanoid == "" {
		return nil
	}
	pub := m.pub
	inbox := msgpkg.T("humanoid", humanoid, "inbox")
	msgOut := &msgpkg.MsgHumanoidEmailCompose{To: to, Subject: subject, Body: body, InReplyTo: inReplyTo}
	return func() tea.Msg {
		_ = pub.Send(inbox, msgOut)
		return nil
	}
}

// emailSubmitCmd submits a prompt to the humanoid through the pane's email-mode
// input path (routed to the humanoid as an agentic prompt by
// PaneActor.handleSubmitInput). Used for the AI-draft ("Draft a reply…") and the
// "send" confirmation.
func (m Model) emailSubmitCmd(paneID, text string) tea.Cmd {
	pub := m.pub
	inbox := m.workspaceInbox
	out := &msgpkg.MsgSubmitInput{Text: text, Mode: "email", PaneID: paneID}
	return func() tea.Msg {
		_ = pub.Send(inbox, out)
		return nil
	}
}

// ---- Key handling (m.mode == modeEmail) -----------------------------------

// syncEmailMode enters modeEmail when the active pane is showing the email client
// and leaves it when it no longer is — the analogue of syncRawMode for the
// email surface. Called alongside syncRawMode on snapshot/layout ticks.
func (m *Model) syncEmailMode() {
	pane := m.findPaneInSnapshot(m.snapshot.ActivePaneID)
	inEmail := pane != nil && m.paneInputModeFor(pane.ID) == "email"
	if inEmail && m.mode == modeNormal {
		m.mode = modeEmail
		m.syncPaneInputFocus()
	} else if !inEmail && m.mode == modeEmail {
		m.mode = modeNormal
		m.syncPaneInputFocus()
	}
}

// updateEmailMode owns all keys while the active pane shows the email client, so
// navigation keys never leak into a shell input line.
func (m Model) updateEmailMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	pane := m.findPaneInSnapshot(m.snapshot.ActivePaneID)
	if pane == nil {
		m.mode = modeNormal
		return m, nil
	}
	ev := m.emailViewFor(*pane)

	// Compose sub-view captures text input verbatim.
	if ev.answer == answerCompose {
		return m.updateEmailCompose(pane.ID, ev, msg)
	}

	switch msg.String() {
	case "ctrl+o": // never trap the user — allow the global prefix chord out
		m.mode = modePrefix
		m.syncPaneInputFocus()
		return m, nil
	case "tab":
		ev.focusCol = (ev.focusCol + 1) % 3
		return m, nil
	case "shift+tab":
		ev.focusCol = (ev.focusCol + 2) % 3
		return m, nil
	case "j", "down":
		if ev.focusCol == emailColReader && ev.detail != nil {
			ev.readerScroll++
		} else if len(ev.list) > 0 {
			ev.selected = min(ev.selected+1, len(ev.list)-1)
		}
		return m, nil
	case "k", "up":
		if ev.focusCol == emailColReader && ev.detail != nil {
			ev.readerScroll = max(0, ev.readerScroll-1)
		} else if len(ev.list) > 0 {
			ev.selected = max(0, ev.selected-1)
		}
		return m, nil
	case "g":
		ev.status = "refreshing…"
		return m, m.emailListCmd(ev.humanoid)
	case "enter":
		if ev.answer == answerAI {
			ev.status = "sending…"
			ev.answer = answerNone
			return m, m.emailSubmitCmd(pane.ID, "send")
		}
		if len(ev.list) > 0 && ev.selected < len(ev.list) {
			ev.detailErr = ""
			ev.status = "opening…"
			return m, m.emailReadCmd(ev.humanoid, ev.list[ev.selected].UID)
		}
		return m, nil
	case "r", "a":
		if ev.detail != nil {
			ev.answer = answerAI
			ev.status = "drafting a reply…"
			return m, tea.Batch(
				m.emailFocusCmd(ev.humanoid, ev.detail),
				m.emailSubmitCmd(pane.ID, "Draft a reply to this email."),
			)
		}
		return m, nil
	case "c":
		if ev.detail != nil {
			ev.answer = answerCompose
			ev.compose = ""
			ev.status = "compose — ctrl+d send · esc cancel"
		}
		return m, nil
	case "esc":
		switch {
		case ev.answer != answerNone:
			ev.answer = answerNone
			ev.status = ""
		case ev.detail != nil:
			ev.detail = nil
			ev.focusCol = emailColList
		default:
			// At the list root: leave the email client for the shell view.
			m.setActiveInputMode("shell")
			m.mode = modeNormal
			m.syncPaneInputFocus()
		}
		return m, nil
	}
	return m, nil
}

// updateEmailCompose edits the manual-compose body and sends it on ctrl+d.
func (m Model) updateEmailCompose(paneID string, ev *emailViewState, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		ev.answer = answerNone
		ev.status = ""
		return m, nil
	case tea.KeyCtrlD:
		if ev.detail == nil || strings.TrimSpace(ev.compose) == "" {
			ev.status = "nothing to send"
			return m, nil
		}
		to := ev.detail.From
		subj := replySubject(ev.detail.Subject)
		ev.status = "sending…"
		body := ev.compose
		inReplyTo := ev.detail.MessageID
		ev.answer = answerNone
		return m, m.emailComposeCmd(ev.humanoid, to, subj, body, inReplyTo)
	case tea.KeyEnter:
		ev.compose += "\n"
		return m, nil
	case tea.KeyBackspace:
		ev.compose = trimLastRune(ev.compose)
		return m, nil
	case tea.KeySpace:
		ev.compose += " "
		return m, nil
	case tea.KeyRunes:
		ev.compose += string(msg.Runes)
		return m, nil
	}
	return m, nil
}

// replySubject prefixes "Re: " unless the subject already carries one.
func replySubject(subject string) string {
	s := strings.TrimSpace(subject)
	if strings.HasPrefix(strings.ToLower(s), "re:") {
		return s
	}
	return "Re: " + s
}

func trimLastRune(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return string(r[:len(r)-1])
}

// ---- Render (three-column Gmail-style client) -----------------------------

var (
	emailSelStyle    = lipgloss.NewStyle().Reverse(true)
	emailDimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	emailAccentStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Bold(true)
	emailBadgeStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	emailErrStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	emailBarStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	emailFocusStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("231")).Bold(true)
)

// buildEmailPanel renders the whole email client for a pane in "email" mode: a
// folder rail, the inbox list, the reading pane, an answer dock (when active),
// and an action bar. Sized to the pane so it never overflows.
func (m Model) buildEmailPanel(pane domain.PaneSnapshot, paneWidth, height int) string {
	metaText := paneMetaText(pane)
	st := m.emailViews[pane.ID]
	if st == nil {
		st = &emailViewState{humanoid: pane.RegisteredHumanoid, focusCol: emailColList}
	}

	w := max(1, paneWidth-1)
	// Rows available for the three columns: minus meta, action bar, and the
	// answer dock when one is open.
	dockH := 0
	if st.answer != answerNone {
		dockH = min(8, max(3, height/3))
	}
	bodyH := height - 1 /*meta*/ - 1 /*action bar*/ - dockH
	if bodyH < 1 {
		bodyH = 1
	}

	var columns string
	if w < 46 {
		// Too narrow for three columns: show the reader if open, else the list.
		if st.detail != nil {
			columns = joinRows(m.emailReaderRows(st, w, bodyH), w, bodyH)
		} else {
			columns = joinRows(m.emailListRows(st, w, bodyH), w, bodyH)
		}
	} else {
		railW := 11
		rest := w - railW - 2 // 2 single-space gutters
		listW := max(18, rest*2/5)
		readerW := rest - listW
		rail := joinRows(m.emailFolderRows(st, railW), railW, bodyH)
		list := joinRows(m.emailListRows(st, listW, bodyH), listW, bodyH)
		reader := joinRows(m.emailReaderRows(st, readerW, bodyH), readerW, bodyH)
		columns = lipgloss.JoinHorizontal(lipgloss.Top, rail, " ", list, " ", reader)
	}

	parts := []string{metaStyle.Render(metaText), columns}
	if dockH > 0 {
		parts = append(parts, m.emailDock(pane, st, w, dockH))
	}
	parts = append(parts, emailBarStyle.Render(padTrunc(m.emailActionBar(st), w)))
	return strings.Join(parts, "\n")
}

// emailFolderRows renders the (mostly static) folder rail. Only INBOX is live;
// Drafts/Sent are shown dim as "coming soon", matching the desktop client.
func (m Model) emailFolderRows(st *emailViewState, w int) []string {
	inbox := "INBOX"
	if st.focusCol == emailColFolders {
		inbox = emailFocusStyle.Render(padTrunc("▸ INBOX", w))
	} else {
		inbox = emailAccentStyle.Render(padTrunc("INBOX", w))
	}
	return []string{
		inbox,
		emailDimStyle.Render(padTrunc("Drafts", w)),
		emailDimStyle.Render(padTrunc("Sent", w)),
		"",
		emailDimStyle.Render(padTrunc("(soon)", w)),
	}
}

// emailListRows renders the inbox list column, newest first, one row per email
// (short-id badge + sender + subject), with the selected row highlighted.
func (m Model) emailListRows(st *emailViewState, w, h int) []string {
	title := "Inbox"
	if st.focusCol == emailColList {
		title = "▸ Inbox"
	}
	rows := []string{emailAccentStyle.Render(padTrunc(fmt.Sprintf("%s (%d)", title, len(st.list)), w))}
	if st.listErr != "" {
		rows = append(rows, emailErrStyle.Render(padTrunc("error: "+st.listErr, w)))
		return rows
	}
	if len(st.list) == 0 {
		rows = append(rows, emailDimStyle.Render(padTrunc("(no mail — g to refresh)", w)))
		return rows
	}
	// Scroll the list so the selected row stays visible.
	visible := max(1, h-1)
	start := 0
	if st.selected >= visible {
		start = st.selected - visible + 1
	}
	for i := start; i < len(st.list) && i < start+visible; i++ {
		e := st.list[i]
		badge := emailBadgeStyle.Render(e.ID)
		line := fmt.Sprintf("%s %s  %s", badge, shortFrom(e.From, 14), e.Subject)
		row := padTrunc(line, w)
		if i == st.selected {
			row = emailSelStyle.Render(padTrunc(fmt.Sprintf("%s %s  %s", e.ID, shortFrom(e.From, 14), e.Subject), w))
		}
		rows = append(rows, row)
	}
	return rows
}

// emailReaderRows renders the reading pane: headers then the scrolled body.
func (m Model) emailReaderRows(st *emailViewState, w, h int) []string {
	title := "Message"
	if st.focusCol == emailColReader {
		title = "▸ Message"
	}
	rows := []string{emailAccentStyle.Render(padTrunc(title, w))}
	if st.detailErr != "" {
		rows = append(rows, emailErrStyle.Render(padTrunc("error: "+st.detailErr, w)))
		return rows
	}
	d := st.detail
	if d == nil {
		rows = append(rows, emailDimStyle.Render(padTrunc("(enter to open a message)", w)))
		return rows
	}
	rows = append(rows,
		padTrunc(emailDimStyle.Render("From: ")+d.From, w),
		padTrunc(emailDimStyle.Render("Subj: ")+d.Subject, w),
		padTrunc(emailDimStyle.Render("Date: ")+d.Date, w),
		emailDimStyle.Render(strings.Repeat("─", w)),
	)
	body := wrapPlain(d.Body, w)
	// Scroll offset, clamped so we never scroll past the end.
	bodyLines := max(1, h-len(rows))
	maxOff := max(0, len(body)-bodyLines)
	off := min(st.readerScroll, maxOff)
	for i := off; i < len(body) && i < off+bodyLines; i++ {
		rows = append(rows, padTrunc(body[i], w))
	}
	return rows
}

// emailDock renders the answer area: the streamed AI draft, or the compose box.
func (m Model) emailDock(pane domain.PaneSnapshot, st *emailViewState, w, h int) string {
	var title, bodyText string
	switch st.answer {
	case answerAI:
		title = "Draft (AI) — enter: send · esc: cancel"
		bodyText = strings.TrimSpace(pane.ExternalOutput)
		if bodyText == "" {
			bodyText = "…drafting…"
		}
	case answerCompose:
		title = "Compose reply — ctrl+d: send · esc: cancel"
		bodyText = st.compose + "▏"
	}
	rows := []string{emailAccentStyle.Render(padTrunc(title, w))}
	for _, ln := range wrapPlain(bodyText, w) {
		if len(rows) >= h {
			break
		}
		rows = append(rows, padTrunc(ln, w))
	}
	// When AI-drafting, keep the tail (freshest) visible rather than the head.
	if st.answer == answerAI {
		all := append([]string{rows[0]}, wrapPlain(bodyText, w)...)
		if len(all) > h {
			all = append(all[:1], all[len(all)-(h-1):]...)
		}
		rows = all
	}
	return joinRows(rows, w, h)
}

// emailActionBar renders the context-sensitive key hints.
func (m Model) emailActionBar(st *emailViewState) string {
	if st.answer == answerCompose {
		return "type reply · ctrl+d send · esc cancel"
	}
	if st.answer == answerAI {
		return "enter send · esc cancel"
	}
	base := "j/k move · enter open · tab switch"
	if st.detail != nil {
		base += " · r answer · c compose"
	}
	base += " · g refresh · esc back"
	if st.status != "" {
		base = st.status + "  |  " + base
	}
	return base
}

// ---- small render helpers -------------------------------------------------

// padTrunc truncates s (ANSI-aware) to w columns and right-pads to exactly w.
func padTrunc(s string, w int) string {
	if w <= 0 {
		return ""
	}
	s = ansi.Truncate(s, w, "")
	pad := w - ansi.StringWidth(s)
	if pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return s
}

// joinRows pads a slice of rows to exactly h lines of width w and joins them.
func joinRows(rows []string, w, h int) string {
	out := make([]string, 0, h)
	for i := 0; i < h; i++ {
		if i < len(rows) {
			out = append(out, padTrunc(rows[i], w))
		} else {
			out = append(out, strings.Repeat(" ", w))
		}
	}
	return strings.Join(out, "\n")
}

// shortFrom extracts a compact sender label (display name, else the address
// local-part) truncated to w runes.
func shortFrom(from string, w int) string {
	s := strings.TrimSpace(from)
	if i := strings.IndexByte(s, '<'); i > 0 {
		s = strings.TrimSpace(s[:i]) // "Name <addr>" → "Name"
	} else if i := strings.IndexByte(s, '@'); i > 0 {
		s = s[:i] // "addr@host" → "addr"
	}
	s = strings.Trim(s, "\"")
	r := []rune(s)
	if len(r) > w {
		return string(r[:w])
	}
	return s
}

// wrapPlain hard-wraps plain text (email bodies carry no ANSI) to width w,
// preserving explicit newlines. Returns at least one (possibly empty) line.
func wrapPlain(s string, w int) []string {
	if w <= 0 {
		return []string{""}
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		para = strings.TrimRight(para, "\r")
		if para == "" {
			out = append(out, "")
			continue
		}
		r := []rune(para)
		for len(r) > w {
			out = append(out, string(r[:w]))
			r = r[w:]
		}
		out = append(out, string(r))
	}
	if len(out) == 0 {
		out = append(out, "")
	}
	return out
}
