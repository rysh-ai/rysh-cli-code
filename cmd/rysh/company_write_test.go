// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rysh-ai/rysh-cli-code/internal/company"
)

// CD5 (0.2.16): the company-write MCP server. Two named writes; each checks
// the key's own permission first, names the missing tick (web's label + a
// stable clause with a dashboard PATH), and sends at most one write.
type fakeWriter struct {
	perms    company.Permissions
	writes   []string
	writeErr error
}

func (f *fakeWriter) Permissions(context.Context) (company.Permissions, error) { return f.perms, nil }

func (f *fakeWriter) UpdateEnquiry(_ context.Context, id, status string, note *string) (json.RawMessage, error) {
	n := "<nil>"
	if note != nil {
		n = *note
	}
	f.writes = append(f.writes, "enquiry "+id+" "+status+" "+n)
	if f.writeErr != nil {
		return nil, f.writeErr
	}
	return json.RawMessage(`{"enquiry":{"id":"` + id + `","status":"` + status + `"}}`), nil
}

func (f *fakeWriter) SetOrderStatus(_ context.Context, id, status string) (json.RawMessage, error) {
	f.writes = append(f.writes, "order "+id+" "+status)
	if f.writeErr != nil {
		return nil, f.writeErr
	}
	return json.RawMessage(`{"order":{"id":"` + id + `","status":"` + status + `"}}`), nil
}

const (
	cdCompany = "22222222-2222-2222-2222-222222222222"
	cdEnquiry = "33333333-3333-3333-3333-333333333333"
	cdOrder   = "44444444-4444-4444-4444-444444444444"
)

func writeTools(w companyWriter, now time.Time) *companyWriteTools {
	t := newCompanyWriteTools(w, cdCompany)
	t.now = func() time.Time { return now }
	return t
}

func TestCompanyWriteToolSpecs(t *testing.T) {
	var names []string
	for _, s := range companyWriteToolSpecs {
		names = append(names, s.Name)
	}
	if strings.Join(names, " ") != "company_enquiry_update company_order_status" {
		t.Fatalf("write tools: %v", names)
	}
	for _, s := range companyToolSpecs {
		if strings.HasPrefix(s.Name, "company_enquiry_update") || s.Name == "company_order_status" {
			t.Fatalf("the read server must not carry the write tool %s", s.Name)
		}
	}
}

func TestCompanyWriteNamesTheMissingTickAndSendsNothing(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	w := &fakeWriter{perms: company.Permissions{List: []string{"read"}}}
	text, isErr := writeTools(w, now).call(context.Background(), "company_enquiry_update",
		json.RawMessage(`{"enquiry_id":"`+cdEnquiry+`","status":"contacted","owner_note":"rang back"}`))
	want := "missing permission enquiry.update (allow it at /ai-native-company/" + cdCompany + "/overview#connected-devices)"
	if !isErr || !strings.Contains(text, "Update enquiry status and notes") || !strings.HasSuffix(strings.TrimSpace(text), want) {
		t.Fatalf("missing tick: isErr=%v\n%s", isErr, text)
	}
	if strings.Contains(text, "http") {
		t.Fatalf("the clause must carry a path, never a URL: %s", text)
	}
	if len(w.writes) != 0 {
		t.Fatalf("a write was sent without the permission: %v", w.writes)
	}
	// An expired grant is a missing grant.
	w.perms = company.Permissions{List: []string{"read", "order.status"}, ExpireAt: map[string]time.Time{"order.status": now.Add(-time.Hour)}}
	if text, isErr := writeTools(w, now).call(context.Background(), "company_order_status",
		json.RawMessage(`{"order_id":"`+cdOrder+`","status":"ready"}`)); !isErr || !strings.Contains(text, "Change order status") || len(w.writes) != 0 {
		t.Fatalf("expired grant: isErr=%v writes=%v\n%s", isErr, w.writes, text)
	}
}

func TestCompanyWriteSendsOneWriteAndShowsExpiry(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	w := &fakeWriter{perms: company.Permissions{List: []string{"read", "order.status", "enquiry.update"},
		ExpireAt: map[string]time.Time{"order.status": now.Add(48 * time.Hour), "enquiry.update": now.Add(20 * 24 * time.Hour)}}}
	tools := writeTools(w, now)
	text, isErr := tools.call(context.Background(), "company_order_status", json.RawMessage(`{"order_id":"`+cdOrder+`","status":"ready"}`))
	if isErr || len(w.writes) != 1 || w.writes[0] != "order "+cdOrder+" ready" {
		t.Fatalf("order write: isErr=%v writes=%v\n%s", isErr, w.writes, text)
	}
	if !strings.Contains(text, `"status": "ready"`) || !strings.Contains(text, "2026-11-07") || !strings.Contains(strings.ToLower(text), "expires soon") {
		t.Fatalf("result must show the server's answer and warn about the near expiry:\n%s", text)
	}
	text, isErr = tools.call(context.Background(), "company_enquiry_update", json.RawMessage(`{"enquiry_id":"`+cdEnquiry+`","status":"closed"}`))
	if isErr || len(w.writes) != 2 || w.writes[1] != "enquiry "+cdEnquiry+" closed <nil>" || strings.Contains(strings.ToLower(text), "expires soon") {
		t.Fatalf("enquiry write (no note, far expiry): isErr=%v writes=%v\n%s", isErr, w.writes, text)
	}
	// The server's own refusal (grant removed in between) gets the same message.
	w.writeErr = &company.PermissionError{Permission: "order.status"}
	if text, isErr := tools.call(context.Background(), "company_order_status", json.RawMessage(`{"order_id":"`+cdOrder+`","status":"cancelled"}`)); !isErr ||
		!strings.Contains(text, "missing permission order.status (allow it at /ai-native-company/"+cdCompany+"/overview#connected-devices)") {
		t.Fatalf("server refusal: isErr=%v\n%s", isErr, text)
	}
}

func TestCompanyWriteRefusesBadArgumentsBeforeAnyRequest(t *testing.T) {
	w := &fakeWriter{perms: company.Permissions{List: []string{"read", "order.status", "enquiry.update"}}}
	tools := writeTools(w, time.Now())
	for name, args := range map[string][]string{
		"company_enquiry_update": {
			`{"enquiry_id":"not-a-uuid","status":"closed"}`,
			`{"enquiry_id":"` + cdEnquiry + `","status":"replied"}`,
			`{"enquiry_id":"` + cdEnquiry + `","status":"closed","owner_note":"` + strings.Repeat("x", 2001) + `"}`,
			`{"enquiry_id":"` + cdEnquiry + `","status":"closed","reply":"hi"}`,
			`{"enquiry_id":"` + cdEnquiry + `"}`,
		},
		"company_order_status": {
			`{"order_id":"` + cdOrder + `","status":"shipped"}`,
			`{"order_id":"../x","status":"ready"}`,
		},
	} {
		for _, a := range args {
			if _, isErr := tools.call(context.Background(), name, json.RawMessage(a)); !isErr {
				t.Errorf("%s %s accepted", name, a)
			}
		}
	}
	if len(w.writes) != 0 {
		t.Fatalf("bad arguments reached the server: %v", w.writes)
	}
}
