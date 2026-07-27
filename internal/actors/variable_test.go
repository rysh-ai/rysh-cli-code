package actors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVariableStorePersist covers the ##variable persist round trip and — the
// crux of "same manner as ##secret but a separate folder" — that variables land
// in .rysh/variables and are invisible to the secret store (and vice versa).
func TestVariableStorePersist(t *testing.T) {
	t.Chdir(t.TempDir())
	vars := newVariableStore(nil, nil)

	if err := vars.Set(testScope, "REGION", "eu-west-1", true); err != nil {
		t.Fatalf("persist Set: %v", err)
	}

	// The file lands under .rysh/variables, NOT .rysh/secrets.
	varPath := filepath.Join(".rysh", "variables", testScope, "REGION")
	if data, err := os.ReadFile(varPath); err != nil || string(data) != "eu-west-1" {
		t.Fatalf("variable file: got (%q, %v), want eu-west-1", string(data), err)
	}
	if _, err := os.Stat(filepath.Join(".rysh", "secrets", testScope, "REGION")); !os.IsNotExist(err) {
		t.Fatalf("variable leaked into .rysh/secrets: %v", err)
	}

	if v, src, ok := vars.Get(testScope, "REGION"); !ok || v != "eu-west-1" || src != secretSourcePersist {
		t.Fatalf("variable Get: got (%q,%q,%v), want (eu-west-1,persist,true)", v, src, ok)
	}
	// The secret store, backed by .rysh/secrets, does not see the variable.
	if _, _, ok := newSecretStore(nil, nil).Get(testScope, "REGION"); ok {
		t.Fatalf("secret store resolved a variable-only name")
	}

	// A protective .gitignore is dropped at the variables root.
	if _, err := os.Stat(filepath.Join(".rysh", "variables", ".gitignore")); err != nil {
		t.Fatalf("expected .rysh/variables/.gitignore to be created: %v", err)
	}

	// List tags the row with the persist tier only (no session bucket).
	if got := vars.List(testScope); len(got) != 1 || got[0].Name != "REGION" ||
		got[0].Source() != secretSourcePersist {
		t.Fatalf("List after persist Set: got %+v, want one persist-tier REGION row", got)
	}

	rs, rp, err := vars.Delete(testScope, "REGION")
	if err != nil || rs || !rp {
		t.Fatalf("Delete: got (session=%v, persist=%v, err=%v), want (false,true,nil)", rs, rp, err)
	}
	if _, err := os.Stat(varPath); !os.IsNotExist(err) {
		t.Fatalf("variable file was not removed: %v", err)
	}
}

// TestVariableCommand exercises the ##variable command surface end to end on the
// workspace scope: new persists to .rysh/variables, list/get resolve it, the
// value is NOT masked in the echo (variables are visible), and delete removes it.
func TestVariableCommand(t *testing.T) {
	t.Chdir(t.TempDir())
	w := &WorkspaceActor{
		workspaceName: "ws-main",
		sessionName:   "sess",
		secrets:       newSecretStore(nil, nil),
		variables:     newVariableStore(nil, nil),
	}

	var out strings.Builder
	w.handleVariableSubcommand(&out, "pane-1", []string{"new", "API_BASE", "https://api.example.com"})
	if s := out.String(); !strings.Contains(s, "stored \"API_BASE\"") || !strings.Contains(s, filepath.Join(".rysh", "variables", "ws-main", "API_BASE")) {
		t.Fatalf("new output unexpected: %q", s)
	}
	if data, err := os.ReadFile(filepath.Join(".rysh", "variables", "ws-main", "API_BASE")); err != nil || string(data) != "https://api.example.com" {
		t.Fatalf("variable not persisted: got (%q,%v)", string(data), err)
	}

	out.Reset()
	w.handleVariableSubcommand(&out, "pane-1", []string{"list"})
	if s := out.String(); !strings.Contains(s, "API_BASE") || !strings.Contains(s, "[persist]") || !strings.Contains(s, ".rysh/variables") {
		t.Fatalf("list output unexpected: %q", s)
	}

	out.Reset()
	w.handleVariableSubcommand(&out, "pane-1", []string{"get", "API_BASE"})
	if s := out.String(); !strings.Contains(s, "API_BASE = https://api.example.com") {
		t.Fatalf("get output unexpected: %q", s)
	}

	// Variables are visible config: the command echo is never masked.
	if got := maskSecretCommandEcho("##variable new API_BASE https://api.example.com"); got != "##variable new API_BASE https://api.example.com" {
		t.Fatalf("variable command was masked: %q", got)
	}

	out.Reset()
	w.handleVariableSubcommand(&out, "pane-1", []string{"delete", "API_BASE"})
	// No session bucket in this test, so only the persisted tier held the value.
	if s := out.String(); !strings.Contains(s, "deleted persisted variable file for \"API_BASE\"") {
		t.Fatalf("delete output unexpected: %q", s)
	}
	if _, err := os.Stat(filepath.Join(".rysh", "variables", "ws-main", "API_BASE")); !os.IsNotExist(err) {
		t.Fatalf("variable file survived delete: %v", err)
	}
}

// TestCombinedSecretVariableExpansion verifies the single integration point that
// makes agent/humanoid frontmatter resolve ${NAME} from BOTH stores: secrets are
// tried first, then variables, then the environment, with secrets winning a
// name clash and unresolved references left verbatim.
func TestCombinedSecretVariableExpansion(t *testing.T) {
	t.Chdir(t.TempDir())

	const envName = "RYSH_TEST_COMBINED_ENV"
	os.Setenv(envName, "from-env")
	defer os.Unsetenv(envName)

	w := &WorkspaceActor{
		workspaceName: "ws-main",
		sessionName:   "sess",
		secrets: newSecretStore(nil, map[string]map[string]string{
			"ws-main": {"SEC": "secret-value", "BOTH": "from-secret"},
		}),
		variables: newVariableStore(nil, map[string]map[string]string{
			"ws-main": {"VAR": "variable-value", "BOTH": "from-variable"},
		}),
	}

	expand := w.namedExpandFunc("pane-1")
	got := expand("s=${SEC} v=${VAR} both=${BOTH} env=${" + envName + "} miss=${NOPE}")
	want := "s=secret-value v=variable-value both=from-secret env=from-env miss=${NOPE}"
	if got != want {
		t.Fatalf("combined expansion:\n got %q\nwant %q", got, want)
	}
}
