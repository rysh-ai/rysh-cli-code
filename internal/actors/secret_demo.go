// SPDX-License-Identifier: Apache-2.0

package actors

// ##secret demo <NAME> — a screen-safe stand-in for ##secret get. It prints a
// short SHA-256 fingerprint of the real value (so two people can confirm they
// hold the same secret without showing it) and a FAKE value with the same shape:
// same length, same punctuation at the same positions, and each letter/digit
// replaced by a pseudo-random one of the same class. The fake is derived from
// the fingerprint, so it is stable across runs — safe for demos, recordings and
// screenshots, where a real-looking value reads better than "****".

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

// secretFingerprint returns "sha256:" plus the first 16 hex chars of the
// value's SHA-256 digest.
func secretFingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// fakeSecret returns a deterministic, format-preserving decoy for value.
// Non-alphanumeric characters (-, _, ., :, /, …) keep their positions so the
// decoy looks like the same kind of credential; every letter or digit is
// replaced from a SHA-256 keystream seeded by the value. It never returns the
// value itself.
func fakeSecret(value string) string {
	const (
		lower  = "abcdefghijklmnopqrstuvwxyz"
		upper  = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
		digits = "0123456789"
	)
	seed := sha256.Sum256([]byte("rysh-secret-demo\x00" + value))
	var block [sha256.Size]byte
	var ctr uint64
	pos := len(block) // forces the first refill
	next := func() byte {
		if pos == len(block) {
			var buf [sha256.Size + 8]byte
			copy(buf[:], seed[:])
			binary.BigEndian.PutUint64(buf[sha256.Size:], ctr)
			block = sha256.Sum256(buf[:])
			ctr++
			pos = 0
		}
		b := block[pos]
		pos++
		return b
	}
	pick := func(set string, orig rune) rune {
		for {
			r := rune(set[int(next())%len(set)])
			if r != orig {
				return r
			}
		}
	}
	out := []rune(value)
	for i, r := range out {
		switch {
		case r >= 'a' && r <= 'z':
			out[i] = pick(lower, r)
		case r >= 'A' && r <= 'Z':
			out[i] = pick(upper, r)
		case r >= '0' && r <= '9':
			out[i] = pick(digits, r)
		}
	}
	return string(out)
}
