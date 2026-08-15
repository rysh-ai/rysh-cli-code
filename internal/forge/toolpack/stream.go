// SPDX-License-Identifier: Apache-2.0

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

// Streaming tool surface (design 015 §2.1/§2.2). Streams are exposed through
// TWO constant-footprint tools per integration, mirroring the built-in
// bash_background / bash_session contract exactly so the model already knows
// the ergonomics:
//
//	gRPC:    <prefix>stream_start      (start a server-streaming method → session id)
//	         <prefix>stream_session    (action: read | list | stop)
//	GraphQL: <prefix>graphql_subscribe (start a subscription → session id)
//	         <prefix>stream_session    (same session tool, same semantics)
//
// This matches the existing forge preference for constant context footprint
// (dynamic REST = 3 meta-tools, GraphQL = 2 tools): a service with 40
// server-streaming methods still costs 2 tools. Approval mirrors the unary
// forge convention — invoke-side gating by the target operation's Mutating
// flag (or Policy.ForceApproval), and the session tool is approval-free like
// bash_session (read/list/stop are bounded to sessions this manager started).

// RegisterGRPCStreams installs the stream_start / stream_session pair for an
// API with server-streaming methods (api.Streams). No-op (nil) when the API
// has none. Returns the registered tool names.
func RegisterGRPCStreams(reg *sharedtools.ToolRegistry, api *ir.API, exec *runtime.HTTPExecutor, sm *runtime.StreamManager, pol Policy) []string {
	if len(api.Streams) == 0 {
		return nil
	}
	p := pol.Prefix
	startName := uniqueName(reg, SanitizeName(p+"stream_start"))
	sessName := uniqueName(reg, SanitizeName(p+"stream_session"))

	ids := make([]string, 0, len(api.Streams))
	for i := range api.Streams {
		ids = append(ids, api.Streams[i].ID)
	}
	sort.Strings(ids)

	reg.Register(startName, &grpcStreamStartExecutor{
		api:           api,
		exec:          exec,
		sm:            sm,
		sessName:      sessName,
		forceApproval: pol.ForceApproval,
		spec: sharedtools.ToolSpec{
			Name: startName,
			Description: fmt.Sprintf("[forge:%s] Start a server-streaming gRPC method as a background session and return a session ID. Frames are captured into a ring buffer; read them with %s(action:\"read\"). Available streams: %s. Requires a JSON-over-HTTP bridge (grpc-gateway NDJSON).",
				api.Name, sessName, capList(ids, 10)),
			Parameters: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"the streaming method id to start"},"args":{"type":"object","description":"the request message fields"}},"required":["id"]}`),
		},
	})
	registerStreamSession(reg, sessName, api.Name, sm)
	return []string{startName, sessName}
}

// registerGraphQLStreams installs graphql_subscribe / stream_session for an API
// with subscription fields. Returns the registered tool names (nil when none).
func registerGraphQLStreams(reg *sharedtools.ToolRegistry, api *ir.API, exec *runtime.GraphQLExecutor, sm *runtime.StreamManager, pol Policy) []string {
	if len(api.Streams) == 0 {
		return nil
	}
	p := pol.Prefix
	subName := uniqueName(reg, SanitizeName(p+"graphql_subscribe"))
	sessName := uniqueName(reg, SanitizeName(p+"stream_session"))

	reg.Register(subName, &gqlSubscribeExecutor{
		api:           api,
		exec:          exec,
		sm:            sm,
		sessName:      sessName,
		forceApproval: pol.ForceApproval,
		spec: sharedtools.ToolSpec{
			Name: subName,
			Description: fmt.Sprintf("[forge:%s] Start a GraphQL subscription as a background session (graphql-transport-ws) and return a session ID. 'next' events are captured into a ring buffer; read them with %s(action:\"read\"). Pass a full subscription document, e.g. 'subscription { onEvent { id } }'.",
				api.Name, sessName),
			Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"the GraphQL subscription document"},"variables":{"type":"object","description":"GraphQL variables object"}},"required":["query"]}`),
		},
	})
	registerStreamSession(reg, sessName, api.Name, sm)
	return []string{subName, sessName}
}

// registerStreamSession installs the shared read/list/stop session tool.
func registerStreamSession(reg *sharedtools.ToolRegistry, name, apiName string, sm *runtime.StreamManager) {
	reg.Register(name, &streamSessionExecutor{
		sm:      sm,
		apiName: apiName,
		spec: sharedtools.ToolSpec{
			Name: name,
			Description: fmt.Sprintf("[forge:%s] Manage streaming sessions. action=read returns the frames received since your last read; action=list lists sessions; action=stop cancels a session and returns its final unread frames. Idle sessions expire after %s.",
				apiName, runtime.DefaultStreamIdleTTL),
			Parameters: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["read","list","stop"],"description":"read=new frames since last read; list=all sessions; stop=cancel a session"},"session_id":{"type":"string","description":"session ID (required for read/stop)"},"max_frames":{"type":"integer","description":"read action: cap on frames returned (default 50)"}},"required":["action"]}`),
		},
	})
}

// grpcStreamStartExecutor implements <prefix>stream_start.
type grpcStreamStartExecutor struct {
	api           *ir.API
	exec          *runtime.HTTPExecutor
	sm            *runtime.StreamManager
	sessName      string
	forceApproval bool
	spec          sharedtools.ToolSpec
}

func (e *grpcStreamStartExecutor) Spec() sharedtools.ToolSpec { return e.spec }

// RequiresApproval mirrors invoke_endpoint: gate by the target stream method's
// Mutating flag (AIP-130 heuristic; Watch/List-style streams are reads), or
// everything under ForceApproval. Unknown target → be safe.
func (e *grpcStreamStartExecutor) RequiresApproval(params json.RawMessage) bool {
	if e.forceApproval {
		return true
	}
	var p struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(params, &p)
	if op := e.api.StreamByID(p.ID); op != nil {
		return op.Mutating
	}
	return true
}

func (e *grpcStreamStartExecutor) Execute(_ context.Context, params json.RawMessage) (*sharedtools.ToolOutput, error) {
	var p struct {
		ID   string         `json:"id"`
		Args map[string]any `json:"args"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.ID == "" {
		return &sharedtools.ToolOutput{Error: "parameter 'id' (streaming method id) is required"}, nil
	}
	op := e.api.StreamByID(p.ID)
	if op == nil {
		return &sharedtools.ToolOutput{Error: fmt.Sprintf("unknown streaming method %q (see the tool description for the available streams)", p.ID)}, nil
	}
	if p.Args == nil {
		p.Args = map[string]any{}
	}
	id, err := e.exec.StartServerStream(e.sm, op, p.Args)
	if err != nil {
		return &sharedtools.ToolOutput{Error: err.Error()}, nil
	}
	return &sharedtools.ToolOutput{
		Content: startedMessage(id, op.ID, e.sessName),
		Metadata: map[string]string{
			"integration": e.api.Name, "operation": op.ID, "session_id": id, "kind": "grpc-stream",
		},
	}, nil
}

// gqlSubscribeExecutor implements <prefix>graphql_subscribe.
type gqlSubscribeExecutor struct {
	api           *ir.API
	exec          *runtime.GraphQLExecutor
	sm            *runtime.StreamManager
	sessName      string
	forceApproval bool
	spec          sharedtools.ToolSpec
}

func (e *gqlSubscribeExecutor) Spec() sharedtools.ToolSpec { return e.spec }

// RequiresApproval: subscriptions are reads (mirroring graphql_query, which
// gates only mutations), so approval is required only under ForceApproval.
func (e *gqlSubscribeExecutor) RequiresApproval(json.RawMessage) bool { return e.forceApproval }

func (e *gqlSubscribeExecutor) Execute(_ context.Context, params json.RawMessage) (*sharedtools.ToolOutput, error) {
	var p struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal(params, &p); err != nil || strings.TrimSpace(p.Query) == "" {
		return &sharedtools.ToolOutput{Error: "parameter 'query' (a GraphQL subscription document) is required"}, nil
	}
	if !isGraphQLSubscription(p.Query) {
		return &sharedtools.ToolOutput{Error: "the document must be a subscription (starts with 'subscription'); use graphql_query for queries and mutations"}, nil
	}
	id, err := e.exec.Subscribe(e.sm, p.Query, p.Variables)
	if err != nil {
		return &sharedtools.ToolOutput{Error: err.Error()}, nil
	}
	return &sharedtools.ToolOutput{
		Content: startedMessage(id, subscriptionSummary(p.Query), e.sessName),
		Metadata: map[string]string{
			"integration": e.api.Name, "session_id": id, "kind": "graphql-subscription",
		},
	}, nil
}

// streamSessionExecutor implements the shared <prefix>stream_session tool.
type streamSessionExecutor struct {
	sm      *runtime.StreamManager
	apiName string
	spec    sharedtools.ToolSpec
}

func (e *streamSessionExecutor) Spec() sharedtools.ToolSpec            { return e.spec }
func (e *streamSessionExecutor) RequiresApproval(json.RawMessage) bool { return false }

func (e *streamSessionExecutor) Execute(_ context.Context, params json.RawMessage) (*sharedtools.ToolOutput, error) {
	var p struct {
		Action    string `json:"action"`
		SessionID string `json:"session_id"`
		MaxFrames int    `json:"max_frames"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return &sharedtools.ToolOutput{Error: "invalid stream_session params"}, nil
	}
	switch p.Action {
	case "read":
		if p.SessionID == "" {
			return e.list()
		}
		res, err := e.sm.Poll(p.SessionID, p.MaxFrames)
		if err != nil {
			return &sharedtools.ToolOutput{Error: err.Error()}, nil
		}
		return &sharedtools.ToolOutput{
			Content:  runtime.RenderPoll(p.SessionID, res),
			Metadata: map[string]string{"integration": e.apiName, "session_id": p.SessionID},
		}, nil
	case "list":
		return e.list()
	case "stop":
		if p.SessionID == "" {
			return &sharedtools.ToolOutput{Error: "session_id is required for action=stop"}, nil
		}
		res, err := e.sm.Stop(p.SessionID)
		if err != nil {
			return &sharedtools.ToolOutput{Error: err.Error()}, nil
		}
		content := fmt.Sprintf("Session %s stopped.", p.SessionID)
		if len(res.Frames) > 0 {
			content += fmt.Sprintf("\nFinal unread frames:\n%s", runtime.RenderPoll(p.SessionID, res))
		}
		return &sharedtools.ToolOutput{
			Content:  content,
			Metadata: map[string]string{"integration": e.apiName, "session_id": p.SessionID},
		}, nil
	default:
		return &sharedtools.ToolOutput{Error: fmt.Sprintf("unknown action %q (use read|list|stop)", p.Action)}, nil
	}
}

func (e *streamSessionExecutor) list() (*sharedtools.ToolOutput, error) {
	infos := e.sm.List()
	if len(infos) == 0 {
		return &sharedtools.ToolOutput{Content: "No streaming sessions."}, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Streaming sessions (%d):\n", len(infos))
	for _, in := range infos {
		status := "running"
		if in.Done {
			status = "ended"
			if in.EndReason != "" {
				status = "ended: " + in.EndReason
			}
		}
		fmt.Fprintf(&sb, "  %s  %-21s  frames:%d (unread %d)  %s  %s\n",
			in.ID, in.Kind, in.Frames, in.Pending, status, in.Label)
	}
	return &sharedtools.ToolOutput{Content: sb.String()}, nil
}

// startedMessage mirrors the bash_background start message shape.
func startedMessage(id, what, sessName string) string {
	return fmt.Sprintf("Streaming session started: %s\nStream: %s\nUse %s(action: \"read\", session_id: \"%s\") to read new frames.\nUse %s(action: \"stop\", session_id: \"%s\") to stop it.",
		id, what, sessName, id, sessName, id)
}

// isGraphQLSubscription reports whether a document's first operation is a
// subscription (skipping leading comment lines, like isGraphQLMutation).
func isGraphQLSubscription(query string) bool {
	s := strings.TrimSpace(query)
	for strings.HasPrefix(s, "#") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = strings.TrimSpace(s[i+1:])
		} else {
			break
		}
	}
	return strings.HasPrefix(strings.ToLower(s), "subscription")
}

// subscriptionSummary is a short label for the start message.
func subscriptionSummary(query string) string {
	s := strings.Join(strings.Fields(query), " ")
	if len(s) > 80 {
		s = s[:79] + "…"
	}
	return s
}

// capList joins up to max ids, summarizing the remainder.
func capList(ids []string, max int) string {
	if len(ids) <= max {
		return strings.Join(ids, ", ")
	}
	return strings.Join(ids[:max], ", ") + fmt.Sprintf(", … (+%d more)", len(ids)-max)
}
