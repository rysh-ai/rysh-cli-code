package javasdk

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/gen"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
)

func sampleAPI() *ir.API {
	return &ir.API{
		Name:       "Petstore",
		Version:    "1.2.3",
		SourceType: "openapi",
		Servers:    []ir.Server{{URL: "https://api.example.com"}},
		Auth:       []ir.AuthScheme{{Name: "k", Type: "apiKey", In: "header", KeyName: "X-Api-Key"}},
		Schemas: map[string]*ir.Schema{
			"Pet": {Type: "object", Properties: map[string]*ir.Schema{
				"id":    {Type: "string"},
				"name":  {Type: "string"},
				"owner": {Ref: "Owner"},
			}, Required: []string{"name"}},
			"Owner": {Type: "object", Properties: map[string]*ir.Schema{"id": {Type: "string"}}},
		},
		Operations: []ir.Operation{
			{ID: "listPets", Method: "GET", Path: "/pets", Summary: "List pets",
				Params:     []ir.Param{{Name: "cursor", In: "query", Schema: &ir.Schema{Type: "string"}}},
				Pagination: &ir.PaginationHint{Style: "cursor", Param: "cursor", NextField: "next_cursor"}},
			{ID: "getPet", Method: "GET", Path: "/pets/{id}",
				Params: []ir.Param{{Name: "id", In: "path", Required: true, Schema: &ir.Schema{Type: "string"}}}},
			{ID: "createPet", Method: "POST", Path: "/pets", Mutating: true,
				RequestBody: &ir.Schema{Ref: "Pet"}},
		},
	}
}

func TestJavaGeneratorName(t *testing.T) {
	if (&generator{}).Name() != "java-sdk" {
		t.Fatalf("Name() = %q, want java-sdk", (&generator{}).Name())
	}
}

func TestJavaGenerateStructure(t *testing.T) {
	fs, err := (&generator{}).Generate(sampleAPI(), gen.Options{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	base := "src/main/java/com/rysh/forge/client"
	wantFiles := []string{
		"pom.xml", "README.md",
		base + "/Client.java", base + "/ApiException.java", base + "/Json.java",
		base + "/model/Pet.java", base + "/model/Owner.java",
	}
	for _, f := range wantFiles {
		if _, ok := fs.Files[f]; !ok {
			t.Errorf("missing generated file %q; have %v", f, fs.Paths())
		}
	}

	client := string(fs.Files[base+"/Client.java"])
	for _, want := range []string{
		"public final class Client",
		"public String listPets(ListPetsRequest req)",
		"public String getPet(GetPetRequest req)",
		"public String createPet(CreatePetRequest req)",
		"path.replace(\"{id}\"",              // path param substitution
		"private void applyAuth(",            // auth method present
		"public ListPetsPager listPetsPaged", // pagination iterator
		"implements java.util.Iterator<String>",
		"Json.encode(body)", // mutating body encoding
	} {
		if !strings.Contains(client, want) {
			t.Errorf("Client.java missing %q", want)
		}
	}
	// apiKey-in-header auth must be injected.
	if !strings.Contains(client, "headers.put(\"X-Api-Key\", apiKey)") {
		t.Errorf("Client.java missing apiKey header injection")
	}

	pet := string(fs.Files[base+"/model/Pet.java"])
	if !strings.Contains(pet, "public final class Pet") {
		t.Errorf("Pet model malformed: %s", pet)
	}
	if !strings.Contains(pet, "com.rysh.forge.client.model.Owner owner") {
		t.Errorf("Pet.owner did not resolve $ref to the Owner model type:\n%s", pet)
	}
}

func TestJavaGenerateDeterministic(t *testing.T) {
	a, _ := (&generator{}).Generate(sampleAPI(), gen.Options{})
	b, _ := (&generator{}).Generate(sampleAPI(), gen.Options{})
	if len(a.Files) != len(b.Files) {
		t.Fatalf("file count differs: %d vs %d", len(a.Files), len(b.Files))
	}
	for p, ca := range a.Files {
		if string(b.Files[p]) != string(ca) {
			t.Fatalf("non-deterministic output for %q", p)
		}
	}
}

// TestJavaEmittedCompiles compiles the emitted sources with javac when it is
// available; otherwise it is skipped. This catches syntax errors in the
// generated Client/Json/model code.
func TestJavaEmittedCompiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping javac compile in -short mode")
	}
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("javac not found; skipping emitted-Java compile")
	}
	fs, err := (&generator{}).Generate(sampleAPI(), gen.Options{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	dir := t.TempDir()
	var javaFiles []string
	for rel, content := range fs.Files {
		if !strings.HasSuffix(rel, ".java") {
			continue
		}
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatal(err)
		}
		javaFiles = append(javaFiles, full)
	}
	out := filepath.Join(dir, "out")
	_ = os.MkdirAll(out, 0o755)
	args := append([]string{"-d", out}, javaFiles...)
	cmd := exec.Command(javac, args...)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("emitted Java does not compile: %v\n%s", err, combined)
	}
}
