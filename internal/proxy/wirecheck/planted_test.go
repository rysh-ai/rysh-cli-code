// SPDX-License-Identifier: Apache-2.0

package wirecheck

import (
	"strings"
	"testing"

	"github.com/rysh-ai/rysh-cli-shared/secretnat"
)

// The planted secret used to be one const literal, shaped like a live key, in
// the source and in every binary built from it. It is now assembled at run
// time. This pins the one property that matters: the SecretNAT detector tier
// still CATCHES the assembled value. Failures report counts only, never the
// value or anything derived from it.
func TestTheDetectorCatchesTheAssembledPlantedSecret(t *testing.T) {
	mgr, err := secretnat.NewManager(secretnat.Options{Enabled: true, Mode: secretnat.ModeSemantic})
	if err != nil {
		t.Fatalf("secretnat: %v", err)
	}
	secret := PlantedSecret()
	out := mgr.Session("wo6-planted").Sanitize("In one sentence: is " + secret + " a live or test key?")
	if n := strings.Count(out, secret); n != 0 {
		t.Fatalf("the detector let the assembled planted secret through %dx", n)
	}
	if n := len(snatToken.FindAllString(out, -1)); n != 1 {
		t.Fatalf("want exactly 1 SNAT stand-in token in its place, got %d", n)
	}
}
