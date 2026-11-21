package speechconfig

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/cloudburrow/cloudburrow/internal/speaker"
)

// The feature matrix (#2127): which languages each model takes in each
// location, and which recognition features it takes in each, exactly as
// Google's supported languages tables mark them. data/supported_languages.tsv
// holds both tables (v2's per location, v1's per single_utterance), row for
// row, and cites them; CheckV2 and CheckV1 are the only checks of a model's
// language and features, used by Speech-to-Text v2 (Recognize,
// BatchRecognize, StreamingRecognize), v1 (Recognize, LongRunningRecognize,
// StreamingRecognize) and BigQuery's ML.TRANSCRIBE.
//
// A feature the table leaves empty for the model and language is
// INVALID_ARGUMENT, as is a language the model does not take in the
// location, or a model not offered there. Google documents no error for
// them: UNVERIFIED (re-read 2026-11-21: the two tables, the v2 reference's
// RecognitionFeatures and the Chirp 2 and Chirp 3 pages, which list the
// features without an error): the texts of these errors, and that Google
// refuses rather than ignores the feature.

//go:embed data/supported_languages.tsv
var supportTSV string

// Feature is a set of the tables' feature columns.
type Feature uint8

// The tables' feature columns.
const (
	AutomaticPunctuation Feature = 1 << iota
	Diarization
	ModelAdaptation
	WordConfidence
	ProfanityFilter
	SpokenPunctuationFeature
	SpokenEmojiFeature
)

// features are the columns in the data file's order: its letter, the
// table's column name and the request field that asks for it, in v2 and v1.
var features = []struct {
	f              Feature
	letter, column string
	v2, v1         string
}{
	{AutomaticPunctuation, "P", "Automatic punctuation", "features.enableAutomaticPunctuation", "enable_automatic_punctuation"},
	{Diarization, "D", "Diarization", "features.diarizationConfig", "diarization_config"},
	{ModelAdaptation, "A", "Model adaptation", "adaptation", "adaptation"},
	{WordConfidence, "C", "Word-level confidence", "features.enableWordConfidence", "enable_word_confidence"},
	{ProfanityFilter, "F", "Profanity filter", "features.profanityFilter", "profanity_filter"},
	{SpokenPunctuationFeature, "S", "Spoken punctuation", "features.enableSpokenPunctuation", "enable_spoken_punctuation"},
	{SpokenEmojiFeature, "E", "Spoken emoji", "features.enableSpokenEmojis", "enable_spoken_emojis"},
}

type v2Key struct{ location, language, model string }
type v1Key struct {
	language, model string
	single          bool
}

var (
	v2Rows      = map[v2Key]Feature{}
	v2Locations = map[string]map[string]bool{} // model -> locations
	v2Any       = map[[2]string]Feature{}      // model, location -> every language's features
	v1Rows      = map[v1Key]Feature{}
	v1Models    = map[string]bool{}
)

func init() {
	for n, line := range strings.Split(supportTSV, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 6 {
			panic(fmt.Sprintf("speechconfig: supported_languages.tsv line %d: %d columns", n+1, len(f)))
		}
		var set Feature
		for _, c := range features {
			if strings.Contains(f[5], c.letter) {
				set |= c.f
			}
		}
		lang := strings.ToLower(f[2])
		switch f[0] {
		case "v2":
			v2Rows[v2Key{f[1], lang, f[3]}] = set
			if v2Locations[f[3]] == nil {
				v2Locations[f[3]] = map[string]bool{}
			}
			v2Locations[f[3]][f[1]] = true
			v2Any[[2]string{f[3], f[1]}] |= set
		case "v1":
			v1Rows[v1Key{lang, f[3], f[4] == "true"}] = set
			v1Models[f[3]] = true
		}
	}
}

// Requested is the set of table features a checked config asks for.
func (c Config) Requested() Feature {
	var f Feature
	set := func(on bool, x Feature) {
		if on {
			f |= x
		}
	}
	set(c.AutomaticPunctuation, AutomaticPunctuation)
	set(c.Diarization, Diarization)
	set(c.Adaptation != nil, ModelAdaptation)
	set(c.WordConfidence, WordConfidence)
	set(c.ProfanityFilter, ProfanityFilter)
	set(c.SpokenPunctuation, SpokenPunctuationFeature)
	set(c.SpokenEmojis, SpokenEmojiFeature)
	return f
}

// Locations are the locations the v2 table offers a model in, sorted.
func Locations(model string) []string {
	var out []string
	for l := range v2Locations[model] {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// CheckV2 checks a checked v2 config against the v2 table in a location:
// its model is offered there, takes each of its languageCodes there, and
// takes there each feature the config asks for. "auto" (language-agnostic
// recognition) takes the features the model takes in some language there.
// The error is INVALID_ARGUMENT's message, nil when the table allows it.
func (c Config) CheckV2(location string) error {
	location = strings.ToLower(location)
	if !v2Locations[c.Model][location] {
		return fmt.Errorf("The model %q is not supported in the location %q: Speech-to-Text's supported languages table offers it in %s",
			c.Model, location, strings.Join(Locations(c.Model), ", "))
	}
	codes := c.Codes
	if len(codes) == 0 {
		codes = []string{"auto"}
	}
	want := c.Requested()
	for _, code := range codes {
		lc := strings.ToLower(code)
		have, ok := v2Rows[v2Key{location, lc, c.Model}]
		if lc == "auto" {
			have, ok = v2Any[[2]string{c.Model, location}], true
		}
		if !ok {
			return fmt.Errorf("The language %q is not supported by the model %q in the location %q (Speech-to-Text's supported languages table)",
				code, c.Model, location)
		}
		if missing := want &^ have; missing != 0 {
			return unsupported(missing, code, c.Model, "in the location "+location, false)
		}
	}
	return nil
}

// CheckV1 checks a v1 request against the v1 table: model ("" is
// default) takes language, with single_utterance as asked, and each
// feature in want.
func CheckV1(model, language string, single bool, want Feature) error {
	if model == "" {
		model = "default"
	}
	have, ok := v1Rows[v1Key{strings.ToLower(language), model, single}]
	if !ok {
		if !v1Models[model] {
			return fmt.Errorf("The model %q is not a Speech-to-Text v1 model", model)
		}
		if _, other := v1Rows[v1Key{strings.ToLower(language), model, !single}]; other && single {
			return fmt.Errorf("single_utterance is not supported by the model %q for the language %q (Speech-to-Text v1's supported languages table)", model, language)
		}
		return fmt.Errorf("The language %q is not supported by the model %q (Speech-to-Text v1's supported languages table)", language, model)
	}
	if missing := want &^ have; missing != 0 {
		return unsupported(missing, language, model, "", true)
	}
	return nil
}

func unsupported(missing Feature, code, model, where string, v1 bool) error {
	var names []string
	for _, c := range features {
		if missing&c.f != 0 {
			if v1 {
				names = append(names, c.v1+" ("+c.column+")")
			} else {
				names = append(names, c.v2+" ("+c.column+")")
			}
		}
	}
	if where != "" {
		where = " " + where
	}
	return fmt.Errorf("Recognizer does not support feature: %s: the model %q does not support it for the language %q%s (Speech-to-Text's supported languages table)",
		strings.Join(names, ", "), model, code, where)
}

// punctuatingModels are the models that punctuate by themselves: the Chirp
// 2 and Chirp 3 pages' "Feature support and limitations"
// (https://docs.cloud.google.com/speech-to-text/docs/models/chirp-2 and
// .../chirp-3, read 2026-11-21) say of automatic punctuation "Automatically
// generated by the model and can be optionally disabled". The other models
// follow the v2 reference's enableAutomaticPunctuation: "The default false
// value does not add punctuation to result hypotheses". UNVERIFIED
// (re-read 2026-11-21: the Chirp 2 and 3 pages; chirp has no page of its
// own): that chirp punctuates by itself
// as Chirp 2 and 3 do, and how Chirp 2 and 3's punctuation is disabled.
var punctuatingModels = map[string]bool{"chirp": true, "chirp_2": true, "chirp_3": true}

// Punctuated reports whether final transcripts keep their punctuation:
// the config asks for automatic punctuation (which CheckV2 or CheckV1 has
// allowed), or its model punctuates by itself.
func (c Config) Punctuated() bool {
	return c.AutomaticPunctuation || punctuatingModels[c.Model]
}

// StripPunctuation removes punctuation from a transcript, as a model
// without automatic punctuation writes it: every punctuation mark but an
// apostrophe or hyphen inside a word and a decimal point or digit group
// separator inside a number.
func StripPunctuation(text string) string {
	rs := []rune(text)
	var b strings.Builder
	for i, r := range rs {
		if unicode.IsPunct(r) {
			prev, next := rune(0), rune(0)
			if i > 0 {
				prev = rs[i-1]
			}
			if i+1 < len(rs) {
				next = rs[i+1]
			}
			inWord := unicode.IsLetter(prev) && unicode.IsLetter(next) && (r == '\'' || r == '’' || r == '-')
			inNumber := unicode.IsDigit(prev) && unicode.IsDigit(next) && (r == '.' || r == ',')
			if !inWord && !inNumber {
				continue
			}
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// Unserved is the 501 of a feature the table allows but this binary cannot
// serve locally: speaker diarization (chirp_3's, and v1's models'), which
// internal/speaker answers with 3D-Speaker's CAM++ model on ONNX Runtime,
// in the opt-in `-tags onnx` build only (#2131). Google publishes no
// speaker model, and the static CLI has no ONNX Runtime. Callers ask it
// after CheckV2 or CheckV1.
func (c Config) Unserved() error {
	if c.Diarization {
		if err := speaker.Unavailable(); err != nil {
			return fmt.Errorf("Not implemented here: features.diarizationConfig of Speech-to-Text: CloudBurrow transcribes locally with whisper.cpp, "+
				"which has no speaker identities, and never calls Google: %v", err)
		}
	}
	return nil
}
