// SPDX-License-Identifier: Apache-2.0

// Package trigger holds the pure half of rysh's inbound webhook triggers
// (gap06): the Trigger model, input validation, the per-trigger secret file,
// HMAC-SHA256 signature verification, and the loopback HTTP handler. It has no
// actor or NATS dependencies, like internal/cron; the store, the ##trigger
// command and the fire itself live in internal/actors/workspace_trigger.go.
//
// A trigger is a fixed input bound to a name. A signed POST to /t/<name> only
// says "fire": the request body never reaches the agent, so the input that
// runs is the one the local user typed when creating the trigger.
package trigger
