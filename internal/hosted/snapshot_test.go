// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSnapshotSaveListDeleteAndStartFrom(t *testing.T) {
	var calls []string
	var saveBody, startBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/workspaces/ws1/hosted-runs/423b54f6fd36fa94/snapshots":
			_ = json.NewDecoder(r.Body).Decode(&saveBody)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(Snapshot{ID: "aaaa000000000001", Name: "base", SourceRunID: "423b54f6fd36fa94", ExpiresAt: time.Now().Add(time.Hour)})
		case r.Method == "GET" && r.URL.Path == "/api/workspaces/ws1/hosted-snapshots":
			_ = json.NewEncoder(w).Encode(map[string]any{"snapshots": []Snapshot{{ID: "aaaa000000000001", Name: "base"}}})
		case r.Method == "DELETE" && r.URL.Path == "/api/workspaces/ws1/hosted-snapshots/aaaa000000000001":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "POST" && r.URL.Path == "/api/workspaces/ws1/hosted-runs":
			_ = json.NewDecoder(r.Body).Decode(&startBody)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(testRun())
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{Conn: Conn{URL: srv.URL, APIKey: "k", WorkspaceID: "ws1"}}
	ctx := context.Background()
	s, err := c.SaveSnapshot(ctx, "423b54f6fd36fa94", "base")
	if err != nil || s.ID != "aaaa000000000001" || saveBody["name"] != "base" {
		t.Fatalf("save = %+v %v body %v", s, err, saveBody)
	}
	list, err := c.ListSnapshots(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "base" {
		t.Fatalf("list = %+v %v", list, err)
	}
	if err := c.DeleteSnapshot(ctx, "aaaa000000000001"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Start(ctx, StartOptions{FromSnapshot: "aaaa000000000001"}, "ssh-ed25519 AAAA pub"); err != nil {
		t.Fatal(err)
	}
	if startBody["from_snapshot"] != "aaaa000000000001" {
		t.Fatalf("start body = %v", startBody)
	}
	// Bad ids never reach the server.
	n := len(calls)
	if _, err := c.SaveSnapshot(ctx, "../x", "base"); err == nil {
		t.Fatal("bad run id sent")
	}
	if err := c.DeleteSnapshot(ctx, "../x"); err == nil {
		t.Fatal("bad snapshot id sent")
	}
	if len(calls) != n {
		t.Fatalf("calls = %v", calls)
	}
}

func TestStartWithoutSnapshotSendsNoField(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(testRun())
	}))
	defer srv.Close()
	c := &Client{Conn: Conn{URL: srv.URL, APIKey: "k", WorkspaceID: "ws1"}}
	if _, err := c.Start(context.Background(), StartOptions{}, "pub"); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["from_snapshot"]; ok {
		t.Fatalf("an empty from_snapshot was sent (a server without snapshots would refuse it): %v", body)
	}
}

func TestSnapshot404IsPlain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer srv.Close()
	c := &Client{Conn: Conn{URL: srv.URL, APIKey: "k", WorkspaceID: "ws1"}}
	if _, err := c.ListSnapshots(context.Background()); err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("err = %v", err)
	}
}
