// Package speechconfig reads Speech-to-Text v2's RecognitionConfig
// (https://docs.cloud.google.com/speech-to-text/docs/reference/rest/v2/projects.locations.recognizers#RecognitionConfig,
// read 2026-11-18) for local transcription with whisper.cpp (#1799): both
// the local Vertex AI endpoint's recognize route and BigQuery's
// ML.TRANSCRIBE check a config with it, so it imports nothing heavy.
package speechconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Decoding is a config's decoding_config: autoDecodingConfig, or
// explicitDecodingConfig's encoding, sampleRateHertz and audioChannelCount.
type Decoding struct {
	Auto       bool
	Encoding   string
	SampleRate int
	Channels   int
}

// Options are the transcription settings a config asks for.
type Options struct {
	// Words asks for word offsets or confidences (WordOffsets,
	// WordConfidence).
	Words, WordOffsets, WordConfidence bool
	// PerChannel is features.multiChannelMode
	// SEPARATE_RECOGNITION_PER_CHANNEL: each channel is transcribed on
	// its own, its results carrying its channelTag.
	PerChannel bool
	// ProfanityFilter, SpokenPunctuation and SpokenEmojis are the
	// features of those names, applied to each final transcript (Format).
	ProfanityFilter, SpokenPunctuation, SpokenEmojis bool
	// AutomaticPunctuation is features.enableAutomaticPunctuation:
	// without it, a model that does not punctuate by itself
	// (PunctuatesByDefault) answers with the punctuation removed.
	AutomaticPunctuation bool
	// Diarization is a features.diarizationConfig, served by
	// internal/speaker in the onnx build and UNIMPLEMENTED (Unserved)
	// without it, where the table allows it. MinSpeakers and MaxSpeakers
	// bound the speakers found, 0 leaving them free: v1 sets them from its
	// min_speaker_count and max_speaker_count; v2's are not used.
	Diarization              bool
	MinSpeakers, MaxSpeakers int
}

// Encodings are ExplicitDecodingConfig's AudioEncoding values, true for
// those decoded locally (internal/speech's Decode).
var Encodings = map[string]bool{
	"LINEAR16": true, "MULAW": true, "ALAW": true, "MP3": true, "FLAC": true,
	"MP4_AAC": true, "M4A_AAC": true, "MOV_AAC": true,
	"AMR": true, "AMR_WB": true, "OGG_OPUS": true, "WEBM_OPUS": true,
}

// Config is a checked RecognitionConfig.
type Config struct {
	Decoding Decoding
	// Language is whisper's code of the language ("en"), "" to detect it.
	Language string
	// LanguageCode is the languageCodes entry in lower case, as results
	// answer it, "" for auto.
	LanguageCode string
	Options
	// Model is the recognition model asked for.
	Model string
	// Codes are languageCodes as given ("auto" included), checked
	// against the supported languages table (CheckV2).
	Codes []string
	// Adaptation is the config's SpeechAdaptation in JSON, nil for none:
	// the Speech-to-Text front resolves its phrase sets and custom
	// classes and biases whisper with them (#1930).
	Adaptation json.RawMessage
	// Normalization is transcriptNormalization's entries, applied to
	// each final transcript in order (Normalize).
	Normalization []NormalizationEntry
	// Candidates are languageCodes when there are several (at most
	// three): each code in lower case and whisper's code for it. The
	// audio is recognised in the best-fit one, which the results name.
	Candidates []Candidate
	// Denoise is denoiserConfig.denoiseAudio: the audio is denoised
	// before it is transcribed (internal/speech's Denoise).
	Denoise bool
	// SNRThreshold is denoiserConfig.snrThreshold: windows of a lower
	// signal-to-noise ratio are silenced (internal/speech's FilterSNR).
	SNRThreshold float64
	// TranslateTo is translationConfig.targetLanguage, "" for none: each
	// final transcript is translated to it with the served Translation
	// API's model.
	TranslateTo string
}

// Candidate is one of several languageCodes.
type Candidate struct {
	Code    string // the languageCodes entry, lower case
	Whisper string // whisper's code for it
}

// Format removes the punctuation local whisper.cpp writes when the config
// does not ask for it (Punctuated), then applies the config's spoken punctuation and emojis, profanity
// filter and transcriptNormalization, in that order, to a final transcript
// recognised in the language code.
func (c Config) Format(text, code string) string {
	if !c.Punctuated() {
		text = StripPunctuation(text)
	}
	if c.SpokenPunctuation {
		text = SpokenPunctuation(text, code)
	}
	if c.SpokenEmojis {
		text = SpokenEmojis(text, code)
	}
	if c.ProfanityFilter {
		text = FilterProfanity(text, code)
	}
	return c.Normalize(text)
}

// NormalizationEntry is one TranscriptNormalization.Entry.
type NormalizationEntry struct {
	Search        string `json:"search"`
	Replace       string `json:"replace"`
	CaseSensitive bool   `json:"caseSensitive"`
}

// maxNormalization are TranscriptNormalization's documented limits: at
// most 100 entries, search and replace at most 100 characters each.
const maxNormalization = 100

// Normalize applies the config's transcriptNormalization to a transcript,
// as the reference documents it (read 2026-11-20): each entry in turn
// replaces every occurrence of its search with its replace, matched case
// sensitively or not, so an earlier entry's replacement can keep a later
// one from matching ("cat" before "mountain cat").
//
// UNVERIFIED (re-read 2026-11-20:
// https://docs.cloud.google.com/speech-to-text/docs/reference/rest/v2/projects.locations.recognizers#TranscriptNormalization,
// which does not say): whether a search matches inside a word (here it
// does, as a plain substring), and that words[] keep the recognised words.
func (c Config) Normalize(text string) string {
	for _, e := range c.Normalization {
		if e.CaseSensitive {
			text = strings.ReplaceAll(text, e.Search, e.Replace)
			continue
		}
		text = replaceFold(text, e.Search, e.Replace)
	}
	return text
}

// replaceFold replaces every case-insensitive (Unicode simple folding)
// occurrence of search in text.
func replaceFold(text, search, replace string) string {
	if search == "" {
		return text
	}
	var b strings.Builder
	n := utf8.RuneCountInString(search)
	for i := 0; i < len(text); {
		j, k := i, 0
		for k < n && j < len(text) {
			_, size := utf8.DecodeRuneInString(text[j:])
			j += size
			k++
		}
		if k == n && strings.EqualFold(text[i:j], search) {
			b.WriteString(replace)
			i = j
			continue
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		b.WriteString(text[i : i+size])
		i += size
	}
	return b.String()
}

// Parse reads a RecognitionConfig in JSON (camelCase or snake_case keys)
// and checks it. code is 0, 400, or 501 for what is not served locally.
func Parse(raw []byte) (c Config, code int, err error) {
	d, lang, opt, code, err := parse(raw)
	if code != 0 {
		return Config{}, code, err
	}
	var generic any
	_ = json.Unmarshal(raw, &generic)
	norm, _ := json.Marshal(camelKeys(generic))
	var rc recognitionConfig
	_ = json.Unmarshal(norm, &rc)
	c = Config{Decoding: d, Language: lang, Options: opt, Model: rc.Model, Codes: rc.LanguageCodes}
	if rc.TranscriptNormalization != nil {
		c.Normalization = rc.TranscriptNormalization.Entries
	}
	if len(rc.Adaptation) > 0 && string(rc.Adaptation) != "null" {
		c.Adaptation = rc.Adaptation
	}
	if !strings.EqualFold(rc.LanguageCodes[0], "auto") {
		c.LanguageCode = strings.ToLower(rc.LanguageCodes[0])
	}
	if len(rc.LanguageCodes) > 1 {
		c.LanguageCode = ""
		for _, l := range rc.LanguageCodes {
			c.Candidates = append(c.Candidates, Candidate{Code: strings.ToLower(l), Whisper: whisperLanguage(l)})
		}
	}
	if len(rc.DenoiserConfig) > 0 && string(rc.DenoiserConfig) != "null" {
		var dc denoiserConfig
		_ = json.Unmarshal(rc.DenoiserConfig, &dc)
		c.Denoise, c.SNRThreshold = dc.DenoiseAudio, dc.SnrThreshold
	}
	if len(rc.TranslationConfig) > 0 && string(rc.TranslationConfig) != "null" {
		var tc struct {
			TargetLanguage string `json:"targetLanguage"`
		}
		_ = json.Unmarshal(rc.TranslationConfig, &tc)
		c.TranslateTo = tc.TargetLanguage
	}
	return c, 0, nil
}

// speechModels are the recognition models served: Google's v2 general
// and telephony models, each read with the same local whisper.cpp model
// (https://docs.cloud.google.com/speech-to-text/docs/speech-to-text-supported-languages
// and .../transcription-model, read 2026-11-19), so what each recognises
// is whisper's, not Google's model's. The medical models (medical_dictation,
// medical_conversation) are not served: whisper has no medical vocabulary
// or their spoken-punctuation dictation behaviour.
var speechModels = map[string]bool{
	"chirp": true, "chirp_2": true, "chirp_3": true, "chirp_telephony": true,
	"long": true, "short": true, "telephony": true, "telephony_short": true,
}

// Models are the served model names, sorted.
func Models() []string {
	out := make([]string, 0, len(speechModels))
	for m := range speechModels {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// whisperLanguage maps a BCP-47 code to whisper's: its primary subtag,
// with the codes whisper names differently.
func whisperLanguage(code string) string {
	primary, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(code)), "-")
	switch primary {
	case "cmn":
		return "zh"
	case "iw":
		return "he"
	case "fil":
		return "tl"
	case "nb":
		return "no"
	case "jv":
		return "jw"
	}
	return primary
}

// camelKeys rewrites a JSON value's snake_case keys as lowerCamelCase, as
// protobuf's JSON parser accepts both.
// CamelKeys rewrites a decoded JSON value's snake_case keys as protojson's
// lowerCamelCase, so configs written either way compare and merge.
func CamelKeys(v any) any { return camelKeys(v) }

func camelKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			parts := strings.Split(k, "_")
			for i := 1; i < len(parts); i++ {
				if parts[i] != "" {
					parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
				}
			}
			key := strings.Join(parts, "")
			out[key] = enumName(key, camelKeys(e))
		}
		return out
	case []any:
		for i := range x {
			x[i] = camelKeys(x[i])
		}
	}
	return v
}

// The enums a RecognitionConfig carries, by number: the official REST
// clients write enums as numbers (protojson's UseEnumNumbers), which
// protobuf's JSON parser accepts as well as names
// (google.cloud.speech.v2's ExplicitDecodingConfig.AudioEncoding and
// RecognitionFeatures.MultiChannelMode).
var (
	audioEncodingNames = []string{"AUDIO_ENCODING_UNSPECIFIED", "LINEAR16", "MULAW", "ALAW", "AMR", "AMR_WB", "FLAC", "MP3",
		"OGG_OPUS", "WEBM_OPUS", "MP4_AAC", "M4A_AAC", "MOV_AAC"}
	multiChannelModeNames = []string{"MULTI_CHANNEL_MODE_UNSPECIFIED", "SEPARATE_RECOGNITION_PER_CHANNEL"}
)

// enumName is v, an enum field's JSON value, as its name when it is a
// known number.
func enumName(key string, v any) any {
	n, ok := v.(float64)
	if !ok || n != float64(int(n)) || n < 0 {
		return v
	}
	var names []string
	switch key {
	case "encoding":
		names = audioEncodingNames
	case "multiChannelMode":
		names = multiChannelModeNames
	}
	if int(n) < len(names) {
		return names[int(n)]
	}
	return v
}

// recognitionConfig is the part of a RecognitionConfig read here.
type recognitionConfig struct {
	Model                  string          `json:"model"`
	LanguageCodes          []string        `json:"languageCodes"`
	AutoDecodingConfig     json.RawMessage `json:"autoDecodingConfig"`
	ExplicitDecodingConfig *struct {
		Encoding          string `json:"encoding"`
		SampleRateHertz   int    `json:"sampleRateHertz"`
		AudioChannelCount int    `json:"audioChannelCount"`
	} `json:"explicitDecodingConfig"`
	Features *struct {
		ProfanityFilter            bool            `json:"profanityFilter"`
		EnableWordTimeOffsets      bool            `json:"enableWordTimeOffsets"`
		EnableWordConfidence       bool            `json:"enableWordConfidence"`
		EnableAutomaticPunctuation bool            `json:"enableAutomaticPunctuation"`
		EnableSpokenPunctuation    bool            `json:"enableSpokenPunctuation"`
		EnableSpokenEmojis         bool            `json:"enableSpokenEmojis"`
		MultiChannelMode           string          `json:"multiChannelMode"`
		DiarizationConfig          json.RawMessage `json:"diarizationConfig"`
		MaxAlternatives            int             `json:"maxAlternatives"`
	} `json:"features"`
	Adaptation              json.RawMessage `json:"adaptation"`
	TranscriptNormalization *struct {
		Entries []NormalizationEntry `json:"entries"`
	} `json:"transcriptNormalization"`
	TranslationConfig json.RawMessage `json:"translationConfig"`
	DenoiserConfig    json.RawMessage `json:"denoiserConfig"`
}

// parse reads a RecognitionConfig in JSON (camelCase or
// snake_case keys) and checks it: the decoding, the whisper language ("" to
// detect), the word options, and an error with its HTTP status (400, or 501
// for what is not served).
func parse(raw []byte) (Decoding, string, Options, int, error) {
	var d Decoding
	var opt Options
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return d, "", opt, 400, fmt.Errorf("invalid RecognitionConfig: %v", err)
	}
	if _, ok := generic.(map[string]any); !ok {
		return d, "", opt, 400, errors.New("invalid RecognitionConfig: expected a JSON object")
	}
	norm, _ := json.Marshal(camelKeys(generic))
	var c recognitionConfig
	dec := json.NewDecoder(bytes.NewReader(norm))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return d, "", opt, 400, fmt.Errorf("invalid RecognitionConfig: %v", err)
	}
	unimpl := func(what string) (Decoding, string, Options, int, error) {
		return d, "", opt, 501, fmt.Errorf("Not implemented here: %s of Speech-to-Text: CloudBurrow transcribes locally with whisper.cpp, "+
			"which has no equivalent, and never calls Google", what)
	}
	switch {
	case c.Model == "":
		return d, "", opt, 400, errors.New("RecognitionConfig.model is required: the default recognizer has no model")
	case !speechModels[c.Model]:
		return unimpl("the " + strconv.Quote(c.Model) + " model (served: " + strings.Join(Models(), ", ") + ")")
	case len(c.LanguageCodes) == 0:
		return d, "", opt, 400, errors.New("RecognitionConfig.languageCodes is required: the default recognizer has no language")
	case len(c.LanguageCodes) > maxLanguageCodes:
		return d, "", opt, 400, fmt.Errorf("languageCodes has %d entries: at most %d languages can be listed", len(c.LanguageCodes), maxLanguageCodes)
	case len(c.LanguageCodes) > 1 && !multiLanguageModels[c.Model]:
		return d, "", opt, 400, fmt.Errorf("several languageCodes are only supported by the long, short and telephony models, not %q", c.Model)
	case c.TranscriptNormalization != nil && len(c.TranscriptNormalization.Entries) > maxNormalization:
		return d, "", opt, 400, fmt.Errorf("transcriptNormalization has %d entries: at most %d are allowed", len(c.TranscriptNormalization.Entries), maxNormalization)
	case c.TranslationConfig != nil && string(c.TranslationConfig) != "null":
		if code, err := checkTranslation(c.Model, c.LanguageCodes, c.TranslationConfig); err != nil {
			return d, "", opt, code, err
		}
	case c.DenoiserConfig != nil && string(c.DenoiserConfig) != "null":
		if code, err := checkDenoiser(c.DenoiserConfig); err != nil {
			return d, "", opt, code, err
		}
	case c.AutoDecodingConfig != nil && c.ExplicitDecodingConfig != nil:
		return d, "", opt, 400, errors.New("RecognitionConfig takes one of autoDecodingConfig and explicitDecodingConfig")
	case c.AutoDecodingConfig == nil && c.ExplicitDecodingConfig == nil:
		return d, "", opt, 400, errors.New("RecognitionConfig requires autoDecodingConfig or explicitDecodingConfig")
	}
	if tn := c.TranscriptNormalization; tn != nil {
		for i, e := range tn.Entries {
			switch {
			case e.Search == "":
				return d, "", opt, 400, fmt.Errorf("transcriptNormalization.entries[%d].search is empty", i)
			case utf8.RuneCountInString(e.Search) > maxNormalization:
				return d, "", opt, 400, fmt.Errorf("transcriptNormalization.entries[%d].search is longer than %d characters", i, maxNormalization)
			case utf8.RuneCountInString(e.Replace) > maxNormalization:
				return d, "", opt, 400, fmt.Errorf("transcriptNormalization.entries[%d].replace is longer than %d characters", i, maxNormalization)
			}
		}
	}
	if f := c.Features; f != nil {
		switch {
		case f.MultiChannelMode != "" && f.MultiChannelMode != "MULTI_CHANNEL_MODE_UNSPECIFIED" && f.MultiChannelMode != "SEPARATE_RECOGNITION_PER_CHANNEL":
			return d, "", opt, 400, fmt.Errorf("invalid features.multiChannelMode %q", f.MultiChannelMode)
		case f.MaxAlternatives < 0 || f.MaxAlternatives > 30:
			return d, "", opt, 400, fmt.Errorf("features.maxAlternatives %d is out of range: valid values are 0-30", f.MaxAlternatives)
		}
		// v2's minSpeakerCount and maxSpeakerCount: "The system
		// automatically determines the number of speakers. This value is
		// not currently used" (SpeakerDiarizationConfig, v2 reference, read
		// 2026-11-21), so they are type-checked and ignored; v1, whose
		// reference does use them, sets MinSpeakers and MaxSpeakers itself.
		if len(f.DiarizationConfig) > 0 && string(f.DiarizationConfig) != "null" {
			var dz struct {
				Min      int32 `json:"minSpeakerCount"`
				Max      int32 `json:"maxSpeakerCount"`
				MinSnake int32 `json:"min_speaker_count"`
				MaxSnake int32 `json:"max_speaker_count"`
			}
			if err := json.Unmarshal(f.DiarizationConfig, &dz); err != nil {
				return d, "", opt, 400, fmt.Errorf("invalid features.diarizationConfig: %v", err)
			}
		}
		opt = Options{Words: f.EnableWordTimeOffsets || f.EnableWordConfidence, WordOffsets: f.EnableWordTimeOffsets, WordConfidence: f.EnableWordConfidence,
			PerChannel:      f.MultiChannelMode == "SEPARATE_RECOGNITION_PER_CHANNEL",
			ProfanityFilter: f.ProfanityFilter, SpokenPunctuation: f.EnableSpokenPunctuation, SpokenEmojis: f.EnableSpokenEmojis,
			AutomaticPunctuation: f.EnableAutomaticPunctuation, Diarization: f.DiarizationConfig != nil && string(f.DiarizationConfig) != "null"}
	}
	if e := c.ExplicitDecodingConfig; e != nil {
		served, known := Encodings[e.Encoding]
		switch {
		case e.Encoding == "" || e.Encoding == "AUDIO_ENCODING_UNSPECIFIED":
			return d, "", opt, 400, errors.New("explicitDecodingConfig.encoding is required")
		case !known:
			return d, "", opt, 400, fmt.Errorf("invalid explicitDecodingConfig.encoding %q", e.Encoding)
		case !served:
			return d, "", opt, 501, fmt.Errorf("Not implemented here: %s audio has no local decoder in CloudBurrow, and it never calls Google", e.Encoding)
		case e.Encoding == "AMR" && e.SampleRateHertz != 0 && e.SampleRateHertz != 8000:
			// "AMR: Adaptive Multi-Rate Narrowband codec. sample_rate_hertz
			// must be 8000." (ExplicitDecodingConfig.AudioEncoding, read
			// 2026-11-20), and AMR_WB's 16000.
			return d, "", opt, 400, fmt.Errorf("explicitDecodingConfig.sampleRateHertz must be 8000 for AMR, not %d", e.SampleRateHertz)
		case e.Encoding == "AMR_WB" && e.SampleRateHertz != 0 && e.SampleRateHertz != 16000:
			return d, "", opt, 400, fmt.Errorf("explicitDecodingConfig.sampleRateHertz must be 16000 for AMR_WB, not %d", e.SampleRateHertz)
		}
		d = Decoding{Encoding: e.Encoding, SampleRate: e.SampleRateHertz, Channels: e.AudioChannelCount}
	} else {
		d.Auto = true
	}
	if len(c.LanguageCodes) > 1 {
		for _, l := range c.LanguageCodes {
			if strings.EqualFold(l, "auto") || whisperLanguage(l) == "" {
				return d, "", opt, 400, fmt.Errorf("invalid language code %q among several languageCodes", l)
			}
		}
		return d, "", opt, 0, nil // detected among them
	}
	lang := c.LanguageCodes[0]
	if strings.EqualFold(lang, "auto") {
		return d, "", opt, 0, nil
	}
	w := whisperLanguage(lang)
	if w == "" {
		return d, "", opt, 400, fmt.Errorf("invalid language code %q", lang)
	}
	return d, w, opt, 0, nil
}
