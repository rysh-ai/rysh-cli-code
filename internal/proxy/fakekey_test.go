// SPDX-License-Identifier: Apache-2.0

package proxy

import "strings"

// plantedKey is live-SHAPED but synthetic. Real shape is the point: it is what
// the SecretNAT detector tier has to catch, in a request or in a response. It is
// the same value wirecheck.PlantedSecret plants (that package imports this one,
// so the tests here cannot call it), and like it the parts are joined at run
// time: no Stripe-live key shape sits in the tree, test files included, so neither
// the private tree nor the OSS export carries one for a scanner to flag.
var plantedKey = strings.Join([]string{"sk_", "live_", "ABCD1234abcd", "5678EFGH9012"}, "")

// snatTokenPrefix starts every semantic-mode SNAT token minted for plantedKey.
var snatTokenPrefix = strings.Join([]string{"sk_", "live_", "SNAT"}, "")
