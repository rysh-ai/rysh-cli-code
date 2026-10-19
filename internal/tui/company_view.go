// SPDX-License-Identifier: Apache-2.0

package tui

// The company view (design 037, "Company Connect") — a pane in "company"
// content mode shows the AI-native company this session is bound to: orders,
// enquiries, the mailbox, the phone number, services and channels, and a live
// activity feed. It renders the CompanySnapshot the daemon's CompanyActor
// pushes on company.snapshot; nothing here talks to the server, so the desktop
// app can paint the same snapshot from the same daemon.
//
// Read-only in this cut. Keys: j/k move, tab/shift+tab (or h/l, 1-6) switch
// section, enter toggles the detail, g asks the follower to fetch now, esc
// closes the detail and then returns the pane to the shell.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/nats-io/nats.go"

	"github.com/rysh-ai/rysh-cli-code/internal/bus"
	"github.com/rysh-ai/rysh-cli-code/internal/company"
	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

type companySection int

const (
	companySecOrders companySection = iota
	companySecEnquiries
	companySecEmail
	companySecPhone
	companySecServices
	companySecActivity
	companySectionCount
)

var companySectionNames = [companySectionCount]string{"Orders", "Enquiries", "Email", "Phone", "Services", "Activity"}

// companyViewState is the per-pane cursor state; the data is Model.companySnap.
type companyViewState struct {
	section  companySection
	selected [companySectionCount]int
	detail   bool
	status   string
}

// companyEvent is one snapshot push routed from NATS to the Model.
type companyEvent struct{ snap *msgpkg.CompanySnapshot }

// companyEventMsg is a push (re-arms the listener); companyQueryMsg is the
// one-shot reply to the attach-time query and never re-arms anything.
type companyEventMsg struct{ ev companyEvent }
type companyQueryMsg struct {
	snap *msgpkg.CompanySnapshot
	err  string
}

// setupCompanySubscriptions subscribes to the follower's snapshot pushes.
// Drop-on-full is safe: the next push carries the whole state again.
func setupCompanySubscriptions(b *bus.Bus) chan companyEvent {
	ch := make(chan companyEvent, 64)
	codecs := b.Codecs()
	_, _ = b.Conn().Subscribe(msgpkg.CompanySnapshotSubject(), func(n *nats.Msg) {
		var env msgpkg.NATSEnvelope
		if json.Unmarshal(n.Data, &env) != nil {
			return
		}
		d, err := codecs.Decode(env.TypeTag, env.Payload)
		if err != nil {
			return
		}
		if r, ok := d.(*msgpkg.MsgCompanySnapshot); ok {
			snap := r.Snapshot
			select {
			case ch <- companyEvent{snap: &snap}:
			default:
			}
		}
	})
	return ch
}

func (m Model) listenCompanyEventCmd() tea.Cmd {
	ch := m.companyEventCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg { return companyEventMsg{ev: <-ch} }
}

// companyQueryCmd asks the daemon for the current snapshot once, at attach.
// A session with no company follower simply times out; the reply is ignored.
func (m Model) companyQueryCmd() tea.Cmd {
	pub := m.pub
	if pub == nil {
		return nil
	}
	return func() tea.Msg {
		res, err := pub.Request(msgpkg.CompanyQuerySubject(), &msgpkg.MsgCompanyQuery{}, 3*time.Second)
		if err != nil {
			return companyQueryMsg{err: err.Error()}
		}
		if r, ok := res.(*msgpkg.MsgCompanySnapshot); ok {
			snap := r.Snapshot
			return companyQueryMsg{snap: &snap}
		}
		return companyQueryMsg{err: fmt.Sprintf("unexpected reply %T", res)}
	}
}

func (m Model) companyRefreshCmd() tea.Cmd {
	pub := m.pub
	if pub == nil {
		return nil
	}
	return func() tea.Msg {
		_ = pub.Send(msgpkg.CompanyRefreshSubject(), &msgpkg.MsgCompanyRefresh{})
		return nil
	}
}

func (m *Model) companyViewFor(paneID string) *companyViewState {
	if m.companyViews == nil {
		m.companyViews = make(map[string]*companyViewState)
	}
	st := m.companyViews[paneID]
	if st == nil {
		st = &companyViewState{}
		m.companyViews[paneID] = st
	}
	return st
}

// applyCompanySnapshot stores a new snapshot and keeps every cursor in range.
func (m *Model) applyCompanySnapshot(snap *msgpkg.CompanySnapshot) {
	if snap == nil {
		return
	}
	m.companySnap = snap
	for _, st := range m.companyViews {
		st.status = ""
		for sec := companySection(0); sec < companySectionCount; sec++ {
			n := len(companyRows(snap, sec))
			if st.selected[sec] >= n {
				st.selected[sec] = max(0, n-1)
			}
		}
	}
}

// syncCompanyMode enters modeCompany when the active pane shows the company
// view and leaves it when it no longer does — the analogue of syncEmailMode.
func (m *Model) syncCompanyMode() {
	pane := m.findPaneInSnapshot(m.snapshot.ActivePaneID)
	in := pane != nil && m.paneInputModeFor(pane.ID) == "company"
	if in && m.mode == modeNormal {
		m.mode = modeCompany
		m.syncPaneInputFocus()
	} else if !in && m.mode == modeCompany {
		m.mode = modeNormal
		m.syncPaneInputFocus()
	}
}

// updateCompanyMode owns every key while the active pane shows the company
// view, so navigation never leaks into a shell input line.
func (m Model) updateCompanyMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	pane := m.findPaneInSnapshot(m.snapshot.ActivePaneID)
	if pane == nil {
		m.mode = modeNormal
		return m, nil
	}
	st := m.companyViewFor(pane.ID)
	rows := len(companyRows(m.companySnap, st.section))
	switchTo := func(sec companySection) {
		st.section = sec
		st.detail = false
	}
	switch key := msg.String(); key {
	case "ctrl+o": // never trap the user — allow the global prefix chord out
		m.mode = modePrefix
		m.syncPaneInputFocus()
	case "tab", "l", "right":
		switchTo((st.section + 1) % companySectionCount)
	case "shift+tab", "h", "left":
		switchTo((st.section + companySectionCount - 1) % companySectionCount)
	case "1", "2", "3", "4", "5", "6":
		switchTo(companySection(key[0] - '1'))
	case "j", "down":
		if rows > 0 {
			st.selected[st.section] = min(st.selected[st.section]+1, rows-1)
		}
	case "k", "up":
		if rows > 0 {
			st.selected[st.section] = max(0, st.selected[st.section]-1)
		}
	case "enter":
		if rows > 0 {
			st.detail = !st.detail
		}
	case "g":
		st.status = "refreshing…"
		return m, m.companyRefreshCmd()
	case "esc":
		if st.detail {
			st.detail = false
		} else {
			m.setActiveInputMode("shell")
			m.mode = modeNormal
			m.syncPaneInputFocus()
		}
	}
	return m, nil
}

// ---- Rendering --------------------------------------------------------------

// companyRow is one list line and the lines its detail shows.
type companyRow struct {
	line   string
	detail []string
}

// buildCompanyPanel renders the company view for a pane in "company" mode:
// meta line, two header lines, the section tabs, the body (list, or list +
// detail when wide enough), and an action bar. Sized to the pane.
func (m Model) buildCompanyPanel(pane domain.PaneSnapshot, paneWidth, height int) string {
	w := max(1, paneWidth-1)
	st := m.companyViews[pane.ID]
	if st == nil {
		st = &companyViewState{}
	}
	snap := m.companySnap

	lines := []string{metaStyle.Render(paneMetaText(pane))}
	lines = append(lines, companyHeaderLines(snap, w)...)
	lines = append(lines, companyTabsLine(snap, st, w))
	bodyH := height - len(lines) - 1
	if bodyH < 1 {
		bodyH = 1
	}

	var body string
	switch {
	case snap == nil:
		body = joinRows([]string{emailDimStyle.Render(padTrunc("waiting for the company follower… (g to ask again)", w))}, w, bodyH)
	default:
		rows := companyRows(snap, st.section)
		sel := st.selected[st.section]
		if sel >= len(rows) {
			sel = max(0, len(rows)-1)
		}
		switch {
		case st.detail && len(rows) > 0 && w >= 72:
			listW := max(24, w*2/5)
			detailW := w - listW - 1
			list := joinRows(companyListRows(rows, sel, listW, bodyH, true), listW, bodyH)
			detail := joinRows(companyDetailRows(rows[sel], detailW), detailW, bodyH)
			body = lipgloss.JoinHorizontal(lipgloss.Top, list, " ", detail)
		case st.detail && len(rows) > 0:
			body = joinRows(companyDetailRows(rows[sel], w), w, bodyH)
		default:
			body = joinRows(companyListRows(rows, sel, w, bodyH, true), w, bodyH)
		}
	}
	lines = append(lines, body)
	bar := "j/k move  tab section  enter detail  g refresh  esc shell  ctrl+o prefix"
	if st.status != "" {
		bar = st.status + "  ·  " + bar
	}
	lines = append(lines, emailBarStyle.Render(padTrunc(bar, w)))
	return strings.Join(lines, "\n")
}

// companyHeaderLines: "Name · fqdn · status" and "link · updated".
func companyHeaderLines(snap *msgpkg.CompanySnapshot, w int) []string {
	if snap == nil {
		return []string{
			emailAccentStyle.Render(padTrunc("company", w)),
			emailDimStyle.Render(padTrunc("no snapshot yet", w)),
		}
	}
	c := snap.Company
	title := c.Name
	if title == "" {
		title = c.Slug
	}
	first := title
	if c.FQDN != "" {
		first += " · " + c.FQDN
	}
	if c.Status != "" {
		first += " · " + strings.ReplaceAll(c.Status, "_", " ")
	}
	second := string(company.DescribeLink(snap.Link))
	if !snap.Link.LastFetch.IsZero() {
		second += " · updated " + snap.Link.LastFetch.Local().Format("15:04:05")
	}
	secondStyle := emailDimStyle
	if snap.Link.AuthFailed || (!snap.Link.Connected && snap.Link.LastError != "") {
		secondStyle = emailErrStyle
	}
	return []string{
		emailAccentStyle.Render(padTrunc(first, w)),
		secondStyle.Render(padTrunc(second, w)),
	}
}

func companyTabsLine(snap *msgpkg.CompanySnapshot, st *companyViewState, w int) string {
	parts := make([]string, 0, companySectionCount)
	for sec := companySection(0); sec < companySectionCount; sec++ {
		label := companySectionNames[sec]
		if snap != nil {
			switch sec {
			case companySecOrders:
				label = fmt.Sprintf("%s (%d)", label, len(snap.Orders))
			case companySecEnquiries:
				label = fmt.Sprintf("%s (%d)", label, len(snap.Enquiries))
			}
		}
		if sec == st.section {
			parts = append(parts, emailFocusStyle.Render("▸ "+label))
		} else {
			parts = append(parts, emailDimStyle.Render(label))
		}
	}
	return padTrunc(strings.Join(parts, "   "), w)
}

// companyListRows renders rows with the selection highlighted and scrolled
// into view.
func companyListRows(rows []companyRow, sel, w, h int, focused bool) []string {
	if len(rows) == 0 {
		return []string{emailDimStyle.Render(padTrunc("(nothing here yet — g to refresh)", w))}
	}
	visible := max(1, h)
	start := 0
	if sel >= visible {
		start = sel - visible + 1
	}
	out := make([]string, 0, visible)
	for i := start; i < len(rows) && i < start+visible; i++ {
		line := padTrunc(rows[i].line, w)
		if i == sel && focused {
			out = append(out, emailFocusStyle.Render(line))
		} else {
			out = append(out, line)
		}
	}
	return out
}

func companyDetailRows(row companyRow, w int) []string {
	out := make([]string, 0, len(row.detail)+1)
	for _, line := range row.detail {
		out = append(out, padTrunc(line, w))
	}
	if len(out) == 0 {
		out = append(out, emailDimStyle.Render(padTrunc("(no detail)", w)))
	}
	return out
}

// companyRows turns one section of the snapshot into list rows.
func companyRows(snap *msgpkg.CompanySnapshot, sec companySection) []companyRow {
	if snap == nil {
		return nil
	}
	switch sec {
	case companySecOrders:
		rows := make([]companyRow, 0, len(snap.Orders))
		for _, o := range snap.Orders {
			line := fmt.Sprintf("%-6s %-9s %-8s %-18s %-10s %9s  %s",
				o.Reference, o.Status, o.PaymentStatus, companyTrim(o.CustomerName, 18),
				o.Fulfilment, companyMoney(o.AmountPence, o.Currency), companyWhen(o.CreatedAt))
			detail := []string{
				"order " + o.Reference + " · " + o.Status + " · " + o.PaymentStatus,
				"placed " + o.CreatedAt.Local().Format("02 Jan 2006 15:04"),
				"customer " + o.CustomerName,
			}
			if o.CustomerPhone != "" {
				detail = append(detail, "phone "+o.CustomerPhone)
			}
			if o.CustomerEmail != "" {
				detail = append(detail, "email "+o.CustomerEmail)
			}
			detail = append(detail, "fulfilment "+o.Fulfilment)
			if o.Address != "" {
				detail = append(detail, "address "+o.Address)
			}
			if o.Notes != "" {
				detail = append(detail, "notes "+o.Notes)
			}
			detail = append(detail, "")
			for _, it := range o.Items {
				detail = append(detail, fmt.Sprintf("%2d × %-24s %9s", it.Quantity, companyTrim(it.Name, 24), companyMoney(it.TotalPence, o.Currency)))
			}
			detail = append(detail, fmt.Sprintf("%-29s %9s", "total", companyMoney(o.AmountPence, o.Currency)))
			rows = append(rows, companyRow{line: line, detail: detail})
		}
		return rows
	case companySecEnquiries:
		rows := make([]companyRow, 0, len(snap.Enquiries))
		for _, e := range snap.Enquiries {
			line := fmt.Sprintf("%-6s %-12s %-9s %-18s %-20s %s",
				e.Reference, e.Kind, e.Status, companyTrim(e.Name, 18), companyTrim(e.Subject, 20), companyWhen(e.CreatedAt))
			detail := []string{
				e.Kind + " " + e.Reference + " · " + e.Status,
				"received " + e.CreatedAt.Local().Format("02 Jan 2006 15:04"),
				"from " + e.Name,
			}
			if e.Subject != "" {
				detail = append(detail, "about "+e.Subject)
			}
			if e.Phone != "" {
				detail = append(detail, "phone "+e.Phone)
			}
			if e.Email != "" {
				detail = append(detail, "email "+e.Email)
			}
			detail = append(detail, companyFields(e.Fields)...)
			if e.Message != "" {
				detail = append(detail, "", e.Message)
			}
			if e.OwnerNote != "" {
				detail = append(detail, "", "note: "+e.OwnerNote)
			}
			rows = append(rows, companyRow{line: line, detail: detail})
		}
		return rows
	case companySecEmail:
		if snap.Email == nil {
			return []companyRow{{line: "no managed mailbox yet"}}
		}
		e := snap.Email
		in := e.IncomingAddress
		if in == "" {
			in = e.Address
		}
		rows := []companyRow{
			{line: "receives at  " + in},
			{line: "sends as     " + firstNonEmpty(e.OutgoingAddress, e.Address)},
		}
		if e.ReplyAddress != "" && e.ReplyAddress != in {
			rows = append(rows, companyRow{line: "replies via  " + e.ReplyAddress})
		}
		rows = append(rows, companyRow{line: "state        " + strings.ReplaceAll(e.State, "_", " ")})
		if e.LastError != "" {
			rows = append(rows, companyRow{line: "error        " + e.LastError})
		}
		return rows
	case companySecPhone:
		if snap.Phone == nil {
			return []companyRow{{line: "no business number yet"}}
		}
		p := snap.Phone
		rows := []companyRow{}
		if p.Number != "" {
			rows = append(rows, companyRow{line: "number  " + p.Number})
		}
		if p.State != "" {
			rows = append(rows, companyRow{line: "state   " + strings.ReplaceAll(p.State, "_", " ")})
		}
		if p.Message != "" {
			rows = append(rows, companyRow{line: "note    " + p.Message})
		}
		return rows
	case companySecServices:
		rows := make([]companyRow, 0, len(snap.Services)+len(snap.Channels))
		for _, s := range snap.Services {
			line := fmt.Sprintf("service  %-12s %-20s %s", s.ServiceKey, strings.ReplaceAll(s.State, "_", " "), s.StatusMessage)
			detail := []string{s.ServiceKey + " · " + s.State, s.StatusMessage}
			if s.NextAction != "" {
				detail = append(detail, "next: "+s.NextAction)
			}
			if s.MonthlyCostMinor > 0 {
				detail = append(detail, "monthly "+companyMoney(s.MonthlyCostMinor, s.Currency))
			}
			rows = append(rows, companyRow{line: line, detail: detail})
		}
		for _, c := range snap.Channels {
			enabled := "on"
			if !c.Enabled {
				enabled = "off"
			}
			line := fmt.Sprintf("channel  %-12s %-20s %s", c.Channel, strings.ReplaceAll(c.State, "_", " ")+" ("+enabled+")", c.StatusMessage)
			detail := []string{c.Channel + " · " + c.State, c.StatusMessage}
			if len(c.CredentialsRequired) > 0 {
				detail = append(detail, "needs: "+strings.Join(c.CredentialsRequired, ", "))
			}
			rows = append(rows, companyRow{line: line, detail: detail})
		}
		return rows
	case companySecActivity:
		rows := make([]companyRow, 0, len(snap.Activity))
		for _, a := range snap.Activity {
			kind := a.Kind
			if a.Action != "" {
				kind += "/" + a.Action
			}
			line := fmt.Sprintf("%s  %-22s %s", a.OccurredAt.Local().Format("02 Jan 15:04:05"), companyTrim(kind, 22), a.Summary)
			detail := []string{a.Summary, kind}
			if a.Reference != "" {
				detail = append(detail, "reference "+a.Reference)
			}
			if a.Status != "" {
				detail = append(detail, "status "+a.Status)
			}
			rows = append(rows, companyRow{line: line, detail: detail})
		}
		return rows
	}
	return nil
}

func companyFields(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return nil
	}
	var fields map[string]string
	if json.Unmarshal([]byte(raw), &fields) != nil || len(fields) == 0 {
		return nil
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, strings.ReplaceAll(k, "_", " ")+" "+fields[k])
	}
	return out
}

func companyMoney(minor int64, currency string) string {
	whole, frac := minor/100, minor%100
	if frac < 0 {
		frac = -frac
	}
	switch strings.ToUpper(currency) {
	case "GBP", "":
		return fmt.Sprintf("£%d.%02d", whole, frac)
	case "EUR":
		return fmt.Sprintf("€%d.%02d", whole, frac)
	case "USD":
		return fmt.Sprintf("$%d.%02d", whole, frac)
	}
	return fmt.Sprintf("%d.%02d %s", whole, frac, strings.ToUpper(currency))
}

// companyWhen is a short relative time for list rows.
func companyWhen(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Local().Format("02 Jan")
}

func companyTrim(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
