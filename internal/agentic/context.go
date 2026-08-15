// SPDX-License-Identifier: Apache-2.0

package agentic

import (
	"context"
	"time"
)

// newContext is a tiny helper that creates a context.WithTimeout from a
// background context. Wrapped so callers like envblock.go can stay free of
// direct context imports / boilerplate.
func newContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}
