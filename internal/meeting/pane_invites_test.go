// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testInviteID = "3f9a2c1e-7b4d-4e0a-9c11-000000000abc"

// inviteServer is a fake rysh-server for the design 047 invite routes.
type inviteServer struct {
	mu     sync.Mutex
	calls  []string
	bodies []map[string]any
	// legacy answers like a server from before design 047 (no guest invites).
	legacy bool
	status int
	body   string
}

func (f *inviteServer) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		f.bodies = append(f.bodies, in)
		status, body, legacy := f.status, f.body, f.legacy
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/meeting-rooms/room1/invites":
			personal := !legacy && (in["email"] != nil || in["guest"] == true)
			join, extra := "https://rysh.test/meet/room1#invite=rysh_mi_secret", ""
			if personal {
				join, extra = "https://rysh.test/join/room1#invite=rysh_mi_secret", `,"guest_identity":"guest-3f9a2c1e7b4d"`
			}
			_, _ = w.Write([]byte(`{"invite":{"id":"` + testInviteID + `","max_uses":1,"expires_at":"2026-09-30T10:00:00Z","label":"jane@example.test","allow_guest":` +
				map[bool]string{true: "true", false: "false"}[personal] + `},"token":"rysh_mi_secret","join_url":"` + join + `"` + extra + `}`))
		case r.Method == "GET" && r.URL.Path == "/api/meeting-rooms/room1/invites":
			_, _ = w.Write([]byte(`{"invites":[{"id":"` + testInviteID + `","label":"jane@example.test","allow_guest":true,"max_uses":1,"uses":0,"expires_at":"2026-09-30T10:00:00Z","last_used_at":"2026-09-29T10:00:00Z"}]}`))
		case r.Method == "DELETE" && r.URL.Path == "/api/meeting-rooms/room1/invites/"+testInviteID:
			_, _ = w.Write([]byte(`{"message":"invite revoked"}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestPaneInvites(t *testing.T) {
	f := &inviteServer{}
	ref := RoomRef{Server: f.start(t), Room: "room1"}
	ctx := context.Background()

	inv, err := CreateInvite(ctx, ref, "rysh_key", InviteRequest{Email: "jane@example.test", Name: "Jane", TTL: 3 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if inv.JoinURL != "https://rysh.test/join/room1#invite=rysh_mi_secret" || inv.GuestIdentity != "guest-3f9a2c1e7b4d" || inv.ID != testInviteID || !inv.AllowGuest {
		t.Fatalf("personal invite %+v", inv)
	}
	if b := f.bodies[0]; b["email"] != "jane@example.test" || b["name"] != "Jane" || b["ttl_minutes"] != float64(180) || b["guest"] != nil {
		t.Fatalf("body %v", b)
	}
	if f.calls[0] != "POST /api/meeting-rooms/room1/invites Bearer rysh_key" {
		t.Fatalf("call %q", f.calls[0])
	}
	if _, err := CreateInvite(ctx, ref, "rysh_key", InviteRequest{Guest: true}); err != nil {
		t.Fatal(err)
	}
	if b := f.bodies[1]; b["guest"] != true || b["email"] != nil || b["ttl_minutes"] != nil {
		t.Fatalf("guest body %v", b)
	}
	shared, err := CreateInvite(ctx, ref, "rysh_key", InviteRequest{})
	if err != nil || shared.GuestIdentity != "" || !strings.Contains(shared.JoinURL, "/meet/room1#invite=") || len(f.bodies[2]) != 0 {
		t.Fatalf("shared invite %+v %v body %v", shared, err, f.bodies[2])
	}

	list, err := ListInvites(ctx, ref, "rysh_key")
	if err != nil || len(list) != 1 || list[0].Label != "jane@example.test" || list[0].LastUsedAt == "" || list[0].JoinURL != "" {
		t.Fatalf("list %+v %v", list, err)
	}
	if err := RevokeInvite(ctx, ref, "rysh_key", testInviteID); err != nil {
		t.Fatal(err)
	}
	if err := RevokeInvite(ctx, ref, "rysh_key", "../x"); err == nil {
		t.Fatal("revoke with a bad invite id")
	}

	// A server from before design 047 would hand out an account-only link
	// for a guest: refused, pointing at `link`.
	f.legacy = true
	if _, err := CreateInvite(ctx, ref, "rysh_key", InviteRequest{Guest: true}); err == nil || !strings.Contains(err.Error(), "does not support guest invites") {
		t.Fatalf("legacy server: %v", err)
	}
	// An unverified free account: the 403 says what to do, without echoing.
	f.legacy, f.status, f.body = false, 403, `{"error":"verify your email address to host meetings"}`
	if _, err := CreateInvite(ctx, ref, "rysh_key", InviteRequest{Guest: true}); err == nil || !strings.Contains(err.Error(), "verify this rysh account's email") {
		t.Fatalf("unverified: %v", err)
	}
	f.body = `{"error":"only the room owner can do that"}`
	if _, err := ListInvites(ctx, ref, "rysh_key"); err == nil || !strings.Contains(err.Error(), "must belong to the room owner") {
		t.Fatalf("owner only: %v", err)
	}
	if _, err := CreateInvite(ctx, RoomRef{Server: ref.Server, Room: "a/b"}, "k", InviteRequest{}); err == nil {
		t.Fatal("bad room id")
	}
	if _, err := CreateInvite(ctx, ref, "", InviteRequest{}); err == nil || !strings.Contains(err.Error(), RyshAPIKeySecret) {
		t.Fatalf("no key: %v", err)
	}
}

func TestInviteValidators(t *testing.T) {
	for s, want := range map[string]bool{"jane@example.test": true, "Jane <jane@example.test>": false, "jane": false, "a@b": true} {
		if ValidInviteEmail(s) != want {
			t.Errorf("email %q: want %v", s, want)
		}
	}
	if !ValidInviteID(testInviteID) || ValidInviteID("abc") || ValidInviteID(strings.ToUpper(testInviteID)) {
		t.Error("invite id")
	}
}
