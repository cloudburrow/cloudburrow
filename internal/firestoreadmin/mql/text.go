package mql

// Firestore with MongoDB compatibility (#1738): text search, as Google
// documents it ("Use text searches",
// https://docs.cloud.google.com/firestore/mongodb-compatibility/docs/text-search,
// and "Create a text index"):
//
//   - $text: {$search, $language, $caseSensitive, $diacriticSensitive}
//     over the fields of the collection's text index (a string, or an
//     array of strings, at each field path);
//   - the search string's terms are OR'd, its quoted phrases (a quoted
//     single word too) AND'd with them, and a term or phrase prefixed by a
//     hyphen excludes the documents that hold it: Google's example
//     "\"best\" \"french\" bread is" is best AND french AND (bread OR is);
//   - matching folds case and diacritics, and stems the words of the
//     languages a stemmer is kept for here (English); the document's
//     language is its language override field's (default "language"),
//     else the index's default language (default English), else the
//     query's $language;
//   - {$meta: "textScore"} is a matched document's relevance, in a
//     projection, a sort and an aggregation expression;
//   - Google's limits: one $text per query, nested only in $and and $or,
//     the first stage of an aggregation, no $natural sort or hint.
//
// Google also expands a search with "context-aware synonyms ... and
// spelling-corrected terms"; that expansion is a model of Google's own,
// which is not reproduced here.

import (
	"strings"
	"unicode"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// TextScoreKey is the field a matched document carries its text score in,
// while a query runs; it never reaches a client or a stored document.
const TextScoreKey = "$__textScore"

// TextIndex is a collection's text index.
type TextIndex struct {
	Fields           []string
	Language         string // default language, an ISO code or a name
	LanguageOverride string // the document field that names its language
	Partition        string // the partition field, "" for none
}

// TextQuery is a parsed $text.
type TextQuery struct {
	terms, negTerms     []string
	phrases, negPhrases [][]string
	language            string
	caseSensitive       bool
	diacriticSensitive  bool
}

// FindText is the $text of filter, and whether there is one, refusing more
// than one and one nested in anything but $and and $or.
func FindText(filter bson.D) (bson.D, bool, error) {
	var found []bson.D
	var walk func(f bson.D, ok bool) error
	walk = func(f bson.D, ok bool) error {
		for _, e := range f {
			switch e.Key {
			case "$text":
				if !ok {
					return badValue("$text can only be nested inside $and and $or")
				}
				d, isDoc := e.Value.(bson.D)
				if !isDoc {
					return badValue("$text expects an object")
				}
				found = append(found, d)
			case "$and", "$or":
				arr, _ := e.Value.(bson.A)
				for _, c := range arr {
					if cd, isDoc := c.(bson.D); isDoc {
						if err := walk(cd, ok); err != nil {
							return err
						}
					}
				}
			case "$nor":
				arr, _ := e.Value.(bson.A)
				for _, c := range arr {
					if cd, isDoc := c.(bson.D); isDoc {
						if err := walk(cd, false); err != nil {
							return err
						}
					}
				}
			default:
				if d, isDoc := e.Value.(bson.D); isDoc && !strings.HasPrefix(e.Key, "$") {
					for _, x := range d {
						if x.Key == "$not" || x.Key == "$elemMatch" {
							if xd, ok := x.Value.(bson.D); ok {
								if err := walk(xd, false); err != nil {
									return err
								}
							}
						}
					}
				}
			}
		}
		return nil
	}
	if err := walk(filter, true); err != nil {
		return nil, false, err
	}
	switch len(found) {
	case 0:
		return nil, false, nil
	case 1:
		return found[0], true, nil
	}
	return nil, false, badValue("Too many text expressions")
}

// ParseText parses a $text's argument.
func ParseText(spec bson.D) (*TextQuery, error) {
	q := &TextQuery{}
	search, hasSearch := lookupE(spec, "$search")
	for _, e := range spec {
		switch e.Key {
		case "$search":
		case "$language":
			s, ok := e.Value.(string)
			if !ok {
				return nil, merr(14, "TypeMismatch", "$language requires a string value")
			}
			q.language = s
		case "$caseSensitive":
			b, ok := e.Value.(bool)
			if !ok {
				return nil, merr(14, "TypeMismatch", "$caseSensitive requires a boolean value")
			}
			q.caseSensitive = b
		case "$diacriticSensitive":
			b, ok := e.Value.(bool)
			if !ok {
				return nil, merr(14, "TypeMismatch", "$diacriticSensitive requires a boolean value")
			}
			q.diacriticSensitive = b
		default:
			return nil, badValue("extra fields in $text: %s", e.Key)
		}
	}
	s, ok := search.(string)
	if !hasSearch || !ok {
		return nil, merr(14, "TypeMismatch", "$search requires a string value")
	}
	// Quoted phrases, then the words outside them.
	rest := s
	for {
		i := strings.Index(rest, "\"")
		if i < 0 {
			break
		}
		j := strings.Index(rest[i+1:], "\"")
		if j < 0 {
			break
		}
		neg := i > 0 && rest[i-1] == '-'
		phrase := rest[i+1 : i+1+j]
		words := splitWords(phrase)
		if len(words) > 0 {
			if neg {
				q.negPhrases = append(q.negPhrases, words)
			} else {
				q.phrases = append(q.phrases, words)
			}
		}
		pre := rest[:i]
		if neg {
			pre = rest[:i-1]
		}
		rest = pre + " " + rest[i+2+j:]
	}
	for _, f := range strings.Fields(rest) {
		neg := strings.HasPrefix(f, "-")
		for _, w := range splitWords(strings.TrimLeft(f, "-")) {
			if neg {
				q.negTerms = append(q.negTerms, w)
			} else {
				q.terms = append(q.terms, w)
			}
		}
	}
	return q, nil
}

func splitWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' })
}

// diacritics folds the Latin letters with diacritics to their base letter.
var diacritics = func() map[rune]rune {
	m := map[rune]rune{}
	add := func(base rune, chars string) {
		for _, c := range chars {
			m[c] = base
		}
	}
	add('a', "àáâãäåāăąǎȁȃȧạảấầẩẫậắằẳẵặ")
	add('A', "ÀÁÂÃÄÅĀĂĄǍȀȂȦẠẢẤẦẨẪẬẮẰẲẴẶ")
	add('c', "çćĉċč")
	add('C', "ÇĆĈĊČ")
	add('d', "ďđ")
	add('D', "ĎĐ")
	add('e', "èéêëēĕėęěȅȇẹẻẽếềểễệ")
	add('E', "ÈÉÊËĒĔĖĘĚȄȆẸẺẼẾỀỂỄỆ")
	add('g', "ĝğġģ")
	add('G', "ĜĞĠĢ")
	add('h', "ĥħ")
	add('H', "ĤĦ")
	add('i', "ìíîïĩīĭįıǐȉȋịỉ")
	add('I', "ÌÍÎÏĨĪĬĮİǏȈȊỊỈ")
	add('j', "ĵ")
	add('J', "Ĵ")
	add('k', "ķ")
	add('K', "Ķ")
	add('l', "ĺļľŀł")
	add('L', "ĹĻĽĿŁ")
	add('n', "ñńņňŉ")
	add('N', "ÑŃŅŇ")
	add('o', "òóôõöøōŏőǒȍȏọỏốồổỗộớờởỡợơ")
	add('O', "ÒÓÔÕÖØŌŎŐǑȌȎỌỎỐỒỔỖỘỚỜỞỠỢƠ")
	add('r', "ŕŗř")
	add('R', "ŔŖŘ")
	add('s', "śŝşšș")
	add('S', "ŚŜŞŠȘ")
	add('t', "ţťŧț")
	add('T', "ŢŤŦȚ")
	add('u', "ùúûüũūŭůűųǔȕȗụủứừửữựư")
	add('U', "ÙÚÛÜŨŪŬŮŰŲǓȔȖỤỦỨỪỬỮỰƯ")
	add('w', "ŵ")
	add('W', "Ŵ")
	add('y', "ýÿŷỳỵỷỹ")
	add('Y', "ÝŸŶỲỴỶỸ")
	add('z', "źżž")
	add('Z', "ŹŻŽ")
	return m
}()

// english is whether lang names English.
func english(lang string) bool {
	switch strings.ToLower(lang) {
	case "", "en", "english":
		return true
	}
	return false
}

// stemEnglish strips the common English inflectional suffixes, so a word
// and its plural or verb forms match.
func stemEnglish(w string) string {
	n := len(w)
	switch {
	case n > 5 && strings.HasSuffix(w, "ies"):
		return w[:n-3] + "y"
	case n > 5 && strings.HasSuffix(w, "ing"):
		return trimDouble(w[:n-3])
	case n > 4 && strings.HasSuffix(w, "ed") && !strings.HasSuffix(w, "eed"):
		return trimDouble(w[:n-2])
	case n > 4 && (strings.HasSuffix(w, "ches") || strings.HasSuffix(w, "shes") || strings.HasSuffix(w, "sses") || strings.HasSuffix(w, "xes")):
		return w[:n-2]
	case n > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && !strings.HasSuffix(w, "us") && !strings.HasSuffix(w, "is"):
		return w[:n-1]
	}
	return w
}

func trimDouble(w string) string {
	n := len(w)
	if n > 2 && w[n-1] == w[n-2] && !strings.ContainsRune("lsz", rune(w[n-1])) {
		return w[:n-1]
	}
	return w
}

// norm is a word as matching compares it.
func (q *TextQuery) norm(w, lang string) string {
	if !q.caseSensitive {
		w = strings.ToLower(w)
	}
	if !q.diacriticSensitive {
		w = strings.Map(func(r rune) rune {
			if b, ok := diacritics[r]; ok {
				return b
			}
			return r
		}, w)
	}
	if english(lang) {
		w = stemEnglish(w)
	}
	return w
}

// fieldTokens are the words of each indexed field of doc.
func fieldTokens(doc bson.D, ix *TextIndex) [][]string {
	var out [][]string
	for _, f := range ix.Fields {
		v, ok := getPath(doc, f)
		if !ok {
			continue
		}
		var texts []string
		switch x := v.(type) {
		case string:
			texts = []string{x}
		case bson.A:
			for _, e := range x {
				if s, ok := e.(string); ok {
					texts = append(texts, s)
				}
			}
		}
		for _, t := range texts {
			out = append(out, splitWords(t))
		}
	}
	return out
}

// docLanguage is the language doc's words are stemmed in.
func (q *TextQuery) docLanguage(doc bson.D, ix *TextIndex) string {
	override := ix.LanguageOverride
	if override == "" {
		override = "language"
	}
	if l, ok := lookup(doc, override).(string); ok && l != "" {
		return l
	}
	if q.language != "" {
		return q.language
	}
	return ix.Language
}

// Match is whether doc matches q under the text index ix, and its score.
func (q *TextQuery) Match(doc bson.D, ix *TextIndex) (bool, float64) {
	lang := q.docLanguage(doc, ix)
	qlang := q.language
	if qlang == "" {
		qlang = lang
	}
	var fields [][]string
	for _, toks := range fieldTokens(doc, ix) {
		n := make([]string, len(toks))
		for i, t := range toks {
			n[i] = q.norm(t, lang)
		}
		fields = append(fields, n)
	}
	count := func(w string) (int, float64) {
		w = q.norm(w, qlang)
		total := 0
		score := 0.0
		for _, f := range fields {
			c := 0
			for _, t := range f {
				if t == w {
					c++
				}
			}
			if c > 0 && len(f) > 0 {
				total += c
				score += 0.5 + 0.5*float64(c)/float64(len(f))
			}
		}
		return total, score
	}
	hasPhrase := func(p []string) bool {
		np := make([]string, len(p))
		for i, w := range p {
			np[i] = q.norm(w, qlang)
		}
		for _, f := range fields {
			for i := 0; i+len(np) <= len(f); i++ {
				ok := true
				for j := range np {
					if f[i+j] != np[j] {
						ok = false
						break
					}
				}
				if ok {
					return true
				}
			}
		}
		return false
	}
	for _, w := range q.negTerms {
		if c, _ := count(w); c > 0 {
			return false, 0
		}
	}
	for _, p := range q.negPhrases {
		if hasPhrase(p) {
			return false, 0
		}
	}
	score := 0.0
	for _, p := range q.phrases {
		if !hasPhrase(p) {
			return false, 0
		}
		for _, w := range p {
			_, s := count(w)
			score += s
		}
	}
	if len(q.terms) > 0 {
		any := false
		for _, w := range q.terms {
			if c, s := count(w); c > 0 {
				any = true
				score += s
			}
		}
		if !any {
			return false, 0
		}
	} else if len(q.phrases) == 0 {
		return false, 0
	}
	return true, score
}

// ReplaceText is filter with its $text replaced by a condition that is
// matched's value.
func ReplaceText(filter bson.D, matched bool) bson.D {
	out := make(bson.D, 0, len(filter))
	for _, e := range filter {
		switch e.Key {
		case "$text":
			out = append(out, bson.E{Key: "$expr", Value: matched})
		case "$and", "$or", "$nor":
			arr, _ := e.Value.(bson.A)
			na := make(bson.A, len(arr))
			for i, c := range arr {
				if cd, ok := c.(bson.D); ok {
					na[i] = ReplaceText(cd, matched)
				} else {
					na[i] = c
				}
			}
			out = append(out, bson.E{Key: e.Key, Value: na})
		default:
			out = append(out, e)
		}
	}
	return out
}

// PartitionValue is the value an equality on field in filter's top level
// or its top-level $and gives it, for a partitioned text index.
func PartitionValue(filter bson.D, field string) (string, bool) {
	for _, e := range filter {
		if e.Key == field {
			if s, ok := e.Value.(string); ok {
				return s, true
			}
			if d, ok := e.Value.(bson.D); ok && len(d) == 1 && d[0].Key == "$eq" {
				s, ok := d[0].Value.(string)
				return s, ok
			}
		}
		if e.Key == "$and" {
			arr, _ := e.Value.(bson.A)
			for _, c := range arr {
				if cd, ok := c.(bson.D); ok {
					if s, ok := PartitionValue(cd, field); ok {
						return s, true
					}
				}
			}
		}
	}
	return "", false
}

// StripTextScore is d without its text score.
func StripTextScore(d bson.D) bson.D {
	for i, e := range d {
		if e.Key == TextScoreKey {
			return append(append(bson.D{}, d[:i]...), d[i+1:]...)
		}
	}
	return d
}
