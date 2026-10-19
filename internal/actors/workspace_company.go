// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"fmt"
	"strings"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/company"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// Company connect (design 037): the workspace's half. A session bound to a
// company opens on it — the first pane flips to the "company" content mode —
// and `##company` reads the follower's snapshot as text.

// autoOpenCompanyPane flips the freshly bootstrapped (or restored) first pane
// into the company view. Enable is idempotent, so a restore that already has
// the mode in KV is harmless; the activate push is what makes an attached
// front-end show it without the user cycling modes by hand.
func (w *WorkspaceActor) autoOpenCompanyPane() {
	if w.activePaneID == "" {
		w.syncActivePane()
	}
	if w.activePaneID == "" {
		return
	}
	w.openCompanyPane(w.activePaneID)
}

// openCompanyPane enables and activates the company mode on one pane — the
// same two pushes a humanoid uses to surface its email client (design 020).
func (w *WorkspaceActor) openCompanyPane(paneID string) {
	_ = w.pub.Send(msg.T("pane", paneID, "inbox"), &msg.MsgPaneEnableMode{PaneID: paneID, Mode: "company"})
	_ = w.pub.Send(msg.T("pane", paneID, "activateMode"), &msg.MsgPaneActivateMode{PaneID: paneID, Mode: "company"})
}

// companySnapshot asks the CompanyActor for its latest snapshot.
func (w *WorkspaceActor) companySnapshot() (*msg.CompanySnapshot, error) {
	res, err := w.pub.Request(msg.CompanyQuerySubject(), &msg.MsgCompanyQuery{}, 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("the company follower did not answer: %w", err)
	}
	reply, ok := res.(*msg.MsgCompanySnapshot)
	if !ok {
		return nil, fmt.Errorf("unexpected reply %T from the company follower", res)
	}
	snap := reply.Snapshot
	return &snap, nil
}

// handleCompanyCommand implements ##company [status|orders|enquiries|activity|refresh|open].
func (w *WorkspaceActor) handleCompanyCommand(out *strings.Builder, paneID string, args []string) {
	o := ryshWriter(out)
	if !w.cfg.Company.Enabled() {
		o.Header("no company")
		o.Row("  this session is not bound to an AI-native company")
		o.Row("  bind one from the dashboard's connect code:  ry connect <server> <code>")
		w.failRysh("no company bound to this session")
		return
	}
	sub := "status"
	if len(args) > 0 {
		sub = strings.ToLower(strings.TrimSpace(args[0]))
	}
	switch sub {
	case "refresh":
		_ = w.pub.Send(msg.CompanyRefreshSubject(), &msg.MsgCompanyRefresh{})
		o.Row("  refresh requested for %s", w.cfg.Company.Name)
	case "open":
		w.openCompanyPane(paneID)
		o.Row("  company view opened in this pane (esc returns to the shell)")
	case "status", "orders", "enquiries", "activity":
		snap, err := w.companySnapshot()
		if err != nil {
			o.Header("company")
			o.Row("  %v", err)
			w.failRysh("company: %v", err)
			return
		}
		switch sub {
		case "status":
			printCompanyStatus(o, snap)
		case "orders":
			printCompanyOrders(o, snap)
		case "enquiries":
			printCompanyEnquiries(o, snap)
		case "activity":
			printCompanyActivity(o, snap)
		}
	default:
		o.UsageLine("##company [status|orders|enquiries|activity|refresh|open]")
		w.failRyshUsage("unknown subcommand for ##company: %q", sub)
	}
}

func printCompanyStatus(o ryshOut, snap *msg.CompanySnapshot) {
	c := snap.Company
	o.Header("company")
	o.Rule()
	o.Field("name", "%s", c.Name)
	o.Field("site", "%s", c.SiteURL)
	o.Field("status", "%s", c.Status)
	o.Field("server", "%s", c.ServerURL)
	o.Field("link", "%s", company.DescribeLink(snap.Link))
	if !snap.Link.LastFetch.IsZero() {
		o.Field("updated", "%s", snap.Link.LastFetch.Local().Format("15:04:05"))
	}
	o.Field("orders", "%d", len(snap.Orders))
	o.Field("enquiries", "%d", len(snap.Enquiries))
	if snap.Email != nil {
		addr := snap.Email.IncomingAddress
		if addr == "" {
			addr = snap.Email.Address
		}
		o.Field("email", "%s (%s)", addr, snap.Email.State)
	}
	if snap.Phone != nil && snap.Phone.Number != "" {
		o.Field("phone", "%s (%s)", snap.Phone.Number, snap.Phone.State)
	}
	o.Rule()
	o.Row("  ##company orders | enquiries | activity | refresh | open")
	o.Row("")
}

func printCompanyOrders(o ryshOut, snap *msg.CompanySnapshot) {
	o.Header("orders (%d)", len(snap.Orders))
	for _, ord := range snap.Orders {
		o.Row("  %-6s %-9s %-8s %-20s %s  %s", ord.Reference, ord.Status, ord.PaymentStatus,
			trimTo(ord.CustomerName, 20), formatMinor(ord.AmountPence, ord.Currency), ord.CreatedAt.Local().Format("02 Jan 15:04"))
	}
	if len(snap.Orders) == 0 {
		o.Row("  (none yet)")
	}
	o.Row("")
}

func printCompanyEnquiries(o ryshOut, snap *msg.CompanySnapshot) {
	o.Header("enquiries (%d)", len(snap.Enquiries))
	for _, e := range snap.Enquiries {
		o.Row("  %-6s %-12s %-9s %-20s %s  %s", e.Reference, e.Kind, e.Status,
			trimTo(e.Name, 20), trimTo(e.Subject, 24), e.CreatedAt.Local().Format("02 Jan 15:04"))
	}
	if len(snap.Enquiries) == 0 {
		o.Row("  (none yet)")
	}
	o.Row("")
}

func printCompanyActivity(o ryshOut, snap *msg.CompanySnapshot) {
	o.Header("activity (%d)", len(snap.Activity))
	for _, a := range snap.Activity {
		o.Row("  %s  %-14s %s", a.OccurredAt.Local().Format("02 Jan 15:04:05"), a.Kind+"/"+a.Action, a.Summary)
	}
	if len(snap.Activity) == 0 {
		o.Row("  (nothing yet)")
	}
	o.Row("")
}

func trimTo(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

// formatMinor renders a minor-unit amount: £12.34 for GBP, otherwise 12.34 CUR.
func formatMinor(minor int64, currency string) string {
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
