// SPDX-License-Identifier: Apache-2.0

package channels

// WeCom callback cryptography (企业微信 消息加解密).
//
// Unlike WeChat Official Accounts, where "safe mode" is a switch, WeCom
// encrypts EVERY callback — including the echostr of the URL-verification
// handshake. There is no plaintext mode to fall back to, so this file is not
// optional hardening: without it the channel cannot complete setup.
//
// The scheme, per developer.work.weixin.qq.com/document/path/90968:
//
//	AESKey    = Base64Decode(EncodingAESKey + "=")   // 43 chars → 32 bytes
//	cipher    = AES-256-CBC, IV = AESKey[:16]
//	padding   = PKCS#7 to a multiple of 32
//	plaintext = random(16) ‖ msgLen(4, big-endian) ‖ msg ‖ receiveid
//	msg_signature = sha1(sort(token, timestamp, nonce, msg_encrypt))
//
// Two details are easy to get subtly wrong and are called out where they are
// implemented: the length field is NETWORK byte order (big-endian), and the
// signature is computed over the ENCRYPTED body, not the decrypted one — so it
// must be checked before anything is decrypted.

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// weComAESKeyLen is the decoded AES-256 key length.
const weComAESKeyLen = 32

// weComRandPrefixLen is the random prefix every plaintext carries.
const weComRandPrefixLen = 16

// weComPadBlock is the PKCS#7 block size WeCom pads to. Note this is 32, NOT
// the AES block size of 16 — the spec pads to a multiple of 32.
const weComPadBlock = 32

// decodeWeComAESKey turns the 43-character EncodingAESKey into the 32-byte AES
// key. The trailing "=" is what makes the 43 characters valid base64.
func decodeWeComAESKey(encodingAESKey string) ([]byte, error) {
	k := strings.TrimSpace(encodingAESKey)
	if len(k) != 43 {
		return nil, fmt.Errorf("encoding_aes_key must be exactly 43 characters, got %d", len(k))
	}
	key, err := base64.StdEncoding.DecodeString(k + "=")
	if err != nil {
		return nil, fmt.Errorf("encoding_aes_key is not valid base64: %w", err)
	}
	if len(key) != weComAESKeyLen {
		return nil, fmt.Errorf("encoding_aes_key decoded to %d bytes, want %d", len(key), weComAESKeyLen)
	}
	return key, nil
}

// weComSignature computes msg_signature over the ENCRYPTED body: the token,
// timestamp, nonce and ciphertext are sorted lexicographically, concatenated
// and SHA1'd.
func weComSignature(token, timestamp, nonce, encrypted string) string {
	parts := []string{token, timestamp, nonce, encrypted}
	sort.Strings(parts)
	sum := sha1.Sum([]byte(strings.Join(parts, "")))
	return hex.EncodeToString(sum[:])
}

// weComSignatureValid checks a received msg_signature in constant time.
//
// It is compared with subtle.ConstantTimeCompare rather than == because this is
// the only thing standing between a public endpoint and a forged callback, and
// an early-exit comparison leaks how much of a guess was right.
//
// An empty token never validates: a caller who has not configured one must not
// end up with an endpoint that accepts anything.
func weComSignatureValid(token, timestamp, nonce, encrypted, got string) bool {
	if token == "" || got == "" {
		return false
	}
	want := weComSignature(token, timestamp, nonce, encrypted)
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(got)), []byte(want)) == 1
}

// weComDecrypt decrypts a base64 ciphertext and returns the inner message.
//
// receiveid is the corpid; when non-empty it is checked against the trailer, so
// a payload encrypted for a different corp is rejected rather than processed.
func weComDecrypt(aesKey []byte, encrypted, receiveid string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return nil, fmt.Errorf("ciphertext is not valid base64: %w", err)
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext length %d is not a multiple of the AES block size", len(raw))
	}

	plain := make([]byte, len(raw))
	cipher.NewCBCDecrypter(block, aesKey[:aes.BlockSize]).CryptBlocks(plain, raw)

	plain, err = pkcs7Unpad(plain)
	if err != nil {
		return nil, err
	}
	// random(16) ‖ msgLen(4) ‖ msg ‖ receiveid
	if len(plain) < weComRandPrefixLen+4 {
		return nil, fmt.Errorf("plaintext too short (%d bytes) to hold the header", len(plain))
	}
	// Big-endian: the spec says network byte order, and reading this as
	// little-endian yields a wildly wrong length that fails the bounds check
	// below rather than corrupting silently — but only because it is checked.
	msgLen := int(binary.BigEndian.Uint32(plain[weComRandPrefixLen : weComRandPrefixLen+4]))
	start := weComRandPrefixLen + 4
	if msgLen < 0 || start+msgLen > len(plain) {
		return nil, fmt.Errorf("declared message length %d overruns the %d-byte plaintext", msgLen, len(plain))
	}

	msg := plain[start : start+msgLen]
	if receiveid != "" {
		gotID := string(plain[start+msgLen:])
		if gotID != receiveid {
			return nil, fmt.Errorf("payload is addressed to %q, not this corp", gotID)
		}
	}
	return msg, nil
}

// weComEncrypt builds a ciphertext in WeCom's format. Outbound callbacks do not
// need it, but the URL-verification round trip and the tests do, and having
// both halves in one place is what makes the layout verifiable.
func weComEncrypt(aesKey, msg []byte, receiveid string) (string, error) {
	prefix := make([]byte, weComRandPrefixLen)
	if _, err := rand.Read(prefix); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	buf.Write(prefix)
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(msg)))
	buf.Write(lenBuf)
	buf.Write(msg)
	buf.WriteString(receiveid)

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", err
	}
	padded := pkcs7Pad(buf.Bytes(), weComPadBlock)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, aesKey[:aes.BlockSize]).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

// pkcs7Pad appends PKCS#7 padding to a multiple of blockSize. A full block is
// added when the input is already aligned, so unpadding is never ambiguous.
func pkcs7Pad(b []byte, blockSize int) []byte {
	n := blockSize - len(b)%blockSize
	return append(b, bytes.Repeat([]byte{byte(n)}, n)...)
}

// pkcs7Unpad removes PKCS#7 padding, rejecting malformed input rather than
// returning a silently truncated message.
func pkcs7Unpad(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("cannot unpad empty plaintext")
	}
	n := int(b[len(b)-1])
	if n < 1 || n > weComPadBlock || n > len(b) {
		return nil, fmt.Errorf("invalid PKCS#7 padding byte %d", n)
	}
	// Every padding byte must carry the same value; a mismatch means the
	// ciphertext or key is wrong, which is worth an error rather than a guess.
	for _, c := range b[len(b)-n:] {
		if int(c) != n {
			return nil, fmt.Errorf("inconsistent PKCS#7 padding")
		}
	}
	return b[:len(b)-n], nil
}
