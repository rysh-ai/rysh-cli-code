// SPDX-License-Identifier: Apache-2.0

package msg

import "encoding/json"

// ForgedInvokeRequest is the payload a subscriber sends to invoke a shared
// forge-origin operation (Task 2, phase 2b). It travels inside a
// MsgUpstreamCommand of CommandType "invoke_op" (Payload = JSON of this struct)
// and, on the subscriber side, as the body of the local invoke request to the
// RemoteShareListenerActor. Op is the forge tool name (e.g. "weather_getWeather");
// Args is the tool's input JSON.
type ForgedInvokeRequest struct {
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
	// Auth, when non-empty, is the SUBSCRIBER's current access token (forged-API
	// auth plan, Model B / delegated identity). The owner injects it as the bearer
	// for this one call so the backend enforces the subscriber's authorization,
	// then discards it — it is never cached, persisted, or logged. It is carried
	// out-of-band from Args (so it is excluded from arg-schema validation) and is
	// honored ONLY when the owner's share opted into delegated auth.
	Auth string `json:"auth,omitempty"`
}

// ForgedInvokeResult is the owner's reply to a ForgedInvokeRequest. It mirrors
// tools.ToolOutput's user-facing fields so the subscriber proxy can reconstruct
// a ToolOutput. Content is redacted on the owner side when the share's --redact
// is on (the default). ErrorKind uses the tools.ErrKind* taxonomy.
type ForgedInvokeResult struct {
	Content   string `json:"content,omitempty"`
	Error     string `json:"error,omitempty"`
	ErrorKind string `json:"error_kind,omitempty"`
}
