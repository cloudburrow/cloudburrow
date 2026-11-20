package speechconfig

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	c, code, err := Parse([]byte(`{"language_codes": ["en-US"], "model": "chirp", "auto_decoding_config": {},
		"features": {"enable_word_time_offsets": true}}`))
	if code != 0 || err != nil {
		t.Fatalf("snake_case config: %d %v", code, err)
	}
	if !c.Decoding.Auto || c.Language != "en" || c.LanguageCode != "en-us" || !c.Words || !c.WordOffsets || c.WordConfidence || c.Model != "chirp" {
		t.Errorf("config %+v", c)
	}
	c, code, _ = Parse([]byte(`{"languageCodes": ["cmn-Hans-CN"], "model": "chirp",
		"explicitDecodingConfig": {"encoding": "MULAW", "sampleRateHertz": 8000, "audioChannelCount": 1}}`))
	if code != 0 || c.Language != "zh" || c.Decoding.Auto || c.Decoding.Encoding != "MULAW" || c.Decoding.SampleRate != 8000 {
		t.Errorf("camelCase config: %d %+v", code, c)
	}
	if c, code, _ := Parse([]byte(`{"languageCodes": ["auto"], "model": "chirp", "autoDecodingConfig": {}}`)); code != 0 || c.Language != "" || c.LanguageCode != "" {
		t.Errorf("auto: %d %+v", code, c)
	}
	for _, tc := range []struct {
		cfg  string
		code int
		want string
	}{
		{`{"languageCodes": ["en-US"], "autoDecodingConfig": {}}`, 400, "model is required"},
		{`{"model": "chirp", "autoDecodingConfig": {}}`, 400, "languageCodes is required"},
		{`{"model": "chirp", "languageCodes": ["en-US"]}`, 400, "autoDecodingConfig or explicitDecodingConfig"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}, "explicitDecodingConfig": {"encoding": "LINEAR16"}}`, 400, "one of"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}, "bogus": 1}`, 400, "unknown field"},
		{`{"model": "medical_dictation", "languageCodes": ["en-US"], "autoDecodingConfig": {}}`, 501, "served: chirp, chirp_2"},
		{`{"model": "chirp", "languageCodes": ["en-US", "de-DE"], "autoDecodingConfig": {}}`, 501, "several"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}, "features": {"profanityFilter": true}}`, 501, "profanityFilter"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}, "features": {"diarizationConfig": {}}}`, 501, "diarizationConfig"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}, "translationConfig": {"targetLanguage": "de"}}`, 501, "translationConfig"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "explicitDecodingConfig": {"encoding": "AMR_WB"}}`, 501, "AMR_WB"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "explicitDecodingConfig": {"encoding": "WAV"}}`, 400, "invalid explicitDecodingConfig.encoding"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}, "features": {"maxAlternatives": 31}}`, 400, "maxAlternatives"},
		{`[1]`, 400, "JSON object"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}, "transcriptNormalization": {"entries": [{"replace": "x"}]}}`, 400, "search is empty"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}, "transcriptNormalization": {"entries": [{"search": "x", "replace": "` + strings.Repeat("y", 101) + `"}]}}`, 400, "replace is longer than 100"},
		{`{"model": "chirp", "languageCodes": ["en-US"], "autoDecodingConfig": {}, "transcriptNormalization": {"entries": [` + strings.Repeat(`{"search": "a"},`, 100) + `{"search": "b"}]}}`, 400, "at most 100"},
	} {
		_, code, err := Parse([]byte(tc.cfg))
		if code != tc.code || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %d %v, want %d %q", tc.cfg, code, err, tc.code, tc.want)
		}
	}
}

// TestNormalize (#1799): transcriptNormalization's entries apply in order,
// case-insensitively unless caseSensitive, an earlier one keeping a later
// one from matching (the reference's "cat" before "mountain cat").
func TestNormalize(t *testing.T) {
	c, code, err := Parse([]byte(`{"model": "chirp", "language_codes": ["en-US"], "auto_decoding_config": {}, "transcript_normalization": {"entries": [
		{"search": "cat", "replace": "dog"}, {"search": "mountain cat", "replace": "puma"},
		{"search": "Country", "replace": "nation", "case_sensitive": true}, {"search": "ÉTÉ", "replace": "summer"}]}}`))
	if code != 0 || err != nil || len(c.Normalization) != 4 {
		t.Fatalf("Parse: %d %v %+v", code, err, c.Normalization)
	}
	for in, want := range map[string]string{
		"A mountain Cat and a CAT.":             "A mountain dog and a dog.",
		"ask not what your country can do":      "ask not what your country can do",
		"ask what you can do for your Country.": "ask what you can do for your nation.",
		"en été":                                "en summer",
	} {
		if got := c.Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
