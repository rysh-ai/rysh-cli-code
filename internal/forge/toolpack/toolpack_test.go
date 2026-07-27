package toolpack

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/runtime"
)

func smallAPI() *ir.API {
	return &ir.API{
		Name: "pet", Version: "1", SourceType: "openapi",
		Servers: []ir.Server{{URL: "https://x"}},
		Operations: []ir.Operation{
			{ID: "listPets", Method: "GET", Path: "/pets", Summary: "List", Tags: []string{"read"}},
			{ID: "getPet", Method: "GET", Path: "/pets/{id}", Tags: []string{"read"},
				Params: []ir.Param{{Name: "id", In: "path", Required: true}}},
			{ID: "createPet", Method: "POST", Path: "/pets", Mutating: true, Tags: []string{"write"}},
		},
	}
}

func bigAPI(n int) *ir.API {
	api := &ir.API{Name: "big", Version: "1", Servers: []ir.Server{{URL: "https://x"}}}
	for i := 0; i < n; i++ {
		api.Operations = append(api.Operations, ir.Operation{
			ID: fmt.Sprintf("op%d", i), Method: "GET", Path: fmt.Sprintf("/r%d", i),
		})
	}
	return api
}

func dummyExec() *runtime.HTTPExecutor {
	return runtime.NewHTTPExecutor("https://x", nil, runtime.Credential{}, runtime.Options{})
}

func TestBuildToolPack(t *testing.T) {
	tp := Build(smallAPI(), "pet_")
	if len(tp.Tools) != 3 {
		t.Fatalf("want 3 tools, got %d", len(tp.Tools))
	}
	for _, td := range tp.Tools {
		if !strings.HasPrefix(td.Name, "pet_") {
			t.Errorf("tool %q missing prefix", td.Name)
		}
		if len(td.InputSchema) == 0 {
			t.Errorf("tool %q missing input schema", td.Name)
		}
	}
}

func TestExposureStatic(t *testing.T) {
	api := smallAPI()
	reg := sharedtools.NewToolRegistry()
	tp := Build(api, "pet_")
	names, mode := tp.Register(reg, api, dummyExec(), Policy{Prefix: "pet_"})
	if mode != ModeStatic {
		t.Fatalf("want static mode, got %q", mode)
	}
	if len(names) != 3 {
		t.Fatalf("want 3 registered, got %d: %v", len(names), names)
	}
	// The mutating op's tool requires approval; reads do not.
	create, ok := reg.Get("pet_createPet")
	if !ok || !create.RequiresApproval(nil) {
		t.Fatalf("createPet tool should require approval")
	}
	get, ok := reg.Get("pet_getPet")
	if !ok || get.RequiresApproval(nil) {
		t.Fatalf("getPet tool should not require approval")
	}
	// jq_filter is injected into every tool's schema.
	var schema map[string]any
	_ = json.Unmarshal(create.Spec().Parameters, &schema)
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["jq_filter"]; !ok {
		t.Fatalf("jq_filter not injected into tool schema: %v", props)
	}
}

func TestExposureDynamicAuto(t *testing.T) {
	api := bigAPI(60) // above DefaultDynamicThreshold
	reg := sharedtools.NewToolRegistry()
	tp := Build(api, "big_")
	names, mode := tp.Register(reg, api, dummyExec(), Policy{Prefix: "big_"})
	if mode != ModeDynamic {
		t.Fatalf("want dynamic mode for 60 ops, got %q", mode)
	}
	if len(names) != 3 {
		t.Fatalf("dynamic should register 3 meta-tools, got %d: %v", len(names), names)
	}
	for _, suffix := range []string{"list_endpoints", "get_endpoint_schema", "invoke_endpoint"} {
		if _, ok := reg.Get("big_" + suffix); !ok {
			t.Errorf("missing meta-tool big_%s", suffix)
		}
	}
}

func TestDynamicInvokeApprovalPerCall(t *testing.T) {
	api := smallAPI()
	reg := sharedtools.NewToolRegistry()
	tp := Build(api, "pet_")
	tp.Register(reg, api, dummyExec(), Policy{Mode: ModeDynamic, Prefix: "pet_"})
	invoke, ok := reg.Get("pet_invoke_endpoint")
	if !ok {
		t.Fatalf("invoke_endpoint not registered")
	}
	// Targeting a mutating op requires approval; a read does not.
	if !invoke.RequiresApproval(json.RawMessage(`{"id":"createPet"}`)) {
		t.Errorf("invoke of createPet should require approval")
	}
	if invoke.RequiresApproval(json.RawMessage(`{"id":"listPets"}`)) {
		t.Errorf("invoke of listPets should not require approval")
	}
}

func TestExposureReadOnlyFilter(t *testing.T) {
	api := smallAPI()
	reg := sharedtools.NewToolRegistry()
	tp := Build(api, "pet_")
	names, _ := tp.Register(reg, api, dummyExec(), Policy{Mode: ModeStatic, ReadOnly: true, Prefix: "pet_"})
	if len(names) != 2 { // listPets + getPet, not createPet
		t.Fatalf("read-only filter should yield 2 tools, got %d: %v", len(names), names)
	}
	if _, ok := reg.Get("pet_createPet"); ok {
		t.Fatalf("createPet should be filtered out by read-only")
	}
}
