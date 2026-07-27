package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// introspectionHandler serves the canned sampleGraphQL introspection result
// after checking the request is a well-formed standard introspection POST.
func introspectionHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("introspection request method = %s, want POST", r.Method)
		}
		var req struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("introspection request body is not {\"query\":…} JSON: %v", err)
		}
		if !strings.Contains(req.Query, "__schema") || !strings.Contains(req.Query, "queryType") {
			t.Errorf("request does not carry the standard introspection query: %q", req.Query)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(sampleGraphQL))
	}
}

// TestGraphQLIntrospect_MatchesFileIngest is the core B4 guarantee: pulling
// the schema from a running endpoint must produce the exact IR the file-based
// ingest of the same introspection JSON produces — same pipeline, new front-end.
func TestGraphQLIntrospect_MatchesFileIngest(t *testing.T) {
	srv := httptest.NewServer(introspectionHandler(t))
	defer srv.Close()

	live, err := GraphQLIntrospect(context.Background(), srv.URL, nil, 5*time.Second)
	if err != nil {
		t.Fatalf("GraphQLIntrospect: %v", err)
	}
	liveAPI, err := GraphQL(live)
	if err != nil {
		t.Fatalf("GraphQL(live bytes): %v", err)
	}
	fileAPI, err := GraphQL([]byte(sampleGraphQL))
	if err != nil {
		t.Fatalf("GraphQL(file bytes): %v", err)
	}
	if !reflect.DeepEqual(liveAPI, fileAPI) {
		t.Fatalf("live-introspected IR differs from file-based IR:\nlive: %+v\nfile: %+v", liveAPI, fileAPI)
	}
	if len(liveAPI.Operations) != 2 {
		t.Fatalf("want 2 operations from the sample schema, got %d", len(liveAPI.Operations))
	}
}

func TestGraphQLIntrospect_SendsHeaders(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Write([]byte(sampleGraphQL))
	}))
	defer srv.Close()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer sekret")
	if _, err := GraphQLIntrospect(context.Background(), srv.URL, hdr, 5*time.Second); err != nil {
		t.Fatalf("GraphQLIntrospect: %v", err)
	}
	if got != "Bearer sekret" {
		t.Fatalf("Authorization header = %q, want %q", got, "Bearer sekret")
	}
}

func TestGraphQLIntrospect_Errors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantSub string
	}{
		{"http 500", http.StatusInternalServerError, "boom", "HTTP 500"},
		{
			// The common introspection-disabled shape: 200 + errors array.
			"graphql errors array", http.StatusOK,
			`{"errors":[{"message":"introspection has been disabled"}]}`,
			"introspection has been disabled",
		},
		{
			// Well-formed but schema-less: some gateways null the field out.
			"null __schema", http.StatusOK,
			`{"data":{"__schema":null}}`,
			"introspection appears to be disabled",
		},
		{"not json", http.StatusOK, "<html>login</html>", "not JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := GraphQLIntrospect(context.Background(), srv.URL, nil, 5*time.Second)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err, tc.wantSub)
			}
		})
	}
}
