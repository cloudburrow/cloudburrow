package vertexai

import (
	"errors"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/syntaxnet"
)

// Natural Language v1's documents:analyzeSyntax (#1799), on SyntaxNet's
// Parsey McParseface, the English tagger and parser Google published
// (internal/syntaxnet), run locally: no model is asked, and nothing is sent
// to Google. The answer is v1's AnalyzeSyntaxResponse
// (https://docs.cloud.google.com/natural-language/docs/reference/rest/v1/documents/analyzeSyntax
// and analyzing-syntax, read 2026-11-21): the sentences (split as the
// other methods split them), every token with its text and beginOffset,
// partOfSpeech, dependencyEdge (headTokenIndex counting the document's
// tokens, a root pointing at itself) and lemma, and the language.
//
//   - partOfSpeech.tag is the token's universal category (Parsey's
//     fine-to-universal map, "." as PUNCT); the morphology fields are read
//     from its Penn Treebank tag (NNS a PLURAL number, NNP PROPER, VBD a PAST
//     tense, VBZ a THIRD person SINGULAR PRESENT, personal pronouns' person,
//     number and case), every other field its *_UNKNOWN value.
//   - dependencyEdge.label is Parsey's label in upper case (punct as P,
//     possessive as PS), the API's Label enum names.
//   - English alone is parsed: the API's other syntax languages have no
//     local model and are UNIMPLEMENTED, naming the language.
//
// UNVERIFIED (re-read 2026-11-21: the references define the fields, not
// these values): that
// Google's English model answers Parsey's tags and edges (the NL API's
// syntax descends from SyntaxNet, and the analyzing-syntax sample's edges,
// Google NSUBJ of unveiled and the final period P, are Parsey's for that
// sentence); the morphology Google answers for each tag; the lemmas,
// which are a rule-based English lemmatizer's (irregular verbs and plurals
// from a list, regular inflections by rule); the texts of the errors.

// nlLabels maps Parsey's labels to the API's Label names where they are
// not simply its upper case.
var nlLabels = map[string]string{"punct": "P", "possessive": "PS"}

func (s *Server) analyzeSyntax(r *http.Request, text, lang string, off func(int) int) (any, error) {
	base, _, _ := strings.Cut(strings.ToLower(lang), "-")
	if base != "en" {
		return nil, apierror.Unimplemented("analyzeSyntax in %s: CloudBurrow's local parser, SyntaxNet's Parsey McParseface, "+
			"reads English alone, and it never calls Google", lang)
	}
	m, err := syntaxnet.Default(r.Context())
	switch {
	case errors.Is(err, syntaxnet.ErrNoModel):
		return nil, apierror.Unimplemented("analyzeSyntax: this process has no local syntax model")
	case errors.Is(err, syntaxnet.ErrFetch):
		return nil, &apierror.Error{Code: codes.Unavailable, Message: "analyzeSyntax: the local syntax model could not be fetched", Reason: "backendError", Cause: err}
	case err != nil:
		return nil, apierror.Internal(err, "loading the local syntax model")
	}
	sentences := []map[string]any{}
	tokens := []map[string]any{}
	for _, sp := range splitSentences(text) {
		sentences = append(sentences, map[string]any{"text": map[string]any{"content": sp.text, "beginOffset": off(sp.start)}})
		spans := syntaxnet.Tokenize(sp.text)
		words := make([]string, len(spans))
		for i, t := range spans {
			words[i] = t.Word
		}
		first := len(tokens)
		for i, t := range m.Parse(words) {
			content := sp.text[spans[i].Start:spans[i].End]
			head := first + i
			if t.Head >= 0 {
				head = first + t.Head
			}
			label := nlLabels[t.Label]
			if label == "" {
				label = strings.ToUpper(t.Label)
			}
			tokens = append(tokens, map[string]any{
				"text":           map[string]any{"content": content, "beginOffset": off(sp.start + spans[i].Start)},
				"partOfSpeech":   partOfSpeech(t, content),
				"dependencyEdge": map[string]any{"headTokenIndex": head, "label": label},
				"lemma":          lemma(content, t.Tag),
			})
		}
	}
	return map[string]any{"sentences": sentences, "tokens": tokens, "language": lang}, nil
}

// pronouns are the personal pronouns' person, number and case.
var pronouns = map[string][3]string{
	"i": {"FIRST", "SINGULAR", "NOMINATIVE"}, "me": {"FIRST", "SINGULAR", "ACCUSATIVE"},
	"we": {"FIRST", "PLURAL", "NOMINATIVE"}, "us": {"FIRST", "PLURAL", "ACCUSATIVE"},
	"you": {"SECOND", "NUMBER_UNKNOWN", "CASE_UNKNOWN"},
	"he":  {"THIRD", "SINGULAR", "NOMINATIVE"}, "him": {"THIRD", "SINGULAR", "ACCUSATIVE"},
	"she": {"THIRD", "SINGULAR", "NOMINATIVE"}, "her": {"THIRD", "SINGULAR", "CASE_UNKNOWN"},
	"it":   {"THIRD", "SINGULAR", "CASE_UNKNOWN"},
	"they": {"THIRD", "PLURAL", "NOMINATIVE"}, "them": {"THIRD", "PLURAL", "ACCUSATIVE"},
	"my": {"FIRST", "SINGULAR", "GENITIVE"}, "our": {"FIRST", "PLURAL", "GENITIVE"}, "your": {"SECOND", "NUMBER_UNKNOWN", "GENITIVE"},
	"his": {"THIRD", "SINGULAR", "GENITIVE"}, "its": {"THIRD", "SINGULAR", "GENITIVE"}, "their": {"THIRD", "PLURAL", "GENITIVE"},
}

// partOfSpeech is a token's PartOfSpeech.
func partOfSpeech(t syntaxnet.Token, content string) map[string]any {
	tag := t.Category
	switch tag {
	case ".":
		tag = "PUNCT"
	case "":
		tag = "X"
	}
	p := map[string]any{"tag": tag, "aspect": "ASPECT_UNKNOWN", "case": "CASE_UNKNOWN", "form": "FORM_UNKNOWN",
		"gender": "GENDER_UNKNOWN", "mood": "MOOD_UNKNOWN", "number": "NUMBER_UNKNOWN", "person": "PERSON_UNKNOWN",
		"proper": "PROPER_UNKNOWN", "reciprocity": "RECIPROCITY_UNKNOWN", "tense": "TENSE_UNKNOWN", "voice": "VOICE_UNKNOWN"}
	switch t.Tag {
	case "NN":
		p["number"] = "SINGULAR"
	case "NNS":
		p["number"] = "PLURAL"
	case "NNP":
		p["number"], p["proper"] = "SINGULAR", "PROPER"
	case "NNPS":
		p["number"], p["proper"] = "PLURAL", "PROPER"
	case "VBD":
		p["tense"], p["mood"] = "PAST", "INDICATIVE"
	case "VBZ":
		p["tense"], p["mood"], p["person"], p["number"] = "PRESENT", "INDICATIVE", "THIRD", "SINGULAR"
	case "VBP":
		p["tense"], p["mood"] = "PRESENT", "INDICATIVE"
	case "VBN":
		p["tense"] = "PAST"
	case "VBG":
		p["aspect"] = "PROGRESSIVE"
	case "PRP", "PRP$":
		if m, ok := pronouns[strings.ToLower(content)]; ok {
			p["person"], p["number"], p["case"] = m[0], m[1], m[2]
		}
	}
	return p
}

// irregular are irregular English forms and their lemmas.
var irregular = map[string]string{
	"am": "be", "is": "be", "are": "be", "was": "be", "were": "be", "been": "be", "being": "be", "'s": "be", "'re": "be", "'m": "be",
	"has": "have", "had": "have", "having": "have", "'ve": "have", "'d": "have",
	"does": "do", "did": "do", "done": "do", "doing": "do", "n't": "not", "'ll": "will", "wo": "will", "ca": "can", "sha": "shall",
	"went": "go", "gone": "go", "goes": "go", "made": "make", "said": "say", "says": "say", "saw": "see", "seen": "see",
	"took": "take", "taken": "take", "came": "come", "got": "get", "gotten": "get", "gave": "give", "given": "give",
	"knew": "know", "known": "know", "thought": "think", "told": "tell", "found": "find", "became": "become",
	"left": "leave", "felt": "feel", "brought": "bring", "began": "begin", "begun": "begin", "kept": "keep",
	"held": "hold", "wrote": "write", "written": "write", "stood": "stand", "heard": "hear", "meant": "mean",
	"met": "meet", "ran": "run", "paid": "pay", "sat": "sit", "spoke": "speak", "spoken": "speak", "led": "lead",
	"grew": "grow", "grown": "grow", "lost": "lose", "fell": "fall", "fallen": "fall", "sent": "send", "built": "build",
	"understood": "understand", "drew": "draw", "drawn": "draw", "broke": "break", "broken": "break", "spent": "spend",
	"rose": "rise", "risen": "rise", "drove": "drive", "driven": "drive", "bought": "buy", "wore": "wear", "worn": "wear",
	"chose": "choose", "chosen": "choose", "sold": "sell", "ate": "eat", "eaten": "eat", "taught": "teach",
	"caught": "catch", "fought": "fight", "flew": "fly", "flown": "fly", "sang": "sing", "sung": "sing",
	"swam": "swim", "threw": "throw", "thrown": "throw", "won": "win", "woke": "wake", "forgot": "forget",
	"forgotten": "forget", "hid": "hide", "hidden": "hide", "rode": "ride", "ridden": "ride", "shook": "shake",
	"slept": "sleep", "stole": "steal", "stolen": "steal", "struck": "strike", "swore": "swear", "tore": "tear",
	"torn": "tear", "understands": "understand", "unveiled": "unveil", "headquartered": "headquarter",
	"men": "man", "women": "woman", "children": "child", "people": "person", "feet": "foot", "teeth": "tooth",
	"mice": "mouse", "geese": "goose", "better": "good", "best": "good", "worse": "bad", "worst": "bad",
}

func isVowel(c byte) bool { return strings.IndexByte("aeiou", c) >= 0 }

// lemma is a token's lemma: its irregular form's, else its regular
// inflection's stem by its tag, else the token.
func lemma(w, tag string) string {
	if tag == "NNP" || tag == "NNPS" {
		return w
	}
	lw := strings.ToLower(w)
	if l, ok := irregular[lw]; ok && (strings.HasPrefix(tag, "V") || strings.HasPrefix(tag, "N") || strings.HasPrefix(tag, "J") ||
		tag == "MD" || tag == "RB" || tag == "POS") {
		if tag == "POS" {
			return w
		}
		return l
	}
	switch tag {
	case "NNS", "VBZ":
		switch {
		case strings.HasSuffix(lw, "ies") && len(lw) > 4:
			return lw[:len(lw)-3] + "y"
		case strings.HasSuffix(lw, "sses"), strings.HasSuffix(lw, "xes"), strings.HasSuffix(lw, "ches"),
			strings.HasSuffix(lw, "shes"), strings.HasSuffix(lw, "zes"):
			return lw[:len(lw)-2]
		case strings.HasSuffix(lw, "s") && !strings.HasSuffix(lw, "ss") && len(lw) > 2:
			return lw[:len(lw)-1]
		}
		return lw
	case "VBD", "VBN":
		if strings.HasSuffix(lw, "ied") && len(lw) > 4 {
			return lw[:len(lw)-3] + "y"
		}
		if strings.HasSuffix(lw, "ed") && len(lw) > 3 {
			return regularStem(lw[:len(lw)-2])
		}
		return lw
	case "VBG":
		if strings.HasSuffix(lw, "ing") && len(lw) > 4 {
			return regularStem(lw[:len(lw)-3])
		}
		return lw
	case "JJR", "JJS", "RBR", "RBS":
		return lw
	}
	if strings.HasPrefix(tag, "V") || tag == "MD" || tag == "NN" {
		return lw
	}
	return w
}

// regularStem restores a stem from which -ed or -ing was taken: a doubled
// final consonant is undoubled (stopped as stop), and a stem ending in a
// consonant, vowel, consonant, or in a soft c, g, v, z or s after a
// vowel, gets its e back (baked as bake).
func regularStem(s string) string {
	n := len(s)
	if n >= 3 && s[n-1] == s[n-2] && !isVowel(s[n-1]) && strings.IndexByte("lsz", s[n-1]) < 0 {
		return s[:n-1]
	}
	if n >= 2 && strings.IndexByte("cgvz", s[n-1]) >= 0 && isVowel(s[n-2]) {
		return s + "e"
	}
	if n == 3 && !isVowel(s[0]) && isVowel(s[1]) && !isVowel(s[2]) && strings.IndexByte("wxy", s[2]) < 0 {
		return s + "e"
	}
	return s
}
