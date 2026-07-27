package msg

import sharedmsg "github.com/rysh-ai/rysh-cli-shared/msg"

// T constructs a NATS subject by joining the session prefix with the given parts.
// Delegates to rysh-shared/msg so that the session prefix is consistent
// across all publishers (CLI actors and shared agentic actors).
var T = sharedmsg.T

// SetSessionPrefix sets the NATS subject prefix for this session.
// All subjects constructed by T() and by rysh-shared publishers will use this prefix.
var SetSessionPrefix = sharedmsg.SetSessionPrefix

// SessionPrefix returns the current NATS subject prefix.
var SessionPrefix = sharedmsg.SessionPrefix
