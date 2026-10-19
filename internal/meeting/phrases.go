// SPDX-License-Identifier: Apache-2.0

package meeting

import "strings"

// The bot's fixed spoken phrases, by language (design 048, E-85). Everything
// else a bot says is an agent's answer, which the agent already writes in the
// meeting's language. Shown text (the wall, the Recall page) is not here: it
// is read on screen, not spoken.

// Phrase ids.
const (
	// phraseFailure is what hands-free mode says instead of staying silent
	// when a turn fails. It never claims a result.
	phraseFailure = "failure"
)

// failurePhrase is the English failure phrase, the fallback for every
// language without its own.
const failurePhrase = "Sorry, that failed."

// spokenPhrases maps a phrase id to its text per language key (phraseLang).
// Every phrase has "en".
var spokenPhrases = map[string]map[string]string{
	phraseFailure: {
		"en": failurePhrase,
		// Kurmanji: "Sorry, this did not succeed." Needs founder check by ear
		// (written by the agent, not by a Kurmanji speaker; spoken through
		// ElevenLabs eleven_v4_turbo, which does not list Kurdish).
		"kmr": "Bibore, ev bi ser neket.",
	},
}

// phraseLang is the spokenPhrases key for an ISO 639 code: the Kurmanji
// codes (kmr, and ku/kur, which langName speaks as Kurmanji) share "kmr";
// "" is English. Sorani (ckb) is its own language and falls back to English.
func phraseLang(lang string) string {
	l := strings.ToLower(strings.TrimSpace(lang))
	switch l {
	case "", "en":
		return "en"
	case "ku", "kmr", "kur":
		return "kmr"
	}
	return l
}

// spokenPhrase is phrase id in lang, else in English; "" for an unknown id.
func spokenPhrase(id, lang string) string {
	byLang := spokenPhrases[id]
	if t := byLang[phraseLang(lang)]; t != "" {
		return t
	}
	return byLang["en"]
}
