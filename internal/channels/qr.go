// SPDX-License-Identifier: Apache-2.0

package channels

// QR rendering for device-link pairing (X4, design 009). Two render targets from
// one encoder (rsc.io/qr): a Unicode half-block string for the terminal pane and a
// black-on-white PNG data URI for the web dashboard. Callers always keep the raw
// payload as a third, render-independent fallback.

import (
	"encoding/base64"
	"fmt"
	"strings"

	"rsc.io/qr"
)

// qrTerminalQuietZone is the light border (in modules) drawn around the terminal
// code. The spec calls for 4; 2 keeps a version-7 code inside an ~50-column pane
// and still scans on a clean background. The dashboard PNG carries rsc/qr's own
// full quiet zone, so it is the reliable path when the terminal is cramped.
const qrTerminalQuietZone = 2

// qrLevel is the error-correction level. M (≈15% recovery) is the usual choice for
// short device-link URIs — enough resilience without inflating the module count.
const qrLevel = qr.M

// QRHalfBlocks renders payload as a scannable QR using Unicode half-block runes —
// two vertical modules per text row, so modules come out roughly square in a
// terminal cell. It is tuned for a DARK background: light modules (including the
// quiet zone) are drawn as block "ink" and dark modules as spaces, which on a dark
// terminal reads as a normal dark-on-light code that phone cameras scan. On a
// light-background terminal the code appears inverted; QRPNGDataURI and the raw
// payload are the theme-independent fallbacks.
func QRHalfBlocks(payload string) (string, error) {
	code, err := qr.Encode(payload, qrLevel)
	if err != nil {
		return "", fmt.Errorf("qr encode: %w", err)
	}
	n := code.Size
	dim := n + 2*qrTerminalQuietZone

	// light reports whether the module at padded-grid (x,y) is light (not black).
	// Coordinates in the quiet border — or one row past the grid, used when an odd
	// height leaves a dangling bottom half — resolve to light.
	light := func(x, y int) bool {
		x -= qrTerminalQuietZone
		y -= qrTerminalQuietZone
		if x < 0 || y < 0 || x >= n || y >= n {
			return true
		}
		return !code.Black(x, y)
	}

	var b strings.Builder
	for y := 0; y < dim; y += 2 {
		for x := 0; x < dim; x++ {
			top, bottom := light(x, y), light(x, y+1)
			switch {
			case top && bottom:
				b.WriteRune('█')
			case top && !bottom:
				b.WriteRune('▀')
			case !top && bottom:
				b.WriteRune('▄')
			default:
				b.WriteRune(' ')
			}
		}
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// QRPNGDataURI renders payload as a black-on-white PNG QR and returns it as a
// "data:image/png;base64,…" URI an <img> can show directly. This is the
// theme-independent, guaranteed-scannable form carried to the web dashboard.
func QRPNGDataURI(payload string) (string, error) {
	code, err := qr.Encode(payload, qrLevel)
	if err != nil {
		return "", fmt.Errorf("qr encode: %w", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG()), nil
}
