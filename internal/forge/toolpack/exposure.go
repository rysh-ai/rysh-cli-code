package toolpack

import (
	"encoding/json"
	"fmt"
	"log/slog"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/runtime"
)

// ExposureMode controls how many tools a tool-pack advertises to the model — the
// central lever against "tool explosion" (a 400-operation API would both blow
// the context budget and degrade selection accuracy if registered 1:1).
type ExposureMode string

const (
	ModeAuto    ExposureMode = ""        // dynamic above DynamicThreshold, else static
	ModeStatic  ExposureMode = "static"  // one tool per (filtered) operation
	ModeDynamic ExposureMode = "dynamic" // three constant meta-tools regardless of size
)

// DefaultDynamicThreshold mirrors Stainless's auto-switch (~50 methods).
const DefaultDynamicThreshold = 50

// DefaultMaxTools caps static registration to protect the context budget.
const DefaultMaxTools = 200

// Policy is the per-integration exposure configuration.
type Policy struct {
	Mode             ExposureMode `json:"mode,omitempty"`
	DynamicThreshold int          `json:"dynamic_threshold,omitempty"`
	Tags             []string     `json:"tags,omitempty"`      // include only ops carrying one of these tags
	ReadOnly         bool         `json:"read_only,omitempty"` // include only GET/HEAD
	MaxTools         int          `json:"max_tools,omitempty"`
	ForceApproval    bool         `json:"force_approval,omitempty"` // gate ALL calls (else mutating-only)
	Prefix           string       `json:"prefix,omitempty"`
}

// Register installs the tool-pack's executors into reg according to the policy.
// It returns the registered tool names and the effective exposure mode. exec is
// the shared HTTP runtime (already carrying credentials + redaction).
func (tp *ToolPack) Register(reg *sharedtools.ToolRegistry, api *ir.API, exec *runtime.HTTPExecutor, pol Policy) ([]string, ExposureMode) {
	threshold := pol.DynamicThreshold
	if threshold <= 0 {
		threshold = DefaultDynamicThreshold
	}
	maxTools := pol.MaxTools
	if maxTools <= 0 {
		maxTools = DefaultMaxTools
	}

	// Candidate set after tag/read-only filtering.
	candidates := filterTools(tp, pol)

	mode := pol.Mode
	if mode == ModeAuto {
		if len(candidates) > threshold {
			mode = ModeDynamic
		} else {
			mode = ModeStatic
		}
	}

	if mode == ModeDynamic {
		names := tp.registerDynamic(reg, api, exec, candidates, pol)
		return names, ModeDynamic
	}
	return tp.registerStatic(reg, api, exec, candidates, pol, maxTools), ModeStatic
}

func filterTools(tp *ToolPack, pol Policy) []ToolDef {
	tagSet := map[string]bool{}
	for _, t := range pol.Tags {
		tagSet[t] = true
	}
	var out []ToolDef
	for _, td := range tp.Tools {
		if pol.ReadOnly && td.Mutating {
			continue
		}
		if len(tagSet) > 0 {
			match := false
			for _, t := range td.Tags {
				if tagSet[t] {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		out = append(out, td)
	}
	return out
}

func (tp *ToolPack) registerStatic(reg *sharedtools.ToolRegistry, api *ir.API, exec *runtime.HTTPExecutor, candidates []ToolDef, pol Policy, maxTools int) []string {
	var names []string
	for i := range candidates {
		if len(names) >= maxTools {
			slog.Warn("forge: MaxTools cap reached; remaining operations not registered (use --mode dynamic for full coverage)",
				"integration", tp.Name, "cap", maxTools, "candidates", len(candidates), "skipped", len(candidates)-i)
			break
		}
		td := candidates[i]
		op := api.OperationByID(td.OperationID)
		if op == nil {
			continue
		}
		name := uniqueName(reg, td.Name)
		ex := &opExecutor{
			exec:     exec,
			api:      api,
			op:       op,
			approval: pol.ForceApproval || op.Mutating,
			spec: sharedtools.ToolSpec{
				Name:             name,
				Description:      describeTool(tp.Name, td),
				Parameters:       withJQFilter(td.InputSchema),
				RequiresApproval: pol.ForceApproval || op.Mutating,
			},
		}
		reg.Register(name, ex)
		names = append(names, name)
	}
	return names
}

func (tp *ToolPack) registerDynamic(reg *sharedtools.ToolRegistry, api *ir.API, exec *runtime.HTTPExecutor, candidates []ToolDef, pol Policy) []string {
	// The meta-tools operate over the filtered candidate set.
	sub := &ToolPack{Name: tp.Name, Version: tp.Version, Description: tp.Description, BaseURL: tp.BaseURL, Auth: tp.Auth, Tools: candidates}
	p := pol.Prefix

	listName := uniqueName(reg, SanitizeName(p+"list_endpoints"))
	getName := uniqueName(reg, SanitizeName(p+"get_endpoint_schema"))
	invokeName := uniqueName(reg, SanitizeName(p+"invoke_endpoint"))

	reg.Register(listName, &listEndpointsExecutor{
		tp: sub,
		spec: sharedtools.ToolSpec{
			Name:        listName,
			Description: fmt.Sprintf("[forge:%s] Search the %q API's endpoint catalog. Returns matching operation ids you can then inspect with get_endpoint_schema and run with invoke_endpoint.", tp.Name, tp.Name),
			Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"optional keyword filter over operation id / path / summary / tags"}}}`),
		},
	})
	reg.Register(getName, &getEndpointSchemaExecutor{
		tp: sub,
		spec: sharedtools.ToolSpec{
			Name:        getName,
			Description: fmt.Sprintf("[forge:%s] Return one endpoint's input JSON schema by operation id.", tp.Name),
			Parameters:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"operation id from list_endpoints"}},"required":["id"]}`),
		},
	})
	reg.Register(invokeName, &invokeEndpointExecutor{
		tp:            sub,
		api:           api,
		exec:          exec,
		forceApproval: pol.ForceApproval,
		spec: sharedtools.ToolSpec{
			Name:        invokeName,
			Description: fmt.Sprintf("[forge:%s] Execute an endpoint by operation id. Mutating endpoints require approval. Pass args as an object; optionally pass jq_filter to trim the response.", tp.Name),
			Parameters:  json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"operation id"},"args":{"type":"object","description":"endpoint arguments (path/query/header params + body fields)"},"jq_filter":{"type":"string","description":"optional jq-style filter to trim the JSON response, e.g. .data[0].id"}},"required":["id"]}`),
		},
	})
	return []string{listName, getName, invokeName}
}

func describeTool(integration string, td ToolDef) string {
	s := td.Summary
	if s == "" {
		s = fmt.Sprintf("%s %s", td.Method, td.Path)
	}
	approval := ""
	if td.Mutating {
		approval = " (mutating)"
	}
	return fmt.Sprintf("[forge:%s] %s%s", integration, s, approval)
}

// withJQFilter injects a top-level "jq_filter" string property into a tool's
// input schema so the model can trim large responses before they enter context.
func withJQFilter(schema json.RawMessage) json.RawMessage {
	var obj map[string]any
	if err := json.Unmarshal(schema, &obj); err != nil {
		return schema
	}
	props, _ := obj["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	if _, exists := props["jq_filter"]; !exists {
		props["jq_filter"] = map[string]any{
			"type":        "string",
			"description": "optional jq-style filter to trim the JSON response before it is returned, e.g. .data[0].id",
		}
	}
	obj["properties"] = props
	if obj["type"] == nil {
		obj["type"] = "object"
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return schema
	}
	return out
}

func uniqueName(reg *sharedtools.ToolRegistry, base string) string {
	name := base
	for n := 2; ; n++ {
		if _, exists := reg.Get(name); !exists {
			return name
		}
		name = SanitizeName(fmt.Sprintf("%s_%d", base, n))
	}
}
