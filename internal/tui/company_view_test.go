// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/rysh-ai/rysh-cli-code/internal/domain"
	msgpkg "github.com/rysh-ai/rysh-cli-code/internal/msg"
)

const companyPaneID = "company-pane-1"

// buildCompanyModel builds a Model whose active pane is in "company" content
// mode over the given snapshot. No bus: rendering and navigation publish
// nothing, so the tests need none.
func buildCompanyModel(snap *msgpkg.CompanySnapshot) Model {
	pane := domain.PaneSnapshot{ID: companyPaneID, Title: "company", Flex: 1, EnabledModes: []string{"shell", "company"}}
	tab := domain.TabSnapshot{
		ID: "tab-1", Title: "t1", ActivePaneID: companyPaneID,
		Lanes: []domain.LaneSnapshot{{
			ID: "lane-1", Flex: 1, ActivePaneID: companyPaneID,
			PaneGroups: []domain.PaneGroupSnapshot{{ID: "g-1", RowFlex: 1, ActivePaneID: companyPaneID, Panes: []domain.PaneSnapshot{pane}}},
		}},
	}
	m := Model{
		snapshot:          domain.WorkspaceSnapshot{Tabs: []domain.TabSnapshot{tab}, ActiveTabID: "tab-1", ActivePaneID: companyPaneID},
		mode:              modeNormal,
		width:             120,
		height:            40,
		inputs:            map[string]textinput.Model{},
		paneInputModes:    map[string]string{companyPaneID: "company"},
		panePastedText:    map[string]string{},
		paneHistoryIdx:    map[string]int{},
		paneHistorySaved:  map[string]string{},
		paneScrollOffsets: map[string]int{},
		pipelineOutputs:   map[string]string{},
		attentionState:    map[string]*attentionInfo{},
		companyViews:      map[string]*companyViewState{},
		companySnap:       snap,
		workspaceInbox:    msgpkg.T("ws", "inbox"),
	}
	m.recomputePaneRects()
	return m
}

func sampleCompanySnapshot() *msgpkg.CompanySnapshot {
	now := time.Now()
	return &msgpkg.CompanySnapshot{
		Company: msgpkg.CompanyInfo{ID: "c1", Name: "Modern Bistro", Slug: "firat", FQDN: "firat.dns.rysh.ai", Status: "active"},
		Orders: []msgpkg.CompanyOrder{
			{ID: "o1", Reference: "ABC23", CustomerName: "Ada", Fulfilment: "delivery", AmountPence: 1850, Currency: "GBP", Status: "received", PaymentStatus: "paid", CreatedAt: now, Items: []msgpkg.CompanyOrderLine{{Name: "Pizza", Quantity: 2, TotalPence: 1850}}},
			{ID: "o2", Reference: "DEF56", CustomerName: "Bo", Fulfilment: "collection", AmountPence: 700, Currency: "GBP", Status: "ready", PaymentStatus: "unpaid", CreatedAt: now},
		},
		Enquiries: []msgpkg.CompanyEnquiry{{ID: "e1", Reference: "QRS45", Kind: "reservation", Name: "Cy", Status: "new", Fields: `{"party_size":"4"}`, Message: "window seat please", CreatedAt: now}},
		Email:     &msgpkg.CompanyEmail{IncomingAddress: "info@firat.dns.rysh.ai", OutgoingAddress: "info@letchat.ai", State: "active"},
		Activity:  []msgpkg.CompanyActivity{{Kind: "order", Action: "created", Reference: "ABC23", Summary: "order ABC23 from Ada", OccurredAt: now}},
		Link:      msgpkg.CompanyLink{Connected: true, Live: true, LastFetch: now},
	}
}

func TestCompanyPanelRendersHeaderTabsAndOrders(t *testing.T) {
	m := buildCompanyModel(sampleCompanySnapshot())
	pane := m.findPaneInSnapshot(companyPaneID)
	out := m.buildCompanyPanel(*pane, 100, 24)
	for _, want := range []string{"Modern Bistro", "firat.dns.rysh.ai", "live", "Orders (2)", "Enquiries (1)", "ABC23", "£18.50", "Ada", "g refresh"} {
		if !strings.Contains(out, want) {
			t.Fatalf("panel lacks %q:\n%s", want, out)
		}
	}
	if lines := strings.Split(out, "\n"); len(lines) > 24 {
		t.Fatalf("panel overflows its height: %d lines", len(lines))
	}
}

func TestCompanyPanelWithoutSnapshotSaysSo(t *testing.T) {
	m := buildCompanyModel(nil)
	pane := m.findPaneInSnapshot(companyPaneID)
	out := m.buildCompanyPanel(*pane, 80, 12)
	if !strings.Contains(out, "waiting for the company follower") {
		t.Fatalf("no-snapshot panel:\n%s", out)
	}
}

func TestCompanyModeKeysNavigateAndNeverTrap(t *testing.T) {
	m := buildCompanyModel(sampleCompanySnapshot())
	m.syncCompanyMode()
	if m.mode != modeCompany {
		t.Fatalf("a pane in company content mode must put the model in modeCompany, got %q", m.mode)
	}
	press := func(k string) {
		var km tea.KeyMsg
		switch k {
		case "enter":
			km = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			km = tea.KeyMsg{Type: tea.KeyEsc}
		case "tab":
			km = tea.KeyMsg{Type: tea.KeyTab}
		default:
			km = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		next, _ := m.updateCompanyMode(km)
		m = next.(Model)
	}
	st := m.companyViewFor(companyPaneID)
	press("j")
	if st.selected[companySecOrders] != 1 {
		t.Fatalf("j must move down: %d", st.selected[companySecOrders])
	}
	press("j")
	if st.selected[companySecOrders] != 1 {
		t.Fatalf("j must clamp at the last row: %d", st.selected[companySecOrders])
	}
	press("enter")
	if !st.detail {
		t.Fatal("enter must open the detail")
	}
	pane := m.findPaneInSnapshot(companyPaneID)
	out := m.buildCompanyPanel(*pane, 100, 24)
	if !strings.Contains(out, "customer Bo") {
		t.Fatalf("detail of the selected order must render:\n%s", out)
	}
	press("esc")
	if st.detail {
		t.Fatal("esc must close the detail first")
	}
	press("tab")
	if st.section != companySecEnquiries {
		t.Fatalf("tab must move to the next section: %d", st.section)
	}
	press("6")
	if st.section != companySecActivity {
		t.Fatalf("6 must jump to Activity: %d", st.section)
	}
	press("esc")
	if m.mode != modeNormal || m.paneInputModeFor(companyPaneID) != "shell" {
		t.Fatalf("esc at the root must return the pane to the shell: mode=%q input=%q", m.mode, m.paneInputModeFor(companyPaneID))
	}
	m.paneInputModes[companyPaneID] = "company"
	m.syncCompanyMode()
	press("ctrl+o")
	if m.mode != modePrefix {
		t.Fatalf("ctrl+o must reach the prefix chord: %q", m.mode)
	}
}

func TestCompanyModeIsTheDefaultForAPaneTheDaemonPutThere(t *testing.T) {
	m := buildCompanyModel(nil)
	delete(m.paneInputModes, companyPaneID)
	m.syncPaneInputs()
	if got := m.paneInputModeFor(companyPaneID); got != "company" {
		t.Fatalf("a newly seen pane with company enabled must open on it, got %q", got)
	}
	// A pane without the mode still defaults to the shell.
	m.snapshot.Tabs[0].Lanes[0].PaneGroups[0].Panes[0].EnabledModes = []string{"shell", "prompt"}
	delete(m.paneInputModes, companyPaneID)
	m.syncPaneInputs()
	if got := m.paneInputModeFor(companyPaneID); got != "shell" {
		t.Fatalf("an ordinary pane must still default to the shell, got %q", got)
	}
}

func TestApplyCompanySnapshotClampsCursors(t *testing.T) {
	m := buildCompanyModel(sampleCompanySnapshot())
	st := m.companyViewFor(companyPaneID)
	st.selected[companySecOrders] = 1
	smaller := sampleCompanySnapshot()
	smaller.Orders = smaller.Orders[:1]
	m.applyCompanySnapshot(smaller)
	if st.selected[companySecOrders] != 0 || m.companySnap != smaller {
		t.Fatalf("cursor must clamp to the new list: %d", st.selected[companySecOrders])
	}
	m.applyCompanySnapshot(nil)
	if m.companySnap != smaller {
		t.Fatal("a nil snapshot must be ignored")
	}
}
