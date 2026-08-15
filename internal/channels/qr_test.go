// SPDX-License-Identifier: Apache-2.0

package channels

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestQRHalfBlocksStructure locks the invariants a terminal QR must hold:
// non-empty, rectangular (every row the same rune width), built only from the
// four half-block runes, and framed by a light quiet zone (the top border row is
// solid █). The exact module pattern is rsc/qr's business, not ours.
func TestQRHalfBlocksStructure(t *testing.T) {
	out, err := QRHalfBlocks("sgnl://linkdevice?uuid=abc&pub_key=def")
	if err != nil {
		t.Fatalf("QRHalfBlocks: %v", err)
	}
	rows := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(rows) < 8 {
		t.Fatalf("QR too small: %d rows", len(rows))
	}
	width := len([]rune(rows[0]))
	for i, r := range rows {
		if w := len([]rune(r)); w != width {
			t.Fatalf("row %d width %d, want %d (not rectangular)", i, w, width)
		}
		for _, ch := range r {
			switch ch {
			case '█', '▀', '▄', ' ':
			default:
				t.Fatalf("row %d has unexpected rune %q", i, ch)
			}
		}
	}
	// The 2-module quiet zone is one half-block row of solid ink top and bottom.
	if strings.ContainsRune(rows[0], ' ') || strings.ContainsAny(rows[0], "▀▄") {
		t.Fatalf("top quiet-zone row is not solid ink: %q", rows[0])
	}
}

// TestQRHalfBlocksDeterministicAndDistinct: same payload → identical render;
// different payloads → different renders (so the pane can't silently show a stale
// or wrong code).
func TestQRHalfBlocksDeterministicAndDistinct(t *testing.T) {
	a1, err := QRHalfBlocks("payload-A")
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := QRHalfBlocks("payload-A")
	if a1 != a2 {
		t.Fatal("QRHalfBlocks is not deterministic for one payload")
	}
	b, _ := QRHalfBlocks("payload-B")
	if a1 == b {
		t.Fatal("distinct payloads produced identical QR renders")
	}
}

// TestQRPNGDataURI: valid data URI whose body decodes to a real PNG.
func TestQRPNGDataURI(t *testing.T) {
	uri, err := QRPNGDataURI("sgnl://linkdevice?uuid=abc")
	if err != nil {
		t.Fatalf("QRPNGDataURI: %v", err)
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(uri, prefix) {
		t.Fatalf("bad data URI prefix: %q", uri[:min(len(uri), 40)])
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, prefix))
	if err != nil {
		t.Fatalf("data URI body is not valid base64: %v", err)
	}
	if len(raw) < 8 || string(raw[1:4]) != "PNG" {
		t.Fatalf("data URI body is not a PNG (magic=%x)", raw[:min(len(raw), 8)])
	}
}

// TestQREncodeErrorPropagates: an over-long payload exceeds QR capacity and both
// renderers must surface the error rather than a blank or panicking output.
func TestQREncodeErrorPropagates(t *testing.T) {
	huge := strings.Repeat("x", 8000) // beyond any QR version's byte capacity
	if _, err := QRHalfBlocks(huge); err == nil {
		t.Fatal("QRHalfBlocks accepted an over-capacity payload")
	}
	if _, err := QRPNGDataURI(huge); err == nil {
		t.Fatal("QRPNGDataURI accepted an over-capacity payload")
	}
}
