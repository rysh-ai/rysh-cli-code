// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"crypto/rand"
	"math/big"
)

// shortIDAlphabet is lowercase base36 — easy to read out and type into a prompt.
const shortIDAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// shortIDLen is the length of a message handle (36^4 = 1,679,616 combinations,
// ample for the bounded recent-message windows these IDs label).
const shortIDLen = 4

// newShortID returns a random shortIDLen-character base36 ID (e.g. "a3f9"). The
// caller is responsible for collision-checking against whatever set it indexes.
func newShortID() string {
	b := make([]byte, shortIDLen)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(shortIDAlphabet))))
		if err != nil {
			// crypto/rand failure is effectively impossible; fall back to 'a'.
			b[i] = shortIDAlphabet[0]
			continue
		}
		b[i] = shortIDAlphabet[n.Int64()]
	}
	return string(b)
}
