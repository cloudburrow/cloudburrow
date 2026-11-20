package speechconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Several languageCodes (#1799), as "Automatically detect language"
// documents them (https://docs.cloud.google.com/speech-to-text/docs/multiple-languages,
// read 2026-11-20): "you can provide one or more languages that the audio
// data might include in the language_codes field ... Cloud Speech-to-Text
// attempts to transcribe the audio using the best-fit language from the
// list ... then labels the transcription results with the predicted
// language code. ... You can list up to three languages", "only available
// in the Cloud Speech-to-Text global region and the us and eu
// multi-regions" and "only ... with the long, short, and telephony models".

// maxLanguageCodes is the most languageCodes a config can list.
const maxLanguageCodes = 3

// multiLanguageModels are the models that take several languageCodes.
var multiLanguageModels = map[string]bool{"long": true, "short": true, "telephony": true}

// MultiLanguageLocations are the locations that take several
// languageCodes.
var MultiLanguageLocations = map[string]bool{"global": true, "us": true, "eu": true}

// Speech translation (translationConfig, #1799), as Chirp 2's page
// documents it (https://docs.cloud.google.com/speech-to-text/docs/models/chirp-2,
// read 2026-11-20): Chirp 2 is the model that translates, "The model
// automatically translates from the spoken language to the target
// language", between the language pairs its "Language availability for
// translation" tables list (chirp2TranslationPairs), its sample asking for
// the spoken language as the target too (fr-FR to fr-FR). "In the case of
// translation, confidence scores are not returned."

// chirp2TranslationPairs are Chirp 2's source -> target pairs, lower case.
var chirp2TranslationPairs = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range strings.Fields("ar-eg ar-x-gulf ar-x-levant ar-x-maghrebi ca-es cy-gb de-de es-419 es-es es-us et-ee " +
		"fr-ca fr-fr fa-ir id-id it-it ja-jp lv-lv mn-mn nl-nl pt-br ru-ru sl-si sv-se ta-in tr-tr cmn-hans-cn") {
		m[c+">en-us"] = true
	}
	for _, c := range strings.Fields("ar-eg ar-x-gulf ar-x-levant ar-x-maghrebi ca-es cy-gb de-de et-ee fa-ir id-id ja-jp " +
		"lv-lv mn-mn sl-si sv-se ta-in tr-tr cmn-hans-cn") {
		m["en-us>"+c] = true
	}
	return m
}()

// checkTranslation checks a translationConfig: Chirp 2's, one of its
// pairs (or the spoken language itself), from the one languageCodes entry.
func checkTranslation(model string, codes []string, raw json.RawMessage) (int, error) {
	var tc struct {
		TargetLanguage string `json:"targetLanguage"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return 400, fmt.Errorf("invalid translationConfig: %v", err)
	}
	switch {
	case tc.TargetLanguage == "":
		return 400, errors.New("translationConfig.targetLanguage is required")
	case model != "chirp_2":
		return 400, fmt.Errorf("translationConfig is supported by the chirp_2 model, not %q", model)
	case len(codes) != 1 || strings.EqualFold(codes[0], "auto"):
		return 400, errors.New("translationConfig needs the spoken language: one languageCodes entry")
	}
	src, tgt := strings.ToLower(codes[0]), strings.ToLower(tc.TargetLanguage)
	if src != tgt && !chirp2TranslationPairs[src+">"+tgt] {
		return 400, fmt.Errorf("translation from %s to %s is not supported by chirp_2", codes[0], tc.TargetLanguage)
	}
	return 0, nil
}
