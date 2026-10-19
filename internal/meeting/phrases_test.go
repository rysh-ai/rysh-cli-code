// SPDX-License-Identifier: Apache-2.0

package meeting

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// E-85: with --lang / MEETING_LANGUAGE the bot's fixed spoken phrases follow
// the meeting's language, falling back to English.

func TestSpokenPhraseByLanguage(t *testing.T) {
	kmr := spokenPhrases[phraseFailure]["kmr"]
	if kmr == "" || kmr == failurePhrase {
		t.Fatalf("no Kurmanji failure phrase: %q", kmr)
	}
	for _, c := range []struct{ lang, want string }{
		{"kmr", kmr},
		{"ku", kmr},  // Kurdish without a dialect is spoken as Kurmanji (langName)
		{"kur", kmr}, // Scribe's code
		{"KMR", kmr},
		{"", failurePhrase},
		{"en", failurePhrase},
		{"ckb", failurePhrase}, // Sorani: no phrase yet → English
		{"xx", failurePhrase},
	} {
		if got := spokenPhrase(phraseFailure, c.lang); got != c.want {
			t.Errorf("spokenPhrase(failure, %q) = %q, want %q", c.lang, got, c.want)
		}
	}
	if got := spokenPhrase("no-such-phrase", "kmr"); got != "" {
		t.Errorf("unknown phrase id = %q", got)
	}
	for id, byLang := range spokenPhrases {
		if byLang["en"] == "" {
			t.Errorf("phrase %q has no English fallback", id)
		}
	}
}

func TestMeetingHandsFreeFailureIsSpokenInTheMeetingLanguage(t *testing.T) {
	s, conn := handsFree(t, func(context.Context, string) (string, error) { return "unverified success", errors.New("boom") })
	var spoken atomic.Value
	s.mu.Lock()
	s.cfg.Lang = "kmr"
	s.speech = func(_ context.Context, text string) ([]byte, error) { spoken.Store(text); return []byte("MP3"), nil }
	s.mu.Unlock()
	deliver(t, s, "wake", said("hey rysh do it", 1, "Host", true, 1))
	settle(s)
	readAudio(t, conn)
	if want := spokenPhrases[phraseFailure]["kmr"]; spoken.Load() != want {
		t.Fatalf("spoke %q, want the Kurmanji failure phrase %q", spoken.Load(), want)
	}
}
