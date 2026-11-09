// SPDX-License-Identifier: Apache-2.0

package company

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// S2.4 (sprint 0.2.16, design 037): the desktop app's company mode gets a Books
// tab. The app never calls rysh-server itself, so the daemon's snapshot carries
// the read-only Books data the key may read: deadlines (Companies House), and,
// while the bookkeeping assistant is on, the monthly summary and the first page
// of the ledger. The server's JSON is passed through, like the MCP tools.

const booksID = "22222222-2222-2222-2222-222222222222"

// booksServer serves the company record plus the three Books routes; ledger
// selects the ledger route's behaviour: "on", "not_enabled", "missing", "500".
func booksServer(t *testing.T, ledger string, seen *[]string) *httptest.Server {
	t.Helper()
	base := "/api/ai-native-companies/" + booksID
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer rysh_key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if seen != nil {
			*seen = append(*seen, r.URL.Path+"?"+r.URL.RawQuery)
		}
		notEnabled := func() {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not enabled","code":"not_enabled"}`))
		}
		switch r.URL.Path {
		case base:
			_, _ = w.Write([]byte(`{"company":{"id":"` + booksID + `","name":"Modern Bistro","slug":"firat","status":"active"}}`))
		case base + "/accountant/deadlines":
			if ledger == "missing" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"linked":true,"accounts":{"next_due":"2027-06-30"}}`))
		case base + "/accountant/ledger":
			switch ledger {
			case "on":
				_, _ = w.Write([]byte(`{"entries":[{"id":"l1","amount_pence":1250,"direction":"out"}]}`))
			case "not_enabled":
				notEnabled()
			case "500":
				w.WriteHeader(http.StatusInternalServerError)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		case base + "/accountant/summary":
			if ledger != "on" {
				notEnabled()
				return
			}
			_, _ = w.Write([]byte(`{"from":"2026-06","to":"2026-11","months":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestFetchCarriesBooksWhenOn(t *testing.T) {
	var seen []string
	srv := booksServer(t, "on", &seen)
	defer srv.Close()
	snap, err := NewClient(srv.URL, "rysh_key").Fetch(context.Background(), booksID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	b := snap.Books
	if b == nil || !b.Enabled {
		t.Fatalf("books: %+v", b)
	}
	if !strings.Contains(string(b.Deadlines), `"next_due":"2027-06-30"`) || !strings.Contains(string(b.Ledger), `"l1"`) || !strings.Contains(string(b.Summary), `"from":"2026-06"`) {
		t.Fatalf("books passthrough: %s / %s / %s", b.Deadlines, b.Ledger, b.Summary)
	}
	var ledgerQ, summaryQ string
	for _, s := range seen {
		if strings.Contains(s, "/accountant/ledger?") {
			ledgerQ = s
		}
		if strings.Contains(s, "/accountant/summary?") {
			summaryQ = s
		}
	}
	if !strings.HasSuffix(ledgerQ, "?limit=20") || !strings.HasSuffix(summaryQ, "?months=6") {
		t.Fatalf("queries: ledger %q summary %q", ledgerQ, summaryQ)
	}
	if snap.Link.LastError != "" {
		t.Fatalf("no error expected: %q", snap.Link.LastError)
	}
	// It travels to the app as snapshot.books.
	raw, _ := json.Marshal(snap)
	if !strings.Contains(string(raw), `"books":{"enabled":true,`) {
		t.Fatalf("json: %s", raw)
	}
}

func TestFetchBooksNotEnabledIsNotAnError(t *testing.T) {
	var seen []string
	srv := booksServer(t, "not_enabled", &seen)
	defer srv.Close()
	snap, err := NewClient(srv.URL, "rysh_key").Fetch(context.Background(), booksID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	b := snap.Books
	if b == nil || b.Enabled || b.Ledger != nil || b.Summary != nil {
		t.Fatalf("books off: %+v", b)
	}
	if !strings.Contains(string(b.Deadlines), "2027-06-30") {
		t.Fatalf("deadlines still shown while Books is off: %s", b.Deadlines)
	}
	if snap.Link.LastError != "" {
		t.Fatalf("not_enabled is a state, not an error: %q", snap.Link.LastError)
	}
	for _, s := range seen {
		if strings.Contains(s, "/accountant/summary") {
			t.Fatalf("summary must not be asked while Books is off")
		}
	}
}

func TestFetchBooksAbsentOnAnOlderServer(t *testing.T) {
	srv := booksServer(t, "missing", nil)
	defer srv.Close()
	snap, err := NewClient(srv.URL, "rysh_key").Fetch(context.Background(), booksID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if snap.Books != nil || snap.Link.LastError != "" {
		t.Fatalf("an older server without the routes: books %+v err %q", snap.Books, snap.Link.LastError)
	}
	raw, _ := json.Marshal(snap)
	if strings.Contains(string(raw), `"books"`) {
		t.Fatalf("books must be omitted: %s", raw)
	}
}

func TestFetchBooksFailureNeverBlanksTheView(t *testing.T) {
	srv := booksServer(t, "500", nil)
	defer srv.Close()
	snap, err := NewClient(srv.URL, "rysh_key").Fetch(context.Background(), booksID)
	if err != nil {
		t.Fatalf("a broken Books route must not fail the fetch: %v", err)
	}
	if snap.Company.Name != "Modern Bistro" || !snap.Link.Connected {
		t.Fatalf("view: %+v", snap)
	}
	// Named for the owner (U-27 Q6: never the route's "accountant" path).
	if !strings.Contains(snap.Link.LastError, "the Books ledger") || strings.Contains(snap.Link.LastError, "accountant") {
		t.Fatalf("error: %q", snap.Link.LastError)
	}
}

func TestFetchBooksRefusedKeyStaysUnauthorized(t *testing.T) {
	base := "/api/ai-native-companies/" + booksID
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == base {
			_, _ = w.Write([]byte(`{"company":{"id":"` + booksID + `","name":"x"}}`))
			return
		}
		if strings.HasPrefix(r.URL.Path, base+"/accountant/") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL, "rysh_key").Fetch(context.Background(), booksID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("want ErrUnauthorized, got %v", err)
	}
}

// A company with no legal entity linked (not_configured) or not on the register
// (not_on_register) simply has no deadlines: a state, never a link error.
func TestFetchBooksNoLegalEntityIsNotAnError(t *testing.T) {
	for _, code := range []string{"not_configured", "not_on_register"} {
		base := "/api/ai-native-companies/" + booksID
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case base:
				_, _ = w.Write([]byte(`{"company":{"id":"` + booksID + `","name":"x"}}`))
			case base + "/accountant/deadlines":
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"x","code":"` + code + `"}`))
			case base + "/accountant/ledger":
				_, _ = w.Write([]byte(`{"entries":[]}`))
			case base + "/accountant/summary":
				_, _ = w.Write([]byte(`{"months":[]}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		snap, err := NewClient(srv.URL, "rysh_key").Fetch(context.Background(), booksID)
		srv.Close()
		if err != nil {
			t.Fatalf("%s: fetch: %v", code, err)
		}
		if snap.Books == nil || !snap.Books.Enabled || snap.Books.Deadlines != nil || snap.Link.LastError != "" {
			t.Fatalf("%s: books %+v err %q", code, snap.Books, snap.Link.LastError)
		}
	}
}
