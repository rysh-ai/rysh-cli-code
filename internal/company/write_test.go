// SPDX-License-Identifier: Apache-2.0

package company

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// CD5 (0.2.16): the connect key's own permissions (read-only), the two named
// writes, and the server's refusals mapped to errors the tools can name.
func TestClientWritesAndPermissions(t *testing.T) {
	const id = "22222222-2222-2222-2222-222222222222"
	type seen struct{ method, path, ctype, body string }
	var reqs []seen
	status, reply := http.StatusOK, `{}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer rysh_key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		reqs = append(reqs, seen{r.Method, r.URL.Path, r.Header.Get("Content-Type"), string(b)})
		if r.URL.Path == "/api/ai-native-companies/"+id+"/connect/permissions" {
			_, _ = w.Write([]byte(`{"permissions":["read","order.status"],"permissions_expire_at":{"order.status":"2026-12-05T10:00:00Z"}}`))
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "rysh_key")
	ctx := context.Background()

	p, err := c.Permissions(ctx, id)
	if err != nil || !p.Has("order.status") || p.Has("enquiry.update") || p.ExpiresAt("order.status").IsZero() {
		t.Fatalf("Permissions: %+v %v", p, err)
	}
	if reqs[0].method != http.MethodGet {
		t.Fatalf("the permissions read must be a GET, got %s", reqs[0].method)
	}

	note := "called back"
	reply = `{"enquiry":{"id":"e1","status":"contacted"}}`
	if _, err := c.UpdateEnquiry(ctx, id, "33333333-3333-3333-3333-333333333333", "contacted", &note); err != nil {
		t.Fatal(err)
	}
	reply = `{"order":{"id":"o1","status":"ready"}}`
	if _, err := c.SetOrderStatus(ctx, id, "44444444-4444-4444-4444-444444444444", "ready"); err != nil {
		t.Fatal(err)
	}
	if got := reqs[1]; got.method != http.MethodPatch || got.path != "/api/ai-native-companies/"+id+"/enquiries/33333333-3333-3333-3333-333333333333" ||
		got.ctype != "application/json" || !jsonEq(got.body, `{"status":"contacted","owner_note":"called back"}`) {
		t.Fatalf("enquiry write: %+v", got)
	}
	if got := reqs[2]; got.method != http.MethodPatch || !strings.HasSuffix(got.path, "/orders/44444444-4444-4444-4444-444444444444") || !jsonEq(got.body, `{"status":"ready"}`) {
		t.Fatalf("order write: %+v", got)
	}

	status, reply = http.StatusForbidden, `{"error":"connect keys are read-only","code":"permission_required","permission":"enquiry.update"}`
	_, err = c.UpdateEnquiry(ctx, id, "33333333-3333-3333-3333-333333333333", "closed", nil)
	var pe *PermissionError
	if !errors.As(err, &pe) || pe.Permission != "enquiry.update" {
		t.Fatalf("permission_required must be a *PermissionError naming the permission, got %v", err)
	}
	status, reply = http.StatusForbidden, `{"error":"connect keys are read-only","code":"read_only"}`
	if _, err := c.SetOrderStatus(ctx, id, "44444444-4444-4444-4444-444444444444", "ready"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read_only must be ErrReadOnly, got %v", err)
	}
	status, reply = http.StatusForbidden, `{"error":"invalid api key"}`
	if _, err := c.SetOrderStatus(ctx, id, "44444444-4444-4444-4444-444444444444", "ready"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a plain 403 stays ErrUnauthorized, got %v", err)
	}
}

func jsonEq(a, b string) bool {
	var x, y any
	return json.Unmarshal([]byte(a), &x) == nil && json.Unmarshal([]byte(b), &y) == nil && mustJSON(x) == mustJSON(y)
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// desktop's CD5 ask: the follower's snapshot carries the key's own
// permissions, so the company view can say what this device may change.
// Absent = not read (an older server has no route); [] = read only.
func TestFetchCarriesTheKeysPermissions(t *testing.T) {
	const id = "22222222-2222-2222-2222-222222222222"
	base := fakeCompanyServer(t, "k", nil)
	defer base.Close()

	// An older server: no permissions route -> absent, the rest still loads.
	snap, err := NewClient(base.URL, "k").Fetch(context.Background(), id)
	if err != nil || snap.Permissions != nil || snap.Company.ID != id {
		t.Fatalf("no route: err=%v permissions=%v", err, snap.Permissions)
	}

	for _, tc := range []struct{ body, want string }{
		{`{"permissions":["read","order.status"],"permissions_expire_at":{"order.status":"2026-12-05T10:00:00Z"}}`, `["read","order.status"]`},
		{`{"permissions":[],"permissions_expire_at":{}}`, `[]`},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/ai-native-companies/"+id+"/connect/permissions" {
				_, _ = w.Write([]byte(tc.body))
				return
			}
			proxy, _ := http.NewRequest(r.Method, base.URL+r.URL.Path, nil)
			proxy.Header = r.Header.Clone()
			resp, err := http.DefaultClient.Do(proxy)
			if err != nil {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
		}))
		snap, err := NewClient(srv.URL, "k").Fetch(context.Background(), id)
		srv.Close()
		if err != nil || snap.Permissions == nil {
			t.Fatalf("%s: err=%v permissions=%v", tc.want, err, snap.Permissions)
		}
		if got := mustJSON(*snap.Permissions); got != tc.want {
			t.Fatalf("permissions %s, want %s", got, tc.want)
		}
		if tc.want != "[]" && snap.PermissionsExpireAt["order.status"].IsZero() {
			t.Fatal("permissions_expire_at not carried")
		}
		raw, _ := json.Marshal(snap)
		if !strings.Contains(string(raw), `"permissions":`+tc.want) {
			t.Fatalf("snapshot JSON lacks permissions %s: %s", tc.want, raw)
		}
	}
}
