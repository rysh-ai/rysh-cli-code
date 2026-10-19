// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/forge/ir"
	"github.com/rysh-ai/rysh-cli-shared/forge/runtime"
)

// TestCredentialForResolvesEveryBindableMode covers the def→Credential step
// that Manager.enable performs before handing the credential to the executor.
//
// It exists because that resolution was written out twice, inline, at two
// points in enable() — so a mode could be honored at one call site and silently
// dropped at the other, with nothing to catch it. credentialFor is now the one
// implementation both use.
//
// Only env-var NAMES are stored; the values are read from the environment here,
// at resolve time.
func TestCredentialForResolvesEveryBindableMode(t *testing.T) {
	t.Setenv("T4_API_KEY", "key-abc")
	t.Setenv("T4_USER", "alice")
	t.Setenv("T4_PASS", "s3cret")

	cases := []struct {
		name string
		def  Integration
		want runtime.Credential
	}{
		{
			name: "nothing configured resolves to an empty credential",
			def:  Integration{},
			want: runtime.Credential{},
		},
		{
			name: "apiKey/bearer via CredentialEnv",
			def:  Integration{CredentialEnv: "T4_API_KEY"},
			want: runtime.Credential{APIKey: "key-abc"},
		},
		{
			name: "http basic via BasicUserEnv/BasicPassEnv",
			def:  Integration{BasicUserEnv: "T4_USER", BasicPassEnv: "T4_PASS"},
			want: runtime.Credential{Username: "alice", Password: "s3cret"},
		},
		{
			name: "basic and apiKey together",
			def:  Integration{CredentialEnv: "T4_API_KEY", BasicUserEnv: "T4_USER", BasicPassEnv: "T4_PASS"},
			want: runtime.Credential{APIKey: "key-abc", Username: "alice", Password: "s3cret"},
		},
		{
			name: "an env var that is not set yields empty, not the name",
			def:  Integration{BasicUserEnv: "T4_DEFINITELY_UNSET"},
			want: runtime.Credential{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := credentialFor(tc.def); got != tc.want {
				t.Fatalf("credentialFor(%+v) = %+v, want %+v", tc.def, got, tc.want)
			}
		})
	}
}

// TestBasicAuthReachesTheWire is the end-to-end half: a stored Integration
// naming two env vars must produce a real "Authorization: Basic" header on an
// outbound request. Resolution and injection are separately correct elsewhere;
// this asserts they are actually connected.
func TestBasicAuthReachesTheWire(t *testing.T) {
	t.Setenv("T4_USER", "alice")
	t.Setenv("T4_PASS", "s3cret")

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	def := Integration{BasicUserEnv: "T4_USER", BasicPassEnv: "T4_PASS"}
	schemes := []ir.AuthScheme{{Name: "basicAuth", Type: "http", Scheme: "basic"}}

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/secrets", nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime.InjectAuth(req, schemes, credentialFor(def))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
	if gotAuth != want {
		t.Fatalf("server saw Authorization=%q, want %q — the basic env vars did not reach the wire", gotAuth, want)
	}
}
