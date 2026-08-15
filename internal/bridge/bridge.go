// SPDX-License-Identifier: Apache-2.0

// Package bridge provides NATSBridge for delivering NATS messages to actor mailboxes.
// This package re-exports rysh-shared/bridge so both rysh-cli and rysh-server
// use the same NATSBridge implementation.
package bridge

import sharedbridge "github.com/rysh-ai/rysh-cli-shared/bridge"

// NATSBridge delivers NATS messages to an actor's mailbox.
// This is a type alias — it IS rysh-shared/bridge.NATSBridge.
type NATSBridge = sharedbridge.NATSBridge

// New creates a NATSBridge subscribed to the actor's inbox.
var New = sharedbridge.New
