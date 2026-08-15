// SPDX-License-Identifier: Apache-2.0

// Package tools registry.go — re-exports rysh-shared/tools types as aliases.
// All CLI-specific tools (bash.go, file_read.go, etc.) implement the shared
// ToolExecutor interface and use the shared ToolRegistry/ToolSpec/ToolOutput types.
package tools

import sharedtools "github.com/rysh-ai/rysh-cli-shared/tools"

// Type aliases — same underlying types as rysh-shared/tools.
type ToolRegistry = sharedtools.ToolRegistry
type ToolExecutor = sharedtools.ToolExecutor
type ToolSpec = sharedtools.ToolSpec
type ToolOutput = sharedtools.ToolOutput
type ToolConfig = sharedtools.ToolConfig

// BrowserActionPublisher is the publisher interface the browser_action tool needs.
type BrowserActionPublisher = sharedtools.BrowserActionPublisher

// NewToolRegistry creates a new empty ToolRegistry.
var NewToolRegistry = sharedtools.NewToolRegistry

// NewBrowserActionTool re-exports the shared browser_action tool (drives a
// web-mode pane's embedded browser via NATS browser.request/response).
var NewBrowserActionTool = sharedtools.NewBrowserActionTool

// NewChildRegistry creates a registry that reads the given parent live (own
// tools override). Used so per-pane/per-agent registries pick up tools
// enabled/disabled on the shared registry after creation.
var NewChildRegistry = sharedtools.NewChildRegistry

// Re-exported helpers for typed tool errors. Follow-up item 4.
var (
	ErrOutput  = sharedtools.ErrOutput
	ErrOutputf = sharedtools.ErrOutputf
)

// Re-exported error-kind constants. Same string values as the shared
// package's source-of-truth so the wire format is identical.
const (
	ErrKindValidation       = sharedtools.ErrKindValidation
	ErrKindMissing          = sharedtools.ErrKindMissing
	ErrKindPermissionDenied = sharedtools.ErrKindPermissionDenied
	ErrKindTimeout          = sharedtools.ErrKindTimeout
	ErrKindTransient        = sharedtools.ErrKindTransient
	ErrKindLoopBlocked      = sharedtools.ErrKindLoopBlocked
	ErrKindStaleRead        = sharedtools.ErrKindStaleRead
	ErrKindCancelled        = sharedtools.ErrKindCancelled
	ErrKindInternal         = sharedtools.ErrKindInternal
)
