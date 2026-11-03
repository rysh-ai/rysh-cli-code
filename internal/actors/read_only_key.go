// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"errors"
	"log/slog"
	"strings"

	"github.com/nats-io/nats.go"
)

// A key without write access (an observer member's key, a Company Connect key,
// a user-made read key) may watch a share but not drive one. The upstream proxy
// answers each publish or subscription such a key may not make with
//
//	-ERR 'Permissions Violation for Publish to "<subject>": this key is read-only'
//
// and keeps the connection open. nats.go reports that as an
// ErrPermissionViolation to the connection's async error handler and keeps it
// as the connection's LastError. The share actors turn the first one into a
// single notice in the pane, instead of a stream of refused publishes.

// readOnlyKeyMarker is the reason the proxy appends to a read-only refusal.
const readOnlyKeyMarker = "this key is read-only"

// isReadOnlyKeyRefusal reports whether err is the proxy refusing an operation
// because the connection's key is read-only. Any other permissions violation
// (a subject outside the workspace, a relay held elsewhere) is not one.
func isReadOnlyKeyRefusal(err error) bool {
	return err != nil && errors.Is(err, nats.ErrPermissionViolation) &&
		strings.Contains(strings.ToLower(err.Error()), readOnlyKeyMarker)
}

// readOnlyKeyErrorHandler is the async error handler for an upstream share
// connection: a read-only refusal calls onReadOnly (from nats.go's callback
// goroutine), anything else is logged at debug level only, as it was before
// this handler existed.
func readOnlyKeyErrorHandler(label string, onReadOnly func()) nats.Option {
	return nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
		if isReadOnlyKeyRefusal(err) {
			onReadOnly()
			return
		}
		slog.Debug(label+": upstream async error", "err", err)
	})
}

// msgUpstreamReadOnlyKey tells an owner-side actor (##share, ##upstream) that
// the upstream refused it as read-only. Sent to the actor's own mailbox from
// readOnlyKeyErrorHandler.
type msgUpstreamReadOnlyKey struct{}
