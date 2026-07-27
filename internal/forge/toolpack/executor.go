package toolpack

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"

	"github.com/rysh-ai/rysh-cli-code/internal/forge/ir"
	"github.com/rysh-ai/rysh-cli-code/internal/forge/runtime"
)

// opExecutor backs a single operation exposed as one tool. It implements
// sharedtools.ToolExecutor so it registers into the agent registry like any
// native tool and flows through the approval/redaction/audit path.
type opExecutor struct {
	exec     *runtime.HTTPExecutor
	api      *ir.API
	op       *ir.Operation
	spec     sharedtools.ToolSpec
	approval bool
}

func (e *opExecutor) Spec() sharedtools.ToolSpec            { return e.spec }
func (e *opExecutor) RequiresApproval(json.RawMessage) bool { return e.approval }

func (e *opExecutor) Execute(ctx context.Context, params json.RawMessage) (*sharedtools.ToolOutput, error) {
	args, jq := splitArgs(params)
	out, err := e.exec.Call(ctx, e.op, args, jq)
	if err != nil {
		return &sharedtools.ToolOutput{Error: err.Error()}, nil
	}
	return &sharedtools.ToolOutput{Content: out, Metadata: map[string]string{
		"integration": e.api.Name, "operation": e.op.ID,
	}}, nil
}

// ---- dynamic meta-tools (constant context footprint regardless of API size) ----

// listEndpointsExecutor implements "<prefix>list_endpoints": search the catalog.
type listEndpointsExecutor struct {
	tp   *ToolPack
	spec sharedtools.ToolSpec
}

func (e *listEndpointsExecutor) Spec() sharedtools.ToolSpec            { return e.spec }
func (e *listEndpointsExecutor) RequiresApproval(json.RawMessage) bool { return false }

func (e *listEndpointsExecutor) Execute(_ context.Context, params json.RawMessage) (*sharedtools.ToolOutput, error) {
	var p struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal(params, &p)
	q := strings.ToLower(strings.TrimSpace(p.Query))

	type row struct{ ID, Method, Path, Summary string }
	var rows []row
	for _, t := range e.tp.Tools {
		hay := strings.ToLower(t.OperationID + " " + t.Path + " " + t.Summary + " " + strings.Join(t.Tags, " "))
		if q == "" || strings.Contains(hay, q) {
			rows = append(rows, row{t.OperationID, t.Method, t.Path, t.Summary})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d endpoint(s) in %q", len(rows), e.tp.Name)
	if q != "" {
		fmt.Fprintf(&sb, " matching %q", p.Query)
	}
	sb.WriteString(":\n")
	for _, r := range rows {
		fmt.Fprintf(&sb, "  %-32s %-6s %s — %s\n", r.ID, r.Method, r.Path, r.Summary)
	}
	sb.WriteString("\nUse get_endpoint_schema(id) for input details, then invoke_endpoint(id, args).")
	return &sharedtools.ToolOutput{Content: sb.String()}, nil
}

// getEndpointSchemaExecutor implements "<prefix>get_endpoint_schema": return one
// operation's input schema on demand.
type getEndpointSchemaExecutor struct {
	tp   *ToolPack
	spec sharedtools.ToolSpec
}

func (e *getEndpointSchemaExecutor) Spec() sharedtools.ToolSpec            { return e.spec }
func (e *getEndpointSchemaExecutor) RequiresApproval(json.RawMessage) bool { return false }

func (e *getEndpointSchemaExecutor) Execute(_ context.Context, params json.RawMessage) (*sharedtools.ToolOutput, error) {
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.ID == "" {
		return &sharedtools.ToolOutput{Error: "parameter 'id' (operation id) is required"}, nil
	}
	td := e.tp.find(p.ID)
	if td == nil {
		return &sharedtools.ToolOutput{Error: fmt.Sprintf("unknown endpoint %q (use list_endpoints)", p.ID)}, nil
	}
	schema, _ := json.MarshalIndent(json.RawMessage(td.InputSchema), "", "  ")
	approval := ""
	if td.Mutating {
		approval = "  (mutating — requires approval)"
	}
	out := fmt.Sprintf("%s  %s %s%s\n%s\n\nInput schema:\n%s",
		td.OperationID, td.Method, td.Path, approval, td.Summary, schema)
	return &sharedtools.ToolOutput{Content: out}, nil
}

// invokeEndpointExecutor implements "<prefix>invoke_endpoint": execute an
// operation by id with args + optional jq_filter. Approval is decided per-call
// by inspecting whether the *target* operation mutates.
type invokeEndpointExecutor struct {
	tp            *ToolPack
	api           *ir.API
	exec          *runtime.HTTPExecutor
	spec          sharedtools.ToolSpec
	forceApproval bool
}

func (e *invokeEndpointExecutor) Spec() sharedtools.ToolSpec { return e.spec }

// RequiresApproval inspects the call's target operation: mutating endpoints (or
// any when forceApproval) require approval; reads do not.
func (e *invokeEndpointExecutor) RequiresApproval(params json.RawMessage) bool {
	if e.forceApproval {
		return true
	}
	var p struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(params, &p)
	if op := e.api.OperationByID(p.ID); op != nil {
		return op.Mutating
	}
	return true // unknown target → be safe
}

func (e *invokeEndpointExecutor) Execute(ctx context.Context, params json.RawMessage) (*sharedtools.ToolOutput, error) {
	var p struct {
		ID       string          `json:"id"`
		Args     json.RawMessage `json:"args"`
		JQFilter string          `json:"jq_filter"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.ID == "" {
		return &sharedtools.ToolOutput{Error: "parameters 'id' (operation id) and 'args' are required"}, nil
	}
	op := e.api.OperationByID(p.ID)
	if op == nil {
		return &sharedtools.ToolOutput{Error: fmt.Sprintf("unknown endpoint %q (use list_endpoints)", p.ID)}, nil
	}
	args := map[string]any{}
	if len(p.Args) > 0 {
		_ = json.Unmarshal(p.Args, &args)
	}
	out, err := e.exec.Call(ctx, op, args, p.JQFilter)
	if err != nil {
		return &sharedtools.ToolOutput{Error: err.Error()}, nil
	}
	return &sharedtools.ToolOutput{Content: out, Metadata: map[string]string{
		"integration": e.api.Name, "operation": op.ID,
	}}, nil
}

func (tp *ToolPack) find(id string) *ToolDef {
	for i := range tp.Tools {
		if tp.Tools[i].OperationID == id {
			return &tp.Tools[i]
		}
	}
	return nil
}

// splitArgs separates a tool call's body into operation args and an optional
// top-level "jq_filter" response trimmer (present on every generated call).
func splitArgs(params json.RawMessage) (map[string]any, string) {
	all := map[string]any{}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &all)
	}
	jq := ""
	if v, ok := all["jq_filter"]; ok {
		if s, ok := v.(string); ok {
			jq = s
		}
		delete(all, "jq_filter")
	}
	return all, jq
}
