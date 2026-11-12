// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"strings"

	"github.com/rysh-ai/rysh-cli-shared/forge/ir"
	"gopkg.in/yaml.v3"
)

// httpMethods are the operation keys of an OpenAPI path item.
var httpMethods = map[string]bool{"get": true, "put": true, "post": true, "delete": true, "options": true, "head": true, "patch": true, "trace": true}

// RouteEventStreams moves every OpenAPI operation whose success (2xx)
// responses are server-sent events only (text/event-stream, no JSON) from
// api.Operations to api.Streams (boat gap #5). The unary generators would
// otherwise wrap a stream as one request/response call that never returns;
// the SDK generators give it a streaming method instead. spec is the raw
// OpenAPI document (JSON or YAML) api was ingested from. Other sources, and
// an operation with any JSON success response, are left alone. Idempotent.
//
// It lives on the rysh-cli side so rysh-shared's ingester is unchanged; the
// live tool layer (Manager) does not call it.
func RouteEventStreams(api *ir.API, spec []byte) error {
	if api == nil || api.SourceType != "openapi" {
		return nil
	}
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		return err
	}
	sse := map[string]bool{} // "METHOD /path"
	for path, item := range doc.Paths {
		for method, node := range item {
			if !httpMethods[strings.ToLower(method)] {
				continue // parameters, summary, servers, x-…
			}
			var op struct {
				Responses map[string]struct {
					Content map[string]any `yaml:"content"`
				} `yaml:"responses"`
			}
			if err := node.Decode(&op); err != nil {
				return err
			}
			stream, other := false, false
			for code, resp := range op.Responses {
				if !strings.HasPrefix(code, "2") {
					continue
				}
				for media := range resp.Content {
					mt := strings.ToLower(strings.TrimSpace(strings.SplitN(media, ";", 2)[0]))
					if mt == "text/event-stream" {
						stream = true
					} else {
						other = true
					}
				}
			}
			if stream && !other {
				sse[strings.ToUpper(method)+" "+path] = true
			}
		}
	}
	if len(sse) == 0 {
		return nil
	}
	kept := api.Operations[:0]
	for _, op := range api.Operations {
		if sse[strings.ToUpper(op.Method)+" "+op.Path] {
			api.Streams = append(api.Streams, op)
			continue
		}
		kept = append(kept, op)
	}
	api.Operations = kept
	return nil
}
