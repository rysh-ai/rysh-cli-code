// SPDX-License-Identifier: Apache-2.0

package forgecmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `forge add --grpc-protocol connect|gateway` (E23 T2) selects the wire
// protocol for an API's server-streaming sessions. It must reach the persisted
// store under "grpc_protocol" (what Manager.enable reads), be validated, and
// apply only to gRPC sources. Assertions read the raw store JSON so the test
// pins the on-disk key.

func addGRPC(t *testing.T, extra ...string) (string, string, error) {
	t.Helper()
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "echo.pb"), echoDescriptorSet(t), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	args := append([]string{"add", "echo", "echo.pb", "--base-url", "http://127.0.0.1:1", "--targets", "rysh-toolpack"}, extra...)
	err := Run(work, args, &buf)
	store, _ := os.ReadFile(filepath.Join(work, ".rysh", "forge", "integrations.json"))
	return buf.String(), string(store), err
}

func TestForgeAdd_GRPCProtocolConnectPersisted(t *testing.T) {
	out, store, err := addGRPC(t, "--grpc-protocol", "connect")
	if err != nil {
		t.Fatalf("add --grpc-protocol connect: %v\n%s", err, out)
	}
	if !strings.Contains(store, `"grpc_protocol": "connect"`) {
		t.Fatalf("store does not persist the protocol:\n%s", store)
	}
	if !strings.Contains(out, "Connect") || strings.Contains(out, "not supported") {
		t.Fatalf("add-time note must describe Connect streaming, not deny it:\n%s", out)
	}
}

func TestForgeAdd_GRPCProtocolDefaultIsGateway(t *testing.T) {
	out, store, err := addGRPC(t)
	if err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if strings.Contains(store, "grpc_protocol") {
		t.Fatalf("default must leave the protocol unset (gateway):\n%s", store)
	}
	if !strings.Contains(out, "--grpc-protocol connect") {
		t.Fatalf("add-time note must tell a Connect user which flag to use:\n%s", out)
	}
}

func TestForgeAdd_GRPCProtocolValidation(t *testing.T) {
	if out, _, err := addGRPC(t, "--grpc-protocol", "grpc-web"); err == nil || !strings.Contains(err.Error(), "want gateway or connect") {
		t.Fatalf("bogus protocol accepted: err=%v\n%s", err, out)
	}

	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "spec.json"), []byte(basicAuthSpec), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	err := Run(work, []string{"add", "vault", "spec.json", "--grpc-protocol", "connect", "--targets", "rysh-toolpack"}, &buf)
	if err == nil || !strings.Contains(err.Error(), "only applies to gRPC") {
		t.Fatalf("--grpc-protocol on an OpenAPI source must be refused: err=%v\n%s", err, buf.String())
	}
}
