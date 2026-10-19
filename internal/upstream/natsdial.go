// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"net/http"

	"github.com/nats-io/nats.go"
)

// APIKeyHeader is the header rysh-server's transparent NATS proxy reads the
// workspace API key from on the WebSocket upgrade.
const APIKeyHeader = "X-API-Key"

// NATSAPIKeyHeader is the dial option that authenticates an upstream NATS
// connection through rysh-server's transparent proxy
// (/workspaces/<ws>/nats): the workspace API key rides in the X-API-Key
// upgrade header, sent on the first connect and every reconnect — never as
// ?api_key= in the URL, which access logs record (F-86). rysh-server has read
// the header there since its first commit, so this works against every
// server version. An empty key adds nothing.
func NATSAPIKeyHeader(apiKey string) nats.Option {
	return func(o *nats.Options) error {
		if apiKey == "" {
			return nil
		}
		headers := o.WebSocketConnectionHeaders.Clone()
		if headers == nil {
			headers = http.Header{}
		}
		headers.Set(APIKeyHeader, apiKey)
		o.WebSocketConnectionHeaders = headers
		return nil
	}
}
