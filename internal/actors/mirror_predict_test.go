// SPDX-License-Identifier: Apache-2.0

package actors

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-code/internal/vterm"
)

// TestPredictKeystroke_DisabledNoLocalEcho guards the predictive-echo disable.
// predictKeystroke must create no local prediction while predictiveEchoEnabled is
// false: the Mosh-style overlay mispredicted non-echoing keys, leaving duplicate
// glyphs at the cursor. With it off the mirror renders only the authoritative
// source stream. If predictive echo is ever re-enabled (with a confidence model),
// this test must be revisited.
//
// The gate sits at predictKeystroke's entry, before any per-key logic, so it is
// program-agnostic: vim command keys, top's interactive keys, pagers, and password
// prompts are all covered identically. The cases below assert that across program
// types.
func TestPredictKeystroke_DisabledNoLocalEcho(t *testing.T) {
	cases := []struct {
		name  string
		input string // raw bytes a controlled subscriber would type
	}{
		{"vim command keys", "i:wq"},
		{"top interactive keys", "qkP1 "}, // quit / kill / sort / fields — none echoed by top
		{"shell-ish printable", "ls -la"},
		{"mixed printable", "Hxq:0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &MirrorTabListenerActor{vterms: map[string]*mirrorPaneVTerm{}}
			pv := newPredictPV()
			r.vterms["p1"] = pv
			r.predictKeystroke("p1", base64.StdEncoding.EncodeToString([]byte(tc.input)))
			if len(pv.predictions) != 0 {
				t.Fatalf("predictive echo disabled but %d prediction(s) created for %q",
					len(pv.predictions), tc.input)
			}
			if pv.display != nil {
				t.Fatalf("prediction overlay built for %q while echo is disabled", tc.input)
			}
			if got := activePlain(pv); strings.Contains(got, tc.input) {
				t.Fatalf("keys were locally echoed at the cursor: %q in %q", tc.input, got)
			}
		})
	}
}

// newPredictPV returns a mirrorPaneVTerm seeded with a "$ " prompt (cursor at
// column 2) for predictive-echo tests.
func newPredictPV() *mirrorPaneVTerm {
	pv := &mirrorPaneVTerm{vt: vterm.New(24, 80), rows: 24, cols: 80}
	pv.vt.Write([]byte("$ "))
	return pv
}

// activePlain returns the plain-text screen that would be shown (overlay when
// predictions are pending, else authoritative).
func activePlain(pv *mirrorPaneVTerm) string {
	v := pv.vt
	if len(pv.predictions) > 0 && pv.display != nil {
		v = pv.display
	}
	return strings.Join(v.Render(), "")
}

func TestPredict_ShowsImmediately(t *testing.T) {
	pv := newPredictPV()
	pv.predict('h')
	pv.predict('i')
	if got := activePlain(pv); !strings.Contains(got, "$ hi") {
		t.Fatalf("predicted text not shown immediately: %q", got)
	}
	if len(pv.predictions) != 2 {
		t.Fatalf("expected 2 pending predictions, got %d", len(pv.predictions))
	}
}

func TestPredict_ReconcileDropsConfirmed_NoDouble(t *testing.T) {
	pv := newPredictPV()
	pv.predict('h')
	pv.predict('i')
	// The source echoes both characters.
	pv.vt.Write([]byte("hi"))
	pv.reconcile()
	if len(pv.predictions) != 0 {
		t.Fatalf("expected predictions cleared after echo, got %d", len(pv.predictions))
	}
	got := activePlain(pv)
	if !strings.Contains(got, "$ hi") {
		t.Fatalf("authoritative text missing: %q", got)
	}
	if strings.Contains(got, "hihi") {
		t.Fatalf("characters doubled after echo: %q", got)
	}
}

func TestPredict_FastTyping_PartialEcho(t *testing.T) {
	pv := newPredictPV()
	pv.predict('h')
	pv.predict('i')
	// Only the first char has echoed back.
	pv.vt.Write([]byte("h"))
	pv.reconcile()
	if len(pv.predictions) != 1 {
		t.Fatalf("expected 1 remaining prediction, got %d", len(pv.predictions))
	}
	got := activePlain(pv)
	// Still shows "$ hi" (h authoritative, i predicted) — no flicker, no double.
	if !strings.Contains(got, "$ hi") {
		t.Fatalf("expected '$ hi' during partial echo: %q", got)
	}
	if strings.Contains(got, "hhi") || strings.Contains(got, "hii") {
		t.Fatalf("prediction doubled/misplaced during partial echo: %q", got)
	}
	// Second char echoes; prediction fully drains.
	pv.vt.Write([]byte("i"))
	pv.reconcile()
	if len(pv.predictions) != 0 {
		t.Fatalf("expected predictions cleared, got %d", len(pv.predictions))
	}
}

func TestPredict_Backspace(t *testing.T) {
	pv := newPredictPV()
	pv.predict('h')
	pv.predict('x')
	if !pv.predictBackspace() {
		t.Fatal("backspace should pop the last prediction")
	}
	if len(pv.predictions) != 1 {
		t.Fatalf("expected 1 prediction after backspace, got %d", len(pv.predictions))
	}
	got := activePlain(pv)
	if !strings.Contains(got, "$ h") || strings.Contains(got, "$ hx") {
		t.Fatalf("backspace did not remove the predicted 'x': %q", got)
	}
	// Backspace with no predictions is a no-op (never deletes committed text).
	pv2 := newPredictPV()
	if pv2.predictBackspace() {
		t.Fatal("backspace with no predictions must be a no-op")
	}
}
