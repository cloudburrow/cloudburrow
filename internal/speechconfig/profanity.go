package speechconfig

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// The profanity filter (features.profanityFilter, #1799): a profane word
// keeps its first letter and every other letter becomes an asterisk, as the
// reference documents it ("replacing all but the initial character in each
// filtered word with asterisks, e.g. \"f***\"", RecognitionConfig of v1;
// Chirp 2's feature table: "return only the first letter followed by
// asterisks in the transcript (for example, f***)", read 2026-11-20).
//
// UNVERIFIED: Google does not publish its word list. The list here is
// CloudBurrow's own, English only (strong profanities and their inflected
// forms), so a word Google's filter masks may be left, or the reverse, and
// a transcript in another language is not filtered.

// profaneWords are the filtered words, lower case.
var profaneWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
		fuck fucks fucked fucker fuckers fucking fuckin motherfucker motherfuckers motherfucking
		shit shits shitty shitting shitted bullshit horseshit
		bitch bitches bitching
		bastard bastards asshole assholes arsehole arseholes
		cunt cunts dick dicks dickhead dickheads cock cocks cocksucker cocksuckers
		pussy pussies twat twats wanker wankers prick pricks
		slut sluts whore whores goddamn goddamned`) {
		m[w] = true
	}
	return m
}()

// FilterProfanity masks the profane words of text, for the language code
// ("" when unknown, which is filtered as English).
func FilterProfanity(text, code string) string {
	if p, _, _ := strings.Cut(strings.ToLower(code), "-"); p != "" && p != "en" {
		return text
	}
	var b strings.Builder
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if !unicode.IsLetter(r) {
			b.WriteString(text[i : i+size])
			i += size
			continue
		}
		j := i
		for j < len(text) {
			r, s := utf8.DecodeRuneInString(text[j:])
			if !unicode.IsLetter(r) && r != '\'' {
				break
			}
			j += s
		}
		b.WriteString(MaskWord(text[i:j]))
		i = j
	}
	return b.String()
}

// MaskWord is a word (a recognised words[] entry) masked when it is profane ("f***"), else word.
func MaskWord(word string) string {
	core := strings.TrimRightFunc(word, func(r rune) bool { return !unicode.IsLetter(r) })
	if !profaneWords[strings.ToLower(core)] {
		return word
	}
	_, size := utf8.DecodeRuneInString(core)
	return core[:size] + strings.Repeat("*", utf8.RuneCountInString(core)-1) + word[len(core):]
}
