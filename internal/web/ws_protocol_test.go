// SPDX-License-Identifier: Apache-2.0

package web

// Golden-vector guard for the /ws protocol (E16 T2).
//
// Why this exists: on 2026-08-11 `a712be3` added source_width/source_height to
// the webpane_frame message and nothing told the TypeScript client, which kept
// four fields and silently dropped the two it needed to hit-test forwarded
// input. Nothing failed. The protocol is spoken by a Go server and several
// TypeScript clients that share no type system, so the only thing that can
// notice a one-sided change is a test that reads the server's own source.
//
// How it works: it parses the non-test sources of this package and extracts the
// real wire surface — every outbound message type with its envelope and payload
// field names, every inbound command action with its parameter names — then
// compares that to testdata/ws_protocol.golden.json. Any message or field added,
// removed or renamed changes the extract and fails here.
//
// When it fails, that is the design working. Do BOTH of these, in this order:
//  1. update docs/ws-protocol.md to describe the change, then
//  2. re-record the vector: UPDATE_WS_GOLDEN=1 GOWORK=off go test ./internal/web/ -run TestWSProtocol
//
// Step 2 alone is not enough — TestWSProtocolSpecCoverage fails if a name is in
// the golden and not in the spec, so the vector cannot be re-recorded past a
// stale document.
//
// What it does NOT cover, stated plainly so nobody over-reads a green run:
// payloads whose `data` is a Go value rather than a map literal (snapshot,
// agent_list, …) are recorded as the Go expression that produces them, so the
// SET of such messages is guarded but their interior fields are governed by the
// Go type, not by this test. Those are marked `data_go_expr` in the golden and
// carry a pointer to the owning type in the spec.

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	wsGoldenPath = "testdata/ws_protocol.golden.json"
	wsSpecPath   = "../../docs/ws-protocol.md"
)

// wsOutbound is one server → client message.
type wsOutbound struct {
	Type string `json:"type"`
	// Envelope is every top-level key of the marshalled object, sorted —
	// "type"/"data" for almost all of them, plus the odd extra (snapshot
	// carries layout_only alongside).
	Envelope []string `json:"envelope"`
	// DataFields is the payload's keys when `data` is a map literal. When a
	// message is built at several sites it is the INTERSECTION — the keys a
	// client can count on.
	DataFields []string `json:"data_fields,omitempty"`
	// DataFieldsOptional is sent by some build sites and not others, so a
	// client must treat it as possibly-absent. pane_vt's clearing frame omits
	// the VT screen; pairing_list's NATS-forwarded variant omits channel.
	DataFieldsOptional []string `json:"data_fields_optional,omitempty"`
	// DataGoExpr is the source expression when `data` is a Go value instead,
	// e.g. "r.Agents". Mutually exclusive with DataFields.
	DataGoExpr string `json:"data_go_expr,omitempty"`
}

// wsInbound is one client → server command, i.e. {"type":"command",
// "data":{"action":"<Action>","params":{<Params>}}}.
type wsInbound struct {
	Action string   `json:"action"`
	Params []string `json:"params,omitempty"`
}

type wsProtocol struct {
	Outbound []wsOutbound `json:"outbound"`
	Inbound  []wsInbound  `json:"inbound"`
}

func TestWSProtocolGoldenVector(t *testing.T) {
	got := extractWSProtocol(t, ".")

	if len(got.Outbound) == 0 || len(got.Inbound) == 0 {
		t.Fatalf("extractor found nothing (outbound=%d inbound=%d) — it has stopped "+
			"matching the source and would rubber-stamp any drift",
			len(got.Outbound), len(got.Inbound))
	}

	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal extracted protocol: %v", err)
	}
	encoded = append(encoded, '\n')

	if os.Getenv("UPDATE_WS_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(wsGoldenPath), 0o755); err != nil {
			t.Fatalf("create testdata dir: %v", err)
		}
		if err := os.WriteFile(wsGoldenPath, encoded, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("re-recorded %s (%d outbound, %d inbound) — now update docs/ws-protocol.md",
			wsGoldenPath, len(got.Outbound), len(got.Inbound))
		return
	}

	rawGolden, err := os.ReadFile(wsGoldenPath)
	if err != nil {
		t.Fatalf("read %s: %v\nRecord it with: UPDATE_WS_GOLDEN=1 GOWORK=off go test ./internal/web/ -run TestWSProtocol",
			wsGoldenPath, err)
	}
	var want wsProtocol
	if err := json.Unmarshal(rawGolden, &want); err != nil {
		t.Fatalf("parse %s: %v", wsGoldenPath, err)
	}

	reportWSDiff(t, "server → client message", "§2",
		wsOutboundKeys(want.Outbound), wsOutboundKeys(got.Outbound))
	reportWSDiff(t, "client → server command", "§3",
		wsInboundKeys(want.Inbound), wsInboundKeys(got.Inbound))
}

// TestWSProtocolSpecCoverage keeps docs/ws-protocol.md honest: re-recording the
// golden without writing the change into the spec leaves the document as the
// stale half of exactly the drift this task exists to stop.
func TestWSProtocolSpecCoverage(t *testing.T) {
	spec, err := os.ReadFile(wsSpecPath)
	if err != nil {
		t.Fatalf("read %s: %v", wsSpecPath, err)
	}
	text := string(spec)

	proto := extractWSProtocol(t, ".")
	var missing []string

	note := func(kind, name, owner string) {
		if !strings.Contains(text, name) {
			missing = append(missing, kind+" "+strconv.Quote(name)+" (of "+owner+")")
		}
	}
	for _, m := range proto.Outbound {
		note("message", m.Type, "server → client")
		for _, f := range append(append([]string{}, m.DataFields...), m.DataFieldsOptional...) {
			note("field", f, m.Type)
		}
	}
	for _, c := range proto.Inbound {
		note("command", c.Action, "client → server")
		for _, p := range c.Params {
			note("param", p, c.Action)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("docs/ws-protocol.md does not document %d name(s) the server actually speaks:\n  %s\n\n"+
			"  The spec is the contract every TypeScript client is written against, so a name that is\n"+
			"  only in the code is a name no client author will ever learn. Add each to\n"+
			"  docs/ws-protocol.md — §2 for a server → client message and its fields, §3 for a\n"+
			"  client → server command and its params — rather than deleting it from the golden.\n"+
			"  §6 is the full walkthrough.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// Diff reporting
// ---------------------------------------------------------------------------

func wsOutboundKeys(ms []wsOutbound) map[string]string {
	out := make(map[string]string, len(ms))
	for _, m := range ms {
		shape := "envelope=" + strings.Join(m.Envelope, ",")
		if m.DataGoExpr != "" {
			shape += " data=<go:" + m.DataGoExpr + ">"
		} else {
			shape += " data={" + strings.Join(m.DataFields, ",") + "}"
		}
		if len(m.DataFieldsOptional) > 0 {
			shape += " optional={" + strings.Join(m.DataFieldsOptional, ",") + "}"
		}
		out[m.Type] = shape
	}
	return out
}

func wsInboundKeys(cs []wsInbound) map[string]string {
	out := make(map[string]string, len(cs))
	for _, c := range cs {
		out[c.Action] = "params={" + strings.Join(c.Params, ",") + "}"
	}
	return out
}

// wsFixInstructions is appended to every failure. A guard that fails with a
// bare diff teaches nobody; naming the file, the section and the exact command
// turns it into the workflow for changing the protocol.
const wsFixInstructions = "\n  Fix, in this order:\n" +
	"    1. edit docs/ws-protocol.md — §2 for a server → client message, §3 for a client → server\n" +
	"       command, §6 for the full walkthrough\n" +
	"    2. re-record: UPDATE_WS_GOLDEN=1 GOWORK=off go test ./internal/web/ -run TestWSProtocol\n" +
	"    3. update the TypeScript clients: rysh-cli-app/src/types.ts and src/hooks/useWebSocket.ts,\n" +
	"       then make build-frontend. Nothing in Go will fail if you skip this."

func reportWSDiff(t *testing.T, kind, section string, want, got map[string]string) {
	t.Helper()
	for name, wantShape := range want {
		gotShape, ok := got[name]
		if !ok {
			t.Errorf("%s %q is in the golden vector but no longer in the source.\n"+
				"  If it was removed on purpose, every client still sending or expecting it breaks with\n"+
				"  silence — there is no error frame. Remove it from docs/ws-protocol.md %s too.%s",
				kind, name, section, wsFixInstructions)
			continue
		}
		if gotShape != wantShape {
			t.Errorf("%s %q changed shape:\n    golden: %s\n    source: %s\n"+
				"  A field added or renamed here is invisible to every TypeScript client until someone\n"+
				"  updates it by hand — that is exactly how webpane_frame drifted in a712be3.%s",
				kind, name, wantShape, gotShape, wsFixInstructions)
		}
	}
	for name, gotShape := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s %q exists in the source but is not in the golden vector.\n    source: %s\n"+
				"  Document it in docs/ws-protocol.md %s.%s",
				kind, name, gotShape, section, wsFixInstructions)
		}
	}
}

// ---------------------------------------------------------------------------
// Extraction
// ---------------------------------------------------------------------------

func extractWSProtocol(t *testing.T, dir string) wsProtocol {
	t.Helper()

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	pkg, ok := pkgs["web"]
	if !ok {
		t.Fatalf("package web not found in %s (found %d packages)", dir, len(pkgs))
	}

	// Named in-package struct types, by name, so `var in webPaneInputCmd`
	// contributes its json tags.
	namedStructs := map[string][]string{}
	// Fields of anonymous structs declared in a function OUTSIDE any case
	// clause: handleWebPaneCommand unmarshals pane_id/url/profile once for all
	// its cases, and delegated handlers (handleCompletionGet, handlePaneResize)
	// hold their whole payload that way.
	funcCommonFields := map[string][]string{}

	for _, file := range pkg.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			if st, ok := ts.Type.(*ast.StructType); ok {
				namedStructs[ts.Name.Name] = jsonTags(st)
			}
			return true
		})
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			funcCommonFields[fn.Name.Name] = structFieldsOutsideCases(fn.Body, namedStructs)
		}
	}
	// A second pass would be needed if a named struct is declared after first
	// use; resolve those now that every name is known.
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			funcCommonFields[fn.Name.Name] = structFieldsOutsideCases(fn.Body, namedStructs)
		}
	}

	outbound := map[string]*outboundAcc{}
	inbound := map[string]map[string]bool{}

	for _, file := range pkg.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				if m, ok := outboundFromLiteral(node); ok {
					mergeOutbound(outbound, m)
				}
			case *ast.FuncDecl:
				if node.Body != nil {
					collectInbound(node, namedStructs, funcCommonFields, inbound)
				}
			}
			return true
		})
	}

	proto := wsProtocol{}
	for msgType, a := range outbound {
		proto.Outbound = append(proto.Outbound, a.result(msgType))
	}
	sort.Slice(proto.Outbound, func(i, j int) bool { return proto.Outbound[i].Type < proto.Outbound[j].Type })

	for action, params := range inbound {
		proto.Inbound = append(proto.Inbound, wsInbound{Action: action, Params: sortedKeys(params)})
	}
	sort.Slice(proto.Inbound, func(i, j int) bool { return proto.Inbound[i].Action < proto.Inbound[j].Action })

	return proto
}

// outboundFromLiteral recognises the one shape every server → client message is
// built with: a map literal carrying a string-literal "type" key.
func outboundFromLiteral(lit *ast.CompositeLit) (wsOutbound, bool) {
	var (
		msgType  string
		envelope []string
		dataVal  ast.Expr
	)
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return wsOutbound{}, false
		}
		key, ok := stringLit(kv.Key)
		if !ok {
			return wsOutbound{}, false
		}
		envelope = append(envelope, key)
		switch key {
		case "type":
			if s, ok := stringLit(kv.Value); ok {
				msgType = s
			}
		case "data":
			dataVal = kv.Value
		}
	}
	if msgType == "" {
		return wsOutbound{}, false
	}
	sort.Strings(envelope)
	out := wsOutbound{Type: msgType, Envelope: envelope}

	if dataVal != nil {
		if inner, ok := dataVal.(*ast.CompositeLit); ok {
			if fields, ok := mapLiteralKeys(inner); ok {
				out.DataFields = fields
				return out, true
			}
		}
		out.DataGoExpr = types.ExprString(dataVal)
	}
	return out, true
}

// mapLiteralKeys returns the string keys of a map literal, or false when the
// literal is not keyed by string constants (a struct literal, a slice, …).
func mapLiteralKeys(lit *ast.CompositeLit) ([]string, bool) {
	if len(lit.Elts) == 0 {
		return nil, false
	}
	var keys []string
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return nil, false
		}
		k, ok := stringLit(kv.Key)
		if !ok {
			return nil, false
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, true
}

// outboundAcc folds the build sites of one message type. Required and optional
// have to be derived from the running union AND intersection of the sites'
// own field sets, not from the previous fold's answer: fold the optional set
// back in and a later full site silently promotes an absent field to required.
type outboundAcc struct {
	envelope  []string
	union     []string
	inter     []string
	goExprs   []string
	siteCount int
}

// mergeOutbound records one build site. Fields every site sends are required;
// fields only some sites send are optional — a client written against the union
// alone would dereference a key that is genuinely absent, which is exactly what
// pane_vt's clearing frame (no vt_screen) and pairing_list's NATS-forwarded
// variant (no channel) would hand it.
func mergeOutbound(acc map[string]*outboundAcc, m wsOutbound) {
	a, ok := acc[m.Type]
	if !ok {
		a = &outboundAcc{}
		acc[m.Type] = a
	}
	a.siteCount++
	a.envelope = union(a.envelope, m.Envelope)
	if m.DataGoExpr != "" {
		a.goExprs = dedupe(append(a.goExprs, m.DataGoExpr))
	}
	if a.siteCount == 1 {
		a.union, a.inter = m.DataFields, m.DataFields
		return
	}
	a.inter = intersect(a.inter, m.DataFields)
	a.union = union(a.union, m.DataFields)
}

func (a *outboundAcc) result(msgType string) wsOutbound {
	out := wsOutbound{
		Type:               msgType,
		Envelope:           a.envelope,
		DataFields:         a.inter,
		DataFieldsOptional: subtract(a.union, a.inter),
	}
	if len(a.goExprs) > 0 {
		out.DataGoExpr = strings.Join(a.goExprs, " | ")
	}
	return out
}

// collectInbound walks a `switch action {` and records each case's action names
// with the payload fields the handler reads for them.
func collectInbound(fn *ast.FuncDecl, namedStructs, funcCommonFields map[string][]string, acc map[string]map[string]bool) {
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		ident, ok := sw.Tag.(*ast.Ident)
		if !ok || ident.Name != "action" {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc, ok := stmt.(*ast.CaseClause)
			if !ok || len(cc.List) == 0 {
				continue // default: — carries no action name of its own
			}
			fields := append([]string{}, funcCommonFields[fn.Name.Name]...)
			for _, s := range cc.Body {
				fields = append(fields, structFieldsIn(s, namedStructs)...)
			}
			// Follow delegation: handleClientCommand hands completion_get and
			// pane_resize to helpers that hold the real payload struct.
			for _, callee := range calledFuncs(cc.Body) {
				if f, ok := funcCommonFields[callee]; ok && callee != fn.Name.Name {
					fields = append(fields, f...)
				}
			}
			for _, expr := range cc.List {
				name, ok := stringLit(expr)
				if !ok {
					continue
				}
				if acc[name] == nil {
					acc[name] = map[string]bool{}
				}
				for _, f := range fields {
					acc[name][f] = true
				}
			}
		}
		return true
	})
}

// structFieldsOutsideCases collects json tags of struct types declared in a
// body but not inside a case clause.
func structFieldsOutsideCases(body *ast.BlockStmt, namedStructs map[string][]string) []string {
	var fields []string
	for _, stmt := range body.List {
		if _, isSwitch := stmt.(*ast.SwitchStmt); isSwitch {
			continue
		}
		fields = append(fields, structFieldsIn(stmt, namedStructs)...)
	}
	return dedupe(fields)
}

// structFieldsIn collects json tags from anonymous struct types declared under
// a node, plus the tags of any in-package named struct declared via `var x T`.
func structFieldsIn(n ast.Node, namedStructs map[string][]string) []string {
	var fields []string
	ast.Inspect(n, func(node ast.Node) bool {
		switch v := node.(type) {
		case *ast.StructType:
			fields = append(fields, jsonTags(v)...)
		case *ast.ValueSpec:
			if id, ok := v.Type.(*ast.Ident); ok {
				if f, ok := namedStructs[id.Name]; ok {
					fields = append(fields, f...)
				}
			}
		}
		return true
	})
	return fields
}

// calledFuncs returns the names of functions/methods invoked under a node.
func calledFuncs(stmts []ast.Stmt) []string {
	var names []string
	for _, s := range stmts {
		ast.Inspect(s, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch f := call.Fun.(type) {
			case *ast.Ident:
				names = append(names, f.Name)
			case *ast.SelectorExpr:
				names = append(names, f.Sel.Name)
			}
			return true
		})
	}
	return names
}

// jsonTags returns the wire names of a struct's fields, skipping `json:"-"`.
func jsonTags(st *ast.StructType) []string {
	var out []string
	for _, f := range st.Fields.List {
		if f.Tag == nil {
			continue
		}
		tag, err := strconv.Unquote(f.Tag.Value)
		if err != nil {
			continue
		}
		name := strings.Split(reflect.StructTag(tag).Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	return out
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func union(a, b []string) []string { return dedupe(append(append([]string{}, a...), b...)) }

func intersect(a, b []string) []string {
	inB := map[string]bool{}
	for _, s := range b {
		inB[s] = true
	}
	var out []string
	for _, s := range a {
		if inB[s] {
			out = append(out, s)
		}
	}
	return dedupe(out)
}

func subtract(a, b []string) []string {
	inB := map[string]bool{}
	for _, s := range b {
		inB[s] = true
	}
	var out []string
	for _, s := range a {
		if !inB[s] {
			out = append(out, s)
		}
	}
	return dedupe(out)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
