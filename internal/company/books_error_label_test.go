// SPDX-License-Identifier: Apache-2.0

package company

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// U-27 Q6(c): a Books read that fails names what it read ("the Books ledger"),
// not the server route, whose path says accountant. The error reaches the
// owner through the company_ledger / company_deadlines tool result.
func TestBooksErrorsNameTheResourceNotTheRoute(t *testing.T) {
	const id = "22222222-2222-2222-2222-222222222222"
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"coded refusal", http.StatusInternalServerError, `{"error":"x","code":"internal"}`},
		{"uncoded status", http.StatusBadGateway, `oops`},
		{"invalid JSON", http.StatusOK, `{not json`},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		c := NewClient(srv.URL, "rysh_key")
		_, derr := c.Deadlines(context.Background(), id)
		_, lerr := c.Ledger(context.Background(), id, nil)
		srv.Close()
		for _, e := range []struct {
			err  error
			want string
		}{{derr, "the Books deadlines"}, {lerr, "the Books ledger"}} {
			if e.err == nil {
				t.Fatalf("%s: no error", tc.name)
			}
			if msg := e.err.Error(); strings.Contains(msg, "accountant") || strings.Contains(msg, "/api/") || !strings.Contains(msg, e.want) {
				t.Errorf("%s: error %q; want it to name %q and no route", tc.name, msg, e.want)
			}
		}
	}
}
