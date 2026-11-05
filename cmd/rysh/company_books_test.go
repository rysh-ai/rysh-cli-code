// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/company"
	"github.com/rysh-ai/rysh-cli-code/internal/msg"
)

// AC4: the AI accountant's read-only tools. They pass the server's JSON
// through (amounts stay integer pence), never touch the snapshot, refuse bad
// arguments before any request, and turn not_enabled into a sentence the
// model can relay.
type fakeBooks struct {
	ledgerQ  []url.Values
	deadline int
	err      error
}

func (f *fakeBooks) Deadlines(context.Context) (json.RawMessage, error) {
	f.deadline++
	if f.err != nil {
		return nil, f.err
	}
	return json.RawMessage(`{"linked":true,"company_number":"12345678","accounts":{"next_due":"2027-06-30","overdue":false}}`), nil
}

func (f *fakeBooks) Ledger(_ context.Context, q url.Values) (json.RawMessage, error) {
	f.ledgerQ = append(f.ledgerQ, q)
	if f.err != nil {
		return nil, f.err
	}
	return json.RawMessage(`{"entries":[{"id":"l1","amount_pence":1250,"direction":"out","category":"supplies"}],"totals":{"in_pence":0,"out_pence":1250,"net_pence":-1250}}`), nil
}

func booksTools(t *testing.T, books companyBooks) (*companyTools, *int) {
	t.Helper()
	snaps := 0
	tools := newCompanyTools(func(context.Context) (*msg.CompanySnapshot, error) { snaps++; return fixtureSnapshot(), nil })
	tools.books = books
	return tools, &snaps
}

func TestCompanyLedgerPassesFiltersAndPence(t *testing.T) {
	books := &fakeBooks{}
	tools, snaps := booksTools(t, books)
	text, isErr := tools.call(context.Background(), "company_ledger",
		json.RawMessage(`{"from":"2026-04-01","to":"2026-10-31","category":"supplies","direction":"out","limit":20}`))
	if isErr || !strings.Contains(text, `"amount_pence": 1250`) {
		t.Fatalf("ledger: isErr=%v %s", isErr, text)
	}
	want := url.Values{"from": {"2026-04-01"}, "to": {"2026-10-31"}, "category": {"supplies"}, "direction": {"out"}, "limit": {"20"}}
	if len(books.ledgerQ) != 1 || books.ledgerQ[0].Encode() != want.Encode() {
		t.Fatalf("query %v, want %v", books.ledgerQ, want)
	}
	// limit is clamped to the server's 200; no filter sends no parameter.
	if _, isErr := tools.call(context.Background(), "company_ledger", json.RawMessage(`{"limit":5000}`)); isErr {
		t.Fatal("a large limit is clamped, not refused")
	}
	if got := books.ledgerQ[1].Encode(); got != "limit=200" {
		t.Fatalf("clamped query %q", got)
	}
	// The next page: before = next_before from the previous one.
	if _, isErr := tools.call(context.Background(), "company_ledger", json.RawMessage(`{"before":"2026-10-01T10:00:00Z_l1"}`)); isErr {
		t.Fatal("before must be accepted")
	}
	if got := books.ledgerQ[2].Get("before"); got != "2026-10-01T10:00:00Z_l1" {
		t.Fatalf("before sent as %q", got)
	}
	text, isErr = tools.call(context.Background(), "company_deadlines", nil)
	if isErr || !strings.Contains(text, `"next_due": "2027-06-30"`) || books.deadline != 1 {
		t.Fatalf("deadlines: isErr=%v %s", isErr, text)
	}
	if *snaps != 0 {
		t.Fatalf("the books tools fetched the company snapshot %d time(s)", *snaps)
	}
}

func TestCompanyLedgerRefusesBadArgumentsBeforeAnyRequest(t *testing.T) {
	books := &fakeBooks{}
	tools, _ := booksTools(t, books)
	for _, args := range []string{
		`{"from":"yesterday"}`,
		`{"to":"2026-13-01"}`,
		`{"direction":"sideways"}`,
		`{"category":"a/b?c"}`,
		`{"limit":-1,"extra":1}`,
		`{"before":"a b&c=d"}`,
	} {
		if text, isErr := tools.call(context.Background(), "company_ledger", json.RawMessage(args)); !isErr {
			t.Errorf("%s: want a tool error, got %s", args, text)
		}
	}
	if len(books.ledgerQ) != 0 {
		t.Fatalf("bad arguments reached the server %d time(s)", len(books.ledgerQ))
	}
}

func TestCompanyBooksNotEnabledAndUnavailable(t *testing.T) {
	tools, _ := booksTools(t, &fakeBooks{err: company.ErrBooksNotEnabled})
	text, isErr := tools.call(context.Background(), "company_ledger", nil)
	if !isErr || !strings.Contains(text, "Bookkeeping assistant") {
		t.Fatalf("not_enabled: isErr=%v %s", isErr, text)
	}
	tools, _ = booksTools(t, nil)
	if text, isErr := tools.call(context.Background(), "company_deadlines", nil); !isErr || !strings.Contains(text, "not available") {
		t.Fatalf("no books client: isErr=%v %s", isErr, text)
	}
}
