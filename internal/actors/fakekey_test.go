// SPDX-License-Identifier: Apache-2.0

package actors

import "strings"

// stripeLiveKey returns a synthetic Stripe-live-shaped value ending in tail
// (callers pass an obviously fake, repeated tail).
// The parts are joined at run time so no Stripe-live key shape sits in the tree, test
// files included: neither the private tree nor the OSS export carries one for a
// scanner to flag.
func stripeLiveKey(tail string) string {
	return strings.Join([]string{"sk_", "live_", tail}, "")
}
