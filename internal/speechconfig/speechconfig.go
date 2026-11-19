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
	"strconv"
	"strings"
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
}

// Encodings are ExplicitDecodingConfig's AudioEncoding values, true for
// those decoded locally (internal/speech's Decode).
var Encodings = map[string]bool{
	"LINEAR16": true, "MULAW": true, "ALAW": true, "MP3": true, "FLAC": true,
	"MP4_AAC": true, "M4A_AAC": true, "MOV_AAC": true,
	"AMR": false, "AMR_WB": false, "OGG_OPUS": false, "WEBM_OPUS": false,
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
	c = Config{Decoding: d, Language: lang, Options: opt, Model: rc.Model}
	if !strings.EqualFold(rc.LanguageCodes[0], "auto") {
		c.LanguageCode = strings.ToLower(rc.LanguageCodes[0])
	}
	return c, 0, nil
}

// speechModels are the recognition models served.
var speechModels = map[string]bool{"chirp": true}

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
			out[strings.Join(parts, "")] = camelKeys(e)
		}
		return out
	case []any:
		for i := range x {
			x[i] = camelKeys(x[i])
		}
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
	TranscriptNormalization json.RawMessage `json:"transcriptNormalization"`
	TranslationConfig       json.RawMessage `json:"translationConfig"`
	DenoiserConfig          json.RawMessage `json:"denoiserConfig"`
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
		return unimpl("the " + strconv.Quote(c.Model) + " model (only chirp is served)")
	case len(c.LanguageCodes) == 0:
		return d, "", opt, 400, errors.New("RecognitionConfig.languageCodes is required: the default recognizer has no language")
	case len(c.LanguageCodes) > 1:
		return unimpl("recognising one of several languageCodes")
	case c.Adaptation != nil:
		return unimpl("adaptation")
	case c.TranscriptNormalization != nil:
		return unimpl("transcriptNormalization")
	case c.TranslationConfig != nil:
		return unimpl("translationConfig")
	case c.DenoiserConfig != nil:
		return unimpl("denoiserConfig")
	case c.AutoDecodingConfig != nil && c.ExplicitDecodingConfig != nil:
		return d, "", opt, 400, errors.New("RecognitionConfig takes one of autoDecodingConfig and explicitDecodingConfig")
	case c.AutoDecodingConfig == nil && c.ExplicitDecodingConfig == nil:
		return d, "", opt, 400, errors.New("RecognitionConfig requires autoDecodingConfig or explicitDecodingConfig")
	}
	if f := c.Features; f != nil {
		switch {
		case f.ProfanityFilter:
			return unimpl("features.profanityFilter")
		case f.EnableSpokenPunctuation:
			return unimpl("features.enableSpokenPunctuation")
		case f.EnableSpokenEmojis:
			return unimpl("features.enableSpokenEmojis")
		case f.DiarizationConfig != nil:
			return unimpl("features.diarizationConfig")
		case f.MultiChannelMode == "SEPARATE_RECOGNITION_PER_CHANNEL":
			return unimpl("features.multiChannelMode SEPARATE_RECOGNITION_PER_CHANNEL")
		case f.MultiChannelMode != "" && f.MultiChannelMode != "MULTI_CHANNEL_MODE_UNSPECIFIED":
			return d, "", opt, 400, fmt.Errorf("invalid features.multiChannelMode %q", f.MultiChannelMode)
		case f.MaxAlternatives < 0 || f.MaxAlternatives > 30:
			return d, "", opt, 400, fmt.Errorf("features.maxAlternatives %d is out of range: valid values are 0-30", f.MaxAlternatives)
		}
		opt = Options{Words: f.EnableWordTimeOffsets || f.EnableWordConfidence, WordOffsets: f.EnableWordTimeOffsets, WordConfidence: f.EnableWordConfidence}
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
		}
		d = Decoding{Encoding: e.Encoding, SampleRate: e.SampleRateHertz, Channels: e.AudioChannelCount}
	} else {
		d.Auto = true
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
