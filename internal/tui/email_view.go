// SPDX-License-Identifier: Apache-2.0

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
	"sort"
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
	humanoid         string                // the email humanoid feeding this pane
	list             []msgpkg.EmailSummary // inbox listing (newest first)
	listErr          string                // last list error, if any
	requested        bool                  // a list request has been fired at least once
	selected         int                   // index into list
	focusCol         int                   // emailCol{Folders,List,Reader}
	detail           *msgpkg.EmailDetail   // the opened email (reader column)
	detailErr        string                // last read error, if any
	readerScroll     int                   // body scroll offset (lines)
	answer           emailAnswerMode       // answer dock state
	editor           *composeEditor        // the message being written (new or reply); nil when not composing
	confirmMove      *pendingMove          // a delete/archive waiting for its y/n; nil otherwise
	search           string                // active SUBJECT filter ("" ⇒ unfiltered)
	searchInput      string                // query being typed while searching
	searching        bool                  // the query line is capturing input
	folders          []msgpkg.EmailFolder  // the account's folders (folder rail)
	folder           string                // folder being listed ("" ⇒ INBOX)
	folderSel        int                   // rail cursor, index into folders
	foldersRequested bool                  // a folder listing has been requested
	draftAnchor      string                // tail of the external buffer when the current AI draft was asked for
	flat             bool                  // list in server order instead of threads
	depth            []int                 // reply depth per list row (threaded view)
	listPos          map[int]int           // UID → index in the server's newest-first order
	status           string                // transient status line (send outcome, hints)
}

// emailListCount is the inbox page size requested by the TUI.
const emailListCount = 20

// emailListRequest builds the inbox-listing request. Split out from
// emailListCmd so the query a user typed can be asserted without standing up a
// NATS connection — m.pub is a concrete *NATSPublisher, so the command closure
// itself is not callable from a test.
func emailListRequest(folder, search string) *msgpkg.MsgHumanoidEmailList {
	return &msgpkg.MsgHumanoidEmailList{Count: emailListCount, Search: search, Folder: folder}
}

// emailReadRequest builds a single-email read. IMAP UIDs are only unique
// within one mailbox, so the folder travels with the UID.
func emailReadRequest(folder string, uid int) *msgpkg.MsgHumanoidEmailRead {
	return &msgpkg.MsgHumanoidEmailRead{UID: uid, Folder: folder}
}

// sameFolder reports whether two folder names name the same mailbox, with ""
// meaning INBOX (whose name is case-insensitive, RFC 3501 §5.1).
func sameFolder(a, b string) bool {
	norm := func(f string) string {
		if f == "" || strings.EqualFold(f, "INBOX") {
			return "INBOX"
		}
		return f
	}
	return norm(a) == norm(b)
}

// emailFolderLabel is the display name of the folder a pane is listing.
func emailFolderLabel(st *emailViewState) string {
	for _, f := range st.folders {
		if sameFolder(f.Name, st.folder) {
			if f.Role == "inbox" {
				return "Inbox"
			}
			return f.Label
		}
	}
	if sameFolder(st.folder, "") {
		return "Inbox"
	}
	return st.folder
}

// emailSetSeenRequest builds the read/unread toggle request — split out like
// emailListRequest so it can be asserted without a NATS connection.
func emailSetSeenRequest(folder string, uid int, seen bool) *msgpkg.MsgHumanoidEmailSetSeen {
	return &msgpkg.MsgHumanoidEmailSetSeen{UID: uid, Seen: seen, Folder: folder}
}

// emailUnreadMark prefixes an unread row in the inbox list.
const emailUnreadMark = "●"

// threadEmails groups a newest-first listing into conversations: each thread
// is its root followed by its replies (In-Reply-To chains) depth-first,
// oldest reply first, and threads are ordered by their newest message. When a
// reply's In-Reply-To parent is not in the listing, it hangs under the nearest
// ancestor from its References chain that is; with neither, it is its own
// thread. depth[i] is the reply
// depth of the i-th returned row. Cyclic or self-referencing In-Reply-To
// headers are cut, so every message appears exactly once.
func threadEmails(list []msgpkg.EmailSummary) ([]msgpkg.EmailSummary, []int) {
	byID := make(map[string]int, len(list))
	for i, e := range list {
		if e.MessageID == "" {
			continue
		}
		if _, dup := byID[e.MessageID]; !dup {
			byID[e.MessageID] = i
		}
	}
	parent := make([]int, len(list))
	for i, e := range list {
		parent[i] = -1
		if p, ok := byID[e.InReplyTo]; ok && e.InReplyTo != "" && p != i {
			parent[i] = p
			continue
		}
		// Nearest listed ancestor: References runs oldest → newest.
		for r := len(e.References) - 1; r >= 0; r-- {
			if p, ok := byID[e.References[r]]; ok && p != i {
				parent[i] = p
				break
			}
		}
	}
	// Cut cycles: a node whose ancestor chain leads back to itself becomes a root.
	for i := range list {
		for j, steps := parent[i], 0; j >= 0 && steps <= len(list); j, steps = parent[j], steps+1 {
			if j == i {
				parent[i] = -1
				break
			}
		}
	}
	children := make([][]int, len(list))
	var roots []int
	for i := range list {
		if parent[i] < 0 {
			roots = append(roots, i)
		} else {
			children[parent[i]] = append(children[parent[i]], i)
		}
	}
	// newest[i] = smallest (newest) server index in i's subtree.
	newest := make([]int, len(list))
	var walkNewest func(i int) int
	walkNewest = func(i int) int {
		n := i
		for _, c := range children[i] {
			n = min(n, walkNewest(c))
		}
		newest[i] = n
		return n
	}
	for _, r := range roots {
		walkNewest(r)
	}
	sort.SliceStable(roots, func(a, b int) bool { return newest[roots[a]] < newest[roots[b]] })

	out := make([]msgpkg.EmailSummary, 0, len(list))
	depth := make([]int, 0, len(list))
	var emit func(i, d int)
	emit = func(i, d int) {
		out = append(out, list[i])
		depth = append(depth, d)
		kids := children[i]
		// Conversation order: oldest reply first (larger index = older).
		sort.Slice(kids, func(a, b int) bool { return kids[a] > kids[b] })
		for _, c := range kids {
			emit(c, d+1)
		}
	}
	for _, r := range roots {
		emit(r, 0)
	}
	return out, depth
}

// arrangeEmailList orders st.list for display — threaded, or the server's flat
// newest-first order — keeping the selection on the same mail. It reorders
// the one slice in place of keeping two copies, so per-mail state (a read
// flag flipped with u) survives switching views.
func arrangeEmailList(st *emailViewState) {
	selUID := -1
	if st.selected < len(st.list) {
		selUID = st.list[st.selected].UID
	}
	if st.flat {
		sort.SliceStable(st.list, func(a, b int) bool {
			return st.listPos[st.list[a].UID] < st.listPos[st.list[b].UID]
		})
		st.depth = nil
	} else {
		st.list, st.depth = threadEmails(st.list)
	}
	for i, e := range st.list {
		if e.UID == selUID {
			st.selected = i
		}
	}
}

// ---- Model wiring ---------------------------------------------------------

// emailEventKind tags a decoded humanoid→UI email push.
type emailEventKind int

const (
	emailEvtList emailEventKind = iota
	emailEvtDetail
	emailEvtChanged
	emailEvtCompose
	emailEvtFolders
	emailEvtSaved
	emailEvtMoved
)

// emailEvent is one decoded email push routed from NATS to the Model.
type emailEvent struct {
	kind     emailEventKind
	humanoid string
	folder   string // the folder a list reply is for
	emails   []msgpkg.EmailSummary
	folders  []msgpkg.EmailFolder
	path     string // where a saved attachment was written
	uid      int    // the mail a move reply is about
	target   string // the folder it was moved to
	action   string // "delete" | "archive"
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
				push(emailEvent{kind: emailEvtList, humanoid: r.HumanoidName, folder: r.Folder, emails: r.Emails, err: r.Err})
			}
		}
	})
	_, _ = conn.Subscribe(msgpkg.T("humanoid", "*", "email", "folders"), func(n *nats.Msg) {
		if d, ok := decode(n.Data); ok {
			if r, ok := d.(*msgpkg.MsgHumanoidEmailFoldersReply); ok {
				push(emailEvent{kind: emailEvtFolders, humanoid: r.HumanoidName, folders: r.Folders, err: r.Err})
			}
		}
	})
	_, _ = conn.Subscribe(msgpkg.T("humanoid", "*", "email", "move"), func(n *nats.Msg) {
		if d, ok := decode(n.Data); ok {
			if r, ok := d.(*msgpkg.MsgHumanoidEmailMoveReply); ok {
				push(emailEvent{kind: emailEvtMoved, humanoid: r.HumanoidName, folder: r.Folder, uid: r.UID, target: r.Target, action: r.Action, err: r.Err})
			}
		}
	})
	_, _ = conn.Subscribe(msgpkg.T("humanoid", "*", "email", "attachment"), func(n *nats.Msg) {
		if d, ok := decode(n.Data); ok {
			if r, ok := d.(*msgpkg.MsgHumanoidEmailSaveAttachmentReply); ok {
				push(emailEvent{kind: emailEvtSaved, humanoid: r.HumanoidName, path: r.Path, err: r.Err})
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
	switch {
	case ev.humanoid == "":
		ev.humanoid = pane.RegisteredHumanoid
	case pane.RegisteredHumanoid != "" && pane.RegisteredHumanoid != ev.humanoid:
		// Re-registered to a different humanoid (design 020 §5.7): start a
		// fresh view on the new mailbox — the old list, folders, open mail
		// and draft belong to the previous humanoid — and leave requested
		// false so the next activation fetches the new inbox.
		*ev = emailViewState{focusCol: emailColList, humanoid: pane.RegisteredHumanoid}
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
			ev.foldersRequested = true
			return tea.Batch(m.emailListCmd(ev.humanoid, ev.folder, ev.search), m.emailFoldersCmd(ev.humanoid))
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
			if !sameFolder(ev.folder, st.folder) {
				// A late reply for a folder this pane has switched away from
				// (or another pane's folder on the same humanoid).
				continue
			}
			st.list = ev.emails
			st.listPos = make(map[int]int, len(ev.emails))
			for i, e := range ev.emails {
				st.listPos[e.UID] = i
			}
			st.selected = min(st.selected, max(0, len(st.list)-1))
			arrangeEmailList(st)
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
				// The read fetches BODY[...] without PEEK, which sets \Seen on
				// the server; reflect that in the list without a re-list.
				for i := range st.list {
					if st.list[i].UID == ev.detail.UID {
						st.list[i].Seen = true
					}
				}
			}
		case emailEvtFolders:
			if ev.err == "" {
				st.folders = ev.folders
				st.folderSel = 0
				for i, f := range st.folders {
					if sameFolder(f.Name, st.folder) {
						st.folderSel = i
					}
				}
			}
		case emailEvtMoved:
			if !sameFolder(ev.folder, st.folder) {
				continue
			}
			applyEmailMoved(st, ev)
		case emailEvtSaved:
			if ev.err != "" {
				st.status = "save failed: " + ev.err
			} else {
				st.status = "saved → " + ev.path
			}
		case emailEvtChanged:
			// New mail — refresh the listing, keeping any active filter.
			cmd = m.emailListCmd(st.humanoid, st.folder, st.search)
		case emailEvtCompose:
			if ev.ok {
				st.status = "sent ✓"
				st.answer = answerNone
				st.editor = nil
			} else {
				st.status = "send failed: " + ev.err
				// Reopen the editor with the message intact rather than
				// losing what was typed.
				if st.editor != nil {
					st.answer = answerCompose
				}
			}
		}
	}
	return cmd
}

// ---- Request commands (TUI → humanoid inbox) ------------------------------

// emailListCmd fetches the inbox. search is the active SUBJECT filter and is
// carried on every refresh, so a background MsgHumanoidEmailChanged does not
// silently drop the user back to an unfiltered inbox.
func (m Model) emailListCmd(humanoid, folder, search string) tea.Cmd {
	if humanoid == "" {
		return nil
	}
	pub := m.pub
	subject := msgpkg.T("humanoid", humanoid, "inbox")
	req := emailListRequest(folder, search)
	return func() tea.Msg {
		_ = pub.Send(subject, req)
		return nil
	}
}

func (m Model) emailReadCmd(humanoid, folder string, uid int) tea.Cmd {
	if humanoid == "" {
		return nil
	}
	pub := m.pub
	subject := msgpkg.T("humanoid", humanoid, "inbox")
	req := emailReadRequest(folder, uid)
	return func() tea.Msg {
		_ = pub.Send(subject, req)
		return nil
	}
}

// emailSaveAttachmentRequest builds a save request for attachment index of
// one email — split out so it can be asserted without NATS.
func emailSaveAttachmentRequest(folder string, uid, index int) *msgpkg.MsgHumanoidEmailSaveAttachment {
	return &msgpkg.MsgHumanoidEmailSaveAttachment{Folder: folder, UID: uid, Index: index}
}

// emailSaveAttachmentsCmd asks the humanoid to save every attachment of the
// open email (one request each; each reply reports its own path).
func (m Model) emailSaveAttachmentsCmd(humanoid, folder string, d *msgpkg.EmailDetail) tea.Cmd {
	if humanoid == "" || d == nil || len(d.Attachments) == 0 {
		return nil
	}
	pub := m.pub
	subject := msgpkg.T("humanoid", humanoid, "inbox")
	reqs := make([]*msgpkg.MsgHumanoidEmailSaveAttachment, len(d.Attachments))
	for i := range d.Attachments {
		reqs[i] = emailSaveAttachmentRequest(folder, d.UID, i)
	}
	return func() tea.Msg {
		for _, r := range reqs {
			_ = pub.Send(subject, r)
		}
		return nil
	}
}

// emailFoldersCmd asks the humanoid for the account's folders (the rail).
func (m Model) emailFoldersCmd(humanoid string) tea.Cmd {
	if humanoid == "" {
		return nil
	}
	pub := m.pub
	subject := msgpkg.T("humanoid", humanoid, "inbox")
	return func() tea.Msg {
		_ = pub.Send(subject, &msgpkg.MsgHumanoidEmailFolders{})
		return nil
	}
}

// emailFocusCmd tells the humanoid which email is open, so its "reply to this"
// draft targets this message (mirrors the desktop email_focus wiring).
// emailSetSeenCmd asks the humanoid to set or clear \Seen on one email.
func (m Model) emailSetSeenCmd(humanoid, folder string, uid int, seen bool) tea.Cmd {
	if humanoid == "" {
		return nil
	}
	pub := m.pub
	subject := msgpkg.T("humanoid", humanoid, "inbox")
	req := emailSetSeenRequest(folder, uid, seen)
	return func() tea.Msg {
		_ = pub.Send(subject, req)
		return nil
	}
}

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

	// Compose sub-view captures text input verbatim — including "/", which is a
	// legal character in a reply body and must never be read as the search key.
	if ev.answer == answerCompose {
		return m.updateEmailCompose(pane.ID, ev, msg)
	}
	// The search query line captures input for the same reason.
	if ev.searching {
		return m.updateEmailSearch(ev, msg)
	}
	// A pending delete/archive takes exactly one key: y sends, anything else
	// cancels and sends nothing.
	if p := ev.confirmMove; p != nil {
		ev.confirmMove = nil
		if msg.String() != "y" {
			ev.status = p.action + " cancelled"
			return m, nil
		}
		ev.status = p.action + " — moving…"
		return m, m.emailMoveCmd(ev.humanoid, ev.folder, p.uid, p.action)
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
		if ev.focusCol == emailColFolders {
			if len(ev.folders) > 0 {
				ev.folderSel = min(ev.folderSel+1, len(ev.folders)-1)
			}
		} else if ev.focusCol == emailColReader && ev.detail != nil {
			ev.readerScroll++
		} else if len(ev.list) > 0 {
			ev.selected = min(ev.selected+1, len(ev.list)-1)
		}
		return m, nil
	case "k", "up":
		if ev.focusCol == emailColFolders {
			ev.folderSel = max(0, ev.folderSel-1)
		} else if ev.focusCol == emailColReader && ev.detail != nil {
			ev.readerScroll = max(0, ev.readerScroll-1)
		} else if len(ev.list) > 0 {
			ev.selected = max(0, ev.selected-1)
		}
		return m, nil
	case "/":
		// Open the query line, seeded with the active filter so refining a
		// search does not mean retyping it.
		ev.searching = true
		ev.searchInput = ev.search
		ev.status = "search subject — enter run · esc cancel"
		return m, nil
	case "u":
		// Toggle read/unread on the selected mail: flip the row now, store the
		// flag on the server; the humanoid's email.changed re-list corrects
		// the row if the store failed.
		if len(ev.list) > 0 && ev.selected < len(ev.list) {
			e := &ev.list[ev.selected]
			e.Seen = !e.Seen
			if e.Seen {
				ev.status = "marked read"
			} else {
				ev.status = "marked unread"
			}
			return m, m.emailSetSeenCmd(ev.humanoid, ev.folder, e.UID, e.Seen)
		}
		return m, nil
	case "s":
		// Save the open email's attachments (receive side only).
		if ev.detail == nil {
			return m, nil
		}
		if len(ev.detail.Attachments) == 0 {
			ev.status = "no attachments"
			return m, nil
		}
		ev.status = fmt.Sprintf("saving %d attachment(s)…", len(ev.detail.Attachments))
		return m, m.emailSaveAttachmentsCmd(ev.humanoid, ev.folder, ev.detail)
	case "t":
		ev.flat = !ev.flat
		arrangeEmailList(ev)
		if ev.flat {
			ev.status = "flat list"
		} else {
			ev.status = "threaded"
		}
		return m, nil
	case "g":
		ev.status = "refreshing…"
		ev.foldersRequested = true
		return m, tea.Batch(m.emailListCmd(ev.humanoid, ev.folder, ev.search), m.emailFoldersCmd(ev.humanoid))
	case "enter":
		if ev.answer == answerAI {
			ev.status = "sending…"
			ev.answer = answerNone
			return m, m.emailSubmitCmd(pane.ID, "send")
		}
		if ev.focusCol == emailColFolders {
			// Switch the listing to the folder under the rail cursor. The old
			// folder's mail and open message go: their UIDs mean nothing here.
			if ev.folderSel < len(ev.folders) {
				ev.folder = ev.folders[ev.folderSel].Name
				ev.list = nil
				ev.listErr = ""
				ev.detail = nil
				ev.selected = 0
				ev.focusCol = emailColList
				ev.status = "opening " + emailFolderLabel(ev) + "…"
				return m, m.emailListCmd(ev.humanoid, ev.folder, ev.search)
			}
			return m, nil
		}
		if len(ev.list) > 0 && ev.selected < len(ev.list) {
			ev.detailErr = ""
			ev.status = "opening…"
			return m, m.emailReadCmd(ev.humanoid, ev.folder, ev.list[ev.selected].UID)
		}
		return m, nil
	case "r", "a":
		if ev.detail != nil {
			ev.answer = answerAI
			ev.status = "drafting a reply…"
			// The dock shows only what arrives after this point (E-13).
			ev.draftAnchor = draftAnchorOf(pane.ExternalOutput)
			return m, tea.Batch(
				m.emailFocusCmd(ev.humanoid, ev.detail),
				m.emailSubmitCmd(pane.ID, emailDraftPrompt),
			)
		}
		return m, nil
	case "d", "e":
		// d deletes (→ \Trash), e archives (→ \Archive / Gmail \All); both
		// are moves, never an expunge, and both ask y/n first.
		if p := emailMoveTarget(ev); p != nil {
			if msg.String() == "d" {
				p.action = "delete"
			} else {
				p.action = "archive"
			}
			ev.confirmMove = p
		}
		return m, nil
	case "n":
		// A new message, not a reply: blank To and Subject, cursor in To.
		ev.answer = answerCompose
		ev.editor = newComposeEditor()
		ev.status = "new message — tab next field · ctrl+d send · esc cancel"
		return m, nil
	case "c":
		if ev.detail != nil {
			ev.answer = answerCompose
			ev.editor = newReplyEditor(ev.detail)
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
		case ev.search != "":
			// Back out of the filter before backing out of the client —
			// otherwise a search is a one-way door out of the full inbox.
			ev.search = ""
			ev.selected = 0
			ev.status = "filter cleared"
			return m, m.emailListCmd(ev.humanoid, ev.folder, "")
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

// updateEmailCompose drives the compose editor (email_compose.go) and sends on
// ctrl+d. New message or reply, the send goes through emailComposeCmd — the
// one manual-compose door — and nowhere else.
func (m Model) updateEmailCompose(paneID string, ev *emailViewState, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if ev.editor == nil {
		ev.answer = answerNone
		return m, nil
	}
	send, cancel := ev.editor.handleKey(msg)
	switch {
	case cancel:
		ev.answer = answerNone
		ev.editor = nil
		ev.status = ""
	case send:
		e := ev.editor
		if e.To() == "" {
			ev.status = "add a recipient (To) before sending"
			return m, nil
		}
		if strings.TrimSpace(e.Body()) == "" {
			ev.status = "nothing to send"
			return m, nil
		}
		ev.status = "sending…"
		ev.answer = answerNone
		return m, m.emailComposeCmd(ev.humanoid, e.To(), e.Subject(), e.Body(), e.InReplyTo())
	}
	return m, nil
}

// updateEmailSearch edits the search query line and runs the query on enter.
// Mirrors updateEmailCompose: bubbletea delivers space as tea.KeySpace rather
// than a rune, so it needs its own arm or multi-word queries lose their spaces.
func (m Model) updateEmailSearch(ev *emailViewState, msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		// Abandon the edit; whatever filter was already applied stays applied.
		ev.searching = false
		ev.searchInput = ""
		ev.status = ""
		return m, nil
	case tea.KeyEnter:
		ev.searching = false
		ev.search = strings.TrimSpace(ev.searchInput)
		ev.searchInput = ""
		ev.selected = 0
		if ev.search == "" {
			ev.status = "filter cleared"
		} else {
			ev.status = "searching “" + ev.search + "”…"
		}
		return m, m.emailListCmd(ev.humanoid, ev.folder, ev.search)
	case tea.KeyBackspace:
		ev.searchInput = trimLastRune(ev.searchInput)
		return m, nil
	case tea.KeySpace:
		ev.searchInput += " "
		return m, nil
	case tea.KeyRunes:
		ev.searchInput += string(msg.Runes)
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
	emailUnreadStyle = lipgloss.NewStyle().Bold(true)
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
		rail := joinRows(emailRailWindow(m.emailFolderRows(st, railW), st.folderSel, bodyH), railW, bodyH)
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

// emailFolderRows renders the folder rail: the account's folders as the
// humanoid listed them, the one being shown in the accent colour, the rail
// cursor marked "▸" while the rail has focus (tab to it, j/k, enter opens).
// Before the folder listing arrives only INBOX is shown.
func (m Model) emailFolderRows(st *emailViewState, w int) []string {
	folders := st.folders
	if len(folders) == 0 {
		folders = []msgpkg.EmailFolder{{Name: "INBOX", Label: "INBOX", Role: "inbox"}}
	}
	rows := make([]string, 0, len(folders)+1)
	for i, f := range folders {
		label := f.Label
		if label == "" {
			label = f.Name
		}
		switch {
		case st.focusCol == emailColFolders && i == st.folderSel:
			rows = append(rows, emailFocusStyle.Render(padTrunc("▸ "+label, w)))
		case sameFolder(f.Name, st.folder):
			rows = append(rows, emailAccentStyle.Render(padTrunc(label, w)))
		default:
			rows = append(rows, padTrunc(label, w))
		}
	}
	if len(st.folders) == 0 && st.foldersRequested {
		rows = append(rows, emailDimStyle.Render(padTrunc("…", w)))
	}
	return rows
}

// emailRailWindow keeps the rail cursor visible when there are more folders
// than rows: it returns the slice of rows starting so folderSel is in view.
func emailRailWindow(rows []string, sel, h int) []string {
	if h <= 0 || len(rows) <= h || sel < h {
		return rows
	}
	return rows[sel-h+1:]
}

// emailListRows renders the inbox list column, newest first, one row per email
// (short-id badge + sender + subject), with the selected row highlighted.
func (m Model) emailListRows(st *emailViewState, w, h int) []string {
	title := emailFolderLabel(st)
	if st.focusCol == emailColList {
		title = "▸ " + title
	}
	// Surface the active filter in the column header — a filtered list that
	// looks identical to a full one is how a user concludes their mail is gone.
	if st.search != "" {
		title += " ⌕" + st.search
	}
	unread := 0
	for _, e := range st.list {
		if !e.Seen {
			unread++
		}
	}
	header := fmt.Sprintf("%s (%d)", title, len(st.list))
	if unread > 0 {
		header = fmt.Sprintf("%s (%d · %d unread)", title, len(st.list), unread)
	}
	rows := []string{emailAccentStyle.Render(padTrunc(header, w))}
	if st.listErr != "" {
		rows = append(rows, emailErrStyle.Render(padTrunc("error: "+st.listErr, w)))
		return rows
	}
	if len(st.list) == 0 {
		hint := "(no mail — g to refresh)"
		if st.search != "" {
			hint = "(nothing matches — esc to clear)"
		}
		rows = append(rows, emailDimStyle.Render(padTrunc(hint, w)))
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
		mark := " "
		if !e.Seen {
			mark = emailUnreadMark
		}
		// Threaded view: replies are indented under their root with "↳", and
		// a root with replies counts its thread.
		indent, count := "", ""
		if i < len(st.depth) {
			if d := st.depth[i]; d > 0 {
				indent = strings.Repeat("  ", min(d, 3)-1) + "↳ "
			} else if n := emailThreadSize(st.depth, i); n > 1 {
				count = fmt.Sprintf(" (%d)", n)
			}
		}
		badge := emailBadgeStyle.Render(e.ID)
		line := fmt.Sprintf("%s %s %s%s%s  %s", mark, badge, indent, shortFrom(e.From, 14), count, e.Subject)
		if !e.Seen {
			line = emailUnreadStyle.Render(line)
		}
		row := padTrunc(line, w)
		if i == st.selected {
			row = emailSelStyle.Render(padTrunc(fmt.Sprintf("%s %s %s%s%s  %s", mark, e.ID, indent, shortFrom(e.From, 14), count, e.Subject), w))
		}
		rows = append(rows, row)
	}
	return rows
}

// emailThreadSize counts the thread rooted at row i of a threaded listing: the
// root plus every following reply row until the next root.
func emailThreadSize(depth []int, i int) int {
	n := 1
	for j := i + 1; j < len(depth) && depth[j] > 0; j++ {
		n++
	}
	return n
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
	)
	for i, a := range d.Attachments {
		rows = append(rows, padTrunc(emailDimStyle.Render(fmt.Sprintf("Att%d: ", i+1))+fmt.Sprintf("%s (%s)", a.Filename, humanSize(a.Size)), w))
	}
	rows = append(rows, emailDimStyle.Render(strings.Repeat("─", w)))
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

// emailDraftPrompt is the prompt the email client submits to ask the humanoid
// for a reply draft; the pane echoes it into the external buffer as a "\x02"
// line (pane_exec.go), which marks where a draft's output starts.
const emailDraftPrompt = "Draft a reply to this email."

// draftAnchorLen is how much of the external buffer's tail identifies "where
// the buffer stood" when a draft was asked for.
const draftAnchorLen = 512

// draftAnchorOf returns the tail of the external buffer, used to find later
// where the current draft's output begins.
func draftAnchorOf(ext string) string {
	if len(ext) > draftAnchorLen {
		return ext[len(ext)-draftAnchorLen:]
	}
	return ext
}

// currentDraft is the part of the pane's external buffer that belongs to the
// AI draft being shown (E-13). The buffer is never cleared — it holds earlier
// drafts, echoed prompts, inbound-mail echoes and "send" turns — so the dock
// takes only what followed the buffer's tail at the moment the draft was asked
// for (anchor). When the TUI's cap or a snapshot has removed that tail, it
// falls back to what follows the last draft-prompt echo. Echoed prompt lines
// ("\x02…") are dropped.
func currentDraft(ext, anchor string) string {
	tail := ext
	switch i := strings.LastIndex(ext, anchor); {
	case anchor == "":
	case i >= 0:
		tail = ext[i+len(anchor):]
	default:
		echo := "\x02" + emailDraftPrompt
		if j := strings.LastIndex(ext, echo); j >= 0 {
			tail = ext[j+len(echo):]
		}
	}
	var keep []string
	for _, ln := range strings.Split(tail, "\n") {
		if !strings.HasPrefix(ln, "\x02") {
			keep = append(keep, ln)
		}
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

// emailDock renders the answer area: the streamed AI draft, or the compose box.
func (m Model) emailDock(pane domain.PaneSnapshot, st *emailViewState, w, h int) string {
	var title, bodyText string
	switch st.answer {
	case answerAI:
		title = "Draft (AI) — enter: send · esc: cancel"
		bodyText = currentDraft(pane.ExternalOutput, st.draftAnchor)
		if bodyText == "" {
			bodyText = "…drafting…"
		}
	case answerCompose:
		title = "New message — tab: next field · ctrl+d: send · esc: cancel"
		if st.editor != nil && st.editor.IsReply() {
			title = "Reply — tab: next field · ctrl+d: send · esc: cancel"
		}
		if st.editor != nil {
			bodyText = strings.Join(st.editor.lines(), "\n")
		}
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
	// While typing a query the bar IS the query line — this is the only place
	// the text a user is entering is echoed back to them.
	if st.searching {
		return "search: " + st.searchInput + "▏  (enter run · esc cancel)"
	}
	if p := st.confirmMove; p != nil {
		verb := "Delete"
		if p.action == "archive" {
			verb = "Archive"
		}
		return fmt.Sprintf("%s '%s'? y/n", verb, p.subject)
	}
	if st.answer == answerCompose {
		return "type · tab next field · ←→↑↓ home/end move · ctrl+d send · esc cancel"
	}
	if st.answer == answerAI {
		return "enter send · esc cancel"
	}
	base := "j/k move · enter open · tab switch"
	if st.detail != nil {
		base += " · r answer · c compose"
		if len(st.detail.Attachments) > 0 {
			base += " · s save attachments"
		}
	}
	base += " · d delete · e archive · n new · u read/unread · t threads · / search · g refresh · esc back"
	if st.status != "" {
		base = st.status + "  |  " + base
	}
	return base
}

// ---- small render helpers -------------------------------------------------

// humanSize renders a byte count as "12 B", "121 KB" or "3.4 MB".
func humanSize(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%d KB", (n+512)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

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
