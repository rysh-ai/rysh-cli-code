// SPDX-License-Identifier: Apache-2.0

package company

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The accountant routes (AC2; design note §4) read with the same connect key:
// deadlines from Companies House, and the ledger only while the company's
// bookkeeping assistant is on. 404 {code:"not_enabled"} is its own error.
func TestClientBooksRoutesAndNotEnabled(t *testing.T) {
	const id = "22222222-2222-2222-2222-222222222222"
	var gotQuery url.Values
	ledgerOn := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer rysh_key" || r.Method != http.MethodGet {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/ai-native-companies/" + id + "/accountant/deadlines":
			_, _ = w.Write([]byte(`{"linked":true,"company_number":"12345678","accounts":{"next_due":"2027-06-30","overdue":false}}`))
		case "/api/ai-native-companies/" + id + "/accountant/ledger":
			gotQuery = r.URL.Query()
			if !ledgerOn {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"not enabled","code":"not_enabled"}`))
				return
			}
			_, _ = w.Write([]byte(`{"entries":[{"id":"l1","amount_pence":1250,"direction":"out","category":"supplies"}],"totals":{"in_pence":0,"out_pence":1250,"net_pence":-1250},"next_before":""}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "rysh_key")
	ctx := context.Background()

	d, err := c.Deadlines(ctx, id)
	if err != nil || !strings.Contains(string(d), `"company_number":"12345678"`) {
		t.Fatalf("Deadlines: %s %v", d, err)
	}
	q := url.Values{"from": {"2026-04-01"}, "direction": {"out"}, "limit": {"20"}}
	l, err := c.Ledger(ctx, id, q)
	if err != nil {
		t.Fatalf("Ledger: %v", err)
	}
	var page struct {
		Entries []struct {
			AmountPence int `json:"amount_pence"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(l, &page); err != nil || len(page.Entries) != 1 || page.Entries[0].AmountPence != 1250 {
		t.Fatalf("Ledger page: %s %v", l, err)
	}
	if gotQuery.Encode() != q.Encode() {
		t.Fatalf("query sent %q, want %q", gotQuery.Encode(), q.Encode())
	}

	ledgerOn = false
	if _, err := c.Ledger(ctx, id, nil); !errors.Is(err, ErrBooksNotEnabled) {
		t.Fatalf("a not_enabled 404 must be ErrBooksNotEnabled, got %v", err)
	}
	// Any other 404 is not mistaken for it.
	if _, err := c.Deadlines(ctx, "33333333-3333-3333-3333-333333333333"); err == nil || errors.Is(err, ErrBooksNotEnabled) {
		t.Fatalf("a plain 404 must stay a plain error, got %v", err)
	}
}

// The server's other refusals keep their code, so the model can say why:
// not_on_register / not_configured (404), rate_limited (429), unavailable
// (502), invalid with the field names (400). A coded 404 still counts as
// "not found" for Fetch's best-effort resources.
func TestClientBooksKeepsTheServersCode(t *testing.T) {
	const id = "22222222-2222-2222-2222-222222222222"
	status, body := 0, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "rysh_key")
	for _, tc := range []struct {
		status   int
		body     string
		want     string
		notFound bool
	}{
		{404, `{"error":"x","code":"not_on_register"}`, "not_on_register", true},
		{404, `{"error":"x","code":"not_configured"}`, "not_configured", true},
		{429, `{"error":"x","code":"rate_limited"}`, "rate_limited", false},
		{502, `{"error":"x","code":"unavailable"}`, "unavailable", false},
		{400, `{"error":"x","code":"invalid","fields":{"to":"bad date","from":"bad date"}}`, "invalid (fields: from, to)", false},
	} {
		status, body = tc.status, tc.body
		_, err := c.Deadlines(context.Background(), id)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("HTTP %d: error %v, want it to name %q", tc.status, err, tc.want)
		}
		if errors.Is(err, errNotFound) != tc.notFound {
			t.Errorf("HTTP %d: errNotFound = %v, want %v", tc.status, errors.Is(err, errNotFound), tc.notFound)
		}
		if errors.Is(err, ErrBooksNotEnabled) {
			t.Errorf("HTTP %d %s mistaken for not_enabled", tc.status, tc.want)
		}
	}
}

// security-lead ledger 174 T1: the company id comes from config, not the
// model, but it is still one path segment: a crafted id cannot walk the path.
func TestClientEscapesTheCompanyID(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath())
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "rysh_key")
	_, _ = c.Deadlines(context.Background(), "../../admin?x=1")
	_, _ = c.Ledger(context.Background(), "../../admin", nil)
	_, _ = c.Fetch(context.Background(), "../../admin")
	if len(paths) == 0 {
		t.Fatal("no request made")
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, "/api/ai-native-companies/..%2F..%2Fadmin") {
			t.Fatalf("company id not kept to one escaped segment: %s", p)
		}
	}
}
